package gcloud

import (
	"cloud.google.com/go/storage"
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"google.golang.org/api/iterator"
	"io"
	"strings"
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
	exists, err := g.bucketExists(g.ctx, g.bucketHandle)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	err = g.bucketHandle.Create(g.ctx, g.projectId, &storage.BucketAttrs{
		Location:                   g.location,
		PredefinedACL:              "projectPrivate",
		PredefinedDefaultObjectACL: "projectPrivate",
		PublicAccessPrevention:     storage.PublicAccessPreventionEnforced,
		VersioningEnabled:          true,
	})
	if err == nil {
		common.Logger.Printf("Created GCloud Storage Bucket %s\n", g.bucket)
		g.bucketCreated = aws.Bool(true)
	}
	return err
}

func (g *GStorage) Delete() error {
	exists, err := g.bucketExists(g.ctx, g.bucketHandle)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	common.Logger.Printf("Emptying bucket %s...\n", g.bucket)
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
		common.Logger.Printf("Deleted GCloud Storage Bucket %s\n", g.bucket)
	}
	return err
}

func (g *GStorage) bucketExists(ctx context.Context, bucketHandle *storage.BucketHandle) (bool, error) {
	if g.bucketCreated != nil {
		return *g.bucketCreated, nil
	}
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
	if g.repoMetadata != nil {
		return g.repoMetadata, nil
	}
	exists, err := g.bucketExists(g.ctx, g.bucketHandle)
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
		files = append(files, obj.Name)
	}
	return files, nil
}
