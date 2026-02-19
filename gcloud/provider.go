package gcloud

import (
	"context"
	"fmt"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"google.golang.org/api/option"
)

type gcloudProvider struct {
	ctx          context.Context
	projectId    string
	location     string
	zone         string
	providerType model.ProviderType
	options      []option.ClientOption
}

func NewGCloudProvider(ctx context.Context, gCloud common.GCloud) (model.ResourceProvider, error) {
	options, err := getClientOptions(gCloud)
	if err != nil {
		return nil, err
	}
	return &gcloudProvider{
		ctx:          ctx,
		projectId:    gCloud.ProjectId,
		location:     gCloud.Location,
		zone:         gCloud.Zone,
		providerType: model.GCLOUD,
		options:      options,
	}, nil
}

func (g *gcloudProvider) GetSSM() (model.SSM, error) {
	err := g.enableSecretService()
	if err != nil {
		return nil, fmt.Errorf("failed to enable secret manager API: %w", err)
	}
	sm, err := NewSM(g.ctx, g.options, g.projectId, g.location)
	if err != nil {
		return nil, fmt.Errorf("failed to create secret manager: %w", err)
	}
	return sm, nil
}

func (g *gcloudProvider) enableSecretService() error {
	apiUsage, err := NewApiUsage(g.ctx, g.options, g.projectId)
	if err != nil {
		return fmt.Errorf("failed to create API usage service: %s", err)
	}
	return apiUsage.EnableService("secretmanager.googleapis.com")
}

func (g *gcloudProvider) GetBucket(prefix string) (model.Bucket, error) {
	bucket := getBucketName(prefix, g.projectId, g.location)
	storage, err := NewStorage(g.ctx, g.options, g.projectId, g.location, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage service: %s", err)
	}
	return storage, nil
}

func (g *gcloudProvider) GetProviderType() model.ProviderType {
	return g.providerType
}
