package sa

import (
	"context"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(ctx context.Context, flags *common.Flags) error {
	provider, err := service.GetCloudProvider(ctx, flags)
	if err != nil {
		return err
	}
	return provider.CreateServiceAccount(flags.ServiceAccount)
}
