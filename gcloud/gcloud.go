package gcloud

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
	"strings"
	"time"
)

type gcloudService struct {
	ctx         context.Context
	cloudPrefix string
	projectId   string
	location    string
	zone        string
	resources   Resources
}

type Resources struct {
	model.CloudResources
}

func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"prefix": key,
		"bucket": r.BucketName,
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

func (g *gcloudService) SetupResources() model.Resources {
	// TODO Default clients use gRPC, connections must be closed before exiting
	g.enableApiServices()
	bucket := g.getBucketName()
	codeStorage, err := NewStorage(g.ctx, g.projectId, g.location, bucket)
	if err != nil {
		log.Fatalf("Failed to create storage service: %s", err)
	}
	err = codeStorage.CreateBucket()
	if err != nil {
		log.Fatalf("Failed to create storage bucket: %s", err)
	}
	logging, err := NewLogging(g.ctx, g.projectId)
	if err != nil {
		log.Fatalf("Failed to create logging client: %s", err)
	}
	iam, err := NewIAM(g.ctx, g.projectId)
	if err != nil {
		log.Fatalf("Failed to create IAM service: %s", err)
	}
	serviceAccount := g.createServiceAccount(iam)
	builder, err := NewBuilder(g.ctx, g.projectId, g.location, g.zone, serviceAccount)
	if err != nil {
		log.Fatalf("Failed to create builder: %s", err)
	}
	pipeline, err := NewPipeline(g.ctx, g.projectId, g.location, g.cloudPrefix, serviceAccount, codeStorage, builder, logging)
	if err != nil {
		log.Fatalf("Failed to create pipeline: %s", err)
	}
	sm, err := NewSM(g.ctx, g.projectId)
	if err != nil {
		log.Fatalf("Failed to create secret manager: %s", err)
	}
	return Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			Bucket:       codeStorage,
			CodeBuild:    builder,
			Pipeline:     pipeline,
			SSM:          sm,
			BucketName:   bucket,
			CloudPrefix:  g.cloudPrefix,
		},
	}
}

func (g *gcloudService) GetResources() model.Resources {
	bucket := g.getBucketName()
	codeStorage, err := NewStorage(g.ctx, g.projectId, g.location, bucket)
	if err != nil {
		log.Fatalf("Failed to create storage service: %s", err)
	}
	builder, err := NewBuilder(g.ctx, g.projectId, g.location, g.zone, "")
	if err != nil {
		log.Fatalf("Failed to create builder: %s", err)
	}
	pipeline, err := NewPipeline(g.ctx, g.projectId, g.location, g.cloudPrefix, "", codeStorage, builder, nil)
	if err != nil {
		log.Fatalf("Failed to create pipeline: %s", err)
	}
	g.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			Bucket:       codeStorage,
			CodeBuild:    builder,
			Pipeline:     pipeline,
			CloudPrefix:  g.cloudPrefix,
			BucketName:   bucket,
		},
	}
	return g.resources
}

func (g *gcloudService) DeleteResources(deleteBucket bool) {
	agentJob := fmt.Sprintf("%s-agent-%s", g.cloudPrefix, common.RunCommand)
	err := g.resources.GetBuilder().(*Builder).deleteJob(agentJob)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent job %s: %s", agentJob, err))
	}
	agentJob = fmt.Sprintf("%s-agent-%s", g.cloudPrefix, common.UpdateCommand)
	err = g.resources.GetBuilder().(*Builder).deleteJob(agentJob)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent job %s: %s", agentJob, err))
	}
	err = g.resources.GetPipeline().(*Pipeline).deleteTargets()
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete pipeline targets: %s", err))
	}
	iam, err := NewIAM(g.ctx, g.projectId)
	if err != nil {
		log.Fatalf("Failed to create IAM service: %s", err)
	}
	accountName := fmt.Sprintf("%s-agent-%s", g.cloudPrefix, g.location)
	if len(accountName) > 30 {
		accountName = accountName[:30]
	}
	err = iam.DeleteServiceAccount(accountName)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete service account %s: %s", accountName, err))
	}
	if !deleteBucket {
		log.Printf("Terraform state bucket %s will not be deleted, delete it manually if needed\n", g.resources.GetBucketName())
		return
	}
	err = g.resources.GetBucket().Delete()
	if err != nil {
		bucket := fmt.Sprintf("%s-%s", g.cloudPrefix, g.projectId)
		common.PrintWarning(fmt.Sprintf("Failed to delete storage bucket %s: %s", bucket, err))
	}
}

func (g *gcloudService) enableApiServices() {
	apiUsage, err := NewApiUsage(g.ctx, g.projectId)
	if err != nil {
		log.Fatalf("Failed to create API usage service: %s", err)
	}
	err = apiUsage.EnableServices([]string{"compute.googleapis.com", "cloudresourcemanager.googleapis.com",
		"secretmanager.googleapis.com", "run.googleapis.com", "container.googleapis.com", "dns.googleapis.com",
		"clouddeploy.googleapis.com", "certificatemanager.googleapis.com"})
	if err != nil {
		log.Fatalf("Failed to enable services: %s", err)
	}
}

func (g *gcloudService) createServiceAccount(iam *IAM) string {
	accountName := fmt.Sprintf("%s-agent-%s", g.cloudPrefix, g.location)
	if len(accountName) > 30 {
		accountName = accountName[:30]
	}
	account, created, err := iam.GetOrCreateServiceAccount(accountName, "Entigo infralib service account")
	if err != nil {
		log.Fatalf("Failed to create service account: %s", err)
	}
	err = iam.AddRolesToServiceAccount(account.Name, []string{"roles/editor", "roles/iam.securityAdmin",
		"roles/iam.serviceAccountAdmin"})
	if err != nil {
		log.Fatalf("Failed to add roles to service account: %s", err)
	}
	err = iam.AddRolesToProject(account.Name, []string{"roles/editor", "roles/iam.securityAdmin",
		"roles/iam.serviceAccountAdmin", "roles/container.admin", "roles/secretmanager.secretAccessor"})
	if err != nil {
		log.Fatalf("Failed to add roles to project: %s", err)
	}
	if created {
		log.Println("Waiting 60 seconds for service account permissions to be applied...")
		time.Sleep(60 * time.Second)
	}
	nameParts := strings.Split(account.Name, "/")
	return nameParts[len(nameParts)-1]
}

func (g *gcloudService) getBucketName() string {
	return fmt.Sprintf("%s-%s-%s", g.cloudPrefix, g.projectId, g.location)
}

func (g *gcloudService) CreateServiceAccount() {
	username := fmt.Sprintf("%s-sa-%s", g.cloudPrefix, g.location)
	if len(username) > 30 {
		common.Logger.Fatalf("Service account name %s is too long, must be fewer than 30 characters", username)
	}
	iam, err := NewIAM(g.ctx, g.projectId)
	if err != nil {
		common.Logger.Fatalf("Failed to create IAM service: %s", err)
	}
	secrets, err := NewSM(g.ctx, g.projectId)
	if err != nil {
		common.Logger.Fatalf("Failed to create secret manager: %s", err)
	}
	account, created, err := iam.GetOrCreateServiceAccount(username, "Entigo infralib CI/CD service account")
	if err != nil {
		common.Logger.Fatalf("Failed to create service account: %s", err)
	}
	if !created {
		common.Logger.Printf("Service account %s already exists\n", account.Name)
		return
	}
	err = iam.AddRolesToServiceAccount(account.Name, []string{"roles/editor", "roles/iam.securityAdmin"})
	if err != nil {
		common.Logger.Fatalf("Failed to add roles to service account: %s", err)
	}
	err = iam.AddRolesToProject(account.Name, []string{"roles/editor", "roles/iam.securityAdmin"})
	if err != nil {
		common.Logger.Fatalf("Failed to add roles to project: %s", err)
	}
	time.Sleep(1 * time.Second) // Creating key immediately after account creation may fail with 404
	key, err := iam.CreateServiceAccountKey(account.Name)
	if err != nil {
		common.Logger.Fatalf("Failed to create service account key: %v", err)
	}

	keyParam := fmt.Sprintf("entigo-infralib-%s-key", username)
	err = secrets.PutParameter(keyParam, key.PrivateKeyData)
	if err != nil {
		common.Logger.Fatalf("Failed to create secret %s: %v", keyParam, err)
	}
	common.Logger.Printf("Service account secret %s stored in SM", keyParam)
}
