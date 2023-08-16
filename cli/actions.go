package cli

import (
	"errors"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/updater"
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
	case common.UpdateCommand:
		updater.Run(flags)
	default:
		common.Logger.Fatal(&common.PrefixedError{Reason: errors.New("unsupported command")})
	}

}
