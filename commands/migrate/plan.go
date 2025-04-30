package migrate

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/migrate"
)

func Plan(ctx context.Context, flags *common.Flags) error {
	migrator, err := migrate.NewPlanner(ctx, flags.Migrate)
	if err != nil {
		return err
	}
	migrator.Plan()
	return nil
}
