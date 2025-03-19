package sa

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(ctx context.Context, flags *common.Flags) {
	provider := service.GetCloudProvider(ctx, flags)
	addEncryption(flags, provider.GetResources())
	provider.CreateServiceAccount()
}

func addEncryption(flags *common.Flags, resources model.Resources) {
	keyId := service.GetEncryptionKey(resources.GetProviderType(), resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if keyId == "" {
		return
	}
	resources.GetSSM().AddEncryptionKeyId(keyId)
}
