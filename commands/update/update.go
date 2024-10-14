package update

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Update(ctx context.Context, flags *common.Flags) {
	common.PrintVersion()
	updater := service.NewUpdater(ctx, flags)
	updater.Update()
}