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
		&branchFlag,
		&awsPrefixFlag,
		&projectIdFlag,
		&locationFlag,
		&zoneFlag,
	)
}

func appendCmdSpecificFlags(baseFlags []cli.Flag, cmd common.Command) []cli.Flag {
	switch cmd {
	case common.MergeCommand:
		baseFlags = append(baseFlags, &baseConfigFlag)
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

var branchFlag = cli.StringFlag{
	Name:        "branch",
	Aliases:     []string{"b"},
	EnvVars:     []string{"BRANCH"},
	DefaultText: "main",
	Value:       "main",
	Usage:       "set branch name",
	Destination: &flags.Branch,
	Required:    false,
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

var projectIdFlag = cli.StringFlag{
	Name:        "project-id",
	Aliases:     []string{"pid"},
	EnvVars:     []string{"PROJECT_ID"},
	DefaultText: "",
	Value:       "",
	Usage:       "project id used when creating gcloud resources",
	Destination: &flags.GCloud.ProjectId,
	Required:    false,
}

var locationFlag = cli.StringFlag{
	Name:        "location",
	Aliases:     []string{"loc"},
	EnvVars:     []string{"LOCATION"},
	DefaultText: "",
	Value:       "",
	Usage:       "location used when creating gcloud resources",
	Destination: &flags.GCloud.Location,
}

var zoneFlag = cli.StringFlag{
	Name:        "zone",
	Aliases:     []string{"z"},
	EnvVars:     []string{"ZONE"},
	DefaultText: "",
	Value:       "",
	Usage:       "zone used in run jobs",
	Destination: &flags.GCloud.Zone,
}
