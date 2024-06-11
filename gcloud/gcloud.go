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
	// TODO Add Log messages when creating resources, just like with AWS
	// TODO Default clients use gRPC, connections must be closed before exiting
	bucket := fmt.Sprintf("%s-%s", g.cloudPrefix, g.projectId)
	codeStorage, err := NewStorage(g.ctx, g.projectId, bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create storage bucket: %s", err)
	}
	builder, err := NewBuilder(g.ctx, g.projectId, "infralib-agent@entigo-infralib.iam.gserviceaccount.com", bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create builder: %s", err)
	}
	pipeline, err := NewPipeline(g.ctx, g.projectId, "infralib-agent@entigo-infralib.iam.gserviceaccount.com", bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create pipeline: %s", err)
	}
	return Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			CodeRepo:     codeStorage,
			CodeBuild:    builder,
			Pipeline:     pipeline,
			Bucket:       bucket,
			CloudPrefix:  g.cloudPrefix,
		},
	}
}

func (g *gcloudService) SetupCustomCodeRepo(_ string) (model.CodeRepo, error) {
	bucket := fmt.Sprintf("%s-%s-custom", g.cloudPrefix, g.projectId)
	return NewStorage(g.ctx, g.projectId, bucket)
}
