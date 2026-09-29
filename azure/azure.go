package azure

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"golang.org/x/sync/errgroup"
)

type azureService struct {
	ctx            context.Context
	cloudPrefix    string
	subscriptionId string
	location       string
	resourceGroup  string
	ownsGroup      bool
	credential     azcore.TokenCredential
	pipeline       common.Pipeline
	skipDelay      bool
	resources      Resources
}

type Resources struct {
	model.CloudResources
	TenantId      string
	VaultId       string
	ResourceGroup string
}

// GetBackendConfigVars is the azurerm backend config. Shared key access is disabled on
// the storage account, so the backend authenticates with Entra ID.
func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"storage_account_name": r.BucketName,
		"container_name":       containerName,
		"key":                  key,
		"use_azuread_auth":     "true",
	}
}

// GetBackendEnv is used by local executions, which authenticate with the Azure CLI login.
// The CLI login already has the tenant, and the CLI rejects a token request with both a
// subscription and a tenant, so ARM_TENANT_ID is left out.
func (r Resources) GetBackendEnv() map[string]string {
	return map[string]string{
		common.AzureSubscriptionIdEnv: r.Account,
		common.AzureResourceGroupEnv:  r.ResourceGroup,
		"ARM_SUBSCRIPTION_ID":         r.Account,
		"ARM_USE_AZUREAD":             "true",
	}
}

func (r Resources) GetVaultId() (string, error) {
	if r.VaultId == "" {
		return "", errors.New("key vault has not yet been initialized")
	}
	return r.VaultId, nil
}

func NewAzure(ctx context.Context, cloudPrefix string, azure common.Azure, pipeline common.Pipeline, skipBucketDelay bool) (model.CloudProvider, error) {
	credential, err := newCredential()
	if err != nil {
		return nil, err
	}
	return &azureService{
		ctx:            ctx,
		cloudPrefix:    cloudPrefix,
		subscriptionId: azure.SubscriptionId,
		location:       azure.Location,
		resourceGroup:  resourceGroup(azure, cloudPrefix),
		ownsGroup:      azure.ResourceGroup == "",
		credential:     credential,
		pipeline:       pipeline,
		skipDelay:      skipBucketDelay,
	}, nil
}

func (a *azureService) baseResources(tenantId string, storage *Storage) Resources {
	return Resources{
		ProviderType:   model.AZURE,
		Bucket:         storage,
		BucketName:     storage.account,
		CloudPrefix:    a.cloudPrefix,
		Region:         a.location,
		Account:        a.subscriptionId,
		OrganizationId: tenantId,
		TenantId:       tenantId,
		ResourceGroup:  a.resourceGroup,
	}
}

func (a *azureService) SetupMinimalResources() (model.Resources, error) {
	store, err := a.setupStore()
	if err != nil {
		return nil, err
	}
	a.resources = store.resources
	return a.resources, nil
}

func (a *azureService) SetupResources(manager model.NotificationManager, config model.Config) (model.Resources, error) {
	if a.pipeline.Type == string(common.PipelineTypeLocal) {
		return a.SetupMinimalResources()
	}
	if err := validateJobNames(a.cloudPrefix, config.Steps); err != nil {
		return nil, err
	}
	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location)
	if err != nil {
		return nil, err
	}
	if err = iam.EnsureResourceGroup(); err != nil {
		return nil, err
	}
	environment, err := NewEnvironment(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return nil, err
	}
	var store *store
	var group errgroup.Group
	group.Go(func() error {
		var err error
		if store, err = a.setupStore(); err != nil {
			return err
		}
		return a.assignJobRole(iam, store.identity.PrincipalId)
	})
	group.Go(environment.Ensure)
	if err = group.Wait(); err != nil {
		return nil, err
	}
	resources := store.resources
	builder, err := NewBuilder(a.ctx, a.credential, store.ssm, a.subscriptionId, resources.TenantId, a.resourceGroup,
		a.location, environment, store.identity, resources.BucketName, a.cloudPrefix,
		*a.pipeline.TerraformCache.Value, config.IsOpenTofuEnabled())
	if err != nil {
		return nil, err
	}
	builder.SetUpdateCron(config.Schedule.UpdateCron)
	builder.SetNotificationManager(manager)
	if err = a.reconcileSchedule(builder, config.Schedule, manager); err != nil {
		return nil, err
	}
	pipeline, err := NewPipeline(a.ctx, a.credential, a.subscriptionId, builder, resources.Bucket, manager)
	if err != nil {
		return nil, err
	}
	resources.CodeBuild = builder
	resources.Pipeline = pipeline
	a.resources = resources
	return a.resources, nil
}

// assignJobRole gives the job identity Owner, like the AWS build role's AdministratorAccess, since
// modules create role assignments. An operator whose Owner is conditioned against delegating
// privileged roles, the Azure recommended default, can only give Contributor.
func (a *azureService) assignJobRole(iam *IAM, principalId string) error {
	err := iam.AssignRole(iam.subscriptionScope(), principalId, "ServicePrincipal", roleOwner)
	if !isAuthorizationFailed(err) {
		return err
	}
	slog.Warn(common.PrefixWarning(fmt.Sprintf("Not allowed to assign Owner to managed identity %s, assigning Contributor instead. Modules that create role assignments will fail until an administrator assigns Owner to it",
		identityName(a.cloudPrefix))))
	return iam.AssignRole(iam.subscriptionScope(), principalId, "ServicePrincipal", roleContributor)
}

type store struct {
	resources Resources
	ssm       *SSM
	identity  identity
}

// setupStore creates the trust root: the agent's vault and key, the managed identity
// and the storage account encrypted with the key through that identity. The vault and
// the identity don't depend on each other, so they're created concurrently. The
// identity's Owner role is only for jobs, so SetupResources assigns it.
func (a *azureService) setupStore() (*store, error) {
	executor, err := currentPrincipal(a.ctx, a.credential)
	if err != nil {
		return nil, err
	}
	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location)
	if err != nil {
		return nil, err
	}
	if err = iam.EnsureResourceGroup(); err != nil {
		return nil, err
	}
	kms, err := NewKMS(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, executor.TenantId,
		vaultName(a.cloudPrefix, a.subscriptionId, a.location), agentKeyName(a.cloudPrefix))
	if err != nil {
		return nil, err
	}
	var jobIdentity identity
	var group errgroup.Group
	group.Go(func() error {
		if err := kms.EnsureVault(a.skipDelay); err != nil {
			return err
		}
		if err := iam.AssignRole(kms.VaultId(), executor.ObjectId, executor.Type, roleKeyVaultAdministrator); err != nil {
			return err
		}
		return kms.EnsureKey()
	})
	group.Go(func() error {
		var err error
		jobIdentity, err = iam.EnsureIdentity(identityName(a.cloudPrefix))
		return err
	})
	if err = group.Wait(); err != nil {
		return nil, err
	}
	if err = iam.AssignRole(kms.VaultId(), jobIdentity.PrincipalId, "ServicePrincipal", roleKeyVaultAdministrator); err != nil {
		return nil, err
	}
	storage, err := NewStorage(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location,
		storageAccountName(a.cloudPrefix, a.subscriptionId, a.location))
	if err != nil {
		return nil, err
	}
	if err = storage.CreateAccount(kms, jobIdentity.Id); err != nil {
		return nil, err
	}
	for _, principalId := range []string{executor.ObjectId, jobIdentity.PrincipalId} {
		principalType := "ServicePrincipal"
		if principalId == executor.ObjectId {
			principalType = executor.Type
		}
		if err = iam.AssignRole(storage.AccountId(), principalId, principalType, roleStorageBlobDataContributor); err != nil {
			return nil, err
		}
	}
	if err = storage.EnsureContainer(); err != nil {
		return nil, err
	}
	ssm, err := NewSSM(a.ctx, a.credential, kms.VaultURI())
	if err != nil {
		return nil, err
	}
	resources := a.baseResources(executor.TenantId, storage)
	resources.SSM = ssm
	resources.VaultId = kms.VaultId()
	return &store{resources: resources, ssm: ssm, identity: jobIdentity}, nil
}

func (a *azureService) GetResources() (model.Resources, error) {
	executor, err := currentPrincipal(a.ctx, a.credential)
	if err != nil {
		return nil, err
	}
	storage, err := NewStorage(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location,
		storageAccountName(a.cloudPrefix, a.subscriptionId, a.location))
	if err != nil {
		return nil, err
	}
	resources := a.baseResources(executor.TenantId, storage)
	kms, err := NewKMS(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, executor.TenantId,
		vaultName(a.cloudPrefix, a.subscriptionId, a.location), agentKeyName(a.cloudPrefix))
	if err != nil {
		return nil, err
	}
	found, err := kms.Resolve()
	if err != nil {
		return nil, err
	}
	ssm := newMissingSSM(a.ctx)
	if found {
		ssm, err = NewSSM(a.ctx, a.credential, kms.VaultURI())
		if err != nil {
			return nil, err
		}
		resources.VaultId = kms.VaultId()
	} else {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Key vault %s not found", vaultName(a.cloudPrefix, a.subscriptionId, a.location))))
	}
	resources.SSM = ssm
	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location)
	if err != nil {
		return nil, err
	}
	jobIdentity, err := iam.GetIdentity(identityName(a.cloudPrefix))
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	environment, err := NewEnvironment(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err != nil {
		return nil, err
	}
	builder, err := NewBuilder(a.ctx, a.credential, ssm, a.subscriptionId, executor.TenantId, a.resourceGroup,
		a.location, environment, jobIdentity, resources.BucketName, a.cloudPrefix, true, false)
	if err != nil {
		return nil, err
	}
	pipeline, err := NewPipeline(a.ctx, a.credential, a.subscriptionId, builder, storage, nil)
	if err != nil {
		return nil, err
	}
	resources.CodeBuild = builder
	resources.Pipeline = pipeline
	a.resources = resources
	return a.resources, nil
}

func (a *azureService) PrepareDestroy(resources model.Resources) (model.Resources, error) {
	return resources, nil
}

// reconcileSchedule moves the update job to the configured cron. The job is only
// created by bootstrap, so a missing job is left for it, and CreateAgentProject notifies.
func (a *azureService) reconcileSchedule(builder *Builder, schedule model.Schedule, manager model.NotificationManager) error {
	name := jobName(model.GetAgentProjectName(model.GetAgentPrefix(a.cloudPrefix), common.UpdateCommand), "")
	job, err := builder.getJob(name)
	if err != nil {
		return err
	}
	if job == nil {
		if schedule.UpdateCron == "" {
			manager.ScheduleUnchanged(common.UpdateCommand, model.ScheduleRemoved, "")
		}
		return nil
	}
	current := ""
	configuration := job.Properties.Configuration
	if configuration != nil && configuration.ScheduleTriggerConfig != nil && configuration.ScheduleTriggerConfig.CronExpression != nil &&
		configuration.TriggerType != nil && *configuration.TriggerType == armappcontainers.TriggerTypeSchedule {
		current = *configuration.ScheduleTriggerConfig.CronExpression
	}
	if current == schedule.UpdateCron {
		action := model.ScheduleAdded
		if current == "" {
			action = model.ScheduleRemoved
		}
		manager.ScheduleUnchanged(common.UpdateCommand, action, schedule.UpdateCron)
		return nil
	}
	var cron *string
	if schedule.UpdateCron != "" {
		cron = &schedule.UpdateCron
	}
	if err = builder.ensureJob(name, *job.Properties.EnvironmentID, job.Properties.Template.Containers[0], nil, cron); err != nil {
		return err
	}
	switch {
	case schedule.UpdateCron == "":
		manager.Schedule(common.UpdateCommand, model.ScheduleRemoved, schedule.UpdateCron)
	case current == "":
		manager.Schedule(common.UpdateCommand, model.ScheduleAdded, schedule.UpdateCron)
	default:
		manager.Schedule(common.UpdateCommand, model.ScheduleModified, schedule.UpdateCron)
	}
	return nil
}

func (a *azureService) DeleteResources(deleteBucket, deleteServiceAccount bool) error {
	builder, ok := a.resources.GetBuilder().(*Builder)
	if ok {
		agentPrefix := model.GetAgentPrefix(a.cloudPrefix)
		for _, cmd := range []common.Command{common.RunCommand, common.UpdateCommand} {
			name := jobName(model.GetAgentProjectName(agentPrefix, cmd), "")
			if err := builder.deleteJob(name); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent job %s: %s", name, err)))
			}
		}
	}
	environment, err := NewEnvironment(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
	if err == nil {
		err = environment.Delete(deleteBucket && a.ownsGroup)
	}
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete container apps environment: %s", err)))
	}
	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location)
	if err != nil {
		return err
	}
	if deleteServiceAccount {
		a.deleteServiceAccount(iam)
	}
	if !deleteBucket {
		log.Printf("Storage account %s, key vault and managed identity %s will not be deleted, delete them manually if needed\n",
			a.resources.GetBucketName(), identityName(a.cloudPrefix))
		return nil
	}
	if err = a.resources.GetBucket().Delete(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete storage account %s: %s", a.resources.GetBucketName(), err)))
		slog.Warn(common.PrefixWarning("Key vault and managed identity are kept because the storage account is encrypted with them"))
		return nil
	}
	jobIdentity, err := iam.GetIdentity(identityName(a.cloudPrefix))
	if err == nil {
		for _, role := range []string{roleOwner, roleContributor} {
			if err = iam.DeleteRoleAssignment(iam.subscriptionScope(), jobIdentity.PrincipalId, role); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete role %s assignment of %s: %s", role, identityName(a.cloudPrefix), err)))
			}
		}
	}
	if err = iam.DeleteIdentity(identityName(a.cloudPrefix)); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete managed identity %s: %s", identityName(a.cloudPrefix), err)))
	}
	kms, err := NewKMS(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.resources.TenantId,
		vaultName(a.cloudPrefix, a.subscriptionId, a.location), agentKeyName(a.cloudPrefix))
	if err == nil {
		err = kms.Delete()
	}
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete key vault: %s", err)))
	}
	if !a.ownsGroup {
		return nil
	}
	if err = iam.DeleteResourceGroupIfEmpty(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete resource group %s: %s", a.resourceGroup, err)))
	}
	return nil
}

func (a *azureService) AddEncryption(_ string, _ map[string]model.TFOutput) error {
	slog.Warn(common.PrefixWarning("Module encryption keys are not used for Azure, the agent encrypts its storage with its own key"))
	return nil
}

func (a *azureService) IsRunningLocally() bool {
	return os.Getenv("CONTAINER_APP_JOB_NAME") == ""
}
