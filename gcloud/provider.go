package gcloud

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
)

type gcloudProvider struct {
	ctx          context.Context
	projectId    string
	location     string
	zone         string
	providerType model.ProviderType
}

func NewGCloudProvider(ctx context.Context, gCloud common.GCloud) model.ResourceProvider {
	return &gcloudProvider{
		ctx:          ctx,
		projectId:    gCloud.ProjectId,
		location:     gCloud.Location,
		zone:         gCloud.Zone,
		providerType: model.GCLOUD,
	}
}

func (g *gcloudProvider) GetSSM() model.SSM {
	sm, err := NewSM(g.ctx, g.projectId)
	if err != nil {
		log.Fatalf("Failed to create secret manager: %s", err)
	}
	return sm
}

func (g *gcloudProvider) GetBucket(prefix string) model.Bucket {
	bucket := getBucketName(prefix, g.projectId, g.location)
	storage, err := NewStorage(g.ctx, g.projectId, g.location, bucket)
	if err != nil {
		log.Fatalf("Failed to create storage service: %s", err)
	}
	return storage
}

func (g *gcloudProvider) GetProviderType() model.ProviderType {
	return g.providerType
}
