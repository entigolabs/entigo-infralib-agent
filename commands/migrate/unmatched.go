package migrate

import (
	"context"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/migrate"
)

func Unmatched(ctx context.Context, flags *common.Flags) error {
	migrator, err := migrate.NewUnmatchedFinder(ctx, flags.Migrate)
	if err != nil {
		return err
	}
	migrator.Find()
	return nil
}
