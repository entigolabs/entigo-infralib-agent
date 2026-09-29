package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type azureProvider struct {
	ctx            context.Context
	cloudPrefix    string
	subscriptionId string
	location       string
	azure          common.Azure
	credential     azcore.TokenCredential
}

func NewAzureProvider(ctx context.Context, azure common.Azure, cloudPrefix string) (model.ResourceProvider, error) {
	credential, err := newCredential()
	if err != nil {
		return nil, err
	}
	return &azureProvider{
		ctx:            ctx,
		cloudPrefix:    cloudPrefix,
		subscriptionId: azure.SubscriptionId,
		location:       azure.Location,
		azure:          azure,
		credential:     credential,
	}, nil
}

func (a *azureProvider) GetProviderType() model.ProviderType {
	return model.AZURE
}

// GetSSM resolves the vault without creating it; the vault is created by bootstrap or run.
func (a *azureProvider) GetSSM() (model.SSM, error) {
	executor, err := currentPrincipal(a.ctx, a.credential)
	if err != nil {
		return nil, err
	}
	name := vaultName(a.cloudPrefix, a.subscriptionId, a.location)
	kms, err := NewKMS(a.ctx, a.credential, a.subscriptionId, resourceGroup(a.azure, a.cloudPrefix), a.location,
		executor.TenantId, name, agentKeyName(a.cloudPrefix))
	if err != nil {
		return nil, err
	}
	found, err := kms.Resolve()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("key vault %s not found, run bootstrap first", name)
	}
	return NewSSM(a.ctx, a.credential, kms.VaultURI())
}

func (a *azureProvider) GetBucket(prefix string) (model.Bucket, error) {
	return NewStorage(a.ctx, a.credential, a.subscriptionId, resourceGroup(a.azure, prefix), a.location,
		storageAccountName(prefix, a.subscriptionId, a.location))
}
