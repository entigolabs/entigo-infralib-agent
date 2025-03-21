package cli

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
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
	case common.SACommand:
		return append(baseFlags, getProviderFlags()...)
	case common.BootstrapCommand, common.ListCustomCommand:
		return append(append(baseFlags, getProviderFlags()...), &terraformCacheFlag)
	case common.DestroyCommand:
		return append(append(baseFlags, getProviderFlags()...), &yesFlag, &stepsFlag, &pipelineTypeFlag, &logsPathFlag,
			&printLogsFlag)
	case common.AddCustomCommand:
		return append(append(baseFlags, getProviderFlags()...), &keyFlag, &valueFlag, &overwriteFlag)
	case common.DeleteCustomCommand, common.GetCustomCommand:
		return append(append(baseFlags, getProviderFlags()...), &keyFlag)
	case common.MigratePlanCommand:
		return append(baseFlags, &stateFileFlag, &importFileFlag, &planFileFlag, &typesFileFlag)
	case common.MigrateValidateCommand:
		return append(baseFlags, &stateFileFlag, &importFileFlag, &planFileFlag)
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
		&awsRoleArnFlag,
	}
}

var loggingFlag = cli.StringFlag{
	Name:        "logging",
	Aliases:     []string{"l"},
	EnvVars:     []string{"LOGGING"},
	DefaultText: "info",
	Value:       "info",
	Usage:       "set logging level (debug | info | warn | error)",
	Destination: &flags.LogLevel,
}

var configFlag = cli.StringFlag{
	Name:        "config",
	Aliases:     []string{"c"},
	EnvVars:     []string{"CONFIG"},
	Value:       "",
	Usage:       "set config file",
	Destination: &flags.Config,
	Required:    false,
}

var prefixFlag = cli.StringFlag{
	Name:        "prefix",
	Aliases:     []string{"p", "ap", "aws-prefix"},
	EnvVars:     []string{common.AwsPrefixEnv, common.PrefixEnv},
	DefaultText: "",
	Value:       "",
	Usage:       "prefix used when creating cloud resources",
	Destination: &flags.Prefix,
	Required:    false,
}

var awsRoleArnFlag = cli.StringFlag{
	Name:        "role-arn",
	Aliases:     []string{"ra"},
	EnvVars:     []string{"ROLE_ARN"},
	DefaultText: "",
	Value:       "",
	Usage:       "role arn for assume role, used when creating aws resources in external account",
	Destination: &flags.AWS.RoleArn,
	Required:    false,
}

var projectIdFlag = cli.StringFlag{
	Name:        "project-id",
	Aliases:     []string{"pid"},
	EnvVars:     []string{common.GCloudProjectIdEnv},
	DefaultText: "",
	Value:       "",
	Usage:       "project id used when creating gcloud resources",
	Destination: &flags.GCloud.ProjectId,
	Required:    false,
}

var locationFlag = cli.StringFlag{
	Name:        "location",
	Aliases:     []string{"loc"},
	EnvVars:     []string{common.GCloudLocationEnv},
	DefaultText: "",
	Value:       "",
	Usage:       "location used when creating gcloud resources",
	Destination: &flags.GCloud.Location,
}

var zoneFlag = cli.StringFlag{
	Name:        "zone",
	Aliases:     []string{"z"},
	EnvVars:     []string{common.GCloudZoneEnv},
	DefaultText: "",
	Value:       "",
	Usage:       "zone used in run jobs",
	Destination: &flags.GCloud.Zone,
}

var allowParallelFlag = cli.BoolFlag{
	Name:        "allow-parallel",
	Aliases:     []string{"apl"},
	EnvVars:     []string{"ALLOW_PARALLEL"},
	Value:       true,
	Usage:       "allow running steps in parallel on first execution cycle",
	Destination: &flags.Pipeline.AllowParallel,
}

var yesFlag = cli.BoolFlag{
	Name:        "yes",
	Aliases:     []string{"y"},
	EnvVars:     []string{"YES"},
	Usage:       "skip confirmation prompt",
	DefaultText: "false",
	Value:       false,
	Destination: &flags.Delete.SkipConfirmation,
}

var skipBucketDelayFlag = cli.BoolFlag{
	Name:        "skip-bucket-creation-delay",
	Aliases:     []string{"sb"},
	EnvVars:     []string{"SKIP_BUCKET_CREATION_DELAY"},
	Usage:       "skip bucket creation delay",
	DefaultText: "false",
	Value:       false,
	Destination: &flags.SkipBucketCreationDelay,
}

var deleteBucketFlag = cli.BoolFlag{
	Name:        "delete-bucket",
	Aliases:     []string{"db"},
	EnvVars:     []string{"DELETE_BUCKET"},
	Usage:       "delete the bucket used by terraform state",
	Destination: &flags.Delete.DeleteBucket,
}

var deleteSAFlag = cli.BoolFlag{
	Name:        "delete-service-account",
	Aliases:     []string{"dsa"},
	EnvVars:     []string{"DELETE_SERVICE_ACCOUNT"},
	Usage:       "delete the service account created by service-account command",
	Destination: &flags.Delete.DeleteServiceAccount,
}

var stepsFlag = cli.StringSliceFlag{
	Name:        "steps",
	Aliases:     []string{"s"},
	EnvVars:     []string{"STEPS"},
	Usage:       "steps to run",
	Destination: &flags.Steps,
}

var pipelineTypeFlag = cli.StringFlag{
	Name:        "pipeline-type",
	Aliases:     []string{"pt"},
	EnvVars:     []string{"PIPELINE_TYPE"},
	DefaultText: string(common.PipelineTypeCloud),
	Value:       string(common.PipelineTypeCloud),
	Usage:       "pipeline type (local | cloud)",
	Destination: &flags.Pipeline.Type,
	Required:    false,
}

var logsPathFlag = cli.StringFlag{
	Name:        "logs-path",
	Aliases:     []string{"lp"},
	EnvVars:     []string{"LOGS_PATH"},
	DefaultText: "",
	Value:       "",
	Usage:       "path for storing logs when running local pipelines",
	Destination: &flags.Pipeline.LogsPath,
	Required:    false,
}

var printLogsFlag = cli.BoolFlag{
	Name:        "print-logs",
	Aliases:     []string{"pl"},
	EnvVars:     []string{"PRINT_LOGS"},
	Usage:       "print terraform/helm logs to stdout when using local pipelines",
	Value:       true,
	DefaultText: "true",
	Destination: &flags.Pipeline.PrintLogs,
	Required:    false,
}

var terraformCacheFlag = cli.GenericFlag{
	Name:        "terraform-cache",
	Aliases:     []string{"tc"},
	EnvVars:     []string{"TERRAFORM_CACHE"},
	Usage:       "use terraform caching",
	DefaultText: "true",
	Destination: &flags.Pipeline.TerraformCache,
	Required:    false,
}

var forceFlag = cli.BoolFlag{
	Name:        "force",
	Aliases:     []string{"f"},
	EnvVars:     []string{"FORCE"},
	Usage:       "force",
	Value:       false,
	Destination: &flags.Force,
	Required:    false,
}

var keyFlag = cli.StringFlag{
	Name:        "key",
	Aliases:     []string{"k"},
	EnvVars:     []string{"KEY"},
	Usage:       "parameter key",
	Value:       "",
	Destination: &flags.Params.Key,
	Required:    true,
}

var valueFlag = cli.StringFlag{
	Name:        "value",
	Aliases:     []string{"v"},
	EnvVars:     []string{"VALUE"},
	Usage:       "parameter value",
	Value:       "",
	Destination: &flags.Params.Value,
	Required:    true,
}

var overwriteFlag = cli.BoolFlag{
	Name:        "overwrite",
	Aliases:     []string{"o"},
	EnvVars:     []string{"OVERWRITE"},
	Usage:       "overwrite existing parameter",
	Value:       false,
	Destination: &flags.Params.Overwrite,
	Required:    false,
}

var stateFileFlag = cli.StringFlag{
	Name:        "state-file",
	Aliases:     []string{"sf"},
	EnvVars:     []string{"STATE_FILE"},
	DefaultText: "",
	Value:       "",
	Usage:       "path for terraform state file",
	Destination: &flags.Migrate.StateFile,
	Required:    true,
}

var importFileFlag = cli.StringFlag{
	Name:        "import-file",
	Aliases:     []string{"if"},
	EnvVars:     []string{"IMPORT_FILE"},
	DefaultText: "",
	Value:       "",
	Usage:       "path for import file",
	Destination: &flags.Migrate.ImportFile,
	Required:    true,
}

var planFileFlag = cli.StringFlag{
	Name:        "plan-file",
	Aliases:     []string{"pl"},
	EnvVars:     []string{"PLAN_FILE"},
	DefaultText: "",
	Value:       "",
	Usage:       "path for terraform plan file",
	Destination: &flags.Migrate.PlanFile,
	Required:    true,
}

var typesFileFlag = cli.StringFlag{
	Name:        "types-file",
	Aliases:     []string{"tf"},
	EnvVars:     []string{"TYPES_FILE"},
	DefaultText: "",
	Value:       "",
	Usage:       "path for type identifications file",
	Destination: &flags.Migrate.TypesFile,
	Required:    false,
}
