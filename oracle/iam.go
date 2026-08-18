package oracle

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

// customerSecretKeyObject is the Vault secret name for the provisioned S3-compatible
// credentials. The secret half is returned by the Identity API only at creation time,
// so it must be persisted to survive restarts.
const customerSecretKeyObject = "oracle-customer-secret-key"

// devopsAuthTokenObject persists the OCI auth token used to git-push the build specs.
const devopsAuthTokenObject = "oracle-devops-auth-token"

// gitUsernameObject persists the HTTPS username that pushed the build specs, so later
// runs need no Identity lookup to reconstruct the <tenancy>/<login> form.
const gitUsernameObject = "oracle-git-username"

// stateKeyName and gitTokenDescription label the credentials the agent creates on the
// executing user. They are how the agent recognises its own — replacing or deleting only
// these and never the user's other keys and tokens.
func stateKeyName(cloudPrefix string) string {
	return fmt.Sprintf("entigo-infralib-%s-state", cloudPrefix)
}

func gitTokenDescription(cloudPrefix string) string {
	return fmt.Sprintf("entigo-infralib-%s-devops", cloudPrefix)
}

// secretPersistence is the subset of the Vault-backed SSM that IAM uses to persist the
// credentials it provisions (Customer Secret Key + DevOps auth token).
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
// provisioning needs, extracted so CreateCustomerSecretKey can be unit tested.
type customerSecretKeyClient interface {
	createCustomerSecretKey(userId, displayName string) (id string, secret string, err error)
	listCustomerSecretKeyIds(userId, displayName string) (model.Set[string], error)
	deleteCustomerSecretKey(userId, keyId string) error
}

type storedCredentials struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// loadPersistedCustomerSecretKey returns the S3-compat pair a prior run persisted to the
// Vault, or ("", "") if none is stored. In-container resource-principal runs read the key
// a local bootstrap wrote, since a resource principal can't own a CSK.
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

// CreateCustomerSecretKey mints an S3-compat access/secret pair on userId and persists it
// to the Vault, called only when nothing is persisted yet. Any earlier key of the agent's
// own (same display name) is deleted first: its secret half was returned once at creation
// and is gone, so it is unusable, and OCI caps a user at 2 keys. Other keys on the user
// are never touched.
func CreateCustomerSecretKey(csk customerSecretKeyClient, store secretPersistence, userId, displayName string) (string, string, error) {
	stale, err := csk.listCustomerSecretKeyIds(userId, displayName)
	if err != nil {
		return "", "", err
	}
	for id := range stale {
		if err = csk.deleteCustomerSecretKey(userId, id); err != nil {
			return "", "", fmt.Errorf("failed to delete unusable customer secret key %s: %w", id, err)
		}
	}
	id, secret, err := csk.createCustomerSecretKey(userId, displayName)
	if err != nil {
		return "", "", err
	}
	data, err := json.Marshal(storedCredentials{AccessKey: id, SecretKey: secret})
	if err != nil {
		return "", "", err
	}
	if err = store.PutSecret(customerSecretKeyObject, string(data)); err != nil {
		return "", "", err
	}
	log.Printf("Provisioned Oracle Customer Secret Key %s for terraform state access\n", id)
	return id, secret, nil
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

// listCustomerSecretKeyIds returns the OCIDs of the user's ACTIVE Customer Secret Keys,
// restricted to the given display name unless it is empty (keys aren't uniquely named, so
// the display name is how the agent recognises its own).
func (i *IAM) listCustomerSecretKeyIds(userId, displayName string) (model.Set[string], error) {
	response, err := i.client.ListCustomerSecretKeys(i.ctx, identity.ListCustomerSecretKeysRequest{
		UserId: &userId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list customer secret keys: %w", err)
	}
	ids := model.NewSet[string]()
	for _, key := range response.Items {
		if key.Id == nil || key.LifecycleState != identity.CustomerSecretKeySummaryLifecycleStateActive {
			continue
		}
		if displayName != "" && (key.DisplayName == nil || *key.DisplayName != displayName) {
			continue
		}
		ids.Add(*key.Id)
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

// CreateAuthToken mints an OCI auth token on the user (the git password for the build-spec
// push) and persists it, called only when nothing is persisted yet. Any earlier token of
// the agent's own (same description) is deleted first: OCI returns a token's value once at
// creation, so a token without its persisted value is unusable, and the user is capped at 2.
// Tokens with other descriptions — the user's own — are never touched.
func (i *IAM) CreateAuthToken(store secretPersistence, userId, description string) (string, error) {
	stale, err := i.listAuthTokenIds(userId, description)
	if err != nil {
		return "", err
	}
	for _, id := range stale {
		if err = i.deleteAuthToken(userId, id); err != nil {
			return "", fmt.Errorf("failed to delete unusable auth token %s: %w", id, err)
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

// TenancyName returns the tenancy's name, the prefix of the OCI code-repository HTTPS git
// username (<tenancy>/<login>) — the tenancy NAME, not the object-storage namespace.
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
	// Identity-domain tenancies reject user creation without a primary email; legacy ones
	// ignore it. This is a machine account that never receives mail — the address just
	// marks it as ours.
	email := fmt.Sprintf("%s@entigo.com", name)
	created, err := i.client.CreateUser(i.ctx, identity.CreateUserRequest{
		CreateUserDetails: identity.CreateUserDetails{
			CompartmentId: &i.tenancyId,
			Name:          &name,
			Description:   &description,
			Email:         &email,
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
		if failure, ok := asServiceError(err); ok && failure.GetHTTPStatusCode() == 409 {
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

// ensurePolicy find-or-creates a policy ATTACHED TO THE COMPARTMENT, never the tenancy
// root, so the agent creates no resource outside its compartment. Consequence: every
// statement must be compartment-scoped — a compartment-attached policy cannot carry an
// `in tenancy` statement. Reports whether it changed — a change needs time to take effect.
func (i *IAM) ensurePolicy(name, description string, statements []string) (bool, error) {
	list, err := i.client.ListPolicies(i.ctx, identity.ListPoliciesRequest{
		CompartmentId: &i.compartmentId,
		Name:          &name,
	})
	if err != nil {
		return false, err
	}
	if len(list.Items) > 0 {
		existing := list.Items[0]
		if sameStatements(existing.Statements, statements) {
			return false, nil
		}
		// Self-heal: an earlier run may have created this policy with a narrower
		// statement set. Update it to the desired statements.
		_, err = i.client.UpdatePolicy(i.ctx, identity.UpdatePolicyRequest{
			PolicyId:            existing.Id,
			UpdatePolicyDetails: identity.UpdatePolicyDetails{Statements: statements},
		})
		if err != nil {
			return false, fmt.Errorf("failed to update policy %s: %w", name, err)
		}
		return true, nil
	}
	_, err = i.client.CreatePolicy(i.ctx, identity.CreatePolicyRequest{
		CreatePolicyDetails: identity.CreatePolicyDetails{
			CompartmentId: &i.compartmentId,
			Name:          &name,
			Description:   &description,
			Statements:    statements,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		if isConflictStatus(err) {
			// Policy names are unique tenancy-wide, so a conflict means a same-named policy
			// is attached to another compartment (or the tenancy root) where the agent can't
			// see it.
			return false, fmt.Errorf("policy %s already exists outside compartment %s; delete it or use another prefix: %w",
				name, i.compartmentId, err)
		}
		return false, fmt.Errorf("failed to create policy %s: %w", name, err)
	}
	return true, nil
}

// EnsureDevOpsBuildAccess grants the DevOps build pipelines' resource principal the
// permissions it needs: fetching+decrypting the step's Vault secrets (the spec's
// vaultVariables) and — because the RP is forwarded into the step container where terraform
// runs — managing the infrastructure the steps create.
func (i *IAM) EnsureDevOpsBuildAccess(cloudPrefix string) error {
	_, err := i.ensurePolicy(fmt.Sprintf("%s-infralib", cloudPrefix), "Entigo infralib devops build access",
		devOpsBuildStatements(i.compartmentId))
	return err
}

// devOpsBuildStatements grants the build pipelines' resource principals directly, with no
// dynamic group: OCI creates dynamic groups only in the tenancy root, which the
// compartment-only mandate forbids. `any-user` covers resource principals, so the
// conditions carry the whole scoping — a devopsbuildpipeline principal, from this
// compartment. That is as narrow as the policy language allows: there is no variable for a
// principal's DevOps project, and none for a principal's own tags either
// (request.principal.compartment.tag is the compartment's tags, not the pipeline's), so
// tightening further means enumerating request.principal.id per pipeline. Consequence:
// every build pipeline in the compartment gets this grant, so the compartment should hold
// nothing but this deployment. Deliberately NO tenancy-level grants — steps whose terraform
// creates tenancy IAM are unsupported and fail in the step, not here.
func devOpsBuildStatements(compartmentId string) []string {
	return []string{
		fmt.Sprintf("Allow any-user to manage all-resources in compartment id %s where all "+
			"{ request.principal.type = 'devopsbuildpipeline', request.principal.compartment.id = '%s' }",
			compartmentId, compartmentId),
	}
}

// EnsureObjectStorageKeyAccess lets the Object Storage service principal use the agent's
// KMS key to encrypt the bucket with a customer-managed key. The service name is
// region-qualified (objectstorage-<region>), scoped to the single key by target.key.id.
func (i *IAM) EnsureObjectStorageKeyAccess(cloudPrefix, region, keyId string) error {
	statement := fmt.Sprintf("Allow service objectstorage-%s to use keys in compartment id %s where target.key.id = '%s'",
		region, i.compartmentId, keyId)
	_, err := i.ensurePolicy(fmt.Sprintf("%s-infralib-kms", cloudPrefix), "Entigo infralib Object Storage KMS access", []string{statement})
	return err
}

// agentGrant names the principal the agent's own access policy grants: the EXECUTING user by
// OCID, since a group is a tenancy-root resource the agent must not create, or a pre-created
// group (ORACLE_AGENT_GROUP) so several operators share one policy no run rewrites.
type agentGrant struct {
	group  string
	userId string
}

func (g agentGrant) valid() bool { return g.group != "" || g.userId != "" }

// statement renders one Allow statement. Matching the user by OCID is a condition like any
// other, so it merges into `where all { … }` with the resource conditions.
func (g agentGrant) statement(verb, resource, compartmentId string, conditions ...string) string {
	subject := "group " + g.group
	if g.group == "" {
		subject = "any-user"
		conditions = append(conditions, fmt.Sprintf("request.user.id = '%s'", g.userId))
	}
	statement := fmt.Sprintf("Allow %s to %s %s in compartment id %s", subject, verb, resource, compartmentId)
	switch len(conditions) {
	case 0:
		return statement
	case 1:
		return fmt.Sprintf("%s where %s", statement, conditions[0])
	default:
		return fmt.Sprintf("%s where all { %s }", statement, strings.Join(conditions, ", "))
	}
}

// EnsureAgentAccess grants the agent's own principal what it needs inside the compartment, so
// a deployment can be run by a user holding nothing but `manage policies` there — the
// compartment being the isolation boundary, OCI having no equivalent of separate AWS
// accounts. Reports whether the policy changed; until a change takes effect it authorizes
// nothing.
func (i *IAM) EnsureAgentAccess(cloudPrefix, bucketName string, grant agentGrant) (bool, error) {
	return i.ensurePolicy(fmt.Sprintf("%s-infralib-agent", cloudPrefix), "Entigo infralib agent access",
		agentAccessStatements(grant, i.compartmentId, bucketName))
}

// agentAccessStatements mirrors the OCI calls this package makes as the agent's own principal;
// a new call means a new statement here. Out of reach either way, identity policies attaching
// only to the tenancy root: the git username's tenancy+user reads (ORACLE_GIT_USERNAME covers
// it) and the `sa` command.
func agentAccessStatements(grant agentGrant, compartmentId, bucketName string) []string {
	return []string{
		grant.statement("manage", "vaults", compartmentId),
		grant.statement("manage", "keys", compartmentId),
		// KEY_ASSOCIATE, letting Object Storage encrypt the bucket with the key. Its own
		// resource type — `manage keys` does NOT include it — and Object Storage reports it
		// missing as CreateBucket 409 BucketAlreadyExists.
		grant.statement("use", "key-delegate", compartmentId),
		grant.statement("manage", "secret-family", compartmentId),
		grant.statement("manage", "buckets", compartmentId),
		// Covers terraform's s3 backend traffic too: it is signed with this user's CSK.
		grant.statement("manage", "objects", compartmentId, fmt.Sprintf("target.bucket.name = '%s'", bucketName)),
		grant.statement("manage", "devops-family", compartmentId),
		// CreateLog is a log-groups permission; searching the plan output reads log-content.
		grant.statement("manage", "log-groups", compartmentId),
		grant.statement("read", "log-content", compartmentId),
		grant.statement("manage", "ons-topics", compartmentId),
	}
}

// DeleteAgentCredentials removes the credentials the agent provisioned on the executing
// user — the state Customer Secret Key and the DevOps git auth token, recognised by the
// display name / description the agent gives them — plus the build access policy that
// mirrors EnsureDevOpsBuildAccess. Credentials seeded by a DIFFERENT user stay on that
// user (nothing records who created them), so they must be removed there. The -infralib-kms
// policy is deleted separately by the caller AFTER the bucket (the Object Storage principal
// needs it until the bucket is gone). Best-effort.
func (i *IAM) DeleteAgentCredentials(cloudPrefix, userId string) {
	i.deletePolicyByName(fmt.Sprintf("%s-infralib", cloudPrefix))
	if userId == "" {
		return
	}
	keys, err := i.listCustomerSecretKeyIds(userId, stateKeyName(cloudPrefix))
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to list customer secret keys: %s", err)))
	}
	for id := range keys {
		if err = i.deleteCustomerSecretKey(userId, id); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete customer secret key %s: %s", id, err)))
		} else {
			log.Printf("Deleted Customer Secret Key %s\n", id)
		}
	}
	tokens, err := i.listAuthTokenIds(userId, gitTokenDescription(cloudPrefix))
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to list auth tokens: %s", err)))
	}
	for _, id := range tokens {
		if err = i.deleteAuthToken(userId, id); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete auth token %s: %s", id, err)))
		} else {
			log.Printf("Deleted auth token %s\n", id)
		}
	}
}

// DeleteCICDServiceAccount removes the external CI/CD service account minted by
// CreateServiceAccount (<prefix>-sa user, <prefix>-sa-group group, <prefix>-sa
// policy). Only removed when the delete flag opts in. Best-effort.
func (i *IAM) DeleteCICDServiceAccount(cloudPrefix string) {
	username := fmt.Sprintf("%s-sa", cloudPrefix)
	i.deleteUserByName(username)
	i.deleteGroupByName(fmt.Sprintf("%s-group", username))
	i.deletePolicyByName(username)
}

// deleteUserByName fully removes the named user: OCI refuses to delete a user that
// still has Customer Secret Keys, auth tokens or group memberships, so those are
// purged first. A missing user is a no-op.
func (i *IAM) deleteUserByName(name string) {
	userId, err := i.findUser(name)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to look up IAM user %s: %s", name, err)))
		return
	}
	if userId == "" {
		return
	}
	i.purgeUserCredentials(userId, name)
	if _, err = i.client.DeleteUser(i.ctx, identity.DeleteUserRequest{UserId: &userId}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM user %s: %s", name, err)))
		return
	}
	log.Printf("Deleted IAM user %s\n", name)
}

// purgeUserCredentials removes everything that blocks a user deletion: its Customer
// Secret Keys, auth tokens and group memberships. Best-effort per item.
func (i *IAM) purgeUserCredentials(userId, name string) {
	keys, err := i.listCustomerSecretKeyIds(userId, "")
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to list customer secret keys of %s: %s", name, err)))
	}
	for id := range keys {
		if err = i.deleteCustomerSecretKey(userId, id); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete customer secret key %s: %s", id, err)))
		}
	}
	tokens, err := i.client.ListAuthTokens(i.ctx, identity.ListAuthTokensRequest{UserId: &userId})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to list auth tokens of %s: %s", name, err)))
	} else {
		for _, token := range tokens.Items {
			if token.Id == nil {
				continue
			}
			if err = i.deleteAuthToken(userId, *token.Id); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete auth token %s: %s", *token.Id, err)))
			}
		}
	}
	memberships, err := i.client.ListUserGroupMemberships(i.ctx, identity.ListUserGroupMembershipsRequest{
		CompartmentId: &i.tenancyId,
		UserId:        &userId,
	})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to list group memberships of %s: %s", name, err)))
		return
	}
	for _, membership := range memberships.Items {
		if membership.Id == nil {
			continue
		}
		if _, err = i.client.RemoveUserFromGroup(i.ctx, identity.RemoveUserFromGroupRequest{
			UserGroupMembershipId: membership.Id,
		}); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to remove %s from a group: %s", name, err)))
		}
	}
}

func (i *IAM) findUser(name string) (string, error) {
	list, err := i.client.ListUsers(i.ctx, identity.ListUsersRequest{CompartmentId: &i.tenancyId, Name: &name})
	if err != nil {
		return "", fmt.Errorf("failed to list users: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, nil
	}
	return "", nil
}

func (i *IAM) deleteGroupByName(name string) {
	list, err := i.client.ListGroups(i.ctx, identity.ListGroupsRequest{CompartmentId: &i.tenancyId, Name: &name})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to look up IAM group %s: %s", name, err)))
		return
	}
	if len(list.Items) == 0 {
		return
	}
	if _, err = i.client.DeleteGroup(i.ctx, identity.DeleteGroupRequest{GroupId: list.Items[0].Id}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM group %s: %s", name, err)))
		return
	}
	log.Printf("Deleted IAM group %s\n", name)
}

// deletePolicyByName removes the compartment-attached policy. A missing one is a no-op.
func (i *IAM) deletePolicyByName(name string) {
	list, err := i.client.ListPolicies(i.ctx, identity.ListPoliciesRequest{CompartmentId: &i.compartmentId, Name: &name})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to look up IAM policy %s: %s", name, err)))
		return
	}
	if len(list.Items) == 0 {
		return
	}
	if _, err = i.client.DeletePolicy(i.ctx, identity.DeletePolicyRequest{PolicyId: list.Items[0].Id}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM policy %s: %s", name, err)))
		return
	}
	log.Printf("Deleted IAM policy %s\n", name)
}

// cicdServiceAccountStatements returns the least-privilege policy for the external CI/CD
// service account minted by CreateServiceAccount. It grants exactly what a steady-state
// run mutates or reads, and NONE of the bootstrap's privileges: no policy management (so it
// cannot widen its own access the way agentAccessStatements does) and no KMS/vault/bucket
// creation (the trust root already exists, so it only finds+uses it). Everything is
// compartment-scoped. The terraform-state S3 traffic is signed with the Vault-persisted
// Customer Secret Key of whoever bootstrapped; the object-storage grants cover it either way,
// since this SA can seed a key of its own (self-credential management needs no policy).
func cicdServiceAccountStatements(group, compartmentId, bucketName string) []string {
	return []string{
		// DevOps: create/update/delete the build & deployment pipelines and push the
		// hosted build-spec repo content that config changes drive, and trigger build
		// runs / deployments. This is the "dynamically changed through config" surface.
		fmt.Sprintf("Allow group %s to manage devops-family in compartment id %s", group, compartmentId),
		// Vault secrets: read the bootstrapped CSK / git token and upsert per-source,
		// wrapper and custom secrets. `manage` covers create/update/read-bundle/delete;
		// the family aggregates secrets + secret-versions + secret-bundles.
		fmt.Sprintf("Allow group %s to manage secret-family in compartment id %s", group, compartmentId),
		// Find the agent-owned vault + key by name (KMS.Ensure's find path) and use the
		// key — never manage it, so rotation/scheduling deletion stay with the admin.
		fmt.Sprintf("Allow group %s to read vaults in compartment id %s", group, compartmentId),
		fmt.Sprintf("Allow group %s to use keys in compartment id %s", group, compartmentId),
		// Object Storage: find the state bucket and read/write its objects
		// (terraform-output.json + custom params via this identity; state itself via CSK).
		fmt.Sprintf("Allow group %s to read buckets in compartment id %s", group, compartmentId),
		fmt.Sprintf("Allow group %s to manage objects in compartment id %s where target.bucket.name='%s'", group, compartmentId, bucketName),
		// Logging: find and search the DevOps service log to parse terraform plan changes.
		fmt.Sprintf("Allow group %s to read log-groups in compartment id %s", group, compartmentId),
		fmt.Sprintf("Allow group %s to read log-content in compartment id %s", group, compartmentId),
		// Notifications: publish manual-approval messages to the approvals topic.
		fmt.Sprintf("Allow group %s to use ons-topics in compartment id %s", group, compartmentId),
	}
}

// apiKeyCredentials is an OCI API signing key pair: the PEM private key the caller must
// keep, plus the fingerprint OCI assigned its uploaded public half. API keys never expire.
type apiKeyCredentials struct {
	PrivateKeyPEM string
	Fingerprint   string
}

// EnsureApiKey generates a fresh RSA-2048 API signing key, uploads its public half to the
// user and returns the PEM private key + OCI-assigned fingerprint. When rotate is set it
// first deletes the user's existing API keys (OCI caps them at 3/user). OCI stores only
// the public half, so the caller MUST surface the private key — it can't be retrieved again.
func (i *IAM) EnsureApiKey(userId string, rotate bool) (apiKeyCredentials, error) {
	if rotate {
		if err := i.deleteApiKeys(userId); err != nil {
			return apiKeyCredentials{}, err
		}
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return apiKeyCredentials{}, fmt.Errorf("failed to generate api signing key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return apiKeyCredentials{}, fmt.Errorf("failed to encode api public key: %w", err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	response, err := i.client.UploadApiKey(i.ctx, identity.UploadApiKeyRequest{
		UserId:              &userId,
		CreateApiKeyDetails: identity.CreateApiKeyDetails{Key: &publicPEM},
	})
	if err != nil {
		return apiKeyCredentials{}, fmt.Errorf("failed to upload api signing key: %w", err)
	}
	if response.Fingerprint == nil {
		return apiKeyCredentials{}, fmt.Errorf("api key response missing fingerprint")
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}))
	return apiKeyCredentials{PrivateKeyPEM: privatePEM, Fingerprint: *response.Fingerprint}, nil
}

// deleteApiKeys removes every API signing key on the user. A delete failure is surfaced
// (not best-effort) so it can't silently strand the 3-key limit.
func (i *IAM) deleteApiKeys(userId string) error {
	response, err := i.client.ListApiKeys(i.ctx, identity.ListApiKeysRequest{UserId: &userId})
	if err != nil {
		return fmt.Errorf("failed to list api keys: %w", err)
	}
	for _, key := range response.Items {
		if key.Fingerprint == nil {
			continue
		}
		if _, err = i.client.DeleteApiKey(i.ctx, identity.DeleteApiKeyRequest{
			UserId:      &userId,
			Fingerprint: key.Fingerprint,
		}); err != nil {
			return fmt.Errorf("failed to delete api key %s: %w", *key.Fingerprint, err)
		}
	}
	return nil
}
