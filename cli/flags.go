package cli

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v3"
)

func cliFlags(cmd common.Command) []cli.Flag {
	var flags []cli.Flag
	flags = appendBaseFlags(flags)
	flags = appendCmdSpecificFlags(flags, cmd)
	return flags
}

func appendBaseFlags(flags []cli.Flag) []cli.Flag {
	return append(flags, &loggingFlag)
}

func appendCmdSpecificFlags(baseFlags []cli.Flag, cmd common.Command) []cli.Flag {
	switch cmd {
	case common.DeleteCommand:
		return append(append(baseFlags, getProviderFlags()...), &yesFlag, &deleteBucketFlag, &deleteSAFlag)
	case common.UpdateCommand:
		return append(append(baseFlags, getProviderFlags()...), &stepsFlag, &pipelineTypeFlag,
			&logsPathFlag, &printLogsFlag, &terraformCacheFlag, &skipBucketDelayFlag)
	case common.RunCommand:
		return append(append(baseFlags, getProviderFlags()...), &allowParallelFlag, &stepsFlag,
			&pipelineTypeFlag, &logsPathFlag, &printLogsFlag, &terraformCacheFlag, &skipBucketDelayFlag)
	case common.PullCommand:
		return append(append(baseFlags, getProviderFlags()...), &forceFlag)
	case common.SACommand, common.ListCustomCommand:
		return append(baseFlags, getProviderFlags()...)
	case common.BootstrapCommand:
		return append(append(baseFlags, getProviderFlags()...), &terraformCacheFlag, &startFlag)
	case common.DestroyCommand:
		return append(append(baseFlags, getProviderFlags()...), &yesFlag, &stepsFlag, &pipelineTypeFlag, &logsPathFlag,
			&printLogsFlag)
	case common.AddCustomCommand:
		return append(append(baseFlags, getProviderFlags()...), &keyFlag, &valueFlag, &overwriteFlag)
	case common.DeleteCustomCommand, common.GetCustomCommand:
		return append(append(baseFlags, getProviderFlags()...), &keyFlag)
	case common.MigratePlanCommand:
		return append(baseFlags, &stateFileFlag, importFileFlag(true), &planFileFlag, &typesFileFlag)
	case common.MigrateValidateCommand:
		return append(baseFlags, &stateFileFlag, importFileFlag(true), &planFileFlag)
	case common.MigrateConfigCommand:
		return append(baseFlags, &stateFileFlag, importFileFlag(false))
	default:
		return baseFlags
	}
}

func getProviderFlags() []cli.Flag {
	return []cli.Flag{
		&configFlag,
		&prefixFlag,
		&projectIdFlag,
		&locationFlag,
		&zoneFlag,
		&gcloudCredentialsJsonFlag,
		&awsRoleArnFlag,
	}
}

var loggingFlag = cli.StringFlag{
	Name:        "logging",
	Aliases:     []string{"l"},
	Sources:     cli.EnvVars("LOGGING"),
	DefaultText: "info",
	Value:       "info",
	Usage:       "set logging level (debug | info | warn | error)",
	Destination: &flags.LogLevel,
}

var configFlag = cli.StringFlag{
	Name:        "config",
	Aliases:     []string{"c"},
	Sources:     cli.EnvVars("CONFIG"),
	Value:       "",
	Usage:       "set config file",
	Destination: &flags.Config,
	Required:    false,
}

var prefixFlag = cli.StringFlag{
	Name:        "prefix",
	Aliases:     []string{"p", "ap", "aws-prefix"},
	Sources:     cli.EnvVars(common.AwsPrefixEnv, common.PrefixEnv),
	DefaultText: "",
	Value:       "",
	Usage:       "prefix used when creating cloud resources",
	Destination: &flags.Prefix,
	Required:    false,
}

var awsRoleArnFlag = cli.StringFlag{
	Name:        "role-arn",
	Aliases:     []string{"ra"},
	Sources:     cli.EnvVars("ROLE_ARN"),
	DefaultText: "",
	Value:       "",
	Usage:       "role arn for assume role, used when creating aws resources in external account",
	Destination: &flags.AWS.RoleArn,
	Required:    false,
}

var projectIdFlag = cli.StringFlag{
	Name:        "project-id",
	Aliases:     []string{"pid"},
	Sources:     cli.EnvVars(common.GCloudProjectIdEnv),
	DefaultText: "",
	Value:       "",
	Usage:       "project id used when creating gcloud resources",
	Destination: &flags.GCloud.ProjectId,
	Required:    false,
}

var locationFlag = cli.StringFlag{
	Name:        "location",
	Aliases:     []string{"loc"},
	Sources:     cli.EnvVars(common.GCloudLocationEnv),
	DefaultText: "",
	Value:       "",
	Usage:       "location used when creating gcloud resources",
	Destination: &flags.GCloud.Location,
}

var zoneFlag = cli.StringFlag{
	Name:        "zone",
	Aliases:     []string{"z"},
	Sources:     cli.EnvVars(common.GCloudZoneEnv),
	DefaultText: "",
	Value:       "",
	Usage:       "zone used in run jobs",
	Destination: &flags.GCloud.Zone,
}

var gcloudCredentialsJsonFlag = cli.StringFlag{
	Name:        "google-application-credentials-json",
	Aliases:     []string{"gcj"},
	Sources:     cli.EnvVars("GOOGLE_APPLICATION_CREDENTIALS_JSON"),
	DefaultText: "",
	Value:       "",
	Usage:       "gcloud credentials json content",
	Destination: &flags.GCloud.CredentialsJson,
}

var allowParallelFlag = cli.BoolFlag{
	Name:        "allow-parallel",
	Aliases:     []string{"apl"},
	Sources:     cli.EnvVars("ALLOW_PARALLEL"),
	Value:       true,
	Usage:       "allow running steps in parallel on first execution cycle",
	Destination: &flags.Pipeline.AllowParallel,
}

var yesFlag = cli.BoolFlag{
	Name:        "yes",
	Aliases:     []string{"y"},
	Sources:     cli.EnvVars("YES"),
	Usage:       "skip confirmation prompt",
	DefaultText: "false",
	Value:       false,
	Destination: &flags.Delete.SkipConfirmation,
}

var skipBucketDelayFlag = cli.BoolFlag{
	Name:        "skip-bucket-creation-delay",
	Aliases:     []string{"sb"},
	Sources:     cli.EnvVars("SKIP_BUCKET_CREATION_DELAY"),
	Usage:       "skip bucket creation delay",
	DefaultText: "false",
	Value:       false,
	Destination: &flags.SkipBucketCreationDelay,
}

var deleteBucketFlag = cli.BoolFlag{
	Name:        "delete-bucket",
	Aliases:     []string{"db"},
	Sources:     cli.EnvVars("DELETE_BUCKET"),
	Usage:       "delete the bucket used by terraform state",
	Destination: &flags.Delete.DeleteBucket,
}

var deleteSAFlag = cli.BoolFlag{
	Name:        "delete-service-account",
	Aliases:     []string{"dsa"},
	Sources:     cli.EnvVars("DELETE_SERVICE_ACCOUNT"),
	Usage:       "delete the service account created by service-account command",
	Destination: &flags.Delete.DeleteServiceAccount,
}

var stepsFlag = cli.StringSliceFlag{
	Name:        "steps",
	Aliases:     []string{"s"},
	Sources:     cli.EnvVars("STEPS"),
	Usage:       "steps to run",
	Destination: &flags.Steps,
}

var pipelineTypeFlag = cli.StringFlag{
	Name:        "pipeline-type",
	Aliases:     []string{"pt"},
	Sources:     cli.EnvVars("PIPELINE_TYPE"),
	DefaultText: string(common.PipelineTypeCloud),
	Value:       string(common.PipelineTypeCloud),
	Usage:       "pipeline type (local | cloud)",
	Destination: &flags.Pipeline.Type,
	Required:    false,
}

var logsPathFlag = cli.StringFlag{
	Name:        "logs-path",
	Aliases:     []string{"lp"},
	Sources:     cli.EnvVars("LOGS_PATH"),
	DefaultText: "",
	Value:       "",
	Usage:       "path for storing logs when running local pipelines",
	Destination: &flags.Pipeline.LogsPath,
	Required:    false,
}

var printLogsFlag = cli.BoolFlag{
	Name:        "print-logs",
	Aliases:     []string{"pl"},
	Sources:     cli.EnvVars("PRINT_LOGS"),
	Usage:       "print terraform/helm logs to stdout when using local pipelines",
	Value:       true,
	DefaultText: "true",
	Destination: &flags.Pipeline.PrintLogs,
	Required:    false,
}

var terraformCacheFlag = cli.GenericFlag{
	Name:        "terraform-cache",
	Aliases:     []string{"tc"},
	Sources:     cli.EnvVars("TERRAFORM_CACHE"),
	Usage:       "use terraform caching",
	DefaultText: "true",
	Value:       &flags.Pipeline.TerraformCache,
	Required:    false,
}

var startFlag = cli.BoolFlag{
	Name:        "start",
	Aliases:     []string{"st"},
	Sources:     cli.EnvVars("START"),
	Usage:       "start",
	Value:       true,
	Destination: &flags.Start,
	Required:    false,
}

var forceFlag = cli.BoolFlag{
	Name:        "force",
	Aliases:     []string{"f"},
	Sources:     cli.EnvVars("FORCE"),
	Usage:       "force",
	Value:       false,
	Destination: &flags.Force,
	Required:    false,
}

var keyFlag = cli.StringFlag{
	Name:        "key",
	Aliases:     []string{"k"},
	Sources:     cli.EnvVars("KEY"),
	Usage:       "parameter key",
	Value:       "",
	Destination: &flags.Params.Key,
	Required:    true,
}

var valueFlag = cli.StringFlag{
	Name:        "value",
	Aliases:     []string{"v"},
	Sources:     cli.EnvVars("VALUE"),
	Usage:       "parameter value",
	Value:       "",
	Destination: &flags.Params.Value,
	Required:    true,
}

var overwriteFlag = cli.BoolFlag{
	Name:        "overwrite",
	Aliases:     []string{"o"},
	Sources:     cli.EnvVars("OVERWRITE"),
	Usage:       "overwrite existing parameter",
	Value:       false,
	Destination: &flags.Params.Overwrite,
	Required:    false,
}

var stateFileFlag = cli.StringFlag{
	Name:        "state-file",
	Aliases:     []string{"sf"},
	Sources:     cli.EnvVars("STATE_FILE"),
	DefaultText: "",
	Value:       "",
	Usage:       "path for terraform state file",
	Destination: &flags.Migrate.StateFile,
	Required:    true,
}

func importFileFlag(required bool) *cli.StringFlag {
	return &cli.StringFlag{
		Name:        "import-file",
		Aliases:     []string{"if"},
		Sources:     cli.EnvVars("IMPORT_FILE"),
		DefaultText: "",
		Value:       "",
		Usage:       "path for import file",
		Destination: &flags.Migrate.ImportFile,
		Required:    required,
	}
}

var planFileFlag = cli.StringFlag{
	Name:        "plan-file",
	Aliases:     []string{"pl"},
	Sources:     cli.EnvVars("PLAN_FILE"),
	DefaultText: "",
	Value:       "",
	Usage:       "path for terraform plan file",
	Destination: &flags.Migrate.PlanFile,
	Required:    true,
}

var typesFileFlag = cli.StringFlag{
	Name:        "types-file",
	Aliases:     []string{"tf"},
	Sources:     cli.EnvVars("TYPES_FILE"),
	DefaultText: "",
	Value:       "",
	Usage:       "path for type identifications file",
	Destination: &flags.Migrate.TypesFile,
	Required:    false,
}
