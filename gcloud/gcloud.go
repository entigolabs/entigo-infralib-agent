package gcloud

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type gcloudService struct {
	ctx         context.Context
	cloudPrefix string
	projectId   string
}

type Resources struct {
	model.CloudResources
}

func NewGCloud(ctx context.Context, cloudPrefix string, projectId string) model.CloudProvider {
	return &gcloudService{
		ctx:         ctx,
		cloudPrefix: cloudPrefix,
		projectId:   projectId,
	}
}

func (g *gcloudService) SetupResources(_ string) model.Resources {
	bucket := fmt.Sprintf("%s-%s", g.cloudPrefix, g.projectId)
	_, err := NewStorage(g.ctx, g.projectId, bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create storage bucket: %s", err)
	}
	codeBucket := fmt.Sprintf("%s-%s-code", g.cloudPrefix, g.projectId)
	codeStorage, err := NewStorage(g.ctx, g.projectId, codeBucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create code storage bucket: %s", err)
	}
	return Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			CodeRepo:     codeStorage,
			CodeBuild:    NewBuilder(),
			Bucket:       bucket,
		},
	}
}

func (g *gcloudService) SetupCustomCodeRepo(_ string) (model.CodeRepo, error) {
	bucket := fmt.Sprintf("%s-%s-custom", g.cloudPrefix, g.projectId)
	return NewStorage(g.ctx, g.projectId, bucket)
}
