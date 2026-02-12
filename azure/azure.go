package azure

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type azureService struct {
	ctx            context.Context
	credential     *azidentity.DefaultAzureCredential
	subscriptionId string
	resourceGroup  string
	location       string
	cloudPrefix    string
	resources      Resources
	pipeline       common.Pipeline
	skipDelay      bool
	devOpsOrg      string
	devOpsProject  string
}

type Resources struct {
	model.CloudResources
}

func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"key":                  key,
		"container_name":       "tfstate",
		"storage_account_name": r.BucketName,
		"resource_group_name":  r.Account,
		"use_azuread_auth":     "true",
	}
}

func NewAzure(ctx context.Context, cloudPrefix string, azureFlags common.Azure, pipeline common.Pipeline, skipBucketDelay bool) (model.CloudProvider, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Azure credentials: %w", err)
	}
	log.Printf("Azure session initialized with subscription: %s, resource group: %s, location: %s\n",
		azureFlags.SubscriptionId, azureFlags.ResourceGroup, azureFlags.Location)
	if azureFlags.DevOpsOrg != "" && azureFlags.DevOpsProject != "" {
		log.Printf("Azure DevOps configured: organization: %s, project: %s\n",
			azureFlags.DevOpsOrg, azureFlags.DevOpsProject)
	}
	return &azureService{
		ctx:            ctx,
		credential:     credential,
		subscriptionId: azureFlags.SubscriptionId,
		resourceGroup:  azureFlags.ResourceGroup,
		location:       azureFlags.Location,
		cloudPrefix:    cloudPrefix,
		pipeline:       pipeline,
		skipDelay:      skipBucketDelay,
		devOpsOrg:      azureFlags.DevOpsOrg,
		devOpsProject:  azureFlags.DevOpsProject,
	}, nil
}

func (a *azureService) GetIdentifier() string {
	return fmt.Sprintf("prefix %s, Azure subscription %s, resource group %s, location %s",
		a.cloudPrefix, a.subscriptionId, a.resourceGroup, a.location)
}

func (a *azureService) SetupMinimalResources() (model.Resources, error) {
	storageAccountName := a.getStorageAccountName()
	blob, err := a.createBlobStorage(storageAccountName)
	if err != nil {
		return nil, err
	}
	kv, err := NewKeyVault(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create key vault: %w", err)
	}
	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AZURE,
			Bucket:       blob,
			SSM:          kv,
			CloudPrefix:  a.cloudPrefix,
			BucketName:   storageAccountName,
			Region:       a.location,
			Account:      a.resourceGroup,
		},
	}
	return a.resources, nil
}

func (a *azureService) SetupResources(manager model.NotificationManager, config model.Config) (model.Resources, error) {
	storageAccountName := a.getStorageAccountName()
	blob, err := a.createBlobStorage(storageAccountName)
	if err != nil {
		return nil, err
	}
	kv, err := NewKeyVault(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create key vault: %w", err)
	}

	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AZURE,
			Bucket:       blob,
			SSM:          kv,
			CloudPrefix:  a.cloudPrefix,
			BucketName:   storageAccountName,
			Region:       a.location,
			Account:      a.resourceGroup,
		},
	}

	if a.pipeline.Type == string(common.PipelineTypeLocal) {
		return a.resources, nil
	}

	logging, err := NewLogging(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create logging: %w", err)
	}

	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup)
	if err != nil {
		return nil, fmt.Errorf("failed to create IAM: %w", err)
	}

	identity, err := a.createManagedIdentity(iam)
	if err != nil {
		return nil, err
	}

	builder, err := NewBuilder(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix, identity, *a.pipeline.TerraformCache.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to create builder: %w", err)
	}

	// Create DevOps client if configured
	var devOps *DevOpsClient
	if a.devOpsOrg != "" && a.devOpsProject != "" {
		devOps, err = NewDevOpsClient(a.ctx, a.credential, a.devOpsOrg, a.devOpsProject)
		if err != nil {
			return nil, fmt.Errorf("failed to create Azure DevOps client: %w", err)
		}
	}

	pipeline, err := NewPipeline(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix, identity, blob, builder, logging, manager, devOps)
	if err != nil {
		return nil, fmt.Errorf("failed to create pipeline: %w", err)
	}

	a.resources.CodeBuild = builder
	a.resources.Pipeline = pipeline

	err = a.createSchedule(config.Schedule, identity)
	if err != nil {
		return nil, err
	}

	return a.resources, nil
}

func (a *azureService) GetResources() (model.Resources, error) {
	storageAccountName := a.getStorageAccountName()
	blob, err := NewBlobStorage(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, storageAccountName)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob storage: %w", err)
	}

	builder, err := NewBuilder(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix, "", true)
	if err != nil {
		return nil, fmt.Errorf("failed to create builder: %w", err)
	}

	logging, err := NewLogging(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create logging: %w", err)
	}

	// Create DevOps client if configured
	var devOps *DevOpsClient
	if a.devOpsOrg != "" && a.devOpsProject != "" {
		devOps, err = NewDevOpsClient(a.ctx, a.credential, a.devOpsOrg, a.devOpsProject)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create Azure DevOps client: %s", err)))
		}
	}

	pipeline, err := NewPipeline(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix, "", blob, builder, logging, nil, devOps)
	if err != nil {
		return nil, fmt.Errorf("failed to create pipeline: %w", err)
	}

	kv, err := NewKeyVault(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create key vault: %w", err)
	}

	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AZURE,
			Bucket:       blob,
			CodeBuild:    builder,
			Pipeline:     pipeline,
			CloudPrefix:  a.cloudPrefix,
			BucketName:   storageAccountName,
			SSM:          kv,
			Region:       a.location,
			Account:      a.resourceGroup,
		},
	}
	return a.resources, nil
}

func (a *azureService) DeleteResources(deleteBucket bool, deleteServiceAccount bool) error {
	scheduler, err := NewScheduler(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create scheduler: %s", err)))
	} else {
		err = scheduler.deleteUpdateSchedule()
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete update schedule: %s", err)))
		}
	}

	agentPrefix := model.GetAgentPrefix(a.cloudPrefix)
	agentJobName := model.GetAgentProjectName(agentPrefix, common.RunCommand)
	err = a.resources.GetBuilder().DeleteProject(agentJobName, model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent run job: %s", err)))
	}

	agentJobName = model.GetAgentProjectName(agentPrefix, common.UpdateCommand)
	err = a.resources.GetBuilder().DeleteProject(agentJobName, model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent update job: %s", err)))
	}

	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create IAM: %s", err)))
	} else {
		identityName := a.getManagedIdentityName()
		err = iam.DeleteManagedIdentity(identityName)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete managed identity %s: %s", identityName, err)))
		}
	}

	if deleteServiceAccount {
		a.DeleteServiceAccount()
	}

	if !deleteBucket {
		log.Printf("Terraform state storage account %s will not be deleted, delete it manually if needed\n",
			a.resources.GetBucketName())
		return nil
	}
	err = a.resources.GetBucket().Delete()
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete storage account: %s", err)))
	}
	return nil
}

func (a *azureService) CreateServiceAccount() error {
	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup)
	if err != nil {
		return fmt.Errorf("failed to create IAM: %w", err)
	}

	saName := fmt.Sprintf("%s-sa-%s", a.cloudPrefix, a.location)
	if len(saName) > 24 {
		saName = saName[:24]
	}
	saName = strings.TrimSuffix(saName, "-")

	identity, created, err := iam.GetOrCreateManagedIdentity(saName)
	if err != nil {
		return fmt.Errorf("failed to create service account: %w", err)
	}

	if identity.Properties != nil && identity.Properties.PrincipalID != nil {
		err = iam.AssignRole(*identity.Properties.PrincipalID, "Contributor")
		if err != nil {
			return fmt.Errorf("failed to assign role to service account: %w", err)
		}
	}

	if !created {
		log.Printf("Service account %s already exists\n", saName)
		return nil
	}

	log.Printf("Created service account: %s\n", saName)
	return nil
}

func (a *azureService) DeleteServiceAccount() {
	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create IAM: %s", err)))
		return
	}

	saName := fmt.Sprintf("%s-sa-%s", a.cloudPrefix, a.location)
	if len(saName) > 24 {
		saName = saName[:24]
	}
	saName = strings.TrimSuffix(saName, "-")

	err = iam.DeleteManagedIdentity(saName)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete service account %s: %s", saName, err)))
	}
}

func (a *azureService) AddEncryption(_ string, _ map[string]model.TFOutput) error {
	slog.Warn(common.PrefixWarning("Encryption is not yet supported for Azure"))
	return nil
}

func (a *azureService) IsRunningLocally() bool {
	// Check for Azure Container Apps Job environment
	if os.Getenv("AZURE_CONTAINER_APP_JOB") != "" {
		return false
	}
	// Check for Azure Pipelines environment
	if os.Getenv("BUILD_BUILDID") != "" {
		return false
	}
	return true
}

func (a *azureService) getStorageAccountName() string {
	return getStorageAccountName(a.cloudPrefix, a.subscriptionId)
}

func getStorageAccountName(cloudPrefix, subscriptionId string) string {
	name := strings.ReplaceAll(cloudPrefix, "-", "") + strings.ReplaceAll(subscriptionId, "-", "")
	if len(name) > 24 {
		name = name[:24]
	}
	return strings.ToLower(name)
}

func (a *azureService) createBlobStorage(storageAccountName string) (*BlobStorage, error) {
	blob, err := NewBlobStorage(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, storageAccountName)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob storage client: %w", err)
	}
	err = blob.CreateStorageAccount(a.skipDelay)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage account: %w", err)
	}
	err = blob.CreateContainer("tfstate")
	if err != nil {
		return nil, fmt.Errorf("failed to create tfstate container: %w", err)
	}
	return blob, nil
}

func (a *azureService) getManagedIdentityName() string {
	name := fmt.Sprintf("%s-agent-%s", a.cloudPrefix, a.location)
	if len(name) > 24 {
		name = name[:24]
	}
	return strings.TrimSuffix(name, "-")
}

func (a *azureService) createManagedIdentity(iam *IAM) (string, error) {
	identityName := a.getManagedIdentityName()
	identity, created, err := iam.GetOrCreateManagedIdentity(identityName)
	if err != nil {
		return "", fmt.Errorf("failed to create managed identity: %w", err)
	}
	if identity.Properties != nil && identity.Properties.PrincipalID != nil {
		err = iam.AssignRole(*identity.Properties.PrincipalID, "Contributor")
		if err != nil {
			return "", fmt.Errorf("failed to assign Contributor role: %w", err)
		}
	}
	if created {
		log.Println("Waiting 60 seconds for managed identity permissions to be applied...")
		time.Sleep(60 * time.Second)
	}
	return identityName, nil
}

func (a *azureService) createSchedule(schedule model.Schedule, identity string) error {
	scheduler, err := NewScheduler(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return fmt.Errorf("failed to create scheduler: %w", err)
	}
	updateSchedule, err := scheduler.getUpdateSchedule()
	if err != nil {
		if schedule.UpdateCron == "" {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to get schedule: %s", err)))
			return nil
		}
		return err
	}
	if schedule.UpdateCron == "" {
		if updateSchedule != nil {
			return scheduler.deleteUpdateSchedule()
		}
		return nil
	}
	agentJob := model.GetAgentProjectName(model.GetAgentPrefix(a.cloudPrefix), common.UpdateCommand)
	if updateSchedule == nil {
		return scheduler.createUpdateSchedule(schedule.UpdateCron, agentJob, identity)
	}
	return scheduler.updateUpdateSchedule(schedule.UpdateCron, agentJob, identity)
}
