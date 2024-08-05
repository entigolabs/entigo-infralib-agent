package aws

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsS3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"io"
	"strings"
)

const bucketArnFormat = "arn:aws:s3:::%s"

type S3 struct {
	ctx          context.Context
	awsS3        *awsS3.Client
	region       string
	bucket       string
	repoMetadata *model.RepositoryMetadata
}

func NewS3(ctx context.Context, awsConfig aws.Config, bucket string) *S3 {
	return &S3{
		ctx:    ctx,
		awsS3:  awsS3.NewFromConfig(awsConfig),
		region: awsConfig.Region,
		bucket: bucket,
	}
}

func (s *S3) CreateBucket() (string, bool, error) {
	var createBucketConfiguration *types.CreateBucketConfiguration = nil
	if s.region != "us-east-1" {
		createBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(s.region),
		}
	}
	_, err := s.awsS3.CreateBucket(s.ctx, &awsS3.CreateBucketInput{
		Bucket:                    aws.String(s.bucket),
		ACL:                       types.BucketCannedACLPrivate,
		CreateBucketConfiguration: createBucketConfiguration,
	})
	if err != nil {
		var existsError *types.BucketAlreadyExists
		var ownedError *types.BucketAlreadyOwnedByYou
		if errors.As(err, &existsError) || errors.As(err, &ownedError) {
			return fmt.Sprintf(bucketArnFormat, s.bucket), false, nil
		} else {
			return "", false, err
		}
	}
	common.Logger.Printf("Created S3 Bucket %s\n", s.bucket)
	err = s.putBucketVersioning()
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf(bucketArnFormat, s.bucket), true, nil
}

func (s *S3) Delete() error {
	err := s.truncateBucket()
	if err != nil {
		return checkNotFoundError(err)
	}
	_, err = s.awsS3.DeleteBucket(s.ctx, &awsS3.DeleteBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		return checkNotFoundError(err)
	}
	common.Logger.Printf("Deleted S3 Bucket %s\n", s.bucket)
	return nil
}

func (s *S3) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	if s.repoMetadata != nil {
		return s.repoMetadata, nil
	}
	exists, err := s.bucketExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	s.repoMetadata = &model.RepositoryMetadata{
		Name: s.bucket,
		URL:  fmt.Sprintf("%s/", s.bucket),
	}
	return s.repoMetadata, nil
}

func (s *S3) PutFile(file string, content []byte) error {
	_, err := s.awsS3.PutObject(s.ctx, &awsS3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(file),
		Body:   bytes.NewReader(content),
	})
	return err
}

func (s *S3) GetFile(file string) ([]byte, error) {
	output, err := s.awsS3.GetObject(s.ctx, &awsS3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(file),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		var apiErr smithy.APIError
		if errors.As(err, &noSuchKey) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey") {
			return nil, nil
		}
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(output.Body)
	return io.ReadAll(output.Body)
}

func (s *S3) DeleteFile(file string) error {
	_, err := s.awsS3.DeleteObject(s.ctx, &awsS3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(file),
	})
	return checkNotFoundError(err)
}

func (s *S3) CheckFolderExists(folder string) (bool, error) {
	if !strings.HasSuffix(folder, "/") {
		folder += "/"
	}
	output, err := s.awsS3.ListObjectsV2(s.ctx, &awsS3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(folder),
		MaxKeys: 1,
	})
	if err != nil {
		return false, err
	}
	// Err is nil even if folder doesn't exist, can only check if folder with files exists
	return output.Contents != nil, nil
}

func (s *S3) ListFolderFiles(folder string) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder += "/"
	}
	output, err := s.awsS3.ListObjectsV2(s.ctx, &awsS3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(folder),
	})
	if err != nil {
		return nil, err
	}
	var files []string
	for _, object := range output.Contents {
		files = append(files, *object.Key)
	}
	return files, nil
}

func (s *S3) bucketExists() (bool, error) {
	_, err := s.awsS3.HeadBucket(s.ctx, &awsS3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err == nil {
		return true, nil
	}
	return false, checkNotFoundError(err)
}

func checkNotFoundError(err error) error {
	var noSuchBucket *types.NoSuchBucket
	var apiErr smithy.APIError
	if errors.As(err, &noSuchBucket) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket") {
		return nil
	}
	return err
}

func (s *S3) truncateBucket() error {
	input := &awsS3.ListObjectVersionsInput{
		Bucket: aws.String(s.bucket),
	}

	for {
		list, err := s.awsS3.ListObjectVersions(s.ctx, input)
		if err != nil {
			return err
		}
		common.Logger.Printf("Emptying bucket %s...\n", s.bucket)
		for _, version := range list.Versions {
			_, err = s.awsS3.DeleteObjects(s.ctx, &awsS3.DeleteObjectsInput{
				Bucket: aws.String(s.bucket),
				Delete: &types.Delete{
					Objects: []types.ObjectIdentifier{
						{
							Key:       version.Key,
							VersionId: version.VersionId,
						},
					},
				},
			})
			if err != nil {
				return err
			}
		}

		if list.IsTruncated {
			input.KeyMarker = list.NextKeyMarker
			input.VersionIdMarker = list.NextVersionIdMarker
		} else {
			return nil
		}
	}
}

func (s *S3) putBucketVersioning() error {
	_, err := s.awsS3.PutBucketVersioning(s.ctx, &awsS3.PutBucketVersioningInput{
		Bucket: aws.String(s.bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	})
	return err
}

func (s *S3) addDummyZip() error {
	buffer := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buffer)
	err := zipWriter.Close()
	if err != nil {
		return err
	}
	return s.PutFile(model.AgentSource, buffer.Bytes())
}
