package azure

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/operationalinsights/armoperationalinsights/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"golang.org/x/sync/errgroup"
)

const (
	logRetentionDays        = 30
	azureMonitorDestination = "azure-monitor"
	consumptionProfile      = "Consumption"
	diagnosticSettingName   = "infralib"
	diagnosticAPIVersion    = "2021-05-01-preview"
	deletionTimeout         = 30 * time.Minute
	readyTimeout            = 30 * time.Minute
	scheduledForDeleteCode  = "ManagedEnvironmentScheduledForDelete"
)

// Environment is the Container Apps environment the step jobs run in. Its console
// logs go to a Log Analytics workspace through a diagnostic setting, so plan output is
// reviewable in the Portal. The legacy log-analytics destination isn't used: it ingests
// through the HTTP Data Collector API, which is no longer supported.
// A VNet-attached step runs in the environment that already uses its subnet. The agent
// only waits for such an environment to be ready, it never creates or changes it.
type Environment struct {
	ctx           context.Context
	environments  *armappcontainers.ManagedEnvironmentsClient
	workspaces    *armoperationalinsights.WorkspacesClient
	resources     *armresources.Client
	resourceGroup string
	location      string
	name          string
	workspace     string
	id            string
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
	resources, err := armresources.NewClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	return &Environment{
		ctx:           ctx,
		environments:  environments,
		workspaces:    workspaces,
		resources:     resources,
		resourceGroup: resourceGroup,
		location:      location,
		name:          environmentName(prefix),
		workspace:     workspaceName(prefix),
		subnets:       map[string]string{},
		id: fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/managedEnvironments/%s",
			subscriptionId, resourceGroup, environmentName(prefix)),
	}, nil
}

func (e *Environment) Id() string { return e.id }

// ForSubnet returns the environment whose infrastructure subnet is subnetId. An empty
// subnet means the agent's own environment.
func (e *Environment) ForSubnet(subnetId string) (string, error) {
	if subnetId == "" {
		return e.id, nil
	}
	key := strings.ToLower(subnetId)
	e.mu.Lock()
	defer e.mu.Unlock()
	if id, ok := e.subnets[key]; ok {
		return id, nil
	}
	pager := e.environments.NewListBySubscriptionPager(nil)
	for pager.More() {
		page, err := pager.NextPage(e.ctx)
		if err != nil {
			return "", fmt.Errorf("failed to list container apps environments: %w", err)
		}
		for _, environment := range page.Value {
			if environment.ID == nil || !strings.EqualFold(infrastructureSubnet(*environment), subnetId) {
				continue
			}
			if err = e.waitForReady(*environment); err != nil {
				return "", err
			}
			e.subnets[key] = *environment.ID
			return *environment.ID, nil
		}
	}
	return "", fmt.Errorf("no container apps environment uses subnet %s, create one in it for VNet-attached steps", subnetId)
}

func infrastructureSubnet(environment armappcontainers.ManagedEnvironment) string {
	properties := environment.Properties
	if properties == nil || properties.VnetConfiguration == nil || properties.VnetConfiguration.InfrastructureSubnetID == nil {
		return ""
	}
	return *properties.VnetConfiguration.InfrastructureSubnetID
}

func provisioningState(environment armappcontainers.ManagedEnvironment) armappcontainers.EnvironmentProvisioningState {
	if environment.Properties == nil || environment.Properties.ProvisioningState == nil {
		return ""
	}
	return *environment.Properties.ProvisioningState
}

func logDestination(environment armappcontainers.ManagedEnvironment) string {
	properties := environment.Properties
	if properties == nil || properties.AppLogsConfiguration == nil || properties.AppLogsConfiguration.Destination == nil {
		return ""
	}
	return *properties.AppLogsConfiguration.Destination
}

// waitForReady waits while an environment is being created or upgraded.
func (e *Environment) waitForReady(environment armappcontainers.ManagedEnvironment) error {
	resourceId, err := arm.ParseResourceID(*environment.ID)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(readyTimeout)
	logged := false
	for {
		state := provisioningState(environment)
		switch state {
		case armappcontainers.EnvironmentProvisioningStateFailed, armappcontainers.EnvironmentProvisioningStateCanceled,
			armappcontainers.EnvironmentProvisioningStateScheduledForDelete:
			return fmt.Errorf("container apps environment %s is %s", resourceId.Name, state)
		case armappcontainers.EnvironmentProvisioningStateInfrastructureSetupComplete, armappcontainers.EnvironmentProvisioningStateInfrastructureSetupInProgress,
			armappcontainers.EnvironmentProvisioningStateInitializationInProgress, armappcontainers.EnvironmentProvisioningStateUpgradeRequested,
			armappcontainers.EnvironmentProvisioningStateWaiting:
			// Not ready yet
		default:
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container apps environment %s is still %s after %s", resourceId.Name, state, readyTimeout)
		}
		if !logged {
			log.Printf("Waiting for container apps environment %s to be ready, it's %s\n", resourceId.Name, state)
			logged = true
		}
		if err = util.Sleep(e.ctx, pollInterval); err != nil {
			return err
		}
		response, err := e.environments.Get(e.ctx, resourceId.ResourceGroupName, resourceId.Name, nil)
		if err != nil {
			return fmt.Errorf("failed to get container apps environment %s: %w", resourceId.Name, err)
		}
		environment = response.ManagedEnvironment
	}
}

// Ensure creates the agent's own environment, which runs the steps that aren't attached to a VNet.
// An interrupted run can leave the environment still being created and without its diagnostic
// setting, so an existing environment is waited for and a missing setting is added.
func (e *Environment) Ensure() error {
	existing, err := e.environments.Get(e.ctx, e.resourceGroup, e.name, nil)
	if isNotFound(err) {
		return e.create()
	}
	if err != nil {
		return fmt.Errorf("failed to get container apps environment %s: %w", e.name, err)
	}
	if provisioningState(existing.ManagedEnvironment) == armappcontainers.EnvironmentProvisioningStateScheduledForDelete {
		log.Printf("Waiting for container apps environment %s to be deleted before creating it again, this can take up to 20 minutes\n", e.name)
		if err = e.waitForDeletion(); err != nil {
			return err
		}
		return e.create()
	}
	if err = e.waitForReady(existing.ManagedEnvironment); err != nil {
		return err
	}
	if logDestination(existing.ManagedEnvironment) != azureMonitorDestination {
		return nil
	}
	return e.addMissingDiagnosticSetting()
}

// create makes the workspace alongside the environment, since only the diagnostic setting needs it.
func (e *Environment) create() error {
	log.Printf("Creating container apps environment %s with log analytics workspace %s, this can take up to 15 minutes\n",
		e.name, e.workspace)
	var workspaceId string
	group, groupCtx := errgroup.WithContext(e.ctx)
	group.Go(func() error {
		var err error
		workspaceId, err = e.ensureWorkspace()
		return err
	})
	group.Go(func() error {
		poller, err := e.environments.BeginCreateOrUpdate(groupCtx, e.resourceGroup, e.name, armappcontainers.ManagedEnvironment{
			Location: &e.location,
			Tags:     resourceTags(),
			Properties: &armappcontainers.ManagedEnvironmentProperties{
				AppLogsConfiguration: &armappcontainers.AppLogsConfiguration{
					Destination: new(azureMonitorDestination),
				},
				WorkloadProfiles: []*armappcontainers.WorkloadProfile{{
					Name:                new(consumptionProfile),
					WorkloadProfileType: new(consumptionProfile),
				}},
			},
		}, nil)
		if err == nil {
			_, err = poller.PollUntilDone(groupCtx, nil)
		}
		if err != nil {
			return fmt.Errorf("failed to create container apps environment %s: %w", e.name, err)
		}
		return nil
	})
	if err := group.Wait(); err != nil {
		return err
	}
	if err := e.createDiagnosticSetting(workspaceId); err != nil {
		return err
	}
	log.Printf("Created container apps environment %s\n", e.name)
	return nil
}

func (e *Environment) diagnosticSettingId() string {
	return e.id + "/providers/Microsoft.Insights/diagnosticSettings/" + diagnosticSettingName
}

// addMissingDiagnosticSetting doesn't rewrite an existing setting, since a changed setting can
// take up to 90 minutes to deliver logs again.
func (e *Environment) addMissingDiagnosticSetting() error {
	_, err := e.resources.GetByID(e.ctx, e.diagnosticSettingId(), diagnosticAPIVersion, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("failed to get diagnostic setting of container apps environment %s: %w", e.name, err)
	}
	workspaceId, err := e.ensureWorkspace()
	if err != nil {
		return err
	}
	if err = e.createDiagnosticSetting(workspaceId); err != nil {
		return err
	}
	log.Printf("Routed logs of container apps environment %s to log analytics workspace %s\n", e.name, e.workspace)
	return nil
}

func (e *Environment) createDiagnosticSetting(workspaceId string) error {
	poller, err := e.resources.BeginCreateOrUpdateByID(e.ctx, e.diagnosticSettingId(), diagnosticAPIVersion, armresources.GenericResource{
		Properties: map[string]any{
			"workspaceId": workspaceId,
			"logs": []map[string]any{
				{"category": "ContainerAppConsoleLogs", "enabled": true},
				{"category": "ContainerAppSystemLogs", "enabled": true},
			},
		},
	}, nil)
	if err == nil {
		_, err = poller.PollUntilDone(e.ctx, nil)
	}
	if err != nil {
		return fmt.Errorf("failed to route logs of container apps environment %s to log analytics workspace %s: %w", e.name, e.workspace, err)
	}
	return nil
}

func (e *Environment) ensureWorkspace() (string, error) {
	existing, err := e.workspaces.Get(e.ctx, e.resourceGroup, e.workspace, nil)
	if err == nil {
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", fmt.Errorf("failed to get log analytics workspace %s: %w", e.workspace, err)
	}
	poller, err := e.workspaces.BeginCreateOrUpdate(e.ctx, e.resourceGroup, e.workspace, armoperationalinsights.Workspace{
		Location: &e.location,
		Tags:     resourceTags(),
		Properties: &armoperationalinsights.WorkspaceProperties{
			SKU:             &armoperationalinsights.WorkspaceSKU{Name: new(armoperationalinsights.WorkspaceSKUNameEnumPerGB2018)},
			RetentionInDays: new(int32(logRetentionDays)),
		},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create log analytics workspace %s: %w", e.workspace, err)
	}
	created, err := poller.PollUntilDone(e.ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create log analytics workspace %s: %w", e.workspace, err)
	}
	return *created.ID, nil
}

func (e *Environment) deleteDiagnosticSetting() error {
	poller, err := e.resources.BeginDeleteByID(e.ctx, e.diagnosticSettingId(), diagnosticAPIVersion, nil)
	if err == nil {
		_, err = poller.PollUntilDone(e.ctx, nil)
	}
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// waitForDeletion waits for an earlier delete to finish, since Azure rejects creating an
// environment while one with the same name is being deleted.
func (e *Environment) waitForDeletion() error {
	deadline := time.Now().Add(deletionTimeout)
	for {
		if err := util.Sleep(e.ctx, pollInterval); err != nil {
			return err
		}
		_, err := e.environments.Get(e.ctx, e.resourceGroup, e.name, nil)
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to get container apps environment %s: %w", e.name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container apps environment %s is still being deleted after %s", e.name, deletionTimeout)
		}
	}
}

// Delete removes the agent's environment and its workspace. Azure takes up to 20 minutes to
// delete an environment, so without wait the deletion finishes in the background and the
// next Ensure waits for it.
func (e *Environment) Delete(wait bool) error {
	if err := e.deleteEnvironment(wait); err != nil {
		return err
	}
	poller, err := e.workspaces.BeginDelete(e.ctx, e.resourceGroup, e.workspace, &armoperationalinsights.WorkspacesClientBeginDeleteOptions{Force: new(true)})
	if err == nil {
		_, err = poller.PollUntilDone(e.ctx, nil)
	}
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("failed to delete log analytics workspace %s: %w", e.workspace, err)
	}
	return nil
}

func (e *Environment) deleteEnvironment(wait bool) error {
	// A diagnostic setting outlives its resource and would apply to a new one with the same
	// name. Ensure overwrites it anyway, so a failure only leaves it behind.
	if err := e.deleteDiagnosticSetting(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete diagnostic setting of container apps environment %s: %s", e.name, err)))
	}
	poller, err := e.environments.BeginDelete(e.ctx, e.resourceGroup, e.name, nil)
	scheduled := errorCode(err) == scheduledForDeleteCode
	switch {
	case isNotFound(err):
		return nil
	case err != nil && !scheduled:
		return fmt.Errorf("failed to delete container apps environment %s: %w", e.name, err)
	case !wait:
		log.Printf("Container apps environment %s is being deleted in the background, this can take up to 20 minutes\n", e.name)
		return nil
	}
	log.Printf("Deleting container apps environment %s, this can take up to 20 minutes\n", e.name)
	if scheduled {
		err = e.waitForDeletion()
	} else {
		_, err = poller.PollUntilDone(e.ctx, nil)
	}
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("failed to delete container apps environment %s: %w", e.name, err)
	}
	log.Printf("Deleted container apps environment %s\n", e.name)
	return nil
}
