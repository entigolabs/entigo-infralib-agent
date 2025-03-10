package migrate

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/migrate"
)

func Validate(ctx context.Context, flags *common.Flags) {
	migrate.NewValidator(ctx, flags.Migrate).Validate()
}
