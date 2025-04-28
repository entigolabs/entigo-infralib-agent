package migrate

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/migrate"
)

func Validate(ctx context.Context, flags *common.Flags) error {
	validator, err := migrate.NewValidator(ctx, flags.Migrate)
	if err != nil {
		return err
	}
	validator.Validate()
	return nil
}
