package aws

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
)

type awsProvider struct {
	ctx          context.Context
	awsConfig    aws.Config
	accountId    string
	providerType model.ProviderType
}

func NewAWSProvider(ctx context.Context, awsFlags common.AWS) (model.ResourceProvider, error) {
	awsConfig, err := GetAWSConfig(ctx, awsFlags.RoleArn)
	if err != nil {
		return nil, err
	}
	accountId, err := getAccountId(awsConfig)
	if err != nil {
		return nil, err
	}
	log.Printf("AWS account id: %s\n", accountId)
	return &awsProvider{
		ctx:          ctx,
		awsConfig:    awsConfig,
		accountId:    accountId,
		providerType: model.AWS,
	}, nil
}

func (a *awsProvider) GetSSM() (model.SSM, error) {
	return NewSSM(a.ctx, a.awsConfig), nil
}

func (a *awsProvider) GetBucket(prefix string) (model.Bucket, error) {
	bucket := getBucketName(prefix, a.accountId, a.awsConfig.Region)
	return NewS3(a.ctx, a.awsConfig, bucket), nil
}

func (a *awsProvider) GetProviderType() model.ProviderType {
	return a.providerType
}
