package cli

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
)

func cliCommands() []*cli.Command {
	return []*cli.Command{
		&runCommand,
		&updateCommand,
		&bootstrapCommand,
		&deleteCommand,
	}
}

var runCommand = cli.Command{
	Name:    string(common.RunCommand),
	Aliases: []string{""},
	Usage:   "run agent",
	Action:  action(common.RunCommand),
	Flags:   cliFlags(common.RunCommand),
}

var updateCommand = cli.Command{
	Name:    string(common.UpdateCommand),
	Aliases: []string{"up"},
	Usage:   "update modules",
	Action:  action(common.UpdateCommand),
	Flags:   cliFlags(common.UpdateCommand),
}

var bootstrapCommand = cli.Command{
	Name:    string(common.BootstrapCommand),
	Aliases: []string{"bs"},
	Usage:   "bootstraps agent pipeline and build job",
	Action:  action(common.BootstrapCommand),
	Flags:   cliFlags(common.BootstrapCommand),
}

var deleteCommand = cli.Command{
	Name:    string(common.DeleteCommand),
	Aliases: []string{"del"},
	Usage:   "delete agent resources",
	Action:  action(common.DeleteCommand),
	Flags:   cliFlags(common.DeleteCommand),
}
