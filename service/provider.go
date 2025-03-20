package service

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/gcloud"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
	"strings"
)

func GetCloudProvider(ctx context.Context, flags *common.Flags) model.CloudProvider {
	prefix := GetProviderPrefix(flags)
	if flags.GCloud.ProjectId != "" {
		log.Println("Using GCloud with project ID: ", flags.GCloud.ProjectId)
		return gcloud.NewGCloud(ctx, strings.ToLower(prefix), flags.GCloud, flags.Pipeline, flags.SkipBucketCreationDelay)
	}
	return aws.NewAWS(ctx, strings.ToLower(prefix), flags.AWS, flags.Pipeline, flags.SkipBucketCreationDelay)
}

func GetResourceProvider(ctx context.Context, flags *common.Flags) model.ResourceProvider {
	if flags.GCloud.ProjectId != "" {
		log.Println("Using GCloud with project ID: ", flags.GCloud.ProjectId)
		return gcloud.NewGCloudProvider(ctx, flags.GCloud)
	}
	return aws.NewAWSProvider(ctx, flags.AWS)
}
