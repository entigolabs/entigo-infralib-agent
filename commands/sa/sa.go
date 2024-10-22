package sa

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(ctx context.Context, flags *common.Flags) {
	provider := service.GetCloudProvider(ctx, flags)
	provider.CreateServiceAccount()
}
