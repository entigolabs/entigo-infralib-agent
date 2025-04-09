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
	pipelineFlags := ProcessPipelineFlags(flags.Pipeline)
	if flags.GCloud.ProjectId != "" {
		log.Println("Using GCloud with project ID: ", flags.GCloud.ProjectId)
		return gcloud.NewGCloud(ctx, strings.ToLower(prefix), flags.GCloud, pipelineFlags, flags.SkipBucketCreationDelay)
	}
	return aws.NewAWS(ctx, strings.ToLower(prefix), flags.AWS, pipelineFlags, flags.SkipBucketCreationDelay)
}

func GetResourceProvider(ctx context.Context, flags *common.Flags) model.ResourceProvider {
	if flags.GCloud.ProjectId != "" {
		log.Println("Using GCloud with project ID: ", flags.GCloud.ProjectId)
		return gcloud.NewGCloudProvider(ctx, flags.GCloud)
	}
	return aws.NewAWSProvider(ctx, flags.AWS)
}

func ProcessPipelineFlags(pipeline common.Pipeline) common.Pipeline {
	if pipeline.TerraformCache.Value != nil {
		return pipeline
	}
	var enable bool
	if pipeline.Type == string(common.PipelineTypeLocal) {
		enable = false
	} else {
		enable = true
	}
	pipeline.TerraformCache.Value = &enable
	return pipeline
}
