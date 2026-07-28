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

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

// customerSecretKeyObject is the Vault secret name under which the provisioned
// S3-compatible credentials are persisted. The secret half is only returned by
// the Identity API at creation time, so it must be stored to survive restarts.
const customerSecretKeyObject = "oracle-customer-secret-key"

// devopsAuthTokenObject persists the OCI auth token used to git-push the build
// specs, in the Vault alongside the CSK. Same trust boundary.
const devopsAuthTokenObject = "oracle-devops-auth-token"

// gitUsernameObject persists the HTTPS basic-auth username that successfully pushed
// the build specs, so later runs need no Identity lookup to reconstruct the
// <tenancy>/<login> (or domain-qualified) form.
const gitUsernameObject = "oracle-git-username"

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
// The bool reports whether a NEW token was created — like a fresh Customer Secret
// Key, a new auth token propagates to the git endpoint asynchronously and is not
// usable for the first moments, so the caller retries the git push while it settles.
func (i *IAM) EnsureAuthToken(store secretPersistence, userId, description string) (string, bool, error) {
	existing, err := i.listAuthTokenIds(userId, description)
	if err != nil {
		return "", false, err
	}
	stored, found, err := readPersistedSecret(store, devopsAuthTokenObject)
	if err != nil {
		return "", false, err
	}
	if len(existing) > 0 && found {
		return stored, false, nil
	}
	// Drifted: delete the stale OCI-side token(s) so a fresh one can be created (an auth
	// token's value can't be updated, and the user is capped at 2). The persisted Vault
	// secret is NOT deleted — the PutSecret below overwrites it in place, so there's no
	// need to schedule its deletion (which would also linger for the OCI minimum window).
	for _, id := range existing {
		if err = i.deleteAuthToken(userId, id); err != nil {
			return "", false, fmt.Errorf("failed to delete stale auth token %s: %w", id, err)
		}
	}
	response, err := i.client.CreateAuthToken(i.ctx, identity.CreateAuthTokenRequest{
		UserId: &userId,
		CreateAuthTokenDetails: identity.CreateAuthTokenDetails{
			Description: &description,
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("failed to create auth token: %w", err)
	}
	if response.Token == nil {
		return "", false, fmt.Errorf("auth token response missing token value")
	}
	if err = store.PutSecret(devopsAuthTokenObject, *response.Token); err != nil {
		return "", false, fmt.Errorf("failed to persist auth token: %w", err)
	}
	log.Printf("Provisioned Oracle auth token %q for DevOps build-spec git push\n", description)
	return *response.Token, true, nil
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
	// Identity-domain (IDCS) tenancies reject user creation without a primary email
	// ("error.identity.user.primaryEmailNotSpecified"); legacy tenancies ignore it.
	// This is a machine service account that never receives mail — the entigo.com
	// address just marks the account as ours.
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

func (i *IAM) ensurePolicy(name, description string, statements []string) error {
	list, err := i.client.ListPolicies(i.ctx, identity.ListPoliciesRequest{
		CompartmentId: &i.tenancyId,
		Name:          &name,
	})
	if err != nil {
		return err
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

// EnsureSchedulerAccess grants the two principals in the cron chain
// (Resource Scheduler → Function → agent-update build run):
//   - the Resource Scheduler service principal to invoke the function
//     (request.principal.type='resourceschedule' → use fn-function), and
//   - the function (a dynamic group over fnfunc resources) to trigger the DevOps
//     build run (manage devops-family), using its resource principal.
//
// Both are scoped to the agent's compartment. Reconciled on every setup run.
func (i *IAM) EnsureSchedulerAccess(cloudPrefix string) error {
	dgName := fmt.Sprintf("%s-infralib-fn", cloudPrefix)
	matchingRule := fmt.Sprintf("ALL {resource.type='fnfunc', resource.compartment.id='%s'}", i.compartmentId)
	if err := i.ensureDynamicGroup(dgName, "Entigo infralib scheduler function", matchingRule); err != nil {
		return err
	}
	statements := []string{
		fmt.Sprintf("Allow any-user to use fn-function in compartment id %s where request.principal.type='resourceschedule'", i.compartmentId),
		fmt.Sprintf("Allow dynamic-group %s to manage devops-family in compartment id %s", dgName, i.compartmentId),
	}
	return i.ensurePolicy(fmt.Sprintf("%s-infralib-scheduler", cloudPrefix), "Entigo infralib scheduler access", statements)
}

// DeleteSchedulerAccess removes the scheduler policy and the function dynamic group
// created by EnsureSchedulerAccess. Best-effort.
func (i *IAM) DeleteSchedulerAccess(cloudPrefix string) {
	i.deletePolicyByName(fmt.Sprintf("%s-infralib-scheduler", cloudPrefix))
	i.deleteDynamicGroupByName(fmt.Sprintf("%s-infralib-fn", cloudPrefix))
}

// EnsureAgentServiceAccount find-or-creates the agent's own dedicated IAM user,
// its group and the policy granting the two things the agent's persisted
// credentials actually use: object-storage (the S3-compatible terraform-state
// bucket, signed with the Customer Secret Key) and DevOps code repositories (the
// build-spec git push, authenticated with the auth token). It returns the user
// OCID.
//
// NOTE the DevOps grant is `use devops-repository`, NOT `manage repos` — `repos` is
// the Container Registry (OCIR) resource-type, so it yields a git 401 "not
// authorized". `use` (not `manage`) is enough to push git content while denying the
// SA repo lifecycle actions (create/delete), keeping it least-privilege.
//
// This user is deliberately separate from the CI/CD service account the `sa`
// command mints (<prefix>-sa): rotating, leaking or deleting that externally-used
// SA never disturbs the agent's own state/git credentials. It is find-or-create and
// idempotent, and the caller (reconcileAgentServiceAccount) invokes it best-effort on
// every run that has IAM user-management perms, so tightening the policy statements in
// code re-applies without deleting the persisted credentials. A principal without
// those perms (a Vault-read-only consume run) gets an error the caller tolerates,
// relying on the already-persisted secrets.
func (i *IAM) EnsureAgentServiceAccount(cloudPrefix, bucketName, repoName string) (string, error) {
	name := fmt.Sprintf("%s-infralib-agent", cloudPrefix)
	userId, _, err := i.getOrCreateUser(name, "Entigo infralib agent service account (owns the terraform-state Customer Secret Key and the DevOps git auth token)")
	if err != nil {
		return "", err
	}
	groupId, err := i.getOrCreateGroup(name, "Entigo infralib agent service account group")
	if err != nil {
		return "", err
	}
	if err = i.addUserToGroup(userId, groupId); err != nil {
		return "", err
	}
	// Scoped to the exact bucket and repo the agent's credentials touch: `manage
	// objects` for terraform state, `inspect buckets` for the s3 backend's bucket-level
	// probes, and `use devops-repository` (git content push, not repo lifecycle) on the
	// single build-spec repo by name.
	statements := []string{
		fmt.Sprintf("Allow group %s to manage objects in compartment id %s where target.bucket.name='%s'", name, i.compartmentId, bucketName),
		fmt.Sprintf("Allow group %s to inspect buckets in compartment id %s", name, i.compartmentId),
		fmt.Sprintf("Allow group %s to use devops-repository in compartment id %s where target.repository.name='%s'", name, i.compartmentId, repoName),
	}
	if err = i.ensurePolicy(name, "Entigo infralib agent service account access", statements); err != nil {
		return "", err
	}
	return userId, nil
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

// DeleteAgentServiceAccount removes the agent's own IAM scaffolding, mirror image
// of EnsureAgentServiceAccount + EnsureDevOpsBuildAccess: the agent service account
// user (with its state Customer Secret Key and DevOps auth token), its group, the
// build-pipeline dynamic group, and the <prefix>-infralib-agent/<prefix>-infralib
// policies. The <prefix>-infralib-kms policy is deleted separately by the caller AFTER
// the bucket, since the Object Storage service principal needs it to keep the bucket's
// CMK usable until the bucket is gone. Best-effort — each step warns and continues so
// one stuck resource can't strand the rest.
func (i *IAM) DeleteAgentServiceAccount(cloudPrefix string) {
	name := fmt.Sprintf("%s-infralib-agent", cloudPrefix)
	i.deleteUserByName(name)
	i.deleteGroupByName(name)
	i.deletePolicyByName(name)
	i.deletePolicyByName(fmt.Sprintf("%s-infralib", cloudPrefix))
	i.deleteDynamicGroupByName(fmt.Sprintf("%s-infralib", cloudPrefix))
}

// DeleteCICDServiceAccount removes the external CI/CD service account minted by
// CreateServiceAccount (<prefix>-sa user, <prefix>-sa-group group, <prefix>-sa
// policy). Deliberately separate from the agent's own SA — only removed when the
// delete flag opts in. Best-effort.
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
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to look up IAM user %s: %s", name, err)))
		return
	}
	if userId == "" {
		return
	}
	i.purgeUserCredentials(userId, name)
	if _, err = i.client.DeleteUser(i.ctx, identity.DeleteUserRequest{UserId: &userId}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete IAM user %s: %s", name, err)))
		return
	}
	log.Printf("Deleted IAM user %s\n", name)
}

// purgeUserCredentials removes everything that blocks a user deletion: its Customer
// Secret Keys, auth tokens and group memberships. Best-effort per item.
func (i *IAM) purgeUserCredentials(userId, name string) {
	keys, err := i.listCustomerSecretKeyIds(userId)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list customer secret keys of %s: %s", name, err)))
	}
	for id := range keys {
		if err = i.deleteCustomerSecretKey(userId, id); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete customer secret key %s: %s", id, err)))
		}
	}
	tokens, err := i.client.ListAuthTokens(i.ctx, identity.ListAuthTokensRequest{UserId: &userId})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list auth tokens of %s: %s", name, err)))
	} else {
		for _, token := range tokens.Items {
			if token.Id == nil {
				continue
			}
			if err = i.deleteAuthToken(userId, *token.Id); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete auth token %s: %s", *token.Id, err)))
			}
		}
	}
	memberships, err := i.client.ListUserGroupMemberships(i.ctx, identity.ListUserGroupMembershipsRequest{
		CompartmentId: &i.tenancyId,
		UserId:        &userId,
	})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list group memberships of %s: %s", name, err)))
		return
	}
	for _, membership := range memberships.Items {
		if membership.Id == nil {
			continue
		}
		if _, err = i.client.RemoveUserFromGroup(i.ctx, identity.RemoveUserFromGroupRequest{
			UserGroupMembershipId: membership.Id,
		}); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to remove %s from a group: %s", name, err)))
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
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to look up IAM group %s: %s", name, err)))
		return
	}
	if len(list.Items) == 0 {
		return
	}
	if _, err = i.client.DeleteGroup(i.ctx, identity.DeleteGroupRequest{GroupId: list.Items[0].Id}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete IAM group %s: %s", name, err)))
		return
	}
	log.Printf("Deleted IAM group %s\n", name)
}

func (i *IAM) deletePolicyByName(name string) {
	list, err := i.client.ListPolicies(i.ctx, identity.ListPoliciesRequest{CompartmentId: &i.tenancyId, Name: &name})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to look up IAM policy %s: %s", name, err)))
		return
	}
	if len(list.Items) == 0 {
		return
	}
	if _, err = i.client.DeletePolicy(i.ctx, identity.DeletePolicyRequest{PolicyId: list.Items[0].Id}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete IAM policy %s: %s", name, err)))
		return
	}
	log.Printf("Deleted IAM policy %s\n", name)
}

func (i *IAM) deleteDynamicGroupByName(name string) {
	list, err := i.client.ListDynamicGroups(i.ctx, identity.ListDynamicGroupsRequest{CompartmentId: &i.tenancyId, Name: &name})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to look up dynamic group %s: %s", name, err)))
		return
	}
	if len(list.Items) == 0 {
		return
	}
	if _, err = i.client.DeleteDynamicGroup(i.ctx, identity.DeleteDynamicGroupRequest{DynamicGroupId: list.Items[0].Id}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete dynamic group %s: %s", name, err)))
		return
	}
	log.Printf("Deleted dynamic group %s\n", name)
}

// cicdServiceAccountStatements returns the least-privilege policy for the external
// CI/CD service account minted by CreateServiceAccount — the credentials a gitops
// engineer drops into their pipeline to run the agent (run/update) AFTER an admin
// bootstrap. It deliberately grants NONE of the bootstrap's own privileges:
//   - no identity management (users/groups/policies/dynamic-groups) — it can't mint
//     or rotate service accounts or widen its own access;
//   - no KMS/vault/bucket CREATION — the agent-owned trust root already exists, so the
//     SA only finds+uses it.
//
// It grants exactly what a steady-state run mutates or reads. Everything is
// compartment-scoped (the compartment is the OCI isolation boundary, the analogue of
// the AWS policy's per-prefix ARNs). The terraform-state S3 traffic is NOT covered
// here: it is signed with the agent SA's Customer Secret Key (decision #3), a separate
// identity; this SA only touches state indirectly via the object-storage grant below
// for terraform-output.json and custom-param objects.
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

// apiKeyCredentials is a ready-to-use OCI API signing key pair: the PEM private key
// the caller must keep, plus the fingerprint OCI assigned its uploaded public half.
// API keys never expire, so this is a durable credential for OCI SDK authentication.
type apiKeyCredentials struct {
	PrivateKeyPEM string
	Fingerprint   string
}

// EnsureApiKey generates a fresh RSA-2048 API signing key, uploads its public half to
// the user and returns the PEM private key + OCI-assigned fingerprint. When rotate is
// set it first deletes the user's existing API keys (OCI caps them at 3/user) so a
// re-run can't exhaust the limit. The private key is only ever held in memory here —
// OCI stores only the public half — so the caller MUST surface it to the user; it
// cannot be retrieved again.
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

// deleteApiKeys removes every API signing key on the user (identified by fingerprint,
// their unique key). Best-effort would strand the 3-key limit, so a delete failure is
// surfaced.
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
