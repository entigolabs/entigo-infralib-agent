package service

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsS3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

type S3 interface {
	CreateBucket(bucketName string) (string, error)
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
	_, err := s.awsS3.CreateBucket(context.Background(), &awsS3.CreateBucketInput{
		Bucket: aws.String(bucketName),
		ACL:    types.BucketCannedACLPrivate,
		CreateBucketConfiguration: &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(s.region),
		},
	})
	if err != nil {
		var existsError *types.BucketAlreadyExists
		var ownedError *types.BucketAlreadyOwnedByYou
		if errors.As(err, &existsError) || errors.As(err, &ownedError) {
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
		Key:    aws.String(agentSource),
		Body:   bytes.NewReader(buffer.Bytes()),
	})
	return err
}
