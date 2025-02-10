package cli

import (
	"context"
	"errors"
	"github.com/entigolabs/entigo-infralib-agent/commands/bootstrap"
	"github.com/entigolabs/entigo-infralib-agent/commands/delete"
	"github.com/entigolabs/entigo-infralib-agent/commands/migrate"
	"github.com/entigolabs/entigo-infralib-agent/commands/pull"
	agentRun "github.com/entigolabs/entigo-infralib-agent/commands/run"
	"github.com/entigolabs/entigo-infralib-agent/commands/sa"
	"github.com/entigolabs/entigo-infralib-agent/commands/update"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v2"
	"log"
)

func action(cmd common.Command) func(c *cli.Context) error {
	return func(c *cli.Context) error {
		if err := flags.Setup(cmd); err != nil {
			log.Fatal(&common.PrefixedError{Reason: err})
		}
		common.ChooseLogger(flags.LogLevel)
		run(c.Context, cmd)
		return nil
	}
}

func run(ctx context.Context, cmd common.Command) {
	common.PrintVersion()
	switch cmd {
	case common.RunCommand:
		agentRun.Run(ctx, flags)
	case common.UpdateCommand:
		update.Update(ctx, flags)
	case common.BootstrapCommand:
		bootstrap.Bootstrap(ctx, flags)
	case common.DeleteCommand:
		delete.Delete(ctx, flags)
	case common.SACommand:
		sa.Run(ctx, flags)
	case common.PullCommand:
		pull.Run(ctx, flags)
	case common.MigratePlanCommand:
		migrate.Plan(ctx, flags)
	case common.MigrateValidateCommand:
		migrate.Validate(ctx, flags)
	default:
		log.Fatal(&common.PrefixedError{Reason: errors.New("unsupported command")})
	}
}
