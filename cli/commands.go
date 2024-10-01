package cli

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
)

func cliCommands() []*cli.Command {
	return []*cli.Command{
		&runCommand,
		&bootstrapCommand,
		&deleteCommand,
	}
}

var runCommand = cli.Command{
	Name:    "run",
	Aliases: []string{""},
	Usage:   "run agent",
	Action:  action(common.RunCommand),
	Flags:   cliFlags(common.RunCommand),
}

var bootstrapCommand = cli.Command{
	Name:    "bootstrap",
	Aliases: []string{"bs"},
	Usage:   "bootstraps agent codepipeline and codebuild",
	Action:  action(common.BootstrapCommand),
	Flags:   cliFlags(common.BootstrapCommand),
}

var deleteCommand = cli.Command{
	Name:    "delete",
	Aliases: []string{"del"},
	Usage:   "delete agent resources",
	Action:  action(common.DeleteCommand),
	Flags:   cliFlags(common.DeleteCommand),
}
