package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

// customerSecretKeyObject is the config-bucket key under which the provisioned
// S3-compatible credentials are persisted. The secret half is only returned by
// the Identity API at creation time, so it must be stored to survive restarts.
const customerSecretKeyObject = "oracle-customer-secret-key"

type IAM struct {
	ctx           context.Context
	client        identity.IdentityClient
	tenancyId     string
	compartmentId string
}

func NewIAM(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId string) (*IAM, error) {
	client, err := identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
	}
	tenancyId, err := provider.TenancyOCID()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve tenancy ocid: %w", err)
	}
	return &IAM{
		ctx:           ctx,
		client:        client,
		tenancyId:     tenancyId,
		compartmentId: compartmentId,
	}, nil
}

// customerSecretKeyClient is the subset of Identity operations the credential
// provisioning needs, extracted so EnsureCustomerSecretKey can be unit tested.
type customerSecretKeyClient interface {
	createCustomerSecretKey(userId, displayName string) (id string, secret string, err error)
	listCustomerSecretKeyIds(userId string) (model.Set[string], error)
	deleteCustomerSecretKey(userId, keyId string) error
}

type storedCredentials struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// loadPersistedCustomerSecretKey returns the S3-compat pair a prior run persisted
// to the config bucket, or ("", "") if none is stored. Used when no user OCID is
// available to (re)provision a key — resource-principal (in-container) runs read
// the key a local bootstrap wrote, since a resource principal can't own a CSK.
func loadPersistedCustomerSecretKey(store objectStore) (string, string, error) {
	content, err := store.GetFile(customerSecretKeyObject)
	if err != nil || content == nil {
		return "", "", err
	}
	var stored storedCredentials
	if err := json.Unmarshal(content, &stored); err != nil {
		return "", "", fmt.Errorf("failed to parse persisted customer secret key: %w", err)
	}
	return stored.AccessKey, stored.SecretKey, nil
}

// EnsureCustomerSecretKey returns the S3-compat access/secret pair for userId,
// creating and persisting one if none is stored or the stored key no longer
// exists on the user (deleted/rotated out-of-band). The secret is written to the
// config bucket because the Identity API returns it only once. The bool reports
// whether a new key was created — a fresh key needs to propagate to the bucket
// region before it's usable, so the caller waits harder for it than a reused one.
func EnsureCustomerSecretKey(csk customerSecretKeyClient, store objectStore, userId, displayName string) (string, string, bool, error) {
	content, err := store.GetFile(customerSecretKeyObject)
	if err != nil {
		return "", "", false, err
	}
	if content != nil {
		var stored storedCredentials
		if json.Unmarshal(content, &stored) == nil && stored.AccessKey != "" {
			existing, err := csk.listCustomerSecretKeyIds(userId)
			if err != nil {
				return "", "", false, err
			}
			if existing.Contains(stored.AccessKey) {
				return stored.AccessKey, stored.SecretKey, false, nil
			}
		}
	}
	id, secret, err := csk.createCustomerSecretKey(userId, displayName)
	if err != nil {
		return "", "", false, err
	}
	data, err := json.Marshal(storedCredentials{AccessKey: id, SecretKey: secret})
	if err != nil {
		return "", "", false, err
	}
	if err = store.PutFile(customerSecretKeyObject, data); err != nil {
		return "", "", false, err
	}
	log.Printf("Provisioned Oracle Customer Secret Key %s for terraform state access\n", id)
	return id, secret, true, nil
}

func (i *IAM) createCustomerSecretKey(userId, displayName string) (string, string, error) {
	response, err := i.client.CreateCustomerSecretKey(i.ctx, identity.CreateCustomerSecretKeyRequest{
		UserId: &userId,
		CreateCustomerSecretKeyDetails: identity.CreateCustomerSecretKeyDetails{
			DisplayName: &displayName,
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to create customer secret key: %w", err)
	}
	if response.Id == nil || response.Key == nil {
		return "", "", fmt.Errorf("customer secret key response missing id or secret")
	}
	return *response.Id, *response.Key, nil
}

func (i *IAM) listCustomerSecretKeyIds(userId string) (model.Set[string], error) {
	response, err := i.client.ListCustomerSecretKeys(i.ctx, identity.ListCustomerSecretKeysRequest{
		UserId: &userId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list customer secret keys: %w", err)
	}
	ids := model.NewSet[string]()
	for _, key := range response.Items {
		if key.Id != nil && key.LifecycleState == identity.CustomerSecretKeySummaryLifecycleStateActive {
			ids.Add(*key.Id)
		}
	}
	return ids, nil
}

func (i *IAM) deleteCustomerSecretKey(userId, keyId string) error {
	_, err := i.client.DeleteCustomerSecretKey(i.ctx, identity.DeleteCustomerSecretKeyRequest{
		UserId:              &userId,
		CustomerSecretKeyId: &keyId,
	})
	return err
}

// getOrCreateUser returns the OCID of the named user, creating it in the tenancy
// (root compartment) if absent. The bool reports whether it was newly created.
func (i *IAM) getOrCreateUser(name, description string) (string, bool, error) {
	list, err := i.client.ListUsers(i.ctx, identity.ListUsersRequest{
		CompartmentId: &i.tenancyId,
		Name:          &name,
	})
	if err != nil {
		return "", false, fmt.Errorf("failed to list users: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, false, nil
	}
	created, err := i.client.CreateUser(i.ctx, identity.CreateUserRequest{
		CreateUserDetails: identity.CreateUserDetails{
			CompartmentId: &i.tenancyId,
			Name:          &name,
			Description:   &description,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("failed to create user %s: %w", name, err)
	}
	return *created.Id, true, nil
}

func (i *IAM) getOrCreateGroup(name, description string) (string, error) {
	list, err := i.client.ListGroups(i.ctx, identity.ListGroupsRequest{
		CompartmentId: &i.tenancyId,
		Name:          &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list groups: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, nil
	}
	created, err := i.client.CreateGroup(i.ctx, identity.CreateGroupRequest{
		CreateGroupDetails: identity.CreateGroupDetails{
			CompartmentId: &i.tenancyId,
			Name:          &name,
			Description:   &description,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create group %s: %w", name, err)
	}
	return *created.Id, nil
}

// addUserToGroup is idempotent — an existing membership (HTTP 409) is not an error.
func (i *IAM) addUserToGroup(userId, groupId string) error {
	_, err := i.client.AddUserToGroup(i.ctx, identity.AddUserToGroupRequest{
		AddUserToGroupDetails: identity.AddUserToGroupDetails{
			UserId:  &userId,
			GroupId: &groupId,
		},
	})
	if err != nil {
		if failure, ok := ocicommon.IsServiceError(err); ok && failure.GetHTTPStatusCode() == 409 {
			return nil
		}
		return fmt.Errorf("failed to add user to group: %w", err)
	}
	return nil
}

func (i *IAM) ensurePolicy(name, description string, statements []string) error {
	list, err := i.client.ListPolicies(i.ctx, identity.ListPoliciesRequest{
		CompartmentId: &i.tenancyId,
		Name:          &name,
	})
	if err != nil {
		return fmt.Errorf("failed to list policies: %w", err)
	}
	if len(list.Items) > 0 {
		return nil
	}
	_, err = i.client.CreatePolicy(i.ctx, identity.CreatePolicyRequest{
		CreatePolicyDetails: identity.CreatePolicyDetails{
			CompartmentId: &i.tenancyId,
			Name:          &name,
			Description:   &description,
			Statements:    statements,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create policy %s: %w", name, err)
	}
	return nil
}

// EnsureComputeAccess grants Container Instances in the compartment permission to
// manage resources, via a dynamic group (matching container instances in the
// compartment) plus a policy. This is the resource principal terraform uses
// inside the execution container.
func (i *IAM) EnsureComputeAccess(cloudPrefix string) error {
	dgName := fmt.Sprintf("%s-ci-dg", cloudPrefix)
	matchingRule := fmt.Sprintf("ALL {resource.type='computecontainerinstance', resource.compartment.id='%s'}", i.compartmentId)
	if err := i.ensureDynamicGroup(dgName, "Entigo infralib container instances", matchingRule); err != nil {
		return err
	}
	statement := fmt.Sprintf("Allow dynamic-group %s to manage all-resources in compartment id %s", dgName, i.compartmentId)
	return i.ensurePolicy(fmt.Sprintf("%s-ci", cloudPrefix), "Entigo infralib container instance policy", []string{statement})
}

func (i *IAM) ensureDynamicGroup(name, description, matchingRule string) error {
	list, err := i.client.ListDynamicGroups(i.ctx, identity.ListDynamicGroupsRequest{
		CompartmentId: &i.tenancyId,
		Name:          &name,
	})
	if err != nil {
		return fmt.Errorf("failed to list dynamic groups: %w", err)
	}
	if len(list.Items) > 0 {
		return nil
	}
	_, err = i.client.CreateDynamicGroup(i.ctx, identity.CreateDynamicGroupRequest{
		CreateDynamicGroupDetails: identity.CreateDynamicGroupDetails{
			CompartmentId: &i.tenancyId,
			Name:          &name,
			Description:   &description,
			MatchingRule:  &matchingRule,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create dynamic group %s: %w", name, err)
	}
	return nil
}

func (i *IAM) rotateCustomerSecretKeys(userId string) error {
	ids, err := i.listCustomerSecretKeyIds(userId)
	if err != nil {
		return err
	}
	for id := range ids {
		if err = i.deleteCustomerSecretKey(userId, id); err != nil {
			return fmt.Errorf("failed to delete customer secret key %s: %w", id, err)
		}
	}
	return nil
}
