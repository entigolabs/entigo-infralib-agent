package update

import (
	"context"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Update(ctx context.Context, flags *common.Flags) error {
	runner, err := service.NewRunner(ctx, common.UpdateCommand, flags)
	if err != nil {
		return err
	}
	return runner.Run()
}
