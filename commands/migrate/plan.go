package migrate

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/migrate"
)

func Plan(ctx context.Context, flags *common.Flags) {
	migrate.NewPlanner(ctx, flags.Migrate).Plan()
}
