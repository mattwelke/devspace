package cmd

import (
	"context"
	"fmt"
	"github.com/loft-sh/devspace/cmd/flags"
	"github.com/loft-sh/devspace/pkg/devspace/build"
	config2 "github.com/loft-sh/devspace/pkg/devspace/config"
	"github.com/loft-sh/devspace/pkg/devspace/config/loader"
	"github.com/loft-sh/devspace/pkg/devspace/config/localcache"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/devspace/context/values"
	"github.com/loft-sh/devspace/pkg/devspace/dependency"
	"github.com/loft-sh/devspace/pkg/devspace/dependency/registry"
	"github.com/loft-sh/devspace/pkg/devspace/deploy"
	"github.com/loft-sh/devspace/pkg/devspace/dev"
	"github.com/loft-sh/devspace/pkg/devspace/devpod"
	"github.com/loft-sh/devspace/pkg/devspace/hook"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	fakekube "github.com/loft-sh/devspace/pkg/devspace/kubectl/testing"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline"
	"github.com/loft-sh/devspace/pkg/devspace/pipeline/types"
	"github.com/loft-sh/devspace/pkg/devspace/plugin"
	"github.com/loft-sh/devspace/pkg/devspace/upgrade"
	"github.com/loft-sh/devspace/pkg/util/factory"
	"github.com/loft-sh/devspace/pkg/util/interrupt"
	"github.com/loft-sh/devspace/pkg/util/log"
	"github.com/loft-sh/devspace/pkg/util/message"
	"github.com/loft-sh/devspace/pkg/util/ptr"
	"github.com/loft-sh/devspace/pkg/util/survey"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"io"
	"io/ioutil"
	"k8s.io/client-go/kubernetes/fake"
	"os"
)

// RunPipelineCmd holds the command flags
type RunPipelineCmd struct {
	*flags.GlobalFlags

	Tags                    []string
	Render                  bool
	Pipeline                string
	SkipPush                bool
	SkipPushLocalKubernetes bool

	Dependency     []string
	SkipDependency []string

	ForceBuild          bool
	SkipBuild           bool
	BuildSequential     bool
	MaxConcurrentBuilds int

	ForcePurge bool

	ForceDeploy bool
	SkipDeploy  bool

	Terminal bool

	ShowUI bool

	// used for testing to allow interruption
	Ctx          context.Context
	RenderWriter io.Writer

	configLoader loader.ConfigLoader
	log          log.Logger
}

func (cmd *RunPipelineCmd) AddFlags(command *cobra.Command) {
	command.Flags().StringSliceVar(&cmd.SkipDependency, "skip-dependency", cmd.SkipDependency, "Skips the following dependencies for deployment")
	command.Flags().StringSliceVar(&cmd.Dependency, "dependency", cmd.Dependency, "Deploys only the specified named dependencies")

	command.Flags().BoolVarP(&cmd.ForceBuild, "force-build", "b", cmd.ForceBuild, "Forces to build every image")
	command.Flags().BoolVar(&cmd.SkipBuild, "skip-build", cmd.SkipBuild, "Skips building of images")
	command.Flags().BoolVar(&cmd.BuildSequential, "build-sequential", cmd.BuildSequential, "Builds the images one after another instead of in parallel")
	command.Flags().IntVar(&cmd.MaxConcurrentBuilds, "max-concurrent-builds", cmd.MaxConcurrentBuilds, "The maximum number of image builds built in parallel (0 for infinite)")
	command.Flags().BoolVar(&cmd.Render, "render", cmd.Render, "If true will render manifests and print them instead of actually deploying them")

	command.Flags().BoolVar(&cmd.ForcePurge, "force-purge", cmd.ForcePurge, "Forces to purge every deployment even though it might be in use by another DevSpace project")
	command.Flags().BoolVarP(&cmd.ForceDeploy, "force-deploy", "d", cmd.ForceDeploy, "Forces to deploy every deployment")
	command.Flags().BoolVar(&cmd.SkipDeploy, "skip-deploy", cmd.SkipDeploy, "If enabled will skip deploying")
	command.Flags().StringVar(&cmd.Pipeline, "pipeline", cmd.Pipeline, "The pipeline to execute")

	command.Flags().StringSliceVarP(&cmd.Tags, "tag", "t", cmd.Tags, "Use the given tag for all built images")
	command.Flags().BoolVar(&cmd.SkipPush, "skip-push", cmd.SkipPush, "Skips image pushing, useful for minikube deployment")
	command.Flags().BoolVar(&cmd.SkipPushLocalKubernetes, "skip-push-local-kube", cmd.SkipPushLocalKubernetes, "Skips image pushing, if a local kubernetes environment is detected")

	command.Flags().BoolVar(&cmd.Terminal, "terminal", cmd.Terminal, "Open a terminal instead of showing logs")
	command.Flags().BoolVar(&cmd.ShowUI, "show-ui", cmd.ShowUI, "Shows the ui server")
}

// NewRunPipelineCmd creates a new devspace run-pipeline command
func NewRunPipelineCmd(f factory.Factory, globalFlags *flags.GlobalFlags) *cobra.Command {
	cmd := &RunPipelineCmd{
		GlobalFlags:             globalFlags,
		SkipPushLocalKubernetes: true,
	}
	runPipelineCmd := &cobra.Command{
		Use:   "run-pipeline",
		Short: "Starts the development mode",
		Long: `
#######################################################
############## devspace run-pipeline ##################
#######################################################
Execute a pipeline
#######################################################`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			if len(args) == 0 && cmd.Pipeline == "" {
				return fmt.Errorf("please specify a pipeline through --pipeline or argument")
			} else if len(args) == 1 && cmd.Pipeline == "" {
				cmd.Pipeline = args[0]
			}

			return cmd.Run(cobraCmd, args, f, "run-pipeline", "runPipelineCommand")
		},
	}

	cmd.AddFlags(runPipelineCmd)
	return runPipelineCmd
}

func (cmd *RunPipelineCmd) RunDefault(f factory.Factory) error {
	return cmd.Run(nil, nil, f, "run-pipeline", "runPipelineCommand")
}

// Run executes the command logic
func (cmd *RunPipelineCmd) Run(cobraCmd *cobra.Command, args []string, f factory.Factory, commandName, hookName string) error {
	if cmd.log == nil {
		cmd.log = f.GetLog()
	}
	if cmd.Silent {
		cmd.log.SetLevel(logrus.FatalLevel)
	}

	// Print upgrade message if new version available
	if !cmd.Render {
		upgrade.PrintUpgradeMessage(cmd.log)
	} else if cmd.RenderWriter == nil {
		cmd.RenderWriter = os.Stdout
	}

	if cobraCmd != nil {
		plugin.SetPluginCommand(cobraCmd, args)
	}

	if cmd.Ctx == nil {
		var cancelFn context.CancelFunc
		cmd.Ctx, cancelFn = context.WithCancel(context.Background())
		defer cancelFn()
	}

	// set command in context
	cmd.Ctx = values.WithCommand(cmd.Ctx, commandName)
	options := cmd.BuildOptions(cmd.ToConfigOptions())
	ctx, err := initialize(cmd.Ctx, f, false, options, cmd.log)
	if err != nil {
		return err
	}

	return runWithHooks(ctx, hookName, func() error {
		return runPipeline(ctx, options)
	})
}

type CommandOptions struct {
	flags.GlobalFlags
	types.Options

	ConfigOptions *loader.ConfigOptions

	Pipeline string
	Terminal bool
	ShowUI   bool
	UIPort   int
}

func initialize(ctx context.Context, f factory.Factory, allowFailingKubeClient bool, options *CommandOptions, logger log.Logger) (devspacecontext.Context, error) {
	// start file logging
	log.StartFileLogging()

	// create a temporary folder for us to use
	tempFolder, err := ioutil.TempDir("", "devspace-")
	if err != nil {
		return nil, errors.Wrap(err, "create temporary folder")
	}

	// add temp folder to context
	ctx = values.WithTempFolder(ctx, tempFolder)

	// set config root
	configLoader, err := f.NewConfigLoader(options.ConfigPath)
	if err != nil {
		return nil, err
	}
	configExists, err := configLoader.SetDevSpaceRoot(logger)
	if err != nil {
		return nil, err
	} else if !configExists {
		return nil, errors.New(message.ConfigNotFound)
	}

	// create kubectl client
	client, err := f.NewKubeClientFromContext(options.KubeContext, options.Namespace)
	if err != nil {
		if allowFailingKubeClient {
			logger.Warnf("Unable to create new kubectl client: %v", err)
			logger.Warn("Using fake client to render resources")
			logger.WriteString(logrus.WarnLevel, "\n")

			kube := fake.NewSimpleClientset()
			client = &fakekube.Client{
				Client: kube,
			}
		} else {
			return nil, errors.Errorf("error creating Kubernetes client: %v. Please make sure you have a valid Kubernetes context that points to a working Kubernetes cluster. If in doubt, please check if the following command works locally: `kubectl get namespaces`", err)
		}
	}

	// load generated config
	localCache, err := configLoader.LoadLocalCache()
	if err != nil {
		return nil, errors.Errorf("error loading local cache: %v", err)
	}

	// If the current kube context or namespace is different than old,
	// show warnings and reset kube client if necessary
	client, err = kubectl.CheckKubeContext(client, localCache, options.NoWarn, options.SwitchContext, logger)
	if err != nil {
		return nil, err
	}

	// load config
	configInterface, err := configLoader.LoadWithCache(ctx, localCache, client, options.ConfigOptions, logger)
	if err != nil {
		return nil, err
	}

	// add root name to context
	ctx = values.WithRootName(ctx, configInterface.Config().Name)

	// adjust config
	err = adjustConfig(configInterface, options, logger)
	if err != nil {
		return nil, err
	}

	// create devspace context
	devCtx := devspacecontext.NewContext(ctx, configInterface.Variables(), logger).
		WithConfig(configInterface).
		WithKubeClient(client)

	// print config
	if devCtx.Log().GetLevel() == logrus.DebugLevel {
		out, _ := yaml.Marshal(devCtx.Config().Config())
		devCtx.Log().Debugf("Use config:\n%s\n", string(out))
	}

	// resolve dependencies
	dependencies, err := f.NewDependencyManager(devCtx, options.ConfigOptions).ResolveAll(devCtx, dependency.ResolveOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "deploy dependencies")
	}
	devCtx = devCtx.WithDependencies(dependencies)

	// update last used kube context & save generated yaml
	err = updateLastKubeContext(devCtx)
	if err != nil {
		return nil, errors.Wrap(err, "update last kube context")
	}

	return devCtx, nil
}

func updateLastKubeContext(ctx devspacecontext.Context) error {
	// Update generated if we deploy the application
	if ctx.Config() != nil && ctx.Config().LocalCache() != nil {
		ctx.Config().LocalCache().SetLastContext(&localcache.LastContextConfig{
			Context:   ctx.KubeClient().CurrentContext(),
			Namespace: ctx.KubeClient().Namespace(),
		})

		err := ctx.Config().LocalCache().Save()
		if err != nil {
			return errors.Wrap(err, "save generated")
		}
	}

	return nil
}

func adjustConfig(conf config2.Config, options *CommandOptions, log log.Logger) error {
	// check if terminal is enabled
	c := conf.Config()
	if options.Terminal {
		if len(c.Dev) == 0 {
			return errors.New("No dev config available in DevSpace config")
		}

		devNames := make([]string, 0, len(c.Dev))
		for k := range c.Dev {
			devNames = append(devNames, k)
		}

		// if only one image exists, use it, otherwise show image picker
		devName := ""
		if len(devNames) == 1 {
			devName = devNames[0]
		} else {
			var err error
			devName, err = log.Question(&survey.QuestionOptions{
				Question: "Where do you want to open a terminal to?",
				Options:  devNames,
			})
			if err != nil {
				return err
			}
		}

		// adjust dev config
		for k := range c.Dev {
			if k == devName {
				if c.Dev[devName].Terminal == nil {
					c.Dev[devName].Terminal = &latest.Terminal{}
				}
				c.Dev[devName].Terminal.Enabled = ptr.Bool(true)
			} else {
				c.Dev[devName].Terminal = nil
			}
		}
	}

	return nil
}

func runWithHooks(ctx devspacecontext.Context, command string, fn func() error) (err error) {
	err = hook.ExecuteHooks(ctx, nil, command+":before:execute")
	if err != nil {
		return err
	}

	defer func() {
		// delete temp folder
		deleteTempFolder(ctx.Context(), ctx.Log())

		// execute hooks
		if err != nil {
			hook.LogExecuteHooks(ctx, map[string]interface{}{"error": err}, command+":after:execute", command+":error")
		} else {
			err = hook.ExecuteHooks(ctx, nil, command+":after:execute")
		}
	}()

	return interrupt.Global.Run(fn, func() {
		// delete temp folder
		deleteTempFolder(ctx.Context(), ctx.Log())

		// execute hooks
		hook.LogExecuteHooks(ctx, nil, command+":interrupt")
	})
}

func deleteTempFolder(ctx context.Context, log log.Logger) {
	// delete temp folder
	tempFolder, ok := values.TempFolderFrom(ctx)
	if ok && tempFolder != os.TempDir() {
		err := os.RemoveAll(tempFolder)
		if err != nil {
			log.Debugf("error removing temp folder: %v", err)
		}
	}
}

func (cmd *RunPipelineCmd) BuildOptions(configOptions *loader.ConfigOptions) *CommandOptions {
	return &CommandOptions{
		GlobalFlags: *cmd.GlobalFlags,
		Options: types.Options{
			BuildOptions: build.Options{
				Tags:                      cmd.Tags,
				SkipBuild:                 cmd.SkipBuild,
				SkipPush:                  cmd.SkipPush,
				SkipPushOnLocalKubernetes: cmd.SkipPushLocalKubernetes,
				ForceRebuild:              cmd.ForceBuild,
				Sequential:                cmd.BuildSequential,
				MaxConcurrentBuilds:       cmd.MaxConcurrentBuilds,
			},
			DeployOptions: deploy.Options{
				ForceDeploy:  cmd.ForceDeploy,
				Render:       cmd.Render,
				RenderWriter: cmd.RenderWriter,
				SkipDeploy:   cmd.SkipDeploy,
			},
			PurgeOptions: deploy.PurgeOptions{
				ForcePurge: cmd.ForcePurge,
			},
			DependencyOptions: types.DependencyOptions{
				Exclude: cmd.SkipDependency,
				Only:    cmd.Dependency,
			},
		},
		ConfigOptions: configOptions,
		Terminal:      cmd.Terminal,
		Pipeline:      cmd.Pipeline,
		ShowUI:        cmd.ShowUI,
	}
}

func runPipeline(ctx devspacecontext.Context, options *CommandOptions) error {
	var configPipeline *latest.Pipeline
	if ctx.Config().Config().Pipelines != nil && ctx.Config().Config().Pipelines[options.Pipeline] != nil {
		configPipeline = ctx.Config().Config().Pipelines[options.Pipeline]
	} else {
		var err error
		configPipeline, err = types.GetDefaultPipeline(options.Pipeline)
		if err != nil {
			return err
		}
	}

	// marshal pipeline
	configPipelineBytes, err := yaml.Marshal(configPipeline)
	if err == nil {
		ctx.Log().Debugf("Run pipeline:\n%s\n", string(configPipelineBytes))
	}

	// create dev context
	devCtxCancel, cancelDevCtx := context.WithCancel(ctx.Context())
	ctx = ctx.WithContext(values.WithDevContext(ctx.Context(), devCtxCancel))

	// create a new base dev pod manager
	devPodManager := devpod.NewManager(cancelDevCtx)
	defer devPodManager.Close()

	// create dependency registry
	dependencyRegistry := registry.NewDependencyRegistry(options.DeployOptions.Render)

	// get deploy pipeline
	pipe := pipeline.NewPipeline(ctx.Config().Config().Name, devPodManager, dependencyRegistry, configPipeline, options.Options)

	// start ui & open
	serv, err := dev.UI(ctx, options.UIPort, options.ShowUI, pipe)
	if err != nil {
		return err
	}
	dependencyRegistry.SetServer("http://" + serv.Server.Addr)

	// get a stdout writer
	stdoutWriter := ctx.Log().Writer(ctx.Log().GetLevel(), true)
	defer stdoutWriter.Close()

	// get a stderr writer
	stderrWriter := ctx.Log().Writer(logrus.WarnLevel, true)
	defer stderrWriter.Close()

	// start pipeline
	err = pipe.Run(ctx.WithLogger(log.NewStreamLoggerWithFormat(stdoutWriter, stderrWriter, ctx.Log().GetLevel(), log.TimeFormat)))
	if err != nil {
		return err
	}
	ctx.Log().Debugf("Wait for dev to finish")

	// wait for dev
	err = pipe.WaitDev()
	if err != nil {
		return err
	}

	return nil
}

func defaultStdStreams(stdout io.Writer, stderr io.Writer, stdin io.Reader) (io.Writer, io.Writer, io.Reader) {
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	return stdout, stderr, stdin
}
