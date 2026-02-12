package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/google/uuid"
)

type IAM struct {
	ctx                  context.Context
	credential           *azidentity.DefaultAzureCredential
	subscriptionId       string
	resourceGroup        string
	identityClient       *armmsi.UserAssignedIdentitiesClient
	roleAssignClient     *armauthorization.RoleAssignmentsClient
	roleDefinitionClient *armauthorization.RoleDefinitionsClient
}

func NewIAM(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup string) (*IAM, error) {
	identityClient, err := armmsi.NewUserAssignedIdentitiesClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity client: %w", err)
	}

	roleAssignClient, err := armauthorization.NewRoleAssignmentsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create role assignments client: %w", err)
	}

	roleDefinitionClient, err := armauthorization.NewRoleDefinitionsClient(credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create role definitions client: %w", err)
	}

	return &IAM{
		ctx:                  ctx,
		credential:           credential,
		subscriptionId:       subscriptionId,
		resourceGroup:        resourceGroup,
		identityClient:       identityClient,
		roleAssignClient:     roleAssignClient,
		roleDefinitionClient: roleDefinitionClient,
	}, nil
}

func (i *IAM) GetManagedIdentity(name string) (*armmsi.Identity, error) {
	resp, err := i.identityClient.Get(i.ctx, i.resourceGroup, name, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, err
	}
	return &resp.Identity, nil
}

func (i *IAM) GetOrCreateManagedIdentity(name string) (*armmsi.Identity, bool, error) {
	identity, err := i.GetManagedIdentity(name)
	if err != nil {
		return nil, false, err
	}
	if identity != nil {
		return identity, false, nil
	}

	resp, err := i.identityClient.CreateOrUpdate(i.ctx, i.resourceGroup, name, armmsi.Identity{
		Location: to.Ptr(i.getLocation()),
		Tags: map[string]*string{
			model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
		},
	}, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to create managed identity: %w", err)
	}
	log.Printf("Created managed identity: %s\n", name)
	return &resp.Identity, true, nil
}

func (i *IAM) DeleteManagedIdentity(name string) error {
	_, err := i.identityClient.Delete(i.ctx, i.resourceGroup, name, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil
		}
		return err
	}
	log.Printf("Deleted managed identity: %s\n", name)
	return nil
}

func (i *IAM) AssignRole(principalID, roleName string) error {
	roleDefinitionID, err := i.getRoleDefinitionID(roleName)
	if err != nil {
		return err
	}

	scope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", i.subscriptionId, i.resourceGroup)

	// Use assignedTo() filter format as required by the API
	pager := i.roleAssignClient.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{
		Filter: to.Ptr(fmt.Sprintf("assignedTo('%s')", principalID)),
	})

	for pager.More() {
		resp, err := pager.NextPage(i.ctx)
		if err != nil {
			return err
		}
		for _, assignment := range resp.Value {
			if assignment.Properties != nil && assignment.Properties.RoleDefinitionID != nil &&
				*assignment.Properties.RoleDefinitionID == roleDefinitionID {
				return nil
			}
		}
	}

	_, err = i.roleAssignClient.Create(i.ctx, scope, uuid.New().String(),
		armauthorization.RoleAssignmentCreateParameters{
			Properties: &armauthorization.RoleAssignmentProperties{
				PrincipalID:      to.Ptr(principalID),
				RoleDefinitionID: to.Ptr(roleDefinitionID),
				PrincipalType:    to.Ptr(armauthorization.PrincipalTypeServicePrincipal),
			},
		}, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.ErrorCode == "RoleAssignmentExists" {
			return nil
		}
		return fmt.Errorf("failed to assign role: %w", err)
	}
	log.Printf("Assigned role %s to principal %s\n", roleName, principalID)
	return nil
}

func (i *IAM) getRoleDefinitionID(roleName string) (string, error) {
	scope := fmt.Sprintf("/subscriptions/%s", i.subscriptionId)
	pager := i.roleDefinitionClient.NewListPager(scope, &armauthorization.RoleDefinitionsClientListOptions{
		Filter: to.Ptr(fmt.Sprintf("roleName eq '%s'", roleName)),
	})

	for pager.More() {
		resp, err := pager.NextPage(i.ctx)
		if err != nil {
			return "", err
		}
		for _, roleDef := range resp.Value {
			if roleDef.ID != nil {
				return *roleDef.ID, nil
			}
		}
	}
	return "", fmt.Errorf("role definition not found: %s", roleName)
}

func (i *IAM) getLocation() string {
	return "westeurope"
}

func (i *IAM) GetCurrentPrincipalID() (string, error) {
	// Get an access token and extract the oid (object ID) claim
	token, err := i.credential.GetToken(i.ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}

	// Parse the JWT token to get the oid claim
	parts := strings.Split(token.Token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid token format")
	}

	// Decode the payload (second part)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("failed to decode token payload: %w", err)
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("failed to parse token claims: %w", err)
	}

	// Try oid first (for user/service principal), then appid for managed identity
	if oid, ok := claims["oid"].(string); ok {
		return oid, nil
	}

	return "", fmt.Errorf("could not find principal ID in token claims")
}

func (i *IAM) AssignRoleAtScope(principalID, roleName, scope string) error {
	roleDefinitionID, err := i.getRoleDefinitionID(roleName)
	if err != nil {
		return err
	}

	// Check if role is already assigned
	pager := i.roleAssignClient.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{
		Filter: to.Ptr(fmt.Sprintf("assignedTo('%s')", principalID)),
	})

	for pager.More() {
		resp, err := pager.NextPage(i.ctx)
		if err != nil {
			return err
		}
		for _, assignment := range resp.Value {
			if assignment.Properties != nil && assignment.Properties.RoleDefinitionID != nil &&
				*assignment.Properties.RoleDefinitionID == roleDefinitionID {
				return nil
			}
		}
	}

	_, err = i.roleAssignClient.Create(i.ctx, scope, uuid.New().String(),
		armauthorization.RoleAssignmentCreateParameters{
			Properties: &armauthorization.RoleAssignmentProperties{
				PrincipalID:      to.Ptr(principalID),
				RoleDefinitionID: to.Ptr(roleDefinitionID),
			},
		}, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.ErrorCode == "RoleAssignmentExists" {
			return nil
		}
		return fmt.Errorf("failed to assign role: %w", err)
	}
	log.Printf("Assigned role %s to principal %s at scope %s\n", roleName, principalID, scope)
	return nil
}
