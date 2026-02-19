package gcloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

type GStorage struct {
	ctx           context.Context
	client        storage.Client
	projectId     string
	location      string
	bucket        string
	bucketHandle  *storage.BucketHandle
	bucketCreated *bool
	repoMetadata  *model.RepositoryMetadata
}

func NewStorage(ctx context.Context, options []option.ClientOption, projectId string, location string, bucket string) (*GStorage, error) {
	client, err := storage.NewClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	bucketHandle := client.Bucket(bucket)
	return &GStorage{
		ctx:          ctx,
		client:       *client,
		projectId:    projectId,
		location:     location,
		bucket:       bucket,
		bucketHandle: bucketHandle,
	}, nil
}

func (g *GStorage) CreateBucket(skipDelay bool) error {
	exists, err := g.BucketExists()
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	util.DelayBucketCreation(g.bucket, skipDelay)
	err = g.bucketHandle.Create(g.ctx, g.projectId, &storage.BucketAttrs{
		Location:                 g.location,
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
		VersioningEnabled:        true,
		SoftDeletePolicy:         &storage.SoftDeletePolicy{RetentionDuration: 0},
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		Lifecycle: storage.Lifecycle{
			Rules: []storage.LifecycleRule{{
				Action:    storage.LifecycleAction{Type: storage.DeleteAction},
				Condition: storage.LifecycleCondition{NumNewerVersions: 5},
			}},
		},
		Labels: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
	})
	if err == nil {
		log.Printf("Created GCloud Storage Bucket %s\n", g.bucket)
		g.bucketCreated = aws.Bool(true)
	}
	return err
}

func (g *GStorage) Delete() error {
	exists, err := g.BucketExists()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	log.Printf("Emptying bucket %s...\n", g.bucket)
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{
		Versions: true,
	})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return err
		}
		err = g.bucketHandle.Object(attrs.Name).Generation(attrs.Generation).Delete(g.ctx)
		if err != nil {
			return err
		}
	}
	err = g.bucketHandle.Delete(g.ctx)
	if err == nil {
		log.Printf("Deleted GCloud Storage Bucket %s\n", g.bucket)
	}
	return err
}

func (g *GStorage) addEncryption(kmsKeyName string) error {
	attrs, err := g.bucketHandle.Attrs(g.ctx)
	if err != nil {
		return fmt.Errorf("failed to get bucket attributes: %w", err)
	}
	if attrs.Encryption != nil && attrs.Encryption.DefaultKMSKeyName == kmsKeyName {
		return nil
	}
	_, err = g.bucketHandle.Update(g.ctx, storage.BucketAttrsToUpdate{
		Encryption: &storage.BucketEncryption{
			DefaultKMSKeyName: kmsKeyName,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to set bucket encryption: %w", err)
	}
	log.Printf("Set KMS encryption on bucket %s\n", g.bucket)
	return nil
}

func (g *GStorage) BucketExists() (bool, error) {
	if g.bucketCreated != nil {
		return *g.bucketCreated, nil
	}
	_, err := g.bucketHandle.Attrs(g.ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrBucketNotExist) {
		return false, nil
	}
	return false, err
}

func (g *GStorage) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	if g.repoMetadata != nil {
		return g.repoMetadata, nil
	}
	exists, err := g.BucketExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	g.repoMetadata = &model.RepositoryMetadata{
		Name: g.bucket,
		URL:  g.bucket,
	}
	return g.repoMetadata, nil
}

func (g *GStorage) PutFile(file string, content []byte) error {
	writer := g.bucketHandle.Object(file).NewWriter(g.ctx)
	_, err := writer.Write(content)
	defer func(writer *storage.Writer) {
		_ = writer.Close()
	}(writer)
	return err
}

func (g *GStorage) GetFile(file string) ([]byte, error) {
	reader, err := g.bucketHandle.Object(file).NewReader(g.ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func(reader *storage.Reader) {
		_ = reader.Close()
	}(reader)
	return io.ReadAll(reader)
}

func (g *GStorage) DeleteFiles(files []string) error {
	for _, file := range files {
		err := g.bucketHandle.Object(file).Delete(g.ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *GStorage) DeleteFile(file string) error {
	err := g.bucketHandle.Object(file).Delete(g.ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}

func (g *GStorage) CheckFolderExists(folder string) (bool, error) {
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{Prefix: folder})
	_, err := it.Next()
	if errors.Is(err, iterator.Done) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (g *GStorage) ListFolderFiles(folder string) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder = folder + "/"
	}
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{Prefix: folder})
	var files []string
	for {
		obj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			return nil, err
		}
		files = append(files, obj.Name)
	}
	return files, nil
}

func (g *GStorage) ListFolderFilesWithExclude(folder string, excludeFolders model.Set[string]) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder = folder + "/"
	}
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{
		Prefix:                   folder,
		Delimiter:                "/",
		IncludeFoldersAsPrefixes: true,
	})
	var files []string
	for {
		obj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			return nil, err
		}
		if obj.Name != "" {
			files = append(files, obj.Name)
			continue
		}
		if obj.Prefix == "" || excludeFolders.Contains(strings.TrimSuffix(strings.TrimPrefix(obj.Prefix, folder), "/")) {
			continue
		}
		subFiles, err := g.ListFolderFiles(obj.Prefix)
		if err != nil {
			return nil, err
		}
		files = append(files, subFiles...)
	}
	return files, nil
}
