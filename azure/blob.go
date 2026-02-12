package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

type BlobStorage struct {
	ctx                context.Context
	credential         *azidentity.DefaultAzureCredential
	subscriptionId     string
	resourceGroup      string
	location           string
	storageAccountName string
	storageClient      *armstorage.AccountsClient
	blobClient         *azblob.Client
	repoMetadata       *model.RepositoryMetadata
}

func NewBlobStorage(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup, location, storageAccountName string) (*BlobStorage, error) {
	storageClient, err := armstorage.NewAccountsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage accounts client: %w", err)
	}

	blobServiceURL := fmt.Sprintf("https://%s.blob.core.windows.net", storageAccountName)
	blobClient, err := azblob.NewClient(blobServiceURL, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob client: %w", err)
	}

	return &BlobStorage{
		ctx:                ctx,
		credential:         credential,
		subscriptionId:     subscriptionId,
		resourceGroup:      resourceGroup,
		location:           location,
		storageAccountName: storageAccountName,
		storageClient:      storageClient,
		blobClient:         blobClient,
	}, nil
}

func (b *BlobStorage) CreateStorageAccount(skipDelay bool) error {
	exists, err := b.storageAccountExists()
	if err != nil {
		return err
	}
	if exists {
		return b.initBlobClientWithKey()
	}
	util.DelayBucketCreation(b.storageAccountName, skipDelay)
	poller, err := b.storageClient.BeginCreate(b.ctx, b.resourceGroup, b.storageAccountName,
		armstorage.AccountCreateParameters{
			Location: to.Ptr(b.location),
			Kind:     to.Ptr(armstorage.KindStorageV2),
			SKU: &armstorage.SKU{
				Name: to.Ptr(armstorage.SKUNameStandardLRS),
			},
			Properties: &armstorage.AccountPropertiesCreateParameters{
				AllowBlobPublicAccess:  to.Ptr(false),
				MinimumTLSVersion:      to.Ptr(armstorage.MinimumTLSVersionTLS12),
				EnableHTTPSTrafficOnly: to.Ptr(true),
				AllowSharedKeyAccess:   to.Ptr(true),
				IsHnsEnabled:           to.Ptr(false),
				PublicNetworkAccess:    to.Ptr(armstorage.PublicNetworkAccessEnabled),
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to begin creating storage account: %w", err)
	}

	_, err = poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create storage account: %w", err)
	}

	log.Printf("Created Azure Storage Account %s\n", b.storageAccountName)
	return b.initBlobClientWithKey()
}

func (b *BlobStorage) initBlobClientWithKey() error {
	// Get storage account key for data plane operations
	keysResp, err := b.storageClient.ListKeys(b.ctx, b.resourceGroup, b.storageAccountName, nil)
	if err != nil {
		return fmt.Errorf("failed to list storage account keys: %w", err)
	}
	if len(keysResp.Keys) == 0 || keysResp.Keys[0].Value == nil {
		return fmt.Errorf("no storage account keys found")
	}

	// Create blob client with shared key credential
	accountKey := *keysResp.Keys[0].Value
	cred, err := azblob.NewSharedKeyCredential(b.storageAccountName, accountKey)
	if err != nil {
		return fmt.Errorf("failed to create shared key credential: %w", err)
	}

	blobServiceURL := fmt.Sprintf("https://%s.blob.core.windows.net", b.storageAccountName)
	b.blobClient, err = azblob.NewClientWithSharedKeyCredential(blobServiceURL, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create blob client with shared key: %w", err)
	}
	return nil
}

func (b *BlobStorage) storageAccountExists() (bool, error) {
	_, err := b.storageClient.GetProperties(b.ctx, b.resourceGroup, b.storageAccountName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *BlobStorage) CreateContainer(containerName string) error {
	_, err := b.blobClient.CreateContainer(b.ctx, containerName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.ErrorCode == "ContainerAlreadyExists" {
			return nil
		}
		return fmt.Errorf("failed to create container: %w", err)
	}
	log.Printf("Created container %s\n", containerName)
	return nil
}

func (b *BlobStorage) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	if b.repoMetadata != nil {
		return b.repoMetadata, nil
	}
	exists, err := b.BucketExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	b.repoMetadata = &model.RepositoryMetadata{
		Name: b.storageAccountName,
		URL:  b.storageAccountName,
	}
	return b.repoMetadata, nil
}

func (b *BlobStorage) BucketExists() (bool, error) {
	return b.storageAccountExists()
}

func (b *BlobStorage) PutFile(file string, content []byte) error {
	containerName, blobName := b.parseFilePath(file)
	_, err := b.blobClient.UploadBuffer(b.ctx, containerName, blobName, content, nil)
	return err
}

func (b *BlobStorage) GetFile(file string) ([]byte, error) {
	containerName, blobName := b.parseFilePath(file)
	resp, err := b.blobClient.DownloadStream(b.ctx, containerName, blobName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.ErrorCode == "BlobNotFound" {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	return io.ReadAll(resp.Body)
}

func (b *BlobStorage) DeleteFile(file string) error {
	containerName, blobName := b.parseFilePath(file)
	_, err := b.blobClient.DeleteBlob(b.ctx, containerName, blobName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.ErrorCode == "BlobNotFound" {
			return nil
		}
		return err
	}
	return nil
}

func (b *BlobStorage) DeleteFiles(files []string) error {
	for _, file := range files {
		err := b.DeleteFile(file)
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *BlobStorage) CheckFolderExists(folder string) (bool, error) {
	containerName, prefix := b.parseFilePath(folder)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	pager := b.blobClient.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
		Prefix:     to.Ptr(prefix),
		MaxResults: to.Ptr(int32(1)),
	})
	if pager.More() {
		resp, err := pager.NextPage(b.ctx)
		if err != nil {
			var respErr *azcore.ResponseError
			if errors.As(err, &respErr) && respErr.ErrorCode == "ContainerNotFound" {
				return false, nil
			}
			return false, err
		}
		return len(resp.Segment.BlobItems) > 0, nil
	}
	return false, nil
}

func (b *BlobStorage) ListFolderFiles(folder string) ([]string, error) {
	containerName, prefix := b.parseFilePath(folder)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var files []string
	pager := b.blobClient.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
		Prefix: to.Ptr(prefix),
	})
	for pager.More() {
		resp, err := pager.NextPage(b.ctx)
		if err != nil {
			return nil, err
		}
		for _, blob := range resp.Segment.BlobItems {
			files = append(files, *blob.Name)
		}
	}
	return files, nil
}

func (b *BlobStorage) ListFolderFilesWithExclude(folder string, excludeFolders model.Set[string]) ([]string, error) {
	containerName, prefix := b.parseFilePath(folder)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var files []string
	containerClient := b.blobClient.ServiceClient().NewContainerClient(containerName)
	pager := containerClient.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix: to.Ptr(prefix),
	})
	for pager.More() {
		resp, err := pager.NextPage(b.ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range resp.Segment.BlobItems {
			files = append(files, *item.Name)
		}
		for _, blobPrefix := range resp.Segment.BlobPrefixes {
			folderName := strings.TrimSuffix(strings.TrimPrefix(*blobPrefix.Name, prefix), "/")
			if excludeFolders.Contains(folderName) {
				continue
			}
			subFiles, err := b.ListFolderFiles(*blobPrefix.Name)
			if err != nil {
				return nil, err
			}
			files = append(files, subFiles...)
		}
	}
	return files, nil
}

func (b *BlobStorage) Delete() error {
	exists, err := b.BucketExists()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	err = b.deleteAllContainers()
	if err != nil {
		return err
	}

	_, err = b.storageClient.Delete(b.ctx, b.resourceGroup, b.storageAccountName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil
		}
		return err
	}
	log.Printf("Deleted Azure Storage Account %s\n", b.storageAccountName)
	return nil
}

func (b *BlobStorage) deleteAllContainers() error {
	pager := b.blobClient.NewListContainersPager(nil)
	for pager.More() {
		resp, err := pager.NextPage(b.ctx)
		if err != nil {
			return err
		}
		for _, containerItem := range resp.ContainerItems {
			err = b.deleteAllBlobsInContainer(*containerItem.Name)
			if err != nil {
				return err
			}
			_, err = b.blobClient.DeleteContainer(b.ctx, *containerItem.Name, nil)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *BlobStorage) deleteAllBlobsInContainer(containerName string) error {
	pager := b.blobClient.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
		Include: container.ListBlobsInclude{Versions: true},
	})
	for pager.More() {
		resp, err := pager.NextPage(b.ctx)
		if err != nil {
			return err
		}
		for _, item := range resp.Segment.BlobItems {
			blobClient := b.blobClient.ServiceClient().NewContainerClient(containerName).NewBlobClient(*item.Name)
			var deleteErr error
			if item.VersionID != nil {
				versionedClient, err := blobClient.WithVersionID(*item.VersionID)
				if err != nil {
					return err
				}
				_, deleteErr = versionedClient.Delete(b.ctx, nil)
			} else {
				_, deleteErr = blobClient.Delete(b.ctx, nil)
			}
			if deleteErr != nil {
				return deleteErr
			}
		}
	}
	return nil
}

func (b *BlobStorage) parseFilePath(file string) (containerName, blobName string) {
	// Always use tfstate container, treat full path as blob name (like S3 prefixes)
	return "tfstate", file
}
