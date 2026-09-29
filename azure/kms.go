package azure

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const vaultRetentionDays = 7

// KMS is the agent-owned Key Vault and key. The key encrypts the storage account
// (customer-managed key), which requires purge protection, so a deleted vault keeps
// its name for the retention period and is recovered instead of recreated.
type KMS struct {
	ctx           context.Context
	credential    azcore.TokenCredential
	vaults        *armkeyvault.VaultsClient
	resourceGroup string
	location      string
	tenantId      string
	name          string
	keyName       string
	vaultId       string
	vaultURI      string
}

func NewKMS(ctx context.Context, credential azcore.TokenCredential, subscriptionId, resourceGroup, location, tenantId, name, keyName string) (*KMS, error) {
	vaults, err := armkeyvault.NewVaultsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	return &KMS{
		ctx:           ctx,
		credential:    credential,
		vaults:        vaults,
		resourceGroup: resourceGroup,
		location:      location,
		tenantId:      tenantId,
		name:          name,
		keyName:       keyName,
	}, nil
}

func (k *KMS) VaultId() string  { return k.vaultId }
func (k *KMS) VaultURI() string { return k.vaultURI }
func (k *KMS) KeyName() string  { return k.keyName }

func (k *KMS) EnsureVault(skipDelay bool) error {
	found, err := k.Resolve()
	if err != nil || found {
		return err
	}
	createMode := armkeyvault.CreateModeDefault
	_, err = k.vaults.GetDeleted(k.ctx, k.name, k.location, nil)
	if err == nil {
		log.Printf("Recovering soft-deleted key vault %s\n", k.name)
		createMode = armkeyvault.CreateModeRecover
	} else if !isNotFound(err) {
		return fmt.Errorf("failed to get deleted key vault %s: %w", k.name, err)
	} else {
		available, err := k.vaults.CheckNameAvailability(k.ctx, armkeyvault.VaultCheckNameAvailabilityParameters{
			Name: &k.name,
			Type: new("Microsoft.KeyVault/vaults"),
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to check key vault name %s: %w", k.name, err)
		}
		if available.NameAvailable != nil && !*available.NameAvailable {
			return fmt.Errorf("key vault name %s is taken in another resource group or subscription, use another prefix", k.name)
		}
		util.DelayResourceCreation("Key vault", k.name, skipDelay)
	}
	poller, err := k.vaults.BeginCreateOrUpdate(k.ctx, k.resourceGroup, k.name, armkeyvault.VaultCreateOrUpdateParameters{
		Location: &k.location,
		Tags:     resourceTags(),
		Properties: &armkeyvault.VaultProperties{
			TenantID:                  &k.tenantId,
			SKU:                       &armkeyvault.SKU{Family: new(armkeyvault.SKUFamilyA), Name: new(armkeyvault.SKUNameStandard)},
			EnableRbacAuthorization:   new(true),
			EnablePurgeProtection:     new(true),
			EnableSoftDelete:          new(true),
			SoftDeleteRetentionInDays: new(int32(vaultRetentionDays)),
			CreateMode:                &createMode,
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to create key vault %s: %w", k.name, err)
	}
	response, err := poller.PollUntilDone(k.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create key vault %s: %w", k.name, err)
	}
	k.setVault(response.Vault)
	log.Printf("Created key vault %s\n", k.name)
	return nil
}

// Resolve finds the vault without creating it.
func (k *KMS) Resolve() (bool, error) {
	existing, err := k.vaults.Get(k.ctx, k.resourceGroup, k.name, nil)
	if err == nil {
		k.setVault(existing.Vault)
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to get key vault %s: %w", k.name, err)
}

func (k *KMS) setVault(vault armkeyvault.Vault) {
	if vault.ID != nil {
		k.vaultId = *vault.ID
	}
	if vault.Properties != nil && vault.Properties.VaultURI != nil {
		k.vaultURI = *vault.Properties.VaultURI
	}
}

// EnsureKey needs a data-plane role on the vault; a fresh assignment is retried until it applies.
func (k *KMS) EnsureKey() error {
	client, err := azkeys.NewClient(k.vaultURI, k.credential, nil)
	if err != nil {
		return err
	}
	return retryUntilAuthorized(k.ctx, "creating the agent key", func() error {
		_, err := client.GetKey(k.ctx, k.keyName, "", nil)
		if err == nil {
			return nil
		}
		if !isNotFound(err) {
			return err
		}
		_, err = client.CreateKey(k.ctx, k.keyName, azkeys.CreateKeyParameters{
			Kty:     new(azkeys.KeyTypeRSA),
			KeySize: new(int32(3072)),
			Tags:    resourceTags(),
		}, nil)
		if isStatus(err, http.StatusConflict) {
			log.Printf("Recovering soft-deleted key %s\n", k.keyName)
			_, err = client.RecoverDeletedKey(k.ctx, k.keyName, nil)
			return err
		}
		if err == nil {
			log.Printf("Created key %s in key vault %s\n", k.keyName, k.name)
		}
		return err
	})
}

// Delete soft-deletes the vault; purge protection keeps it recoverable for the retention period.
func (k *KMS) Delete() error {
	_, err := k.vaults.Delete(k.ctx, k.resourceGroup, k.name, nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	log.Printf("Key vault %s is soft-deleted and kept for %d days due to purge protection\n", k.name, vaultRetentionDays)
	return nil
}
