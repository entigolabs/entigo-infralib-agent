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
		&awsPrefixFlag,
		&projectIdFlag,
		&locationFlag,
		&zoneFlag,
		&awsRoleArnFlag,
	)
}

func appendCmdSpecificFlags(baseFlags []cli.Flag, cmd common.Command) []cli.Flag {
	switch cmd {
	case common.MergeCommand:
		baseFlags = append(baseFlags, &baseConfigFlag)
	case common.DeleteCommand:
		baseFlags = append(baseFlags, &yesFlag, &deleteBucketFlag)
	case common.RunCommand:
		baseFlags = append(baseFlags, &allowParallelFlag)
	}
	return baseFlags
}

var loggingFlag = cli.StringFlag{
	Name:        "logging",
	Aliases:     []string{"l"},
	EnvVars:     []string{"LOGGING"},
	DefaultText: "prod",
	Value:       "prod",
	Usage:       "set `logging level` (prod | dev)",
	Destination: &flags.LoggingLevel,
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

var baseConfigFlag = cli.StringFlag{
	Name:        "base-config",
	Aliases:     []string{"bc"},
	EnvVars:     []string{"BASE_CONFIG"},
	Value:       "",
	Usage:       "set base config file",
	Destination: &flags.BaseConfig,
	Required:    true,
}

var awsPrefixFlag = cli.StringFlag{
	Name:        "aws-prefix",
	Aliases:     []string{"ap"},
	EnvVars:     []string{common.AwsPrefixEnv},
	DefaultText: "",
	Value:       "",
	Usage:       "prefix used when creating aws resources",
	Destination: &flags.AWSPrefix,
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
	Aliases:     []string{"pl"},
	EnvVars:     []string{"ALLOW_PARALLEL"},
	Value:       true,
	Usage:       "allow running steps in parallel on first execution cycle",
	Destination: &flags.AllowParallel,
}

var yesFlag = cli.BoolFlag{
	Name:        "yes",
	Aliases:     []string{"y"},
	EnvVars:     []string{"YES"},
	Usage:       "skip confirmation prompt",
	Destination: &flags.Delete.SkipConfirmation,
}

var deleteBucketFlag = cli.BoolFlag{
	Name:        "delete-bucket",
	Aliases:     []string{"db"},
	EnvVars:     []string{"DELETE_BUCKET"},
	Usage:       "delete the bucket used by terraform state",
	Destination: &flags.Delete.DeleteBucket,
}
