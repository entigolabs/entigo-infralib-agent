package gcloud

import (
	"cloud.google.com/go/storage"
	"context"
	"errors"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"google.golang.org/api/iterator"
	"io"
	"strings"
)

type GStorage struct {
	ctx          context.Context
	client       storage.Client
	projectId    string
	location     string
	bucket       string
	bucketHandle *storage.BucketHandle
}

func NewStorage(ctx context.Context, projectId string, location string, bucket string) (*GStorage, error) {
	client, err := storage.NewClient(ctx)
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

func (g *GStorage) CreateBucket() error {
	exists, err := bucketExists(g.ctx, g.bucketHandle)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	common.Logger.Printf("Creating GCloud Storage Bucket %s\n", g.bucket)
	return g.bucketHandle.Create(g.ctx, g.projectId, &storage.BucketAttrs{
		Location:                   g.location,
		PredefinedACL:              "projectPrivate",
		PredefinedDefaultObjectACL: "projectPrivate",
		PublicAccessPrevention:     storage.PublicAccessPreventionEnforced,
		VersioningEnabled:          true,
	})
}

func (g *GStorage) Delete() error {
	exists, err := bucketExists(g.ctx, g.bucketHandle)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	err = g.bucketHandle.Delete(g.ctx)
	if err == nil {
		common.Logger.Printf("Deleted GCloud Storage Bucket %s\n", g.bucket)
	}
	return err
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
	return io.ReadAll(reader)
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
		if !strings.HasPrefix(obj.Name, folder+"/") {
			continue
		}
		files = append(files, strings.TrimPrefix(obj.Name, folder+"/"))
	}
	return files, nil
}
