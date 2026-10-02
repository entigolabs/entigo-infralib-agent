package azure

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
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
	credential    azcore.TokenCredential
	client        *azblob.Client
	domain        string
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
	return &Storage{
		ctx:           ctx,
		accounts:      accounts,
		blobServices:  blobServices,
		policies:      policies,
		credential:    credential,
		resourceGroup: resourceGroup,
		location:      location,
		account:       account,
		accountId: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Storage/storageAccounts/%s",
			subscriptionId, resourceGroup, account),
	}, nil
}

func (s *Storage) AccountId() string { return s.accountId }

// Domain is the storage endpoint suffix of the account's cloud, empty until the account is resolved.
func (s *Storage) Domain() string { return s.domain }

// Resolve loads the account's blob endpoint without creating the account.
func (s *Storage) Resolve() (bool, error) {
	if s.client != nil {
		return true, nil
	}
	existing, err := s.accounts.GetProperties(s.ctx, s.resourceGroup, s.account, nil)
	if err == nil {
		return true, s.setAccount(existing.Account)
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to get storage account %s: %w", s.account, err)
}

func (s *Storage) setAccount(account armstorage.Account) error {
	if account.Properties == nil || account.Properties.PrimaryEndpoints == nil || account.Properties.PrimaryEndpoints.Blob == nil {
		return fmt.Errorf("storage account %s has no blob endpoint", s.account)
	}
	endpoint := *account.Properties.PrimaryEndpoints.Blob
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("failed to parse blob endpoint %s: %w", endpoint, err)
	}
	_, domain, found := strings.Cut(parsed.Hostname(), ".blob.")
	if !found {
		return fmt.Errorf("unexpected blob endpoint %s", endpoint)
	}
	client, err := azblob.NewClient(endpoint, s.credential, nil)
	if err != nil {
		return err
	}
	s.client = client
	s.domain = domain
	return nil
}

func (s *Storage) blobClient() (*azblob.Client, error) {
	found, err := s.Resolve()
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("storage account %s not found", s.account)
	}
	return s.client, nil
}

// CreateAccount creates the account encrypted with the agent's key, accessed through
// the managed identity, which must already hold a key role on the vault. A fresh role
// assignment surfaces as a key access error until it propagates, so that is retried.
func (s *Storage) CreateAccount(kms *KMS, encryptionIdentity string) error {
	exists, err := s.Resolve()
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
		var created armstorage.AccountsClientCreateResponse
		if err == nil {
			created, err = poller.PollUntilDone(s.ctx, nil)
		}
		if err == nil {
			log.Printf("Created storage account %s\n", s.account)
			return s.setAccount(created.Account)
		}
		if !isKeyAccessError(err) || time.Now().After(deadline) {
			return fmt.Errorf("failed to create storage account %s: %w", s.account, err)
		}
		if !logged {
			log.Println("Waiting for the managed identity key access to propagate before creating the storage account")
			logged = true
		}
		if err = util.Sleep(s.ctx, pollInterval); err != nil {
			return err
		}
	}
}

func isKeyAccessError(err error) bool {
	code := errorCode(err)
	return strings.Contains(code, "KeyVault") || strings.Contains(err.Error(), "Key Vault")
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
	client, err := s.blobClient()
	if err != nil {
		return err
	}
	return retryUntilAuthorized(s.ctx, "creating the storage container", func() error {
		_, err := client.CreateContainer(s.ctx, containerName, nil)
		if err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			return err
		}
		return nil
	})
}

func (s *Storage) BucketExists() (bool, error) {
	exists, err := s.Resolve()
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
	client, err := s.blobClient()
	if err != nil {
		return err
	}
	_, err = client.UploadBuffer(s.ctx, containerName, file, content, nil)
	if err != nil {
		return fmt.Errorf("failed to put blob %s: %w", file, err)
	}
	return nil
}

func (s *Storage) GetFile(file string) ([]byte, error) {
	client, err := s.blobClient()
	if err != nil {
		return nil, err
	}
	response, err := client.DownloadStream(s.ctx, containerName, file, nil)
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
	client, err := s.blobClient()
	if err != nil {
		return err
	}
	_, err = client.DeleteBlob(s.ctx, containerName, file, nil)
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
	client, err := s.blobClient()
	if err != nil {
		return false, err
	}
	pager := client.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
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
	client, err := s.blobClient()
	if err != nil {
		return nil, err
	}
	var files []string
	pager := client.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
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
