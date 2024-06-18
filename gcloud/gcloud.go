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
	location    string
	zone        string
}

type Resources struct {
	model.CloudResources
}

func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"prefix": key,
		"bucket": r.Bucket,
	}
}

func NewGCloud(ctx context.Context, cloudPrefix string, gCloud common.GCloud) model.CloudProvider {
	return &gcloudService{
		ctx:         ctx,
		cloudPrefix: cloudPrefix,
		projectId:   gCloud.ProjectId,
		location:    gCloud.Location,
		zone:        gCloud.Zone,
	}
}

func (g *gcloudService) SetupResources(_ string) model.Resources {
	// TODO Default clients use gRPC, connections must be closed before exiting
	bucket := fmt.Sprintf("%s-%s", g.cloudPrefix, g.projectId)
	codeStorage, err := NewStorage(g.ctx, g.projectId, g.location, bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create storage bucket: %s", err)
	}
	logging, err := NewLogging(g.ctx, g.projectId)
	if err != nil {
		common.Logger.Fatalf("Failed to create logging client: %s", err)
	}
	builder, err := NewBuilder(g.ctx, g.projectId, g.location, g.zone, "infralib-agent@entigo-infralib.iam.gserviceaccount.com")
	if err != nil {
		common.Logger.Fatalf("Failed to create builder: %s", err)
	}
	pipeline, err := NewPipeline(g.ctx, g.projectId, g.location, g.cloudPrefix,
		"infralib-agent@entigo-infralib.iam.gserviceaccount.com", codeStorage, bucket, builder, logging)
	if err != nil {
		common.Logger.Fatalf("Failed to create pipeline: %s", err)
	}
	sm, err := NewSM(g.ctx, g.projectId)
	if err != nil {
		common.Logger.Fatalf("Failed to create secret manager: %s", err)
	}
	return Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			CodeRepo:     codeStorage,
			CodeBuild:    builder,
			Pipeline:     pipeline,
			SSM:          sm,
			Bucket:       bucket,
			CloudPrefix:  g.cloudPrefix,
		},
	}
}

func (g *gcloudService) SetupCustomCodeRepo(_ string) (model.CodeRepo, error) {
	bucket := fmt.Sprintf("%s-%s-custom", g.cloudPrefix, g.projectId)
	return NewStorage(g.ctx, g.projectId, g.location, bucket)
}
