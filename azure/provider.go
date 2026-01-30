package azure

import (
	"context"
	"fmt"
	"log"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type azureProvider struct {
	ctx            context.Context
	credential     *azidentity.DefaultAzureCredential
	subscriptionId string
	resourceGroup  string
	location       string
	providerType   model.ProviderType
}

func NewAzureProvider(ctx context.Context, azureFlags common.Azure) (model.ResourceProvider, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Azure credentials: %w", err)
	}
	log.Printf("Azure provider initialized with subscription: %s\n", azureFlags.SubscriptionId)
	return &azureProvider{
		ctx:            ctx,
		credential:     credential,
		subscriptionId: azureFlags.SubscriptionId,
		resourceGroup:  azureFlags.ResourceGroup,
		location:       azureFlags.Location,
		providerType:   model.AZURE,
	}, nil
}

func (a *azureProvider) GetSSM() (model.SSM, error) {
	kv, err := NewKeyVault(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create key vault: %w", err)
	}
	return kv, nil
}

func (a *azureProvider) GetBucket(prefix string) (model.Bucket, error) {
	storageAccountName := getStorageAccountName(prefix, a.subscriptionId)
	blob, err := NewBlobStorage(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, storageAccountName)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob storage: %w", err)
	}
	return blob, nil
}

func (a *azureProvider) GetProviderType() model.ProviderType {
	return a.providerType
}
