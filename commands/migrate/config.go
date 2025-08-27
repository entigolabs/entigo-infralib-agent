package migrate

import (
	"context"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/migrate"
)

func Config(ctx context.Context, flags *common.Flags) error {
	migrator, err := migrate.NewConfigGenerator(ctx, flags.Migrate)
	if err != nil {
		return err
	}
	migrator.Generate()
	return nil
}
