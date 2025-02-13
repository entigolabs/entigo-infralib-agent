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
	pipelineType := common.PipelineType(flags.Pipeline.Type)
	prefix := GetProviderPrefix(flags)
	if flags.GCloud.ProjectId != "" {
		log.Println("Using GCloud with project ID: ", flags.GCloud.ProjectId)
		return gcloud.NewGCloud(ctx, strings.ToLower(prefix), flags.GCloud, pipelineType, flags.SkipBucketCreationDelay)
	}
	return aws.NewAWS(ctx, strings.ToLower(prefix), flags.AWS, pipelineType, flags.SkipBucketCreationDelay)
}
