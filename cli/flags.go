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
	)
}

func appendCmdSpecificFlags(baseFlags []cli.Flag, cmd common.Command) []cli.Flag {
	switch cmd {
	case common.UpdateCommand:
		baseFlags = updateSpecificFlags(baseFlags)
	}
	return baseFlags
}

func updateSpecificFlags(baseFlags []cli.Flag) []cli.Flag {
	return append(baseFlags,
		&configFlag)
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
	Required:    true,
}
