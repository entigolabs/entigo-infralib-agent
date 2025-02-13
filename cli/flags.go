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
	return append(flags,
		&loggingFlag,
		&configFlag,
		&prefixFlag,
		&projectIdFlag,
		&locationFlag,
		&zoneFlag,
		&awsRoleArnFlag,
	)
}

func appendCmdSpecificFlags(baseFlags []cli.Flag, cmd common.Command) []cli.Flag {
	switch cmd {
	case common.DeleteCommand:
		baseFlags = append(baseFlags, &yesFlag, &deleteBucketFlag, &deleteSAFlag)
	case common.UpdateCommand:
		baseFlags = append(baseFlags, &githubToken, &stepsFlag, &pipelineTypeFlag, &logsPathFlag, &printLogsFlag,
			&skipBucketDelayFlag)
	case common.RunCommand:
		baseFlags = append(baseFlags, &allowParallelFlag, &githubToken, &stepsFlag, &pipelineTypeFlag, &logsPathFlag,
			&printLogsFlag, &skipBucketDelayFlag)
	case common.DestroyCommand:
		baseFlags = append(baseFlags, &yesFlag, &stepsFlag, &pipelineTypeFlag, &logsPathFlag, &printLogsFlag)
	case common.PullCommand:
		baseFlags = append(baseFlags, &forceFlag)
	}
	return baseFlags
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
	Destination: &flags.AllowParallel,
}

var githubToken = cli.StringFlag{
	Name:        "github-token",
	Aliases:     []string{"gt"},
	EnvVars:     []string{"GITHUB_TOKEN"},
	Usage:       "github token used for github requests",
	DefaultText: "",
	Value:       "",
	Destination: &flags.GithubToken,
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
	Destination: &flags.Pipeline.PrintLogs,
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
