package azure

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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

func (a *azureService) newIAM() (*IAM, error) {
	return NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location)
}

func (a *azureService) ensureResourceGroup() (*IAM, error) {
	iam, err := a.newIAM()
	if err != nil {
		return nil, err
	}
	if err = iam.EnsureResourceGroup(); err != nil {
		return nil, err
	}
	return iam, nil
}

func (a *azureService) newKMS(tenantId string) (*KMS, error) {
	return NewKMS(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, tenantId,
		vaultName(a.cloudPrefix, a.subscriptionId, a.location), agentKeyName(a.cloudPrefix))
}

func (a *azureService) newStorage() (*Storage, error) {
	return NewStorage(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location,
		storageAccountName(a.cloudPrefix, a.subscriptionId, a.location))
}

func (a *azureService) newEnvironment() (*Environment, error) {
	return NewEnvironment(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
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
		ProviderDomain: storage.Domain(),
		TenantId:       tenantId,
		ResourceGroup:  a.resourceGroup,
	}
}

func (a *azureService) SetupMinimalResources() (model.Resources, error) {
	iam, err := a.ensureResourceGroup()
	if err != nil {
		return nil, err
	}
	store, err := a.setupStore(iam)
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
	iam, err := a.ensureResourceGroup()
	if err != nil {
		return nil, err
	}
	var store *stateStore
	// Creating the environment takes over 10 minutes, so a failed store setup cancels the
	// wait. Azure still finishes the creation, which the next Ensure waits for.
	group, groupCtx := errgroup.WithContext(a.ctx)
	group.Go(func() error {
		var err error
		if store, err = a.setupStore(iam); err != nil {
			return err
		}
		return a.assignJobRole(iam, store.identity)
	})
	group.Go(func() error {
		environment, err := NewEnvironment(groupCtx, a.credential, a.subscriptionId, a.resourceGroup, a.location, a.cloudPrefix)
		if err != nil {
			return err
		}
		return environment.Ensure()
	})
	if err = group.Wait(); err != nil {
		return nil, err
	}
	environment, err := a.newEnvironment()
	if err != nil {
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
	if err = builder.reconcileSchedule(); err != nil {
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
func (a *azureService) assignJobRole(iam *IAM, jobIdentity identity) error {
	err := iam.AssignRole(iam.subscriptionScope(), jobIdentity.principal(), roleOwner)
	if !isAuthorizationFailed(err) {
		return err
	}
	slog.Warn(common.PrefixWarning(fmt.Sprintf("Not allowed to assign Owner to managed identity %s, assigning Contributor instead. Modules that create role assignments will fail until an administrator assigns Owner to it",
		identityName(a.cloudPrefix))))
	return iam.AssignRole(iam.subscriptionScope(), jobIdentity.principal(), roleContributor)
}

type stateStore struct {
	resources Resources
	ssm       *SSM
	identity  identity
}

// setupStore creates the trust root: the agent's vault and key, the job identity and the
// storage account. The storage account reaches the key through its own identity, which
// can only use keys, so changes to the job identity can't break the encryption. The vault
// and the identities don't depend on each other, so they're created concurrently. The job
// identity's Owner role is only for jobs, so SetupResources assigns it.
func (a *azureService) setupStore(iam *IAM) (*stateStore, error) {
	executor, err := currentPrincipal(a.ctx, a.credential)
	if err != nil {
		return nil, err
	}
	kms, err := a.newKMS(executor.TenantId)
	if err != nil {
		return nil, err
	}
	var jobIdentity, storageIdentity identity
	var group errgroup.Group
	group.Go(func() error {
		if err := kms.EnsureVault(a.skipDelay); err != nil {
			return err
		}
		if err := iam.AssignRole(kms.VaultId(), executor, roleKeyVaultAdministrator); err != nil {
			return err
		}
		return kms.EnsureKey()
	})
	group.Go(func() error {
		var err error
		jobIdentity, err = iam.EnsureIdentity(identityName(a.cloudPrefix))
		return err
	})
	group.Go(func() error {
		var err error
		storageIdentity, err = iam.EnsureIdentity(storageIdentityName(a.cloudPrefix))
		return err
	})
	if err = group.Wait(); err != nil {
		return nil, err
	}
	if err = iam.AssignRole(kms.VaultId(), storageIdentity.principal(), roleKeyVaultCryptoEncryption); err != nil {
		return nil, err
	}
	if err = iam.AssignRole(kms.VaultId(), jobIdentity.principal(), roleKeyVaultAdministrator); err != nil {
		return nil, err
	}
	storage, err := a.newStorage()
	if err != nil {
		return nil, err
	}
	if err = storage.CreateAccount(kms, storageIdentity.Id); err != nil {
		return nil, err
	}
	if err = iam.AssignRole(storage.AccountId(), executor, roleStorageBlobDataContributor); err != nil {
		return nil, err
	}
	if err = iam.AssignRole(storage.AccountId(), jobIdentity.principal(), roleStorageBlobDataContributor); err != nil {
		return nil, err
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
	return &stateStore{resources: resources, ssm: ssm, identity: jobIdentity}, nil
}

func (a *azureService) GetResources() (model.Resources, error) {
	executor, err := currentPrincipal(a.ctx, a.credential)
	if err != nil {
		return nil, err
	}
	storage, err := a.newStorage()
	if err != nil {
		return nil, err
	}
	if _, err = storage.Resolve(); err != nil {
		return nil, err
	}
	resources := a.baseResources(executor.TenantId, storage)
	kms, err := a.newKMS(executor.TenantId)
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
	iam, err := a.newIAM()
	if err != nil {
		return nil, err
	}
	jobIdentity, err := iam.GetIdentity(identityName(a.cloudPrefix))
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	environment, err := a.newEnvironment()
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

func (a *azureService) DeleteResources(deleteBucket, deleteServiceAccount bool) error {
	if builder, ok := a.resources.GetBuilder().(*Builder); ok {
		agentPrefix := model.GetAgentPrefix(a.cloudPrefix)
		for _, cmd := range []common.Command{common.RunCommand, common.UpdateCommand} {
			name := jobName(model.GetAgentProjectName(agentPrefix, cmd), "")
			if err := builder.deleteJob(name); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent job %s: %s", name, err)))
			}
		}
	}
	// Only an empty resource group is deleted, so the environment must be gone first
	deletesGroup := deleteBucket && a.ownsGroup
	environment, err := a.newEnvironment()
	if err == nil {
		err = environment.Delete(deletesGroup)
	}
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete container apps environment: %s", err)))
	}
	iam, err := a.newIAM()
	if err != nil {
		return err
	}
	if deleteServiceAccount {
		a.deleteServiceAccount(iam)
	}
	if !deleteBucket {
		log.Printf("Storage account %s, key vault and managed identities %s and %s will not be deleted, delete them manually if needed\n",
			a.resources.GetBucketName(), identityName(a.cloudPrefix), storageIdentityName(a.cloudPrefix))
		return nil
	}
	if err = a.resources.GetBucket().Delete(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete storage account %s: %s", a.resources.GetBucketName(), err)))
		slog.Warn(common.PrefixWarning("Key vault and managed identities are kept because the storage account is encrypted with them"))
		return nil
	}
	a.deleteVaultAndIdentities(iam)
	if !deletesGroup {
		return nil
	}
	if err = iam.DeleteResourceGroupIfEmpty(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete resource group %s: %s", a.resourceGroup, err)))
	}
	return nil
}

// deleteVaultAndIdentities deletes the identities with the vault, since their roles on it would be left behind.
func (a *azureService) deleteVaultAndIdentities(iam *IAM) {
	vaultId := ""
	kms, err := a.newKMS(a.resources.TenantId)
	if err == nil {
		_, err = kms.Resolve()
		vaultId = kms.VaultId()
	}
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to get key vault: %s", err)))
	}
	deleteIdentity(iam, identityName(a.cloudPrefix), map[string]string{
		roleOwner:                 iam.subscriptionScope(),
		roleContributor:           iam.subscriptionScope(),
		roleKeyVaultAdministrator: vaultId,
	})
	deleteIdentity(iam, storageIdentityName(a.cloudPrefix), map[string]string{roleKeyVaultCryptoEncryption: vaultId})
	if vaultId == "" {
		return
	}
	if err = kms.Delete(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete key vault: %s", err)))
	}
}

// deleteIdentity removes the identity's role assignments first, since assignments of a
// deleted principal are left behind. roles maps a role to its scope, an empty scope is skipped.
func deleteIdentity(iam *IAM, name string, roles map[string]string) {
	found, err := iam.GetIdentity(name)
	if err != nil {
		if !isNotFound(err) {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to get managed identity %s: %s", name, err)))
		}
		return
	}
	for role, scope := range roles {
		if scope == "" {
			continue
		}
		if err = iam.DeleteRoleAssignment(scope, found.PrincipalId, role); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete role %s assignment of %s: %s", role, name, err)))
		}
	}
	if err = iam.DeleteIdentity(name); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete managed identity %s: %s", name, err)))
		return
	}
	log.Printf("Deleted managed identity %s\n", name)
}

func (a *azureService) AddEncryption(_ string, _ map[string]model.TFOutput) error {
	slog.Warn(common.PrefixWarning("Module encryption keys are not used for Azure, the agent encrypts its storage with its own key"))
	return nil
}

func (a *azureService) IsRunningLocally() bool {
	return os.Getenv("CONTAINER_APP_JOB_NAME") == ""
}
