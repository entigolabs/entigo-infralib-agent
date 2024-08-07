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
)

type S3 interface {
	CreateBucket(bucketName string) (string, error)
	DeleteBucket(bucketName string) error
}

type s3 struct {
	awsS3  *awsS3.Client
	region string
}

func NewS3(awsConfig aws.Config) S3 {
	return &s3{
		awsS3:  awsS3.NewFromConfig(awsConfig),
		region: awsConfig.Region,
	}
}

func (s *s3) CreateBucket(bucketName string) (string, error) {
	var createBucketConfiguration *types.CreateBucketConfiguration = nil
	if s.region != "us-east-1" {
		createBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(s.region),
		}
	}
	_, err := s.awsS3.CreateBucket(context.Background(), &awsS3.CreateBucketInput{
		Bucket:                    aws.String(bucketName),
		ACL:                       types.BucketCannedACLPrivate,
		CreateBucketConfiguration: createBucketConfiguration,
	})
	if err != nil {
		var existsError *types.BucketAlreadyExists
		var ownedError *types.BucketAlreadyOwnedByYou
		if errors.As(err, &existsError) || errors.As(err, &ownedError) {
			err = s.createDummyZip(bucketName)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("arn:aws:s3:::%s", bucketName), nil
		} else {
			return "", err
		}
	}
	common.Logger.Printf("Created S3 Bucket %s\n", bucketName)
	err = s.putBucketVersioning(bucketName)
	if err != nil {
		return "", err
	}
	err = s.createDummyZip(bucketName)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("arn:aws:s3:::%s", bucketName), nil
}

func (s *s3) DeleteBucket(bucketName string) error {
	err := s.truncateBucket(bucketName)
	if err != nil {
		return checkNotFoundError(err)
	}
	_, err = s.awsS3.DeleteBucket(context.Background(), &awsS3.DeleteBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		return checkNotFoundError(err)
	}
	common.Logger.Printf("Deleted S3 Bucket %s\n", bucketName)
	return nil
}

func checkNotFoundError(err error) error {
	var noSuchBucket *types.NoSuchBucket
	var apiErr smithy.APIError
	if errors.As(err, &noSuchBucket) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket") {
		return nil
	}
	return err
}

func (s *s3) truncateBucket(bucketName string) error {
	input := &awsS3.ListObjectVersionsInput{
		Bucket: aws.String(bucketName),
	}

	for {
		list, err := s.awsS3.ListObjectVersions(context.Background(), input)
		if err != nil {
			return err
		}
		common.Logger.Printf("Emptying bucket %s...\n", bucketName)
		for _, version := range list.Versions {
			_, err = s.awsS3.DeleteObjects(context.Background(), &awsS3.DeleteObjectsInput{
				Bucket: aws.String(bucketName),
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

func (s *s3) putBucketVersioning(bucketName string) error {
	_, err := s.awsS3.PutBucketVersioning(context.Background(), &awsS3.PutBucketVersioningInput{
		Bucket: aws.String(bucketName),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	})
	return err
}

func (s *s3) createDummyZip(bucketName string) error {
	buffer := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buffer)
	err := zipWriter.Close()
	if err != nil {
		return err
	}
	_, err = s.awsS3.PutObject(context.Background(), &awsS3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(model.AgentSource),
		Body:   bytes.NewReader(buffer.Bytes()),
	})
	return err
}
