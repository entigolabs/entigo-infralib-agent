package gcloud

import (
	"cloud.google.com/go/storage"
	"context"
	"errors"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"google.golang.org/api/iterator"
)

type GStorage struct {
	ctx          context.Context
	client       storage.Client
	projectId    string
	bucket       string
	bucketHandle *storage.BucketHandle
}

func NewStorage(ctx context.Context, projectId string, bucket string) (*GStorage, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	bucketHandle := client.Bucket(bucket)
	// TODO make location configurable
	if err = createBucket(ctx, projectId, "europe-north1", bucketHandle); err != nil {
		return nil, err
	}
	return &GStorage{
		ctx:          ctx,
		client:       *client,
		projectId:    projectId,
		bucket:       bucket,
		bucketHandle: bucketHandle,
	}, nil
}

func createBucket(ctx context.Context, projectId string, location string, bucketHandle *storage.BucketHandle) error {
	exists, err := bucketExists(ctx, bucketHandle)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return bucketHandle.Create(ctx, projectId, &storage.BucketAttrs{
		Location:                   location,
		PredefinedACL:              "projectPrivate",
		PredefinedDefaultObjectACL: "projectPrivate",
		PublicAccessPrevention:     storage.PublicAccessPreventionEnforced,
		VersioningEnabled:          true,
	})
}

func bucketExists(ctx context.Context, bucketHandle *storage.BucketHandle) (bool, error) {
	_, err := bucketHandle.Attrs(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrBucketNotExist) {
		return false, nil
	}
	return false, err
}

func (g *GStorage) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	return &model.RepositoryMetadata{
		Name: g.bucket,
		URL:  g.bucket,
	}, nil
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
	var content []byte
	_, err = reader.Read(content)
	return content, err
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