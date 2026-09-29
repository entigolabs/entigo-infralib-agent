package azure

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

const (
	versionRetentionDays = 1
	keyAccessTimeout     = 5 * time.Minute
)

// Storage is a single blob container in the agent's storage account. Shared key
// access is disabled, so every client, terraform included, authenticates with Entra ID.
type Storage struct {
	ctx           context.Context
	accounts      *armstorage.AccountsClient
	blobServices  *armstorage.BlobServicesClient
	policies      *armstorage.ManagementPoliciesClient
	client        *azblob.Client
	resourceGroup string
	location      string
	account       string
	accountId     string
	repoMetadata  *model.RepositoryMetadata
}

func NewStorage(ctx context.Context, credential azcore.TokenCredential, subscriptionId, resourceGroup, location, account string) (*Storage, error) {
	accounts, err := armstorage.NewAccountsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	blobServices, err := armstorage.NewBlobServicesClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	policies, err := armstorage.NewManagementPoliciesClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	client, err := azblob.NewClient(fmt.Sprintf("https://%s.blob.core.windows.net/", account), credential, nil)
	if err != nil {
		return nil, err
	}
	return &Storage{
		ctx:           ctx,
		accounts:      accounts,
		blobServices:  blobServices,
		policies:      policies,
		client:        client,
		resourceGroup: resourceGroup,
		location:      location,
		account:       account,
		accountId: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Storage/storageAccounts/%s",
			subscriptionId, resourceGroup, account),
	}, nil
}

func (s *Storage) AccountId() string { return s.accountId }

// CreateAccount creates the account encrypted with the agent's key, accessed through
// the managed identity, which must already hold a key role on the vault. A fresh role
// assignment surfaces as a key access error until it propagates, so that is retried.
func (s *Storage) CreateAccount(kms *KMS, encryptionIdentity string) error {
	exists, err := s.accountExists()
	if err != nil {
		return err
	}
	if !exists {
		available, err := s.accounts.CheckNameAvailability(s.ctx, armstorage.AccountCheckNameAvailabilityParameters{
			Name: &s.account,
			Type: new("Microsoft.Storage/storageAccounts"),
		}, nil)
		if err != nil {
			return fmt.Errorf("failed to check storage account name %s: %w", s.account, err)
		}
		if available.NameAvailable != nil && !*available.NameAvailable {
			return fmt.Errorf("storage account name %s is taken in another resource group or subscription, use another prefix", s.account)
		}
		if err = s.createAccount(kms, encryptionIdentity); err != nil {
			return err
		}
	}
	if err = s.ensureBlobServices(); err != nil {
		return err
	}
	return s.ensureLifecycle()
}

func (s *Storage) createAccount(kms *KMS, encryptionIdentity string) error {
	parameters := armstorage.AccountCreateParameters{
		Kind:     new(armstorage.KindStorageV2),
		SKU:      &armstorage.SKU{Name: new(armstorage.SKUNameStandardLRS)},
		Location: &s.location,
		Tags:     resourceTags(),
		Identity: &armstorage.Identity{
			Type:                   new(armstorage.IdentityTypeUserAssigned),
			UserAssignedIdentities: map[string]*armstorage.UserAssignedIdentity{encryptionIdentity: {}},
		},
		Properties: &armstorage.AccountPropertiesCreateParameters{
			AllowBlobPublicAccess:        new(false),
			AllowSharedKeyAccess:         new(false),
			DefaultToOAuthAuthentication: new(true),
			EnableHTTPSTrafficOnly:       new(true),
			MinimumTLSVersion:            new(armstorage.MinimumTLSVersionTLS12),
			Encryption: &armstorage.Encryption{
				KeySource: new(armstorage.KeySourceMicrosoftKeyvault),
				KeyVaultProperties: &armstorage.KeyVaultProperties{
					KeyName:     new(kms.KeyName()),
					KeyVaultURI: new(kms.VaultURI()),
				},
				EncryptionIdentity: &armstorage.EncryptionIdentity{EncryptionUserAssignedIdentity: &encryptionIdentity},
				Services: &armstorage.EncryptionServices{
					Blob: &armstorage.EncryptionService{Enabled: new(true), KeyType: new(armstorage.KeyTypeAccount)},
				},
			},
		},
	}
	deadline := time.Now().Add(keyAccessTimeout)
	logged := false
	for {
		poller, err := s.accounts.BeginCreate(s.ctx, s.resourceGroup, s.account, parameters, nil)
		if err == nil {
			_, err = poller.PollUntilDone(s.ctx, nil)
		}
		if err == nil {
			log.Printf("Created storage account %s\n", s.account)
			return nil
		}
		if !isKeyAccessError(err) || time.Now().After(deadline) {
			return fmt.Errorf("failed to create storage account %s: %w", s.account, err)
		}
		if !logged {
			log.Println("Waiting for the managed identity key access to propagate before creating the storage account")
			logged = true
		}
		if err = sleep(s.ctx, pollInterval); err != nil {
			return err
		}
	}
}

func isKeyAccessError(err error) bool {
	code := errorCode(err)
	return strings.Contains(code, "KeyVault") || strings.Contains(err.Error(), "Key Vault")
}

func (s *Storage) accountExists() (bool, error) {
	_, err := s.accounts.GetProperties(s.ctx, s.resourceGroup, s.account, nil)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to get storage account %s: %w", s.account, err)
}

func (s *Storage) ensureBlobServices() error {
	_, err := s.blobServices.SetServiceProperties(s.ctx, s.resourceGroup, s.account, armstorage.BlobServiceProperties{
		BlobServiceProperties: &armstorage.BlobServicePropertiesProperties{
			IsVersioningEnabled: new(true),
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to enable versioning for storage account %s: %w", s.account, err)
	}
	return nil
}

func (s *Storage) ensureLifecycle() error {
	_, err := s.policies.CreateOrUpdate(s.ctx, s.resourceGroup, s.account, armstorage.ManagementPolicyNameDefault,
		armstorage.ManagementPolicy{
			Properties: &armstorage.ManagementPolicyProperties{
				Policy: &armstorage.ManagementPolicySchema{
					Rules: []*armstorage.ManagementPolicyRule{{
						Name:    new("DeleteOlderVersions"),
						Type:    new(armstorage.RuleTypeLifecycle),
						Enabled: new(true),
						Definition: &armstorage.ManagementPolicyDefinition{
							Filters: &armstorage.ManagementPolicyFilter{BlobTypes: []*string{new("blockBlob")}},
							Actions: &armstorage.ManagementPolicyAction{
								Version: &armstorage.ManagementPolicyVersion{
									Delete: &armstorage.DateAfterCreation{
										DaysAfterCreationGreaterThan: new(float32(versionRetentionDays)),
									},
								},
							},
						},
					}},
				},
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to set lifecycle policy for storage account %s: %w", s.account, err)
	}
	return nil
}

// EnsureContainer uses the data plane, so a fresh blob role is retried until it applies.
func (s *Storage) EnsureContainer() error {
	return retryUntilAuthorized(s.ctx, "creating the storage container", func() error {
		_, err := s.client.CreateContainer(s.ctx, containerName, nil)
		if err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			return err
		}
		return nil
	})
}

func (s *Storage) BucketExists() (bool, error) {
	exists, err := s.accountExists()
	if err != nil || !exists {
		return exists, err
	}
	_, err = s.client.ServiceClient().NewContainerClient(containerName).GetProperties(s.ctx, nil)
	if err == nil {
		return true, nil
	}
	if bloberror.HasCode(err, bloberror.ContainerNotFound) {
		return false, nil
	}
	return false, err
}

func (s *Storage) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	if s.repoMetadata != nil {
		return s.repoMetadata, nil
	}
	exists, err := s.BucketExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	s.repoMetadata = &model.RepositoryMetadata{Name: s.account, URL: s.account}
	return s.repoMetadata, nil
}

func (s *Storage) PutFile(file string, content []byte) error {
	_, err := s.client.UploadBuffer(s.ctx, containerName, file, content, nil)
	if err != nil {
		return fmt.Errorf("failed to put blob %s: %w", file, err)
	}
	return nil
}

func (s *Storage) GetFile(file string) ([]byte, error) {
	response, err := s.client.DownloadStream(s.ctx, containerName, file, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	var buffer bytes.Buffer
	if _, err = io.Copy(&buffer, response.Body); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (s *Storage) DeleteFile(file string) error {
	_, err := s.client.DeleteBlob(s.ctx, containerName, file, nil)
	if err != nil && !bloberror.HasCode(err, bloberror.BlobNotFound) {
		return err
	}
	return nil
}

func (s *Storage) DeleteFiles(files []string) error {
	for _, file := range files {
		if err := s.DeleteFile(file); err != nil {
			return err
		}
	}
	return nil
}

func (s *Storage) CheckFolderExists(folder string) (bool, error) {
	pager := s.client.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
		Prefix:     new(folderPrefix(folder)),
		MaxResults: new(int32(1)),
	})
	if !pager.More() {
		return false, nil
	}
	page, err := pager.NextPage(s.ctx)
	if err != nil {
		return false, err
	}
	return len(page.Segment.BlobItems) > 0, nil
}

func (s *Storage) ListFolderFiles(folder string) ([]string, error) {
	var files []string
	pager := s.client.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
		Prefix: new(folderPrefix(folder)),
	})
	for pager.More() {
		page, err := pager.NextPage(s.ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Segment.BlobItems {
			files = append(files, *item.Name)
		}
	}
	return files, nil
}

func (s *Storage) ListFolderFilesWithExclude(folder string, excludeFolders model.Set[string]) ([]string, error) {
	prefix := folderPrefix(folder)
	files, err := s.ListFolderFiles(prefix)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, file := range files {
		relative := strings.TrimPrefix(file, prefix)
		if index := strings.Index(relative, "/"); index > 0 && excludeFolders.Contains(relative[:index]) {
			continue
		}
		result = append(result, file)
	}
	return result, nil
}

func folderPrefix(folder string) string {
	if strings.HasSuffix(folder, "/") {
		return folder
	}
	return folder + "/"
}

func (s *Storage) Delete() error {
	_, err := s.accounts.Delete(s.ctx, s.resourceGroup, s.account, nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	log.Printf("Deleted storage account %s\n", s.account)
	return nil
}
