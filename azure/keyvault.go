package azure

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/keyvault/armkeyvault"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type KeyVault struct {
	ctx            context.Context
	credential     *azidentity.DefaultAzureCredential
	subscriptionId string
	resourceGroup  string
	location       string
	vaultName      string
	vaultClient    *armkeyvault.VaultsClient
	secretsClient  *azsecrets.Client
}

func NewKeyVault(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup, location, cloudPrefix string) (*KeyVault, error) {
	vaultClient, err := armkeyvault.NewVaultsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create vault client: %w", err)
	}

	vaultName := getKeyVaultName(cloudPrefix, subscriptionId)

	kv := &KeyVault{
		ctx:            ctx,
		credential:     credential,
		subscriptionId: subscriptionId,
		resourceGroup:  resourceGroup,
		location:       location,
		vaultName:      vaultName,
		vaultClient:    vaultClient,
	}

	err = kv.ensureVaultExists()
	if err != nil {
		return nil, err
	}

	vaultURL := fmt.Sprintf("https://%s.vault.azure.net", vaultName)
	secretsClient, err := azsecrets.NewClient(vaultURL, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create secrets client: %w", err)
	}
	kv.secretsClient = secretsClient

	return kv, nil
}

func getKeyVaultName(cloudPrefix, subscriptionId string) string {
	name := strings.ReplaceAll(cloudPrefix, "_", "-") + "-" + strings.ReplaceAll(subscriptionId, "-", "")[:8]
	if len(name) > 24 {
		name = name[:24]
	}
	name = strings.TrimSuffix(name, "-")
	return strings.ToLower(name)
}

func (k *KeyVault) ensureVaultExists() error {
	_, err := k.vaultClient.Get(k.ctx, k.resourceGroup, k.vaultName, nil)
	if err == nil {
		return nil
	}
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) || respErr.StatusCode != 404 {
		return err
	}

	tenantID, err := k.getTenantID()
	if err != nil {
		return fmt.Errorf("failed to get tenant ID: %w", err)
	}

	poller, err := k.vaultClient.BeginCreateOrUpdate(k.ctx, k.resourceGroup, k.vaultName,
		armkeyvault.VaultCreateOrUpdateParameters{
			Location: to.Ptr(k.location),
			Properties: &armkeyvault.VaultProperties{
				TenantID:                  to.Ptr(tenantID),
				SKU:                       &armkeyvault.SKU{Family: to.Ptr(armkeyvault.SKUFamilyA), Name: to.Ptr(armkeyvault.SKUNameStandard)},
				EnableRbacAuthorization:   to.Ptr(true),
				EnableSoftDelete:          to.Ptr(true),
				SoftDeleteRetentionInDays: to.Ptr(int32(7)),
				PublicNetworkAccess:       to.Ptr("Enabled"),
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to begin creating key vault: %w", err)
	}

	_, err = poller.PollUntilDone(k.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create key vault: %w", err)
	}

	log.Printf("Created Azure Key Vault %s\n", k.vaultName)
	return nil
}

func (k *KeyVault) getTenantID() (string, error) {
	token, err := k.credential.GetToken(k.ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", err
	}
	parts := strings.Split(token.Token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid token format")
	}
	return k.subscriptionId[:36], nil
}

func (k *KeyVault) AddEncryptionKeyId(_ string) {
}

func (k *KeyVault) GetParameter(name string) (*model.Parameter, error) {
	secretName := k.sanitizeSecretName(name)
	resp, err := k.secretsClient.GetSecret(k.ctx, secretName, "", nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && (respErr.StatusCode == 404 || respErr.ErrorCode == "SecretNotFound") {
			return nil, &model.ParameterNotFoundError{Name: name}
		}
		return nil, err
	}
	return &model.Parameter{
		Value: resp.Value,
		Type:  "SecureString",
	}, nil
}

func (k *KeyVault) ParameterExists(name string) (bool, error) {
	_, err := k.GetParameter(name)
	if err != nil {
		var notFoundErr *model.ParameterNotFoundError
		if errors.As(err, &notFoundErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (k *KeyVault) PutParameter(name string, value string) error {
	secretName := k.sanitizeSecretName(name)
	param, err := k.GetParameter(name)
	if err != nil {
		var notFoundErr *model.ParameterNotFoundError
		if !errors.As(err, &notFoundErr) {
			return err
		}
	}
	if param != nil && param.Value != nil && *param.Value == value {
		return nil
	}
	_, err = k.secretsClient.SetSecret(k.ctx, secretName, azsecrets.SetSecretParameters{
		Value:       to.Ptr(value),
		ContentType: to.Ptr("text/plain"),
		Tags: map[string]*string{
			model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
		},
	}, nil)
	return err
}

func (k *KeyVault) DeleteParameter(name string) error {
	secretName := k.sanitizeSecretName(name)
	_, err := k.secretsClient.DeleteSecret(k.ctx, secretName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && (respErr.StatusCode == 404 || respErr.ErrorCode == "SecretNotFound") {
			return nil
		}
		return err
	}
	return nil
}

func (k *KeyVault) ListParameters() ([]string, error) {
	var keys []string
	pager := k.secretsClient.NewListSecretPropertiesPager(nil)
	for pager.More() {
		resp, err := pager.NextPage(k.ctx)
		if err != nil {
			return nil, err
		}
		for _, secret := range resp.Value {
			if secret.Tags != nil {
				if tag, ok := secret.Tags[model.ResourceTagKey]; ok && tag != nil && *tag == model.ResourceTagValue {
					parts := strings.Split(string(secret.ID.Name()), "/")
					keys = append(keys, parts[len(parts)-1])
				}
			}
		}
	}
	return keys, nil
}

func (k *KeyVault) PutSecret(name string, value string) error {
	return k.PutParameter(name, value)
}

func (k *KeyVault) DeleteSecret(name string) error {
	return k.DeleteParameter(name)
}

func (k *KeyVault) sanitizeSecretName(name string) string {
	name = strings.ReplaceAll(strings.TrimLeft(name, "/"), "/", "-")
	name = strings.ReplaceAll(name, "_", "-")
	if len(name) > 127 {
		name = name[:127]
	}
	return name
}
