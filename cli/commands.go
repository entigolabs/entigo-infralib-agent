package cli

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
)

func cliCommands() []*cli.Command {
	return []*cli.Command{
		&updateCommand,
	}
}

var updateCommand = cli.Command{
	Name:    "run",
	Aliases: []string{""},
	Usage:   "run agent",
	Action:  action(common.UpdateCommand),
	Flags:   cliFlags(common.UpdateCommand),
}
