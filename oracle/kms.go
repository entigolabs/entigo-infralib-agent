package oracle

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/keymanagement"
)

const (
	kmsPollInterval  = 10 * time.Second
	kmsCreateTimeout = 15 * time.Minute
	// kmsKeyLengthBytes is 32 == AES-256.
	kmsKeyLengthBytes = 32
)

// kmsState is the in-memory pointer to the agent-owned vault + master key,
// resolved once per process (no bucket cache — the vault/key are found by name).
type kmsState struct {
	VaultId            string
	KeyId              string
	ManagementEndpoint string
}

// KMS provisions and owns the agent's own KMS vault and master encryption key,
// deliberately independent of any terraform kms module. The agent creates the key
// for its own resources (bucket at-rest encryption + the Vault secret store) so
// that destroying a module never leaves the agent pointing at a deleted key — the
// race the AWS provider suffers when the kms module is removed.
//
// One DEFAULT (shared-partition) vault and one AES-256 key, both named
// <prefix>-infralib, found-or-created by name each process (nothing persisted).
// Bootstrapping mirrors the CSK / auth-token flow: any principal that can manage
// KMS works (the in-container resource principal has manage all-resources), so a
// local first run creates them and every later run just looks them up by name.
type KMS struct {
	ctx           context.Context
	provider      ocicommon.ConfigurationProvider
	vaultClient   keymanagement.KmsVaultClient
	region        string
	compartmentId string
	cloudPrefix   string
	once          sync.Once
	err           error
	state         kmsState
}

func NewKMS(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, cloudPrefix string) (*KMS, error) {
	vaultClient, err := keymanagement.NewKmsVaultClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		vaultClient.SetRegion(region)
	}
	return &KMS{
		ctx:           ctx,
		provider:      provider,
		vaultClient:   vaultClient,
		region:        region,
		compartmentId: compartmentId,
		cloudPrefix:   cloudPrefix,
	}, nil
}

func (k *KMS) resourceName() string { return fmt.Sprintf("%s-infralib", k.cloudPrefix) }

// Ensure resolves the vault + master key once per process (found-or-created by name).
func (k *KMS) Ensure() error {
	k.once.Do(func() { k.err = k.ensure() })
	return k.err
}

func (k *KMS) KeyId() string   { return k.state.KeyId }
func (k *KMS) VaultId() string { return k.state.VaultId }

func (k *KMS) ensure() error {
	vault, err := k.ensureVault()
	if err != nil {
		return err
	}
	// The management client is scoped to the vault's own endpoint, which already
	// encodes the region — no SetRegion.
	mgmt, err := keymanagement.NewKmsManagementClientWithConfigurationProvider(k.provider, *vault.ManagementEndpoint)
	if err != nil {
		return err
	}
	keyId, err := k.ensureKey(mgmt, *vault.Id)
	if err != nil {
		return err
	}
	k.state = kmsState{VaultId: *vault.Id, KeyId: keyId, ManagementEndpoint: *vault.ManagementEndpoint}
	return nil
}

// ensureVault finds a live vault by name or creates a DEFAULT one, polling until
// ACTIVE — DEFAULT-vault creation takes minutes on the first run.
func (k *KMS) ensureVault() (*keymanagement.Vault, error) {
	name := k.resourceName()
	existing, err := k.findVault(name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return k.waitForVaultActive(*existing.Id)
	}
	created, err := k.vaultClient.CreateVault(k.ctx, keymanagement.CreateVaultRequest{
		CreateVaultDetails: keymanagement.CreateVaultDetails{
			CompartmentId: &k.compartmentId,
			DisplayName:   &name,
			VaultType:     keymanagement.CreateVaultDetailsVaultTypeDefault,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create kms vault %s: %w", name, err)
	}
	log.Printf("Creating Oracle KMS vault %s (this can take a few minutes)\n", name)
	return k.waitForVaultActive(*created.Id)
}

// findVault returns the first non-deleted vault with the given display name.
// ListVaults has no server-side name filter and pages, so every page is scanned —
// otherwise an existing vault past page one would be missed and a duplicate (slow,
// quota-limited) DEFAULT vault created.
func (k *KMS) findVault(name string) (*keymanagement.VaultSummary, error) {
	var page *string
	for {
		response, err := k.vaultClient.ListVaults(k.ctx, keymanagement.ListVaultsRequest{
			CompartmentId: &k.compartmentId,
			Page:          page,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list kms vaults: %w", err)
		}
		for i := range response.Items {
			v := response.Items[i]
			if v.DisplayName != nil && *v.DisplayName == name && !vaultDeleted(v.LifecycleState) {
				return &v, nil
			}
		}
		if response.OpcNextPage == nil {
			return nil, nil
		}
		page = response.OpcNextPage
	}
}

func vaultDeleted(state keymanagement.VaultSummaryLifecycleStateEnum) bool {
	switch state {
	case keymanagement.VaultSummaryLifecycleStateDeleting,
		keymanagement.VaultSummaryLifecycleStateDeleted,
		keymanagement.VaultSummaryLifecycleStatePendingDeletion,
		keymanagement.VaultSummaryLifecycleStateSchedulingDeletion:
		return true
	}
	return false
}

func (k *KMS) waitForVaultActive(vaultId string) (*keymanagement.Vault, error) {
	deadline := time.Now().Add(kmsCreateTimeout)
	for {
		response, err := k.vaultClient.GetVault(k.ctx, keymanagement.GetVaultRequest{VaultId: &vaultId})
		if err != nil {
			return nil, fmt.Errorf("failed to get kms vault %s: %w", vaultId, err)
		}
		switch response.LifecycleState {
		case keymanagement.VaultLifecycleStateActive:
			return &response.Vault, nil
		case keymanagement.VaultLifecycleStateCreating,
			keymanagement.VaultLifecycleStateUpdating,
			keymanagement.VaultLifecycleStateRestoring:
			// keep waiting
		default:
			return nil, fmt.Errorf("kms vault %s is in unexpected state %s", vaultId, response.LifecycleState)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for kms vault %s to become active", vaultId)
		}
		select {
		case <-k.ctx.Done():
			return nil, k.ctx.Err()
		case <-time.After(kmsPollInterval):
		}
	}
}

// ensureKey finds a live AES key by name in the vault or creates one, polling
// until ENABLED. The management client is scoped to the vault's endpoint.
func (k *KMS) ensureKey(mgmt keymanagement.KmsManagementClient, vaultId string) (string, error) {
	name := k.resourceName()
	existing, err := k.findKey(mgmt, name)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return k.waitForKeyEnabled(mgmt, existing)
	}
	length := kmsKeyLengthBytes
	created, err := mgmt.CreateKey(k.ctx, keymanagement.CreateKeyRequest{
		CreateKeyDetails: keymanagement.CreateKeyDetails{
			CompartmentId: &k.compartmentId,
			DisplayName:   &name,
			KeyShape: &keymanagement.KeyShape{
				Algorithm: keymanagement.KeyShapeAlgorithmAes,
				Length:    &length,
			},
			FreeformTags: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create kms key %s: %w", name, err)
	}
	log.Printf("Created Oracle KMS key %s\n", name)
	return k.waitForKeyEnabled(mgmt, *created.Id)
}

// findKey returns the OCID of the first non-deleted key with the given display
// name, or "" if none exists. ListKeys has no server-side name filter and pages,
// so every page is scanned — otherwise an existing key past page one would be
// missed and a duplicate created, potentially diverging from the key the bucket
// and secrets were encrypted with.
func (k *KMS) findKey(mgmt keymanagement.KmsManagementClient, name string) (string, error) {
	var page *string
	for {
		response, err := mgmt.ListKeys(k.ctx, keymanagement.ListKeysRequest{
			CompartmentId: &k.compartmentId,
			Page:          page,
		})
		if err != nil {
			return "", fmt.Errorf("failed to list kms keys: %w", err)
		}
		for i := range response.Items {
			key := response.Items[i]
			if key.DisplayName != nil && *key.DisplayName == name && !keyDeleted(key.LifecycleState) {
				return *key.Id, nil
			}
		}
		if response.OpcNextPage == nil {
			return "", nil
		}
		page = response.OpcNextPage
	}
}

func keyDeleted(state keymanagement.KeySummaryLifecycleStateEnum) bool {
	switch state {
	case keymanagement.KeySummaryLifecycleStateDeleting,
		keymanagement.KeySummaryLifecycleStateDeleted,
		keymanagement.KeySummaryLifecycleStatePendingDeletion,
		keymanagement.KeySummaryLifecycleStateSchedulingDeletion:
		return true
	}
	return false
}

func (k *KMS) waitForKeyEnabled(mgmt keymanagement.KmsManagementClient, keyId string) (string, error) {
	deadline := time.Now().Add(kmsCreateTimeout)
	for {
		response, err := mgmt.GetKey(k.ctx, keymanagement.GetKeyRequest{KeyId: &keyId})
		if err != nil {
			return "", fmt.Errorf("failed to get kms key %s: %w", keyId, err)
		}
		switch response.LifecycleState {
		case keymanagement.KeyLifecycleStateEnabled:
			return keyId, nil
		case keymanagement.KeyLifecycleStateCreating,
			keymanagement.KeyLifecycleStateEnabling,
			keymanagement.KeyLifecycleStateUpdating:
			// keep waiting
		default:
			return "", fmt.Errorf("kms key %s is in unexpected state %s", keyId, response.LifecycleState)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out waiting for kms key %s to become enabled", keyId)
		}
		select {
		case <-k.ctx.Done():
			return "", k.ctx.Err()
		case <-time.After(kmsPollInterval):
		}
	}
}
