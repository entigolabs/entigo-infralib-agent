package oracle

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

type Storage struct {
	ctx           context.Context
	client        objectstorage.ObjectStorageClient
	namespace     string
	compartmentId string
	region        string
	bucket        string
	bucketCreated *bool
	repoMetadata  *model.RepositoryMetadata
}

func NewStorage(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, bucket string) (*Storage, error) {
	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
	}
	namespace, err := client.GetNamespace(ctx, objectstorage.GetNamespaceRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object storage namespace: %w", err)
	}
	return &Storage{
		ctx:           ctx,
		client:        client,
		namespace:     *namespace.Value,
		compartmentId: compartmentId,
		region:        region,
		bucket:        bucket,
	}, nil
}

func (s *Storage) Namespace() string {
	return s.namespace
}

func (s *Storage) CreateBucket(skipDelay bool) error {
	exists, err := s.BucketExists()
	if err != nil {
		return err
	}
	if exists {
		s.bucketCreated = &exists // cache so later BucketExists calls skip the API round-trip
		return nil
	}
	util.DelayBucketCreation(s.bucket, skipDelay)
	_, err = s.client.CreateBucket(s.ctx, objectstorage.CreateBucketRequest{
		NamespaceName: &s.namespace,
		CreateBucketDetails: objectstorage.CreateBucketDetails{
			Name:             &s.bucket,
			CompartmentId:    &s.compartmentId,
			PublicAccessType: objectstorage.CreateBucketDetailsPublicAccessTypeNopublicaccess,
			Versioning:       objectstorage.CreateBucketDetailsVersioningEnabled,
			FreeformTags:     map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return err
	}
	log.Printf("Created Oracle Object Storage bucket %s\n", s.bucket)
	created := true
	s.bucketCreated = &created
	return nil
}

func (s *Storage) BucketExists() (bool, error) {
	if s.bucketCreated != nil {
		return *s.bucketCreated, nil
	}
	_, err := s.client.GetBucket(s.ctx, objectstorage.GetBucketRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// AddEncryption sets the bucket's default at-rest encryption to the agent-owned
// customer-managed KMS key, idempotently (skips if already set). Requires the
// Object Storage service principal to have "use keys" on the key first
// (IAM.EnsureObjectStorageKeyAccess), or UpdateBucket returns an authorization
// error. Mirrors the AWS "create bucket, encrypt once the key exists" order.
func (s *Storage) AddEncryption(keyId string) error {
	response, err := s.client.GetBucket(s.ctx, objectstorage.GetBucketRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
	})
	if err != nil {
		return fmt.Errorf("failed to get bucket %s: %w", s.bucket, err)
	}
	if response.KmsKeyId != nil && *response.KmsKeyId == keyId {
		return nil
	}
	_, err = s.client.UpdateBucket(s.ctx, objectstorage.UpdateBucketRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
		UpdateBucketDetails: objectstorage.UpdateBucketDetails{
			KmsKeyId: &keyId,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to set kms key on bucket %s: %w", s.bucket, err)
	}
	log.Printf("Enabled KMS encryption on bucket %s\n", s.bucket)
	return nil
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
	s.repoMetadata = &model.RepositoryMetadata{
		Name: s.bucket,
		URL:  s.bucket,
	}
	return s.repoMetadata, nil
}

func (s *Storage) PutFile(file string, content []byte) error {
	length := int64(len(content))
	_, err := s.client.PutObject(s.ctx, objectstorage.PutObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
		ObjectName:    &file,
		ContentLength: &length,
		PutObjectBody: io.NopCloser(bytes.NewReader(content)),
	})
	if err != nil {
		return fmt.Errorf("failed to put object %s: %w", file, err)
	}
	return nil
}

func (s *Storage) GetFile(file string) ([]byte, error) {
	response, err := s.client.GetObject(s.ctx, objectstorage.GetObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
		ObjectName:    &file,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = response.Content.Close() }()
	return io.ReadAll(response.Content)
}

func (s *Storage) DeleteFile(file string) error {
	_, err := s.client.DeleteObject(s.ctx, objectstorage.DeleteObjectRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
		ObjectName:    &file,
	})
	if err != nil && !isNotFound(err) {
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
	if !strings.HasSuffix(folder, "/") {
		folder = folder + "/" // anchor to the folder so "foo" can't match "foobar/"
	}
	limit := 1
	response, err := s.client.ListObjects(s.ctx, objectstorage.ListObjectsRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
		Prefix:        &folder,
		Limit:         &limit,
	})
	if err != nil {
		return false, err
	}
	return len(response.Objects) > 0, nil
}

func (s *Storage) ListFolderFiles(folder string) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder = folder + "/"
	}
	return s.listObjects(folder, "")
}

func (s *Storage) ListFolderFilesWithExclude(folder string, excludeFolders model.Set[string]) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder = folder + "/"
	}
	files, err := s.listObjects(folder, "/")
	if err != nil {
		return nil, err
	}
	prefixes, err := s.listPrefixes(folder)
	if err != nil {
		return nil, err
	}
	for _, prefix := range prefixes {
		if excludeFolders.Contains(strings.TrimSuffix(strings.TrimPrefix(prefix, folder), "/")) {
			continue
		}
		subFiles, err := s.ListFolderFiles(prefix)
		if err != nil {
			return nil, err
		}
		files = append(files, subFiles...)
	}
	return files, nil
}

func (s *Storage) listObjects(prefix, delimiter string) ([]string, error) {
	var files []string
	var start *string
	for {
		request := objectstorage.ListObjectsRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.bucket,
			Prefix:        &prefix,
			Start:         start,
		}
		if delimiter != "" {
			request.Delimiter = &delimiter
		}
		response, err := s.client.ListObjects(s.ctx, request)
		if err != nil {
			return nil, err
		}
		for _, object := range response.Objects {
			files = append(files, *object.Name)
		}
		if response.NextStartWith == nil {
			break
		}
		start = response.NextStartWith
	}
	return files, nil
}

func (s *Storage) listPrefixes(prefix string) ([]string, error) {
	delimiter := "/"
	response, err := s.client.ListObjects(s.ctx, objectstorage.ListObjectsRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
		Prefix:        &prefix,
		Delimiter:     &delimiter,
	})
	if err != nil {
		return nil, err
	}
	return response.Prefixes, nil
}

func (s *Storage) Delete() error {
	exists, err := s.BucketExists()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	log.Printf("Emptying bucket %s...\n", s.bucket)
	if err = s.deleteAllVersions(); err != nil {
		return err
	}
	_, err = s.client.DeleteBucket(s.ctx, objectstorage.DeleteBucketRequest{
		NamespaceName: &s.namespace,
		BucketName:    &s.bucket,
	})
	if err == nil {
		log.Printf("Deleted Oracle Object Storage bucket %s\n", s.bucket)
	}
	return err
}

func (s *Storage) deleteAllVersions() error {
	var page *string
	for {
		response, err := s.client.ListObjectVersions(s.ctx, objectstorage.ListObjectVersionsRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.bucket,
			Page:          page,
		})
		if err != nil {
			return err
		}
		for _, version := range response.Items {
			_, err = s.client.DeleteObject(s.ctx, objectstorage.DeleteObjectRequest{
				NamespaceName: &s.namespace,
				BucketName:    &s.bucket,
				ObjectName:    version.Name,
				VersionId:     version.VersionId,
			})
			if err != nil && !isNotFound(err) {
				return err
			}
		}
		if response.OpcNextPage == nil {
			break
		}
		page = response.OpcNextPage
	}
	return nil
}

func isNotFound(err error) bool {
	failure, ok := ocicommon.IsServiceError(err)
	return ok && failure.GetHTTPStatusCode() == http.StatusNotFound
}
