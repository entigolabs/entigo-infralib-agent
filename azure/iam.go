package azure

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/google/uuid"
)

const (
	roleOwner                      = "8e3af657-a8ff-443c-a75c-2fe8c4bcb635"
	roleContributor                = "b24988ac-6180-42a0-ab88-20f7382dd24c"
	roleStorageBlobDataContributor = "ba92f5b4-2d11-453d-a403-e96b0029c9fe"
	roleKeyVaultAdministrator      = "00482a5a-887f-4fb3-b363-3b7fe8e74483"
	roleKeyVaultCryptoEncryption   = "e147488a-f6f5-4113-8e2d-b22465e65bf6"

	principalPropagationTimeout = 3 * time.Minute
	pollInterval                = 10 * time.Second
)

type IAM struct {
	ctx            context.Context
	subscriptionId string
	resourceGroup  string
	location       string
	groups         *armresources.ResourceGroupsClient
	resources      *armresources.Client
	identities     *armmsi.UserAssignedIdentitiesClient
	assignments    *armauthorization.RoleAssignmentsClient
}

type identity struct {
	Id          string
	Name        string
	PrincipalId string
	ClientId    string
}

func NewIAM(ctx context.Context, credential azcore.TokenCredential, subscriptionId, resourceGroup, location string) (*IAM, error) {
	groups, err := armresources.NewResourceGroupsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	resources, err := armresources.NewClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	identities, err := armmsi.NewUserAssignedIdentitiesClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	assignments, err := armauthorization.NewRoleAssignmentsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	return &IAM{
		ctx:            ctx,
		subscriptionId: subscriptionId,
		resourceGroup:  resourceGroup,
		location:       location,
		groups:         groups,
		resources:      resources,
		identities:     identities,
		assignments:    assignments,
	}, nil
}

func (i *IAM) EnsureResourceGroup() error {
	exists, err := i.ResourceGroupExists()
	if err != nil || exists {
		return err
	}
	return i.CreateResourceGroup()
}

func (i *IAM) ResourceGroupExists() (bool, error) {
	_, err := i.groups.Get(i.ctx, i.resourceGroup, nil)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to get resource group %s: %w", i.resourceGroup, err)
}

func (i *IAM) CreateResourceGroup() error {
	_, err := i.groups.CreateOrUpdate(i.ctx, i.resourceGroup, armresources.ResourceGroup{
		Location: &i.location,
		Tags:     resourceTags(),
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to create resource group %s: %w", i.resourceGroup, err)
	}
	log.Printf("Created resource group %s\n", i.resourceGroup)
	return nil
}

// DeleteResourceGroupIfEmpty keeps a group that still holds resources the agent didn't delete.
func (i *IAM) DeleteResourceGroupIfEmpty() error {
	pager := i.resources.NewListByResourceGroupPager(i.resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(i.ctx)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		if len(page.Value) > 0 {
			log.Printf("Resource group %s still has %d resources and will not be deleted\n", i.resourceGroup, len(page.Value))
			return nil
		}
	}
	return i.deleteResourceGroup()
}

func (i *IAM) deleteResourceGroup() error {
	poller, err := i.groups.BeginDelete(i.ctx, i.resourceGroup, nil)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	_, err = poller.PollUntilDone(i.ctx, nil)
	if err == nil {
		log.Printf("Deleted resource group %s\n", i.resourceGroup)
	}
	return err
}

func (i *IAM) EnsureIdentity(name string) (identity, error) {
	existing, err := i.identities.Get(i.ctx, i.resourceGroup, name, nil)
	if err == nil {
		return toIdentity(existing.Identity), nil
	}
	if !isNotFound(err) {
		return identity{}, fmt.Errorf("failed to get managed identity %s: %w", name, err)
	}
	created, err := i.identities.CreateOrUpdate(i.ctx, i.resourceGroup, name, armmsi.Identity{
		Location: &i.location,
		Tags:     resourceTags(),
	}, nil)
	if err != nil {
		return identity{}, fmt.Errorf("failed to create managed identity %s: %w", name, err)
	}
	log.Printf("Created managed identity %s\n", name)
	return toIdentity(created.Identity), nil
}

func (i *IAM) GetIdentity(name string) (identity, error) {
	existing, err := i.identities.Get(i.ctx, i.resourceGroup, name, nil)
	if err != nil {
		return identity{}, fmt.Errorf("failed to get managed identity %s: %w", name, err)
	}
	return toIdentity(existing.Identity), nil
}

func (i *IAM) DeleteIdentity(name string) error {
	_, err := i.identities.Delete(i.ctx, i.resourceGroup, name, nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (i identity) principal() principal {
	return principal{ObjectId: i.PrincipalId, Name: i.Name, Type: armauthorization.PrincipalTypeServicePrincipal}
}

func toIdentity(msi armmsi.Identity) identity {
	result := identity{}
	if msi.ID != nil {
		result.Id = *msi.ID
	}
	if msi.Name != nil {
		result.Name = *msi.Name
	}
	if msi.Properties != nil {
		if msi.Properties.PrincipalID != nil {
			result.PrincipalId = *msi.Properties.PrincipalID
		}
		if msi.Properties.ClientID != nil {
			result.ClientId = *msi.Properties.ClientID
		}
	}
	return result
}

func (i *IAM) subscriptionScope() string {
	return "/subscriptions/" + i.subscriptionId
}

func (i *IAM) resourceGroupScope() string {
	return i.subscriptionScope() + "/resourceGroups/" + i.resourceGroup
}

// AssignRole is idempotent: the assignment name is derived from scope, principal and role.
// An existing assignment at or above the scope is kept, so a principal without
// roleAssignments/write, like the job identity with Contributor, can run it too.
// A just-created principal may not have replicated yet, so PrincipalNotFound is retried.
func (i *IAM) AssignRole(scope string, assignee principal, roleId string) error {
	assigned, err := i.hasRole(scope, assignee.ObjectId, roleId)
	if err != nil {
		return err
	}
	if assigned {
		return nil
	}
	definitionId := fmt.Sprintf("%s/providers/Microsoft.Authorization/roleDefinitions/%s", i.subscriptionScope(), roleId)
	name := deterministicUUID(scope, assignee.ObjectId, roleId)
	deadline := time.Now().Add(principalPropagationTimeout)
	logged := false
	for {
		_, err := i.assignments.Create(i.ctx, scope, name, armauthorization.RoleAssignmentCreateParameters{
			Properties: &armauthorization.RoleAssignmentProperties{
				PrincipalID:      &assignee.ObjectId,
				RoleDefinitionID: &definitionId,
				PrincipalType:    &assignee.Type,
			},
		}, nil)
		if err == nil {
			return nil
		}
		if isStatus(err, http.StatusConflict) && errorCode(err) == "RoleAssignmentExists" {
			return nil
		}
		if errorCode(err) != "PrincipalNotFound" || time.Now().After(deadline) {
			return fmt.Errorf("failed to assign role %s to %s on %s: %w", roleId, assignee.ObjectId, scope, err)
		}
		if !logged {
			log.Printf("Waiting for %s to replicate in Entra ID before assigning its roles\n", cmp.Or(assignee.Name, assignee.ObjectId))
			logged = true
		}
		if err = util.Sleep(i.ctx, pollInterval); err != nil {
			return err
		}
	}
}

// hasRole lists at the resource group, which returns assignments at, above and below it, so
// a principal that can only read the group, like the service account, can check the
// subscription and resource scopes too.
func (i *IAM) hasRole(scope, principalId, roleId string) (bool, error) {
	scope = strings.ToLower(scope)
	pager := i.assignments.NewListForScopePager(i.resourceGroupScope(), &armauthorization.RoleAssignmentsClientListForScopeOptions{
		// The SDK adds the filter to the query as is. assignedTo() needs no escaping, but finds only users.
		Filter: new(url.PathEscape(fmt.Sprintf("principalId eq '%s'", principalId))),
	})
	for pager.More() {
		page, err := pager.NextPage(i.ctx)
		if err != nil {
			if isNotFound(err) || errorCode(err) == "PrincipalNotFound" {
				return false, nil
			}
			return false, fmt.Errorf("failed to list role assignments of %s on %s: %w", principalId, scope, err)
		}
		for _, assignment := range page.Value {
			properties := assignment.Properties
			if properties == nil || properties.RoleDefinitionID == nil || properties.Scope == nil {
				continue
			}
			assignmentScope := strings.TrimRight(strings.ToLower(*properties.Scope), "/")
			if strings.HasSuffix(strings.ToLower(*properties.RoleDefinitionID), "/"+roleId) &&
				(scope == assignmentScope || strings.HasPrefix(scope, assignmentScope+"/")) {
				return true, nil
			}
		}
	}
	return false, nil
}

// DeleteRoleAssignment checks existence first: deleting a missing assignment can still be
// denied, e.g. by an Owner condition against deleting Owner assignments.
func (i *IAM) DeleteRoleAssignment(scope, principalId, roleId string) error {
	name := deterministicUUID(scope, principalId, roleId)
	if _, err := i.assignments.Get(i.ctx, scope, name, nil); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	_, err := i.assignments.Delete(i.ctx, scope, name, nil)
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func deterministicUUID(parts ...string) string {
	return uuid.NewSHA1(uuid.Nil, []byte(strings.Join(parts, "/"))).String()
}

// retryUntilAuthorized retries call while it fails with 403, which is how data-plane
// role assignments that haven't propagated yet surface.
func retryUntilAuthorized(ctx context.Context, action string, call func() error) error {
	deadline := time.Now().Add(principalPropagationTimeout)
	logged := false
	for {
		err := call()
		if err == nil || !isStatus(err, http.StatusForbidden) || time.Now().After(deadline) {
			return err
		}
		if !logged {
			log.Printf("Waiting for role assignments to propagate before %s\n", action)
			logged = true
		}
		if err = util.Sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}
