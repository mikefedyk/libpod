package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/buildah"
	"github.com/containers/buildah/imagebuildah"
	buildahcli "github.com/containers/buildah/pkg/cli"
	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/libpod/cmd/podman/libpodruntime"
	"github.com/containers/libpod/pkg/rootless"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

var (
	layerFlags = []cli.Flag{
		cli.BoolTFlag{
			Name:  "force-rm",
			Usage: "Always remove intermediate containers after a build, even if the build is unsuccessful. (default true)",
		},
		cli.BoolTFlag{
			Name:  "layers",
			Usage: "cache intermediate layers during build. Use BUILDAH_LAYERS environment variable to override. ",
		},
	}
	buildDescription = "Builds an OCI or Docker image using instructions from one\n" +
		"or more Dockerfiles and a specified build context directory."
	buildCommand = cli.Command{
		Name:           "build",
		Usage:          "Build an image using instructions from Dockerfiles",
		Description:    buildDescription,
		Flags:          sortFlags(append(append(buildahcli.BudFlags, layerFlags...), buildahcli.FromAndBudFlags...)),
		Action:         buildCmd,
		ArgsUsage:      "CONTEXT-DIRECTORY | URL",
		SkipArgReorder: true,
		OnUsageError:   usageErrorHandler,
	}
)

func getDockerfiles(files []string) []string {
	var dockerfiles []string
	for _, f := range files {
		if f == "-" {
			dockerfiles = append(dockerfiles, "/dev/stdin")
		} else {
			dockerfiles = append(dockerfiles, f)
		}
	}
	return dockerfiles
}

func buildCmd(c *cli.Context) error {
	// The following was taken directly from containers/buildah/cmd/bud.go
	// TODO Find a away to vendor more of this in rather than copy from bud

	output := ""
	tags := []string{}
	if c.IsSet("tag") || c.IsSet("t") {
		tags = c.StringSlice("tag")
		if len(tags) > 0 {
			output = tags[0]
			tags = tags[1:]
		}
	}
	pullPolicy := imagebuildah.PullNever
	if c.BoolT("pull") {
		pullPolicy = imagebuildah.PullIfMissing
	}
	if c.Bool("pull-always") {
		pullPolicy = imagebuildah.PullAlways
	}

	args := make(map[string]string)
	if c.IsSet("build-arg") {
		for _, arg := range c.StringSlice("build-arg") {
			av := strings.SplitN(arg, "=", 2)
			if len(av) > 1 {
				args[av[0]] = av[1]
			} else {
				delete(args, av[0])
			}
		}
	}

	dockerfiles := getDockerfiles(c.StringSlice("file"))
	format, err := getFormat(c)
	if err != nil {
		return nil
	}
	contextDir := ""
	cliArgs := c.Args()

	layers := c.BoolT("layers") // layers for podman defaults to true
	// Check to see if the BUILDAH_LAYERS environment variable is set and override command-line
	if _, ok := os.LookupEnv("BUILDAH_LAYERS"); ok {
		layers = buildahcli.UseLayers()
	}

	if len(cliArgs) > 0 {
		// The context directory could be a URL.  Try to handle that.
		tempDir, subDir, err := imagebuildah.TempDirForURL("", "buildah", cliArgs[0])
		if err != nil {
			return errors.Wrapf(err, "error prepping temporary context directory")
		}
		if tempDir != "" {
			// We had to download it to a temporary directory.
			// Delete it later.
			defer func() {
				if err = os.RemoveAll(tempDir); err != nil {
					logrus.Errorf("error removing temporary directory %q: %v", contextDir, err)
				}
			}()
			contextDir = filepath.Join(tempDir, subDir)
		} else {
			// Nope, it was local.  Use it as is.
			absDir, err := filepath.Abs(cliArgs[0])
			if err != nil {
				return errors.Wrapf(err, "error determining path to directory %q", cliArgs[0])
			}
			contextDir = absDir
		}
		cliArgs = cliArgs.Tail()
	} else {
		// No context directory or URL was specified.  Try to use the
		// home of the first locally-available Dockerfile.
		for i := range dockerfiles {
			if strings.HasPrefix(dockerfiles[i], "http://") ||
				strings.HasPrefix(dockerfiles[i], "https://") ||
				strings.HasPrefix(dockerfiles[i], "git://") ||
				strings.HasPrefix(dockerfiles[i], "github.com/") {
				continue
			}
			absFile, err := filepath.Abs(dockerfiles[i])
			if err != nil {
				return errors.Wrapf(err, "error determining path to file %q", dockerfiles[i])
			}
			contextDir = filepath.Dir(absFile)
			dockerfiles[i], err = filepath.Rel(contextDir, absFile)
			if err != nil {
				return errors.Wrapf(err, "error determining path to file %q", dockerfiles[i])
			}
			break
		}
	}
	if contextDir == "" {
		return errors.Errorf("no context directory specified, and no dockerfile specified")
	}
	if len(dockerfiles) == 0 {
		dockerfiles = append(dockerfiles, filepath.Join(contextDir, "Dockerfile"))
	}
	if err := parse.ValidateFlags(c, buildahcli.BudFlags); err != nil {
		return err
	}

	runtimeFlags := []string{}
	for _, arg := range c.StringSlice("runtime-flag") {
		runtimeFlags = append(runtimeFlags, "--"+arg)
	}
	// end from buildah

	runtime, err := libpodruntime.GetRuntime(c)
	if err != nil {
		return errors.Wrapf(err, "could not get runtime")
	}
	defer runtime.Shutdown(false)

	var stdout, stderr, reporter *os.File
	stdout = os.Stdout
	stderr = os.Stderr
	reporter = os.Stderr
	if c.IsSet("logfile") {
		f, err := os.OpenFile(c.String("logfile"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return errors.Errorf("error opening logfile %q: %v", c.String("logfile"), err)
		}
		defer f.Close()
		logrus.SetOutput(f)
		stdout = f
		stderr = f
		reporter = f
	}

	systemContext, err := parse.SystemContextFromOptions(c)
	if err != nil {
		return errors.Wrapf(err, "error building system context")
	}
	systemContext.AuthFilePath = getAuthFile(c.String("authfile"))
	commonOpts, err := parse.CommonBuildOptions(c)
	if err != nil {
		return err
	}

	namespaceOptions, networkPolicy, err := parse.NamespaceOptions(c)
	if err != nil {
		return errors.Wrapf(err, "error parsing namespace-related options")
	}
	usernsOption, idmappingOptions, err := parse.IDMappingOptions(c)
	if err != nil {
		return errors.Wrapf(err, "error parsing ID mapping options")
	}
	namespaceOptions.AddOrReplace(usernsOption...)

	options := imagebuildah.BuildOptions{
		ContextDirectory:        contextDir,
		PullPolicy:              pullPolicy,
		Compression:             imagebuildah.Gzip,
		Quiet:                   c.Bool("quiet"),
		SignaturePolicyPath:     c.String("signature-policy"),
		Args:                    args,
		Output:                  output,
		AdditionalTags:          tags,
		Out:                     stdout,
		Err:                     stderr,
		ReportWriter:            reporter,
		Runtime:                 c.String("runtime"),
		RuntimeArgs:             runtimeFlags,
		OutputFormat:            format,
		SystemContext:           systemContext,
		NamespaceOptions:        namespaceOptions,
		ConfigureNetwork:        networkPolicy,
		CNIPluginPath:           c.String("cni-plugin-path"),
		CNIConfigDir:            c.String("cni-config-dir"),
		IDMappingOptions:        idmappingOptions,
		CommonBuildOpts:         commonOpts,
		DefaultMountsFilePath:   c.GlobalString("default-mounts-file"),
		IIDFile:                 c.String("iidfile"),
		Squash:                  c.Bool("squash"),
		Labels:                  c.StringSlice("label"),
		Annotations:             c.StringSlice("annotation"),
		Layers:                  layers,
		NoCache:                 c.Bool("no-cache"),
		RemoveIntermediateCtrs:  c.BoolT("rm"),
		ForceRmIntermediateCtrs: c.BoolT("force-rm"),
	}

	if c.Bool("quiet") {
		options.ReportWriter = ioutil.Discard
	}

	if rootless.IsRootless() {
		options.Isolation = buildah.IsolationOCIRootless
	}

	return runtime.Build(getContext(), options, dockerfiles...)
}
