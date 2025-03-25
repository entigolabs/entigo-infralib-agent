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

func NewAWSProvider(ctx context.Context, awsFlags common.AWS) model.ResourceProvider {
	awsConfig := GetAWSConfig(ctx, awsFlags.RoleArn)
	accountId, err := getAccountId(awsConfig)
	if err != nil {
		log.Fatal(err.Error())
	}
	log.Printf("AWS account id: %s\n", accountId)
	return &awsProvider{
		ctx:          ctx,
		awsConfig:    awsConfig,
		accountId:    accountId,
		providerType: model.AWS,
	}
}

func (a *awsProvider) GetSSM() model.SSM {
	return NewSSM(a.ctx, a.awsConfig)
}

func (a *awsProvider) GetBucket(prefix string) model.Bucket {
	bucket := getBucketName(prefix, a.accountId, a.awsConfig.Region)
	return NewS3(a.ctx, a.awsConfig, bucket)
}

func (a *awsProvider) GetProviderType() model.ProviderType {
	return a.providerType
}
