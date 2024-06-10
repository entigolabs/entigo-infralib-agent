package service

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/gcloud"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"strings"
)

func GetCloudProvider(ctx context.Context, flags *common.Flags) model.CloudProvider {
	prefix := GetAwsPrefix(flags)
	if flags.ProjectId != "" {
		common.Logger.Println("Using GCloud with project ID: ", flags.ProjectId)
		return gcloud.NewGCloud(ctx, strings.ToLower(prefix), flags.ProjectId)
	}
	return aws.NewAWS(ctx, strings.ToLower(prefix))
}
