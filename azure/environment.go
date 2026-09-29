package azure

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/operationalinsights/armoperationalinsights/v2"
)

const (
	logRetentionDays       = 30
	deletionTimeout        = 30 * time.Minute
	scheduledForDeleteCode = "ManagedEnvironmentScheduledForDelete"
)

// Environment is the Container Apps environment the step jobs run in. Its console
// logs go to a Log Analytics workspace so plan output is reviewable in the Portal.
// An environment is bound to a VNet subnet at creation, so steps attached to a VNet
// get their own environment per subnet, created on first use.
type Environment struct {
	ctx           context.Context
	environments  *armappcontainers.ManagedEnvironmentsClient
	workspaces    *armoperationalinsights.WorkspacesClient
	sharedKeys    *armoperationalinsights.SharedKeysClient
	resourceGroup string
	location      string
	name          string
	workspace     string
	id            string
	subscription  string
	mu            sync.Mutex
	subnets       map[string]string
}

func NewEnvironment(ctx context.Context, credential azcore.TokenCredential, subscriptionId, resourceGroup, location, prefix string) (*Environment, error) {
	environments, err := armappcontainers.NewManagedEnvironmentsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	workspaces, err := armoperationalinsights.NewWorkspacesClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	sharedKeys, err := armoperationalinsights.NewSharedKeysClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	return &Environment{
		ctx:           ctx,
		environments:  environments,
		workspaces:    workspaces,
		sharedKeys:    sharedKeys,
		resourceGroup: resourceGroup,
		location:      location,
		name:          environmentName(prefix),
		workspace:     workspaceName(prefix),
		subscription:  subscriptionId,
		subnets:       map[string]string{},
		id: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/managedEnvironments/%s",
			subscriptionId, resourceGroup, environmentName(prefix)),
	}, nil
}

func (e *Environment) Id() string { return e.id }

func (e *Environment) Ensure() error {
	return e.ensure(e.name, nil)
}

// ForSubnet returns the environment for a subnet, which must be delegated to
// Microsoft.App/environments. An empty subnet means the default environment.
func (e *Environment) ForSubnet(subnetId string) (string, error) {
	if subnetId == "" {
		return e.id, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if id, ok := e.subnets[subnetId]; ok {
		return id, nil
	}
	name := subnetEnvironmentName(e.name, subnetId)
	if err := e.ensure(name, &armappcontainers.VnetConfiguration{
		InfrastructureSubnetID: &subnetId,
		Internal:               new(true),
	}); err != nil {
		return "", err
	}
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/managedEnvironments/%s",
		e.subscription, e.resourceGroup, name)
	e.subnets[subnetId] = id
	return id, nil
}

func subnetEnvironmentName(base, subnetId string) string {
	return truncate(base, 60-nameHashLen-1) + "-" + nameHash(strings.ToLower(subnetId))
}

func (e *Environment) ensure(name string, vnet *armappcontainers.VnetConfiguration) error {
	existing, err := e.environments.Get(e.ctx, e.resourceGroup, name, nil)
	if err == nil {
		properties := existing.Properties
		if properties == nil || properties.ProvisioningState == nil ||
			*properties.ProvisioningState != armappcontainers.EnvironmentProvisioningStateScheduledForDelete {
			return nil
		}
		log.Printf("Waiting for container apps environment %s to be deleted before creating it again, this can take up to 20 minutes\n", name)
		if err = e.waitForDeletion(name); err != nil {
			return err
		}
	} else if !isNotFound(err) {
		return fmt.Errorf("failed to get container apps environment %s: %w", name, err)
	}
	customerId, sharedKey, err := e.ensureWorkspace()
	if err != nil {
		return err
	}
	if vnet != nil {
		log.Printf("Creating container apps environment %s in subnet %s, this can take several minutes\n", name, *vnet.InfrastructureSubnetID)
	}
	poller, err := e.environments.BeginCreateOrUpdate(e.ctx, e.resourceGroup, name, armappcontainers.ManagedEnvironment{
		Location: &e.location,
		Tags:     resourceTags(),
		Properties: &armappcontainers.ManagedEnvironmentProperties{
			VnetConfiguration: vnet,
			AppLogsConfiguration: &armappcontainers.AppLogsConfiguration{
				Destination: new("log-analytics"),
				LogAnalyticsConfiguration: &armappcontainers.LogAnalyticsConfiguration{
					CustomerID: &customerId,
					SharedKey:  &sharedKey,
				},
			},
			WorkloadProfiles: []*armappcontainers.WorkloadProfile{{
				Name:                new("Consumption"),
				WorkloadProfileType: new("Consumption"),
			}},
		},
	}, nil)
	if err == nil {
		_, err = poller.PollUntilDone(e.ctx, nil)
	}
	if err != nil {
		return fmt.Errorf("failed to create container apps environment %s: %w", name, err)
	}
	log.Printf("Created container apps environment %s\n", name)
	return nil
}

func (e *Environment) ensureWorkspace() (string, string, error) {
	var customerId *string
	existing, err := e.workspaces.Get(e.ctx, e.resourceGroup, e.workspace, nil)
	if err == nil {
		customerId = existing.Properties.CustomerID
	} else if isNotFound(err) {
		poller, err := e.workspaces.BeginCreateOrUpdate(e.ctx, e.resourceGroup, e.workspace, armoperationalinsights.Workspace{
			Location: &e.location,
			Tags:     resourceTags(),
			Properties: &armoperationalinsights.WorkspaceProperties{
				SKU:             &armoperationalinsights.WorkspaceSKU{Name: new(armoperationalinsights.WorkspaceSKUNameEnumPerGB2018)},
				RetentionInDays: new(int32(logRetentionDays)),
			},
		}, nil)
		if err != nil {
			return "", "", fmt.Errorf("failed to create log analytics workspace %s: %w", e.workspace, err)
		}
		created, err := poller.PollUntilDone(e.ctx, nil)
		if err != nil {
			return "", "", fmt.Errorf("failed to create log analytics workspace %s: %w", e.workspace, err)
		}
		log.Printf("Created log analytics workspace %s\n", e.workspace)
		customerId = created.Properties.CustomerID
	} else {
		return "", "", fmt.Errorf("failed to get log analytics workspace %s: %w", e.workspace, err)
	}
	keys, err := e.sharedKeys.GetSharedKeys(e.ctx, e.resourceGroup, e.workspace, nil)
	if err != nil {
		return "", "", fmt.Errorf("failed to get log analytics workspace %s keys: %w", e.workspace, err)
	}
	if customerId == nil || keys.PrimarySharedKey == nil {
		return "", "", fmt.Errorf("log analytics workspace %s has no customer id or shared key", e.workspace)
	}
	return *customerId, *keys.PrimarySharedKey, nil
}

// waitForDeletion waits for an earlier delete to finish, since Azure rejects creating an
// environment while one with the same name is being deleted.
func (e *Environment) waitForDeletion(name string) error {
	deadline := time.Now().Add(deletionTimeout)
	for {
		if err := sleep(e.ctx, pollInterval); err != nil {
			return err
		}
		_, err := e.environments.Get(e.ctx, e.resourceGroup, name, nil)
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to get container apps environment %s: %w", name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container apps environment %s is still being deleted after %s", name, deletionTimeout)
		}
	}
}

// Delete removes the default environment and every subnet environment, then the workspace.
// Azure takes up to 20 minutes to delete an environment, so without wait the deletion
// finishes in the background and the next ensure waits for it.
func (e *Environment) Delete(wait bool) error {
	names := []string{e.name}
	pager := e.environments.NewListByResourceGroupPager(e.resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(e.ctx)
		if err != nil {
			if isNotFound(err) {
				break
			}
			return fmt.Errorf("failed to list container apps environments: %w", err)
		}
		for _, environment := range page.Value {
			if environment.Name != nil && *environment.Name != e.name && strings.HasPrefix(*environment.Name, truncate(e.name, 60-nameHashLen-1)+"-") {
				names = append(names, *environment.Name)
			}
		}
	}
	for _, name := range names {
		poller, err := e.environments.BeginDelete(e.ctx, e.resourceGroup, name, nil)
		scheduled := errorCode(err) == scheduledForDeleteCode
		switch {
		case isNotFound(err):
			continue
		case err != nil && !scheduled:
			return fmt.Errorf("failed to delete container apps environment %s: %w", name, err)
		case !wait:
			log.Printf("Container apps environment %s is being deleted in the background, this can take up to 20 minutes\n", name)
			continue
		}
		log.Printf("Deleting container apps environment %s, this can take up to 20 minutes\n", name)
		if scheduled {
			err = e.waitForDeletion(name)
		} else {
			_, err = poller.PollUntilDone(e.ctx, nil)
		}
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("failed to delete container apps environment %s: %w", name, err)
		}
		log.Printf("Deleted container apps environment %s\n", name)
	}
	workspacePoller, err := e.workspaces.BeginDelete(e.ctx, e.resourceGroup, e.workspace, &armoperationalinsights.WorkspacesClientBeginDeleteOptions{Force: new(true)})
	if err == nil {
		_, err = workspacePoller.PollUntilDone(e.ctx, nil)
	}
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("failed to delete log analytics workspace %s: %w", e.workspace, err)
	}
	return nil
}
