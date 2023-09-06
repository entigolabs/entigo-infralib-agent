package cli

import (
	"errors"
	"github.com/entigolabs/entigo-infralib-agent/commands/bootstrap"
	agentRun "github.com/entigolabs/entigo-infralib-agent/commands/run"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
)

func action(cmd common.Command) func(c *cli.Context) error {
	return func(c *cli.Context) error {
		if err := flags.Setup(cmd); err != nil {
			common.Logger.Fatal(&common.PrefixedError{Reason: err})
		}
		run(cmd)
		return nil
	}
}

func run(cmd common.Command) {
	switch cmd {
	case common.RunCommand:
		agentRun.Run(flags)
	case common.BootstrapCommand:
		bootstrap.Bootstrap(flags)
	default:
		common.Logger.Fatal(&common.PrefixedError{Reason: errors.New("unsupported command")})
	}

}
