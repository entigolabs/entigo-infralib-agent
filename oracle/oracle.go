package oracle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"golang.org/x/sync/errgroup"
)

type oracleService struct {
	ctx           context.Context
	cloudPrefix   string
	compartmentId string
	region        string
	provider      ocicommon.ConfigurationProvider
	pipeline      common.Pipeline
	skipDelay     bool
}

type Resources struct {
	model.CloudResources
	Namespace  string
	S3Endpoint string
	AccessKey  string
	SecretKey  string
}

// GetBackendConfigVars emits the flat part of the terraform s3 backend config
// (backend.conf). Endpoint, region and credentials go via env (GetBackendEnv) since
// backend.conf can't express the nested `endpoints` block and must not carry secrets.
// The skip_* flags disable AWS calls OCI's S3-compatible API doesn't implement.
func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"bucket":                      r.BucketName,
		"key":                         key,
		"region":                      r.Region,
		"use_path_style":              "true",
		"skip_region_validation":      "true",
		"skip_credentials_validation": "true",
		"skip_metadata_api_check":     "true",
		"skip_requesting_account_id":  "true",
		"skip_s3_checksum":            "true",
	}
}

// GetBackendEnv supplies the terraform s3 backend endpoint and region via env. The
// credentials are added when a Customer Secret Key has been provisioned
// (provisionBackendCredentials); otherwise they fall back to the operator's env.
func (r Resources) GetBackendEnv() map[string]string {
	env := map[string]string{
		"AWS_ENDPOINT_URL_S3": r.S3Endpoint,
		"AWS_REGION":          r.Region,
	}
	if r.AccessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = r.AccessKey
		env["AWS_SECRET_ACCESS_KEY"] = r.SecretKey
	}
	return env
}

func NewOracle(ctx context.Context, cloudPrefix string, oracle common.Oracle, pipeline common.Pipeline, skipBucketDelay bool) (model.CloudProvider, error) {
	provider, err := newConfigProvider()
	if err != nil {
		return nil, err
	}
	return &oracleService{
		ctx:           ctx,
		cloudPrefix:   cloudPrefix,
		compartmentId: oracle.CompartmentId,
		region:        oracle.Region,
		provider:      provider,
		pipeline:      pipeline,
		skipDelay:     skipBucketDelay,
	}, nil
}

// bucketResources builds the single bucket and the Resources shell. SSM is left nil
// here and wired once the KMS vault + key exist (the Vault-backed store needs them).
func (o *oracleService) bucketResources() (Resources, *Storage, error) {
	bucket := getBucketName(o.cloudPrefix, o.region)
	storage, err := NewStorage(o.ctx, o.provider, o.region, o.compartmentId, bucket)
	if err != nil {
		return Resources{}, nil, fmt.Errorf("failed to create object storage service: %w", err)
	}
	return Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.ORACLE,
			Bucket:       storage,
			BucketName:   bucket,
			CloudPrefix:  o.cloudPrefix,
			Region:       o.region,
			Account:      o.compartmentId,
		},
		Namespace:  storage.Namespace(),
		S3Endpoint: s3Endpoint(storage.Namespace(), o.region),
	}, storage, nil
}

// setupStore provisions (or loads) the agent-owned KMS vault + key — the trust root
// for the provider (the bucket and every secret are encrypted under the key) — and
// returns the Vault-backed SSM built on them.
func (o *oracleService) setupStore() (*KMS, *SSM, error) {
	kms, err := NewKMS(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create kms service: %w", err)
	}
	if err = kms.Ensure(); err != nil {
		return nil, nil, fmt.Errorf("failed to provision kms vault and key: %w", err)
	}
	ssm, err := NewSSM(o.ctx, o.provider, o.region, o.compartmentId, kms.VaultId(), kms.KeyId())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create secret store: %w", err)
	}
	return kms, ssm, nil
}

// resolveStore is the find-only (no creation) counterpart to setupStore, used by
// GetResources so destroy/delete/read flows never provision the KMS trust root. If the
// vault/key are absent the SSM still constructs but can only operate on existing secrets.
func (o *oracleService) resolveStore() (*SSM, error) {
	kms, err := NewKMS(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create kms service: %w", err)
	}
	found, err := kms.Resolve()
	if err != nil {
		return nil, fmt.Errorf("failed to resolve kms vault and key: %w", err)
	}
	if !found {
		slog.Warn(common.PrefixWarning("Agent KMS vault not found; secret store operations are limited to existing secrets"))
	}
	return NewSSM(o.ctx, o.provider, o.region, o.compartmentId, kms.VaultId(), kms.KeyId())
}

func (o *oracleService) SetupMinimalResources() (model.Resources, error) {
	if err := o.ensureAgentAccess(); err != nil {
		return nil, err
	}
	kms, ssm, err := o.setupStore()
	if err != nil {
		return nil, err
	}
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return nil, err
	}
	// Grant the Object Storage service principal use of the agent's KMS key for the
	// bucket CMK. The policy is compartment-attached, but a consume run (RP or CI/CD SA)
	// holds no policy-management grant and gets NotAuthorizedOrNotFound — the policy
	// already exists, so warn and continue (admin-vs-consume split, like
	// reconcileAgentServiceAccount). Other errors are real and propagate.
	if err = iam.EnsureObjectStorageKeyAccess(o.cloudPrefix, o.region, kms.KeyId()); err != nil {
		if !isNotAuthorized(err) {
			return nil, fmt.Errorf("failed to grant Object Storage access to the kms key: %w", err)
		}
		log.Printf("Skipping Object Storage KMS access policy reconcile (no IAM permissions — expected on a "+
			"non-admin run); using the existing bucket and policy (%s)\n", errSummary(err))
	}
	resources, storage, err := o.bucketResources()
	if err != nil {
		return nil, err
	}
	if err = storage.CreateBucket(kms, o.skipDelay); err != nil {
		return nil, fmt.Errorf("failed to create object storage bucket: %w", err)
	}
	resources.SSM = ssm
	return resources, nil
}

func (o *oracleService) SetupResources(manager model.NotificationManager, config model.Config) (model.Resources, error) {
	if err := o.ensureAgentAccess(); err != nil {
		return nil, err
	}
	resources, _, err := o.bucketResources()
	if err != nil {
		return nil, err
	}
	needGit := o.pipeline.Type != string(common.PipelineTypeLocal)
	if !needGit {
		// Local runs execute in-process and never push build specs — only KMS/SSM +
		// state-backend credentials are needed, no DevOps project.
		_, ssm, err := o.setupStore()
		if err != nil {
			return nil, err
		}
		resources.SSM = ssm
		if _, err = o.provisionBackendCredentials(o.ctx, &resources, ssm, false); err != nil {
			return nil, err
		}
		return resources, nil
	}

	logs, err := o.ensureLogging()
	if err != nil {
		return nil, err
	}
	// The state/secret credentials and the DevOps project+repo+IAM are independent, so
	// resolve them concurrently: on a first-run seed the DevOps creation hides behind the
	// CSK propagation wait. WithContext so either failing cancels the other.
	var ssm *SSM
	var git agentGitAuth
	var build *DevOpsBuilder
	group, gctx := errgroup.WithContext(o.ctx)
	group.Go(func() error {
		var err error
		if _, ssm, err = o.setupStore(); err != nil {
			return err
		}
		resources.SSM = ssm
		log.Println("Provisioning terraform state backend credentials")
		git, err = o.provisionBackendCredentials(gctx, &resources, ssm, true)
		return err
	})
	group.Go(func() error {
		var err error
		log.Println("Setting up DevOps build project, repository and service log")
		build, err = o.setupDevOpsBuild(logs)
		return err
	})
	if err = group.Wait(); err != nil {
		return nil, err
	}

	builder := NewBuilder(o.ctx, ssm, o.region, o.compartmentId, resources.BucketName,
		resources.S3Endpoint, resources.AccessKey, resources.SecretKey, config.IsOpenTofuEnabled(),
		o.terraformCacheEnabled(), o.cloudPrefix)
	builder.devopsBuild = build
	// Inject the git push credentials resolved above so pushSpec does no Vault/IAM calls.
	build.SetGitAuth(git.username, git.token, git.fresh)
	gate, err := NewGate(o.ctx, o.provider, o.region, o.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create approval gate: %w", err)
	}
	// UseProject before Ensure so approval and build pipelines co-locate in one project.
	gate.UseProject(build.ProjectId())
	if err = gate.Ensure(); err != nil {
		return nil, fmt.Errorf("failed to set up approval gate: %w", err)
	}
	resources.CodeBuild = builder
	resources.Pipeline = NewPipeline(o.ctx, builder, gate, logs, resources.Bucket, o.cloudPrefix, manager)
	o.warnScheduleUnsupported(config.Schedule)
	return resources, nil
}

// setupDevOpsBuild provisions the shared <prefix>-infralib project (build pipelines +
// hosted build-spec repo + notification topic), grants the build pipelines' resource
// principal access, and enables the project's service log.
func (o *oracleService) setupDevOpsBuild(logs *Logging) (*DevOpsBuilder, error) {
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return nil, err
	}
	build, err := NewDevOpsBuilder(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		return nil, err
	}
	if err = build.Ensure(); err != nil {
		return nil, err
	}
	// Like EnsureObjectStorageKeyAccess, a consume run without policy management gets
	// NotAuthorizedOrNotFound and warns rather than failing.
	if err = iam.EnsureDevOpsBuildAccess(o.cloudPrefix); err != nil {
		if !isNotAuthorized(err) {
			return nil, err
		}
		log.Printf("Skipping DevOps build access policy reconcile (no IAM permissions — expected on a "+
			"non-admin run); using the existing policy (%s)\n", errSummary(err))
	}
	if logs != nil {
		if err = logs.EnsureDevOpsBuildLog(build.ProjectId()); err != nil {
			return nil, err
		}
	}
	return build, nil
}

// agentGroupEnv pins the IAM group the agent's access policy grants, for a compartment
// several identities drive. Unset, it grants the executing user by OCID.
const agentGroupEnv = "ORACLE_AGENT_GROUP"

const (
	// IAM is global and a policy change replicates for 5-10 min, per Oracle's troubleshooting
	// guidance — far longer than the ~1 min the console implies.
	policyPropagationTimeout  = 10 * time.Minute
	policyPropagationInterval = 15 * time.Second
)

// ensureAgentAccess writes the compartment policy covering the agent's own OCI calls, so a
// user granted nothing but `manage policies` there can run the deployment. Nothing to do for
// an in-container resource principal: it owns no user and devOpsBuildStatements covers it.
func (o *oracleService) ensureAgentAccess() error {
	grant := agentGrant{group: os.Getenv(agentGroupEnv), userId: o.userId()}
	if !grant.valid() {
		return nil
	}
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return err
	}
	changed, err := iam.EnsureAgentAccess(o.cloudPrefix, getBucketName(o.cloudPrefix, o.region), grant)
	if err != nil {
		// Like EnsureDevOpsBuildAccess: a run without policy management (the CI/CD service
		// account) relies on the policy a bootstrap wrote.
		if !isNotAuthorized(err) {
			return fmt.Errorf("failed to grant the agent access to compartment %s: %w", o.compartmentId, err)
		}
		log.Printf("Skipping agent access policy reconcile (no IAM permissions — expected on a non-admin run); "+
			"using the existing policy (%s)\n", errSummary(err))
		return nil
	}
	if changed {
		log.Println("Wrote the agent access policy, waiting for it to take effect")
		o.waitForAgentAccess()
	}
	return nil
}

// waitForAgentAccess polls until a just-written policy is in effect — until then every call it
// authorizes fails NotAuthorizedOrNotFound, failing a first bootstrap. A timeout is not fatal:
// carry on and let the real call report the real error. CAVEAT: the probe only proves THIS
// call is authorized. A principal with an overlapping grant already (the CI/CD SA has
// `read vaults`) passes at once, and the wait becomes a no-op.
func (o *oracleService) waitForAgentAccess() {
	kms, err := NewKMS(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		return
	}
	for start := time.Now(); ; {
		err = kms.authorized()
		if err == nil {
			return
		}
		if time.Since(start) > policyPropagationTimeout {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Agent access policy is still not in effect after %s (%s); continuing",
				policyPropagationTimeout, errSummary(err))))
			return
		}
		select {
		case <-o.ctx.Done():
			return
		case <-time.After(policyPropagationInterval):
		}
	}
}

// ensureLogging returns the Logging service the pipeline reads plan output back from.
// The DevOps service log it searches is provisioned later, by setupDevOpsBuild →
// EnsureDevOpsBuildLog (which needs the build project id).
func (o *oracleService) ensureLogging() (*Logging, error) {
	logs, err := NewLogging(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create logging service: %w", err)
	}
	return logs, nil
}

func (o *oracleService) terraformCacheEnabled() bool {
	return o.pipeline.TerraformCache.Value != nil && *o.pipeline.TerraformCache.Value
}

// warnScheduleUnsupported reports that cron-scheduled updates are not yet wired for
// Oracle. OCI Resource Scheduler targets resource lifecycle actions, not the agent
// update job, so a proper trigger (Events+Functions or an OKE CronJob) is a follow-up.
func (o *oracleService) warnScheduleUnsupported(schedule model.Schedule) {
	if schedule.UpdateCron != "" {
		slog.Warn(common.PrefixWarning("Scheduled updates are not yet supported on Oracle Cloud; ignoring update cron"))
	}
}

// agentGitAuth carries the DevOps build-spec push credentials, fed to the builder via
// DevOpsBuilder.SetGitAuth. Both strings are empty on a run that never bootstrapped them
// and cannot (a resource principal owns no credentials). fresh reports a just-created token
// that must propagate to the git endpoint before it authenticates.
type agentGitAuth struct {
	username string
	token    string
	fresh    bool
}

// complete reports a usable pair — the token authenticates only against the username of
// the user it belongs to, so half of it is worthless.
func (a agentGitAuth) complete() bool {
	return a.username != "" && a.token != ""
}

// provisionBackendCredentials resolves the credentials the agent's own traffic needs: the
// S3-compatible Customer Secret Key for the terraform state backend (always) and, when
// needGit is set, the DevOps git auth token + username. They belong to the EXECUTING user —
// OCI creates users only in the tenancy root, and the agent creates nothing outside its
// compartment, so it mints no service account for them (the `sa` command is the sole
// exception, being explicitly about an external identity).
//
// Whatever the Vault already holds is used AS IS, without a single Identity call, so a
// steady-state run needs no IAM permissions. What's missing is created on the executing
// user and persisted. A credential that later stops working is NOT detected and replaced:
// the stale Vault secret has to be deleted to force a reseed. An in-container resource
// principal has no user to own credentials, so it can only consume.
func (o *oracleService) provisionBackendCredentials(ctx context.Context, resources *Resources, secrets secretPersistence, needGit bool) (agentGitAuth, error) {
	cskAccess, cskSecret, err := loadPersistedCustomerSecretKey(secrets)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Could not read persisted Customer Secret Key: %v", err)))
		return agentGitAuth{}, nil
	}
	git, err := o.loadPersistedGitAuth(secrets, needGit)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Could not read persisted DevOps git credentials: %v", err)))
	}
	needGitSeed := needGit && !git.complete()
	if cskAccess != "" && !needGitSeed {
		return git, o.usePersistedStateCredentials(ctx, resources, cskAccess, cskSecret)
	}
	userId := o.userId()
	if userId == "" {
		return o.consumeCredentials(ctx, resources, cskAccess, cskSecret, git, needGit)
	}
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return agentGitAuth{}, err
	}
	// A new credential of either kind propagates asynchronously, so seed them concurrently
	// to overlap the waits. WithContext so a fast git-auth error isn't masked behind the
	// up-to-10-min CSK propagation wait.
	group, gctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		if cskAccess != "" {
			return o.usePersistedStateCredentials(gctx, resources, cskAccess, cskSecret)
		}
		return o.seedStateCredentials(gctx, resources, secrets, iam, userId)
	})
	if needGitSeed {
		group.Go(func() error { return o.seedGitAuth(secrets, iam, userId, &git) })
	}
	if err = group.Wait(); err != nil {
		return agentGitAuth{}, err
	}
	return git, nil
}

// consumeCredentials handles a run with no user to own credentials — an in-container
// resource principal — which can only use what a prior run persisted. A missing credential
// just warns; minting one needs a user principal.
func (o *oracleService) consumeCredentials(ctx context.Context, resources *Resources, cskAccess, cskSecret string, git agentGitAuth, needGit bool) (agentGitAuth, error) {
	if cskAccess != "" {
		if err := o.usePersistedStateCredentials(ctx, resources, cskAccess, cskSecret); err != nil {
			return agentGitAuth{}, err
		}
	} else {
		slog.Warn(common.PrefixWarning("No persisted Customer Secret Key and no user to create one for (a resource " +
			"principal owns none); the terraform s3 backend will use AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from " +
			"the environment. Run the agent once with user credentials to seed and persist one automatically."))
	}
	if needGit && !git.complete() {
		slog.Warn(common.PrefixWarning("No persisted DevOps git credentials and no user to create them for; a " +
			"build-spec push would fail — run the agent once with user credentials to bootstrap them."))
	}
	return git, nil
}

// loadPersistedGitAuth reads the previously bootstrapped git token + username from the
// Vault. Returns the empty struct when needGit is false (destroy/local flows don't push).
func (o *oracleService) loadPersistedGitAuth(secrets secretPersistence, needGit bool) (agentGitAuth, error) {
	if !needGit {
		return agentGitAuth{}, nil
	}
	token, _, err := readPersistedSecret(secrets, devopsAuthTokenObject)
	if err != nil {
		return agentGitAuth{}, err
	}
	username, _, err := readPersistedSecret(secrets, gitUsernameObject)
	if err != nil {
		return agentGitAuth{}, err
	}
	// The override outranks the persisted value, which is the point of it — a username
	// whose form this tenancy rejects is otherwise stuck in the Vault.
	if override := os.Getenv(gitUsernameEnv); override != "" {
		username = override
	}
	return agentGitAuth{username: username, token: token}, nil
}

// seedStateCredentials mints the state-backend CSK on the executing user, the first time
// the Vault holds none. A new key must propagate before it's broadly usable, so wait for a
// stable streak of successful probes before handing it to terraform.
func (o *oracleService) seedStateCredentials(ctx context.Context, resources *Resources, secrets secretPersistence, iam *IAM, userId string) error {
	access, secret, err := CreateCustomerSecretKey(iam, secrets, userId, stateKeyName(o.cloudPrefix))
	if err != nil {
		return err
	}
	log.Println("Waiting for the new Customer Secret Key to propagate to the state backend (can take a few minutes)")
	if err = waitForS3Credentials(ctx, resources.S3Endpoint, o.region, resources.BucketName, access, secret); err != nil {
		return err
	}
	resources.AccessKey, resources.SecretKey = access, secret
	return nil
}

// usePersistedStateCredentials wires the Vault-persisted CSK into the backend env. It is
// probed first, and a failure is waited out — a key seeded by a run that died
// mid-propagation is still settling. A key that has genuinely stopped working is not
// replaced; the error says to delete the Vault secret, which reseeds on the next run.
func (o *oracleService) usePersistedStateCredentials(ctx context.Context, resources *Resources, access, secret string) error {
	if err := s3CredentialsUsable(ctx, resources.S3Endpoint, o.region, resources.BucketName, access, secret); err != nil {
		log.Println("Persisted Customer Secret Key not yet usable; waiting for it to propagate to the state backend")
		if err = waitForS3Credentials(ctx, resources.S3Endpoint, o.region, resources.BucketName, access, secret); err != nil {
			return fmt.Errorf("persisted Customer Secret Key no longer authenticates to the s3-compatible endpoint: %w; "+
				"delete the %q Vault secret to force a reseed", err, customerSecretKeyObject)
		}
	}
	resources.AccessKey, resources.SecretKey = access, secret
	return nil
}

// seedGitAuth mints the DevOps git credentials on the executing user. Token and username
// are seeded as a PAIR — the token authenticates only as its own user, so keeping a
// username left behind by a different user would 401.
func (o *oracleService) seedGitAuth(secrets secretPersistence, iam *IAM, userId string, git *agentGitAuth) error {
	username := git.username // an ORACLE_GIT_USERNAME override, if one is set
	if username == "" {
		derived, err := o.deriveGitUsername(iam, userId)
		if err != nil {
			return err
		}
		username = derived
	}
	log.Println("Provisioning DevOps git auth token for the build-spec push")
	token, err := iam.CreateAuthToken(secrets, userId, gitTokenDescription(o.cloudPrefix))
	if err != nil {
		return err
	}
	if err = secrets.PutSecret(gitUsernameObject, username); err != nil {
		return fmt.Errorf("failed to persist git username %q: %w", username, err)
	}
	*git = agentGitAuth{username: username, token: token, fresh: true}
	return nil
}

// gitUsernameEnv overrides the derived build-spec push username, for tenancies whose form
// the derivation below can't produce.
const gitUsernameEnv = "ORACLE_GIT_USERNAME"

// deriveGitUsername builds the OCI code-repository HTTPS username for the build-spec push:
// `<tenancy-name>/<login>` (the tenancy NAME, not the object-storage namespace). A user in a
// non-default identity domain instead needs `<tenancy>/<domain>/<login>`, and the domain
// can't be read off the Identity user, so that case sets ORACLE_GIT_USERNAME. Both lookups
// happen once — the result is persisted next to the token.
func (o *oracleService) deriveGitUsername(iam *IAM, userId string) (string, error) {
	tenancy, err := iam.TenancyName()
	if err != nil {
		return "", gitUsernameUnavailable("the tenancy name", err)
	}
	login, err := iam.Username(userId)
	if err != nil {
		return "", gitUsernameUnavailable("the user's login name", err)
	}
	return fmt.Sprintf("%s/%s", tenancy, login), nil
}

// gitUsernameUnavailable explains the way out when the derivation fails. Reading one's OWN
// user and tenancy needs no policy, so this is a genuinely blocked identity read, not the
// ordinary compartment-scoped operator.
func gitUsernameUnavailable(what string, err error) error {
	return fmt.Errorf("failed to read %s to derive the DevOps git push username: %w; reading your own user and "+
		"tenancy normally needs no policy at all, so set %s explicitly — \"<tenancy>/<login>\", or "+
		"\"<tenancy>/<domain>/<login>\" in a non-default identity domain, or \"<tenancy>/Federation/<login>\" for a "+
		"federated user (e.g. \"acme/oracleidentitycloudservice/alice@acme.com\")", what, err, gitUsernameEnv)
}

// userId returns the OCID of the authenticated user, or "" when there is none — an
// in-container resource principal, which owns no Customer Secret Key or auth token.
// API-key auth exposes the user directly; a session token (UPST) carries none in the config
// file, so it comes from the token's `sub` claim, which the SDK surfaces via KeyID().
func (o *oracleService) userId() string {
	if user, err := o.provider.UserOCID(); err == nil && user != "" {
		return user
	}
	keyID, err := o.provider.KeyID()
	if err != nil {
		return ""
	}
	token, ok := strings.CutPrefix(keyID, "ST$")
	if !ok {
		return ""
	}
	// A resource principal's token rides in KeyID the same way, but its subject is the
	// resource (a build run), not a user, so accept only a user OCID.
	if subject := subjectFromJWT(token); strings.HasPrefix(subject, "ocid1.user.") {
		return subject
	}
	return ""
}

// subjectFromJWT extracts the `sub` claim from an unverified JWT. The signature is not
// checked: the claim only names the user credentials get attached to, and the SDK still
// authenticates every API call with the token itself.
func subjectFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

// GetResources returns clients wired to the ALREADY-provisioned resources, for
// read-only, destroy and delete flows. Like AWS/GCloud it must NOT create or enable
// anything (that is SetupResources' job) — it only resolves the existing DevOps project
// by name so destroy executions can trigger its pipelines.
func (o *oracleService) GetResources() (model.Resources, error) {
	resources, _, err := o.bucketResources()
	if err != nil {
		return nil, err
	}
	ssm, err := o.resolveStore()
	if err != nil {
		return nil, err
	}
	resources.SSM = ssm
	logs, err := o.ensureLogging()
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Could not resolve logging: %s", err)))
	}
	builder := NewBuilder(o.ctx, ssm, o.region, o.compartmentId, resources.BucketName,
		resources.S3Endpoint, resources.AccessKey, resources.SecretKey, false, o.terraformCacheEnabled(), o.cloudPrefix)
	if build, err := NewDevOpsBuilder(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Could not create DevOps builder: %s", err)))
	} else {
		build.Resolve() // find-only: no project/log/IAM/git creation
		builder.devopsBuild = build
	}
	resources.CodeBuild = builder
	// No gate: destroy executions run with ApproveForce and never hit approval.
	resources.Pipeline = NewPipeline(o.ctx, builder, nil, logs, resources.Bucket, o.cloudPrefix, nil)
	return resources, nil
}

// PrepareDestroy resolves the state-backend Customer Secret Key so a local destroy can
// reach the s3-compatible backend — GetResources skips credential provisioning, so its
// resources carry no AccessKey and terraform destroy would fail "AWS_ACCESS_KEY_ID is not
// set". Returns a copy with the CSK populated (Resources is a value boxed in the
// interface, so it can't be mutated in place). needGit is false — destroy never pushes.
func (o *oracleService) PrepareDestroy(resources model.Resources) (model.Resources, error) {
	res := resources.(Resources)
	if _, err := o.provisionBackendCredentials(o.ctx, &res, resources.GetSSM(), false); err != nil {
		return resources, err
	}
	return res, nil
}

// DeleteResources tears down the provider-level resources the agent owns. Per-step build
// pipelines and the git-source/wrapper Vault secrets are already removed by the delete
// command executor (service/delete.go); this covers everything else: the shared DevOps
// project (cascading to its repo and pipelines), the approval topic, the service log
// group, the credentials the agent put on the executing user, the state bucket and — last, because it encrypts
// the bucket — the agent-owned KMS vault/key. The KMS vault/key/secrets have no hard
// delete: they are scheduled for deletion (~7 days, revertible in the console). The agent's own
// access policy outlives all of it unless deleteServiceAccount is set.
func (o *oracleService) DeleteResources(deleteBucket, deleteServiceAccount bool) error {
	resources, storage, err := o.bucketResources()
	if err != nil {
		return err
	}
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return err
	}
	// DevOps project (cascades to its build-spec repo, build pipelines and approval
	// deployment pipelines) plus the approval notification topic.
	if build, err := NewDevOpsBuilder(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create DevOps builder for teardown: %s", err)))
	} else {
		build.DeleteBuildResources()
	}
	// Service log + log group the agent reads plan output back from.
	if logs, err := NewLogging(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create logging service for teardown: %s", err)))
	} else {
		logs.Delete()
	}
	// The credentials the agent provisioned on the executing user (state Customer Secret
	// Key, DevOps auth token) and the build access policy.
	iam.DeleteAgentCredentials(o.cloudPrefix, o.userId())
	if deleteServiceAccount {
		iam.DeleteCICDServiceAccount(o.cloudPrefix)
	}
	// The KMS key encrypts the bucket, so schedule the vault for deletion only after the
	// bucket is gone; if the bucket is kept, keep the key too.
	if !deleteBucket {
		log.Printf("Terraform state bucket %s and the KMS vault/key that encrypts it will not be deleted; "+
			"delete the bucket and schedule the KMS vault deletion manually if needed\n", resources.BucketName)
		return nil
	}
	if err = storage.Delete(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete state bucket %s: %s", resources.BucketName, err)))
		slog.Warn(common.PrefixWarning("State bucket deletion failed, so the KMS vault/key that encrypts it is left " +
			"intact; schedule its deletion manually once the bucket is removed"))
		return nil
	}
	iam.deletePolicyByName(fmt.Sprintf("%s-infralib-kms", o.cloudPrefix))
	kms, err := NewKMS(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create kms service for teardown: %s", err)))
		return nil
	}
	if err = kms.ScheduleDeletion(); err != nil {
		slog.Warn(common.PrefixWarning(err.Error()))
	}
	// Dead last — everything above authorizes through it. Kept unless the service account flag
	// opts in, so a re-run of this best-effort teardown still has the access to finish.
	if deleteServiceAccount {
		iam.deletePolicyByName(fmt.Sprintf("%s-infralib-agent", o.cloudPrefix))
	}
	return nil
}

// CreateServiceAccount provisions the external CI/CD service account a gitops engineer
// uses to run the agent from their pipeline AFTER a bootstrap. It gets an API signing key
// and a group/policy scoped to only what a steady-state run needs
// (cicdServiceAccountStatements) — NOT the bootstrap's policy management or
// KMS/bucket-creation privileges. OCI has no impersonation, so TrustRole is ignored;
// the policy is reconciled every invocation, so re-running tightens it in place.
func (o *oracleService) CreateServiceAccount(saFlags common.ServiceAccount) error {
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return err
	}
	username := fmt.Sprintf("%s-sa", o.cloudPrefix)
	groupName := fmt.Sprintf("%s-group", username)
	userId, created, err := iam.getOrCreateUser(username, "Entigo infralib CI/CD service account")
	if err != nil {
		return err
	}
	groupId, err := iam.getOrCreateGroup(groupName, "Entigo infralib CI/CD group")
	if err != nil {
		return err
	}
	if err = iam.addUserToGroup(userId, groupId); err != nil {
		return err
	}
	statements := cicdServiceAccountStatements(groupName, o.compartmentId, getBucketName(o.cloudPrefix, o.region))
	if _, err = iam.ensurePolicy(username, "Entigo infralib CI/CD policy", statements); err != nil {
		return err
	}
	if !created && !saFlags.RotateCredentials {
		log.Printf("Service account %s already exists, use rotate-credentials flag to generate new credentials\n", username)
		return nil
	}
	tenancyId, err := o.provider.TenancyOCID()
	if err != nil {
		return fmt.Errorf("failed to resolve tenancy ocid: %w", err)
	}
	// An API signing key (not a CSK) authenticates the OCI SDK calls the agent makes as
	// this SA; it never expires. The state-backend CSK is read from the Vault at run time
	// (seeded by the bootstrap), so no CSK is minted here.
	key, err := iam.EnsureApiKey(userId, !created)
	if err != nil {
		return err
	}
	printServiceAccountCredentials(username, userId, tenancyId, o.region, key)
	return nil
}

// printServiceAccountCredentials writes ready-to-use OCI API signing key credentials to
// stdout: a ~/.oci/config block plus the PEM private key it references.
func printServiceAccountCredentials(username, userId, tenancyId, region string, key apiKeyCredentials) {
	keyFile := fmt.Sprintf("~/.oci/%s.pem", username)
	fmt.Printf(`Service account %s is ready. API signing key credentials (no expiry) follow.

1. Save the private key below to %s (chmod 600):
%s
2. Add this profile to ~/.oci/config (or your CI/CD's OCI config file):

[DEFAULT]
user=%s
fingerprint=%s
tenancy=%s
region=%s
key_file=%s

Run the agent with this config (default ~/.oci/config, or set OCI_CONFIG_FILE) to execute as the service account.
OCI_CONFIG_FILE environment variable only works if .oci/config doesn't exist and the value must be absolute path.
Generated credentials take a bit of time to propagate.
`, username, keyFile, key.PrivateKeyPEM, userId, key.Fingerprint, tenancyId, region, keyFile)
}

func (o *oracleService) AddEncryption(_ string, _ map[string]model.TFOutput) error {
	// No-op: Oracle owns its own KMS key (see KMS) and never consumes a module-provided
	// key, so there's no module encryption to wire in (and runner.setupEncryption is AWS-only).
	slog.Warn(common.PrefixWarning("Encryption is not yet supported for Oracle Cloud"))
	return nil
}

func (o *oracleService) IsRunningLocally() bool {
	return os.Getenv("OCI_BUILD_RUN_ID") == ""
}
