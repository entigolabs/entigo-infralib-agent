package oracle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

const (
	bucketKmsPropagationTimeout = time.Minute
	bucketKmsPropagationPoll    = 5 * time.Second
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

func (s *Storage) CreateBucket(kms *KMS, skipDelay bool) error {
	exists, err := s.BucketExists()
	if err != nil {
		return err
	}
	if exists {
		s.bucketCreated = &exists // cache so later BucketExists calls skip the API round-trip
		return nil
	}
	util.DelayBucketCreation(s.bucket, skipDelay)
	request := objectstorage.CreateBucketRequest{
		NamespaceName: &s.namespace,
		CreateBucketDetails: objectstorage.CreateBucketDetails{
			Name:             &s.bucket,
			CompartmentId:    &s.compartmentId,
			PublicAccessType: objectstorage.CreateBucketDetailsPublicAccessTypeNopublicaccess,
			Versioning:       objectstorage.CreateBucketDetailsVersioningEnabled,
			FreeformTags:     map[string]string{model.ResourceTagKey: model.ResourceTagValue},
			KmsKeyId:         new(kms.KeyId()),
		},
	}
	// The KMS key-access policy granted just before this call is eventually
	// consistent, so Object Storage may briefly reject the key with
	// NotAuthorizedOrFoundKmsKey. Retry until the policy propagates.
	deadline := time.Now().Add(bucketKmsPropagationTimeout)
	for {
		_, err = s.client.CreateBucket(s.ctx, request)
		if err == nil {
			break
		}
		if !isKmsKeyNotAuthorized(err) || time.Now().After(deadline) {
			return err
		}
		log.Printf("KMS key not yet authorized for Object Storage, retrying bucket %s creation\n", s.bucket)
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(bucketKmsPropagationPoll):
		}
	}
	log.Printf("Created Oracle Object Storage bucket %s\n", s.bucket)
	s.bucketCreated = new(true)
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

func (s *Storage) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	if s.repoMetadata != nil {
		return s.repoMetadata, nil
	}
	metadata := &model.RepositoryMetadata{
		Name: s.bucket,
		URL:  s.bucket,
	}
	exists, err := s.BucketExists()
	if err != nil {
		// Exclusion for the Delete command, other processes should cause an error due to an unusable bucket.
		if isConflict(err, "KmsKeyDisabled") {
			return metadata, nil
		}
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	s.repoMetadata = metadata
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
		if isConflict(err, "KmsKeyDisabled") {
			exists = true
		} else {
			return err
		}
	}
	if !exists {
		return nil
	}
	log.Printf("Emptying bucket %s...\n", s.bucket)
	// In-progress multipart uploads (terraform writes large state via multipart) keep
	// the bucket non-empty even after every object version is gone, so abort them too.
	if err = s.abortMultipartUploads(); err != nil {
		return err
	}
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

func (s *Storage) abortMultipartUploads() error {
	var page *string
	for {
		response, err := s.client.ListMultipartUploads(s.ctx, objectstorage.ListMultipartUploadsRequest{
			NamespaceName: &s.namespace,
			BucketName:    &s.bucket,
			Page:          page,
		})
		if err != nil {
			return err
		}
		for _, upload := range response.Items {
			_, err = s.client.AbortMultipartUpload(s.ctx, objectstorage.AbortMultipartUploadRequest{
				NamespaceName: &s.namespace,
				BucketName:    &s.bucket,
				ObjectName:    upload.Object,
				UploadId:      upload.UploadId,
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

// asServiceError extracts an OCI ServiceError from anywhere in the error chain.
// Unlike ocicommon.IsServiceError (a bare type assertion), errors.As walks past
// fmt.Errorf wrapping, so the predicates below never miss a wrapped service code.
func asServiceError(err error) (ocicommon.ServiceError, bool) {
	var failure ocicommon.ServiceError
	if errors.As(err, &failure) {
		return failure, true
	}
	return nil, false
}

// errSummary condenses an error to a single line for logging: OCI SDK errors
// stringify to a ~10-line block, so for a ServiceError keep just code and message.
func errSummary(err error) string {
	if failure, ok := asServiceError(err); ok {
		return fmt.Sprintf("%s: %s", failure.GetCode(), strings.TrimSpace(failure.GetMessage()))
	}
	return err.Error()
}

func isNotFound(err error) bool {
	failure, ok := asServiceError(err)
	return ok && failure.GetHTTPStatusCode() == http.StatusNotFound
}

func isKmsKeyNotAuthorized(err error) bool {
	failure, ok := asServiceError(err)
	return ok && failure.GetCode() == "NotAuthorizedOrFoundKmsKey"
}

// isNotAuthorized reports OCI's authorization failure (missing permission or a
// resource scoped away). Other codes (throttling, service faults) must propagate.
func isNotAuthorized(err error) bool {
	failure, ok := asServiceError(err)
	return ok && failure.GetCode() == "NotAuthorizedOrNotFound"
}

func isConflict(err error, code string) bool {
	failure, ok := asServiceError(err)
	return ok && failure.GetHTTPStatusCode() == http.StatusConflict && failure.GetCode() == code
}

// isConflictStatus reports any 409, regardless of the specific code — used where the
// concrete code is irrelevant (e.g. a resource already exists from a concurrent bootstrap
// and the find-or-create just needs to fall back to it).
func isConflictStatus(err error) bool {
	failure, ok := asServiceError(err)
	return ok && failure.GetHTTPStatusCode() == http.StatusConflict
}

// isTransactionConflict reports OCI's optimistic-locking failure: a 409 raised when
// concurrent writes to one parent (e.g. CreateBuildPipeline calls on one DevOps
// project) overlap. Transient — the loser retries, unlike an already-exists conflict.
func isTransactionConflict(err error) bool {
	failure, ok := asServiceError(err)
	return ok && failure.GetHTTPStatusCode() == http.StatusConflict &&
		strings.Contains(strings.ToLower(failure.GetMessage()), "transaction conflict")
}
