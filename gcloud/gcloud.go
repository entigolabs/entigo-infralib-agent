package gcloud

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"strings"
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
	g.enableApiServices()
	bucket := fmt.Sprintf("%s-%s", g.cloudPrefix, g.projectId)
	codeStorage, err := NewStorage(g.ctx, g.projectId, g.location, bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create storage bucket: %s", err)
	}
	logging, err := NewLogging(g.ctx, g.projectId)
	if err != nil {
		common.Logger.Fatalf("Failed to create logging client: %s", err)
	}
	iam, err := NewIAM(g.ctx, g.projectId)
	if err != nil {
		common.Logger.Fatalf("Failed to create IAM service: %s", err)
	}
	serviceAccount := g.createServiceAccount(iam)
	builder, err := NewBuilder(g.ctx, g.projectId, g.location, g.zone, serviceAccount)
	if err != nil {
		common.Logger.Fatalf("Failed to create builder: %s", err)
	}
	pipeline, err := NewPipeline(g.ctx, g.projectId, g.location, g.cloudPrefix, serviceAccount, codeStorage, builder, logging)
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

func (g *gcloudService) enableApiServices() {
	apiUsage, err := NewApiUsage(g.ctx, g.projectId)
	if err != nil {
		common.Logger.Fatalf("Failed to create API usage service: %s", err)
	}
	err = apiUsage.EnableServices([]string{"compute.googleapis.com", "cloudresourcemanager.googleapis.com",
		"secretmanager.googleapis.com", "run.googleapis.com", "container.googleapis.com", "dns.googleapis.com",
		"clouddeploy.googleapis.com"})
	if err != nil {
		common.Logger.Fatalf("Failed to enable services: %s", err)
	}
}

func (g *gcloudService) SetupCustomCodeRepo(_ string) (model.CodeRepo, error) {
	bucket := fmt.Sprintf("%s-%s-custom", g.cloudPrefix, g.projectId)
	return NewStorage(g.ctx, g.projectId, g.location, bucket)
}

func (g *gcloudService) createServiceAccount(iam *IAM) string {
	account, err := iam.GetOrCreateServiceAccount(g.cloudPrefix, "Entigo infralib service account")
	if err != nil {
		common.Logger.Fatalf("Failed to create service account: %s", err)
	}
	err = iam.AddRolesToServiceAccount(account.Name, []string{"roles/editor", "roles/iam.securityAdmin",
		"roles/iam.serviceAccountAdmin"})
	if err != nil {
		common.Logger.Fatalf("Failed to add roles to service account: %s", err)
	}
	err = iam.AddRolesToProject(account.Name, []string{"roles/editor", "roles/iam.securityAdmin",
		"roles/iam.serviceAccountAdmin", "roles/container.admin", "roles/secretmanager.secretAccessor"})
	if err != nil {
		common.Logger.Fatalf("Failed to add roles to project: %s", err)
	}
	nameParts := strings.Split(account.Name, "/")
	return nameParts[len(nameParts)-1]
}
