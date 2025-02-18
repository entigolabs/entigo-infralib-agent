package gcloud

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
)

type gcloudProvider struct {
	ctx       context.Context
	projectId string
	location  string
	zone      string
}

func NewGCloudProvider(ctx context.Context, gCloud common.GCloud) model.ResourceProvider {
	return &gcloudProvider{
		ctx:       ctx,
		projectId: gCloud.ProjectId,
		location:  gCloud.Location,
		zone:      gCloud.Zone,
	}
}

func (g *gcloudProvider) GetSSM() model.SSM {
	sm, err := NewSM(g.ctx, g.projectId)
	if err != nil {
		log.Fatalf("Failed to create secret manager: %s", err)
	}
	return sm
}
