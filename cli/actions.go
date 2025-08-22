package cli

import (
	"context"
	"errors"

	"github.com/entigolabs/entigo-infralib-agent/commands/bootstrap"
	"github.com/entigolabs/entigo-infralib-agent/commands/delete"
	"github.com/entigolabs/entigo-infralib-agent/commands/destroy"
	"github.com/entigolabs/entigo-infralib-agent/commands/migrate"
	"github.com/entigolabs/entigo-infralib-agent/commands/params"
	"github.com/entigolabs/entigo-infralib-agent/commands/pull"
	agentRun "github.com/entigolabs/entigo-infralib-agent/commands/run"
	"github.com/entigolabs/entigo-infralib-agent/commands/sa"
	"github.com/entigolabs/entigo-infralib-agent/commands/update"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/urfave/cli/v3"
)

func action(cmd common.Command) cli.ActionFunc {
	return func(ctx context.Context, _ *cli.Command) error {
		if err := flags.Setup(cmd); err != nil {
			return err
		}
		if err := common.ChooseLogger(flags.LogLevel); err != nil {
			return err
		}
		return run(ctx, cmd)
	}
}

func run(ctx context.Context, cmd common.Command) error {
	common.PrintVersion()
	switch cmd {
	case common.RunCommand:
		return agentRun.Run(ctx, flags)
	case common.UpdateCommand:
		return update.Update(ctx, flags)
	case common.BootstrapCommand:
		return bootstrap.Bootstrap(ctx, flags)
	case common.DeleteCommand:
		return delete.Delete(ctx, flags)
	case common.DestroyCommand:
		return destroy.Destroy(ctx, flags)
	case common.SACommand:
		return sa.Run(ctx, flags)
	case common.PullCommand:
		return pull.Run(ctx, flags)
	case common.AddCustomCommand, common.DeleteCustomCommand, common.GetCustomCommand, common.ListCustomCommand:
		return params.Custom(ctx, flags, cmd)
	case common.MigratePlanCommand:
		return migrate.Plan(ctx, flags)
	case common.MigrateValidateCommand:
		return migrate.Validate(ctx, flags)
	case common.MigrateUnmatchedCommand:
		return migrate.Unmatched(ctx, flags)
	default:
		return errors.New("unsupported command")
	}
}
