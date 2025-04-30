package sa

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(ctx context.Context, flags *common.Flags) error {
	provider, err := service.GetCloudProvider(ctx, flags)
	if err != nil {
		return err
	}
	resources, err := provider.GetResources()
	if err != nil {
		return err
	}
	err = addEncryption(flags, resources)
	if err != nil {
		return err
	}
	return provider.CreateServiceAccount()
}

func addEncryption(flags *common.Flags, resources model.Resources) error {
	keyId, err := service.GetEncryptionKey(resources.GetProviderType(), resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if err != nil {
		return err
	}
	if keyId != "" {
		resources.GetSSM().AddEncryptionKeyId(keyId)
	}
	return nil
}
