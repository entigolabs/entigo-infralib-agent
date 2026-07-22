package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

// customerSecretKeyObject is the Vault secret name under which the provisioned
// S3-compatible credentials are persisted. The secret half is only returned by
// the Identity API at creation time, so it must be stored to survive restarts.
const customerSecretKeyObject = "oracle-customer-secret-key"

// secretPersistence is the subset of the Vault-backed SSM that IAM uses to persist
// the credentials it provisions (Customer Secret Key + DevOps auth token). They
// are secrets, so they live in the Vault under the agent's key, not in the bucket.
type secretPersistence interface {
	GetParameter(name string) (*model.Parameter, error)
	PutSecret(name, value string) error
	DeleteSecret(name string) error
}

// readPersistedSecret returns the stored value, or ("", false) when absent.
func readPersistedSecret(store secretPersistence, name string) (string, bool, error) {
	param, err := store.GetParameter(name)
	if err != nil {
		var notFound *model.ParameterNotFoundError
		if errors.As(err, &notFound) {
			return "", false, nil
		}
		return "", false, err
	}
	return *param.Value, true, nil
}

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
// to the Vault, or ("", "") if none is stored. Used when no user OCID is available
// to (re)provision a key — resource-principal (in-container) runs read the key a
// local bootstrap wrote, since a resource principal can't own a CSK.
func loadPersistedCustomerSecretKey(store secretPersistence) (string, string, error) {
	value, found, err := readPersistedSecret(store, customerSecretKeyObject)
	if err != nil || !found {
		return "", "", err
	}
	var stored storedCredentials
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
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
func EnsureCustomerSecretKey(csk customerSecretKeyClient, store secretPersistence, userId, displayName string) (string, string, bool, error) {
	value, found, err := readPersistedSecret(store, customerSecretKeyObject)
	if err != nil {
		return "", "", false, err
	}
	if found {
		var stored storedCredentials
		if json.Unmarshal([]byte(value), &stored) == nil && stored.AccessKey != "" {
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
	if err = store.PutSecret(customerSecretKeyObject, string(data)); err != nil {
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

// EnsureAuthToken returns an OCI auth token for the given user, used only on the
// local bootstrap run that git-pushes the DevOps build-spec (in-container RP runs
// reference the already-seeded repo). It reuses a token only when it is present
// on BOTH sides — persisted in the config bucket AND still an active token on the
// user (matched by our description, since auth tokens have no unique name). The
// token value is returned by OCI exactly once at creation, so a persisted value
// is meaningful only while its user-side token exists, and a user-side token is
// useless to us without the persisted value. If either was deleted (by hand or a
// failed run) the two have drifted, so the leftover is removed and a fresh token
// created — which also stops stale tokens accruing against the 2-per-user limit.
// Mirrors EnsureCustomerSecretKey's persist-and-verify approach.
func (i *IAM) EnsureAuthToken(store secretPersistence, userId, description string) (string, error) {
	existing, err := i.listAuthTokenIds(userId, description)
	if err != nil {
		return "", err
	}
	stored, found, err := readPersistedSecret(store, devopsAuthTokenObject)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 && found {
		return stored, nil
	}
	// Drifted: discard whichever side survives so they can't stay out of sync.
	for _, id := range existing {
		if err = i.deleteAuthToken(userId, id); err != nil {
			return "", fmt.Errorf("failed to delete stale auth token %s: %w", id, err)
		}
	}
	if found {
		if err = store.DeleteSecret(devopsAuthTokenObject); err != nil {
			return "", fmt.Errorf("failed to delete stale persisted auth token: %w", err)
		}
	}
	response, err := i.client.CreateAuthToken(i.ctx, identity.CreateAuthTokenRequest{
		UserId: &userId,
		CreateAuthTokenDetails: identity.CreateAuthTokenDetails{
			Description: &description,
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create auth token: %w", err)
	}
	if response.Token == nil {
		return "", fmt.Errorf("auth token response missing token value")
	}
	if err = store.PutSecret(devopsAuthTokenObject, *response.Token); err != nil {
		return "", fmt.Errorf("failed to persist auth token: %w", err)
	}
	log.Printf("Provisioned Oracle auth token %q for DevOps build-spec git push\n", description)
	return *response.Token, nil
}

// listAuthTokenIds returns the OCIDs of the user's ACTIVE auth tokens whose
// description matches (auth tokens carry no unique name, so the description is
// how we recognise ours).
func (i *IAM) listAuthTokenIds(userId, description string) ([]string, error) {
	response, err := i.client.ListAuthTokens(i.ctx, identity.ListAuthTokensRequest{UserId: &userId})
	if err != nil {
		return nil, fmt.Errorf("failed to list auth tokens: %w", err)
	}
	var ids []string
	for _, token := range response.Items {
		if token.Id != nil && token.LifecycleState == identity.AuthTokenLifecycleStateActive &&
			token.Description != nil && *token.Description == description {
			ids = append(ids, *token.Id)
		}
	}
	return ids, nil
}

func (i *IAM) deleteAuthToken(userId, tokenId string) error {
	_, err := i.client.DeleteAuthToken(i.ctx, identity.DeleteAuthTokenRequest{
		UserId:      &userId,
		AuthTokenId: &tokenId,
	})
	return err
}

// TenancyName returns the tenancy's name, the prefix of the OCI code-repository
// HTTPS git username (<tenancy>/<login>). This is the tenancy NAME, not the
// object-storage namespace — the two differ.
func (i *IAM) TenancyName() (string, error) {
	response, err := i.client.GetTenancy(i.ctx, identity.GetTenancyRequest{TenancyId: &i.tenancyId})
	if err != nil {
		return "", fmt.Errorf("failed to get tenancy %s: %w", i.tenancyId, err)
	}
	if response.Name == nil {
		return "", fmt.Errorf("tenancy %s has no name", i.tenancyId)
	}
	return *response.Name, nil
}

// Username returns the login name of the given user OCID, used as the HTTPS
// basic-auth username for git pushes to OCI code repositories.
func (i *IAM) Username(userId string) (string, error) {
	response, err := i.client.GetUser(i.ctx, identity.GetUserRequest{UserId: &userId})
	if err != nil {
		return "", fmt.Errorf("failed to get user %s: %w", userId, err)
	}
	if response.Name == nil {
		return "", fmt.Errorf("user %s has no name", userId)
	}
	return *response.Name, nil
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

// sameStatements compares policy statements as an order-insensitive set.
func sameStatements(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
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
		existing := list.Items[0]
		if sameStatements(existing.Statements, statements) {
			return nil
		}
		// Self-heal: an earlier run may have created this policy with a narrower
		// statement set (e.g. the build policy started as devops-family before it
		// needed all-resources). Update it to the desired statements.
		_, err = i.client.UpdatePolicy(i.ctx, identity.UpdatePolicyRequest{
			PolicyId:            existing.Id,
			UpdatePolicyDetails: identity.UpdatePolicyDetails{Statements: statements},
		})
		if err != nil {
			return fmt.Errorf("failed to update policy %s: %w", name, err)
		}
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

// EnsureDevOpsBuildAccess grants the DevOps build pipeline's resource principal
// the permissions it needs: reading the hosted build-spec repo and fetching the
// step's Vault secrets (decrypting them with the agent's key), and — because the
// runner's RP is forwarded into the step container where terraform runs —
// managing the infrastructure the steps create. A dynamic group matches build
// pipelines in the compartment and is granted manage all-resources over it.
func (i *IAM) EnsureDevOpsBuildAccess(cloudPrefix string) error {
	dgName := fmt.Sprintf("%s-infralib", cloudPrefix)
	matchingRule := fmt.Sprintf("ALL {resource.type='devopsbuildpipeline', resource.compartment.id='%s'}", i.compartmentId)
	if err := i.ensureDynamicGroup(dgName, "Entigo infralib devops build pipelines", matchingRule); err != nil {
		return err
	}
	statement := fmt.Sprintf("Allow dynamic-group %s to manage all-resources in compartment id %s", dgName, i.compartmentId)
	return i.ensurePolicy(fmt.Sprintf("%s-infralib", cloudPrefix), "Entigo infralib devops build access", []string{statement})
}

// EnsureObjectStorageKeyAccess lets the Object Storage service principal use the
// agent's KMS key so it can encrypt the bucket at rest with a customer-managed
// key. The service name is region-qualified (objectstorage-<region>), and access
// is scoped to the single key by target.key.id.
func (i *IAM) EnsureObjectStorageKeyAccess(cloudPrefix, region, keyId string) error {
	statement := fmt.Sprintf("Allow service objectstorage-%s to use keys in compartment id %s where target.key.id = '%s'",
		region, i.compartmentId, keyId)
	return i.ensurePolicy(fmt.Sprintf("%s-infralib-kms", cloudPrefix), "Entigo infralib Object Storage KMS access", []string{statement})
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
		existing := list.Items[0]
		if existing.MatchingRule != nil && *existing.MatchingRule == matchingRule {
			return nil
		}
		// Self-heal: an earlier run may have created the group with a different
		// matching rule. Update it so the intended principals are actually members.
		_, err = i.client.UpdateDynamicGroup(i.ctx, identity.UpdateDynamicGroupRequest{
			DynamicGroupId:            existing.Id,
			UpdateDynamicGroupDetails: identity.UpdateDynamicGroupDetails{MatchingRule: &matchingRule},
		})
		if err != nil {
			return fmt.Errorf("failed to update dynamic group %s: %w", name, err)
		}
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
