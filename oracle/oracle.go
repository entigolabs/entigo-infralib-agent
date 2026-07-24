package oracle

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
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

// GetBackendConfigVars emits the flat (string-only) part of the terraform s3
// backend config written to backend.conf. The S3-compat endpoint, region and
// credentials are injected via env (GetBackendEnv + operator/Customer Secret Key
// env) because backend.conf cannot express the nested `endpoints` block and must
// not carry secrets. The skip_* flags disable AWS-specific calls that OCI's
// S3-compatible API does not implement.
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

// GetBackendEnv supplies the terraform s3 backend endpoint and region via env,
// read natively by the backend (AWS_ENDPOINT_URL_S3 / AWS_REGION). The credentials
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) are added when a Customer Secret Key
// has been provisioned (provisionBackendCredentials); otherwise they fall back to
// the operator's environment.
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

// bucketResources builds the single bucket (state + non-secret agent objects) and
// the Resources shell. SSM is left nil here and wired by setupStore once the KMS
// vault + key exist, since the Vault-backed store needs them.
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

// setupStore provisions (or loads) the agent-owned KMS vault + key and returns the
// Vault-backed SSM built on them. The vault + key are the trust root for the whole
// provider: the bucket is encrypted with the key and every secret lives in the
// vault under it.
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

// resolveStore returns the Vault-backed SSM built on the EXISTING vault + key,
// resolved find-only (no creation) — the read-only counterpart to setupStore, used
// by GetResources so destroy/delete/read flows never provision the KMS trust root.
// If the vault/key are absent the SSM still constructs but can only operate on what
// already exists.
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
		slog.Warn(common.PrefixWarning("agent KMS vault not found; secret store operations are limited to existing secrets"))
	}
	return NewSSM(o.ctx, o.provider, o.region, o.compartmentId, kms.VaultId(), kms.KeyId())
}

func (o *oracleService) SetupMinimalResources() (model.Resources, error) {
	kms, ssm, err := o.setupStore()
	if err != nil {
		return nil, err
	}
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return nil, err
	}
	if err = iam.EnsureObjectStorageKeyAccess(o.cloudPrefix, o.region, kms.KeyId()); err != nil {
		return nil, fmt.Errorf("failed to grant Object Storage access to the kms key: %w", err)
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
	// resolve them concurrently: on a first-run seed the DevOps creation (tens of seconds
	// of async work requests) hides behind the CSK propagation wait; steady-state saves the
	// two sets of list calls overlapping. WithContext so either failing cancels the other.
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
	resources.Pipeline = NewPipeline(o.ctx, builder, gate, logs, o.cloudPrefix, manager)
	o.warnScheduleUnsupported(config.Schedule)
	return resources, nil
}

// setupDevOpsBuild provisions the shared <prefix>-infralib project (build pipelines +
// hosted build-spec repo + notification topic), grants the build pipeline's dynamic
// group access, and enables the project's service log. Independent of the state/secret
// credentials, so SetupResources runs it concurrently with provisionBackendCredentials.
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
	if err = iam.EnsureDevOpsBuildAccess(o.cloudPrefix); err != nil {
		return nil, err
	}
	if logs != nil {
		if err = logs.EnsureDevOpsBuildLog(build.ProjectId()); err != nil {
			return nil, err
		}
	}
	return build, nil
}

// ensureLogging returns the Logging service the pipeline reads plan output back
// from. The DevOps service log it searches is provisioned later, by
// attachDevOpsBuild → EnsureDevOpsBuildLog (which needs the build project id).
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

// warnScheduleUnsupported reports that cron-scheduled updates are not yet wired
// for Oracle. OCI Resource Scheduler targets resource lifecycle actions, not
// "run the agent update job", so a proper trigger (Events+Functions or an OKE
// CronJob) is a follow-up.
func (o *oracleService) warnScheduleUnsupported(schedule model.Schedule) {
	if schedule.UpdateCron != "" {
		slog.Warn(common.PrefixWarning("scheduled updates are not yet supported on Oracle Cloud; ignoring update cron"))
	}
}

// agentGitAuth carries the DevOps build-spec push credentials resolved for the agent
// service account, fed to the builder via DevOpsBuilder.SetGitAuth. Both strings are
// empty on a consume run that never bootstrapped them (pushSpec then fails loudly
// only if a spec actually changed). fresh reports a just-created token that must
// propagate to the git endpoint before it authenticates.
type agentGitAuth struct {
	username string
	token    string
	fresh    bool
}

// provisionBackendCredentials resolves the agent service account's credentials: the
// S3-compatible Customer Secret Key for the terraform state backend (always) and,
// when needGit is set (a non-local setup that pushes build specs), the DevOps git
// auth token + username. Both belong to the agent's dedicated service account
// (EnsureAgentServiceAccount), not to whoever runs the agent.
//
// Two regimes, decided by whether the caller can reconcile the agent SA (has IAM
// user-management perms):
//   - Admin/seed-capable: resolve both credentials through their Ensure* funcs, which
//     REUSE a still-valid credential or RECREATE one whose SA user/key was deleted out
//     of band (self-heal). A missing CSK and a missing git token each propagate after
//     creation, so they're resolved concurrently — overlapping the waits.
//   - Consume (a CI/CD service account or in-container resource principal, Vault-read
//     only): trust whatever is persisted (probing the CSK to surface a revoked key);
//     a missing credential can't be minted, so it warns and falls back (env credentials
//     for the state backend; a loud pushSpec error for git).
func (o *oracleService) provisionBackendCredentials(ctx context.Context, resources *Resources, secrets secretPersistence, needGit bool) (agentGitAuth, error) {
	cskAccess, cskSecret, err := loadPersistedCustomerSecretKey(secrets)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("could not read persisted Customer Secret Key: %v", err)))
		return agentGitAuth{}, nil
	}
	git, err := o.loadPersistedGitAuth(secrets, needGit)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("could not read persisted DevOps git credentials: %v", err)))
	}
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return agentGitAuth{}, err
	}
	// Reconcile the agent SA + its resource-scoped policy on EVERY run that has the IAM
	// perms — not just when seeding — so changes to the policy statements take effect
	// without deleting the persisted credentials. Best-effort: a Vault-read-only
	// principal can't, and the persisted credentials still work. (SetupResources already
	// needs IAM here for EnsureDevOpsBuildAccess, so this adds no privilege requirement.)
	saUserId := o.reconcileAgentServiceAccount(iam, resources.BucketName)

	// No IAM user-management perms (a CI/CD SA or in-container resource principal): trust
	// whatever is persisted — can't mint or self-heal.
	if saUserId == "" {
		return o.consumeCredentials(ctx, resources, cskAccess, cskSecret, git, needGit)
	}

	// Admin/seed-capable: resolve both credentials through their Ensure* funcs, which REUSE
	// a still-valid credential or RECREATE one whose SA user/key was deleted out of band
	// (self-heal — e.g. the user was deleted while its Vault secrets remained). Run
	// concurrently: a freshly created CSK and auth token each propagate asynchronously, so
	// starting both clocks together overlaps the waits.
	// WithContext so a failure in either goroutine cancels the other — otherwise a fast
	// git-auth error would stay masked behind the up-to-10-min CSK propagation wait.
	group, gctx := errgroup.WithContext(ctx)
	group.Go(func() error { return o.ensureStateCredentials(gctx, resources, secrets, iam, saUserId) })
	if needGit {
		group.Go(func() error { return o.ensureGitAuth(secrets, iam, saUserId, &git) })
	}
	if err = group.Wait(); err != nil {
		return agentGitAuth{}, err
	}
	return git, nil
}

// consumeCredentials trusts the Vault-persisted credentials on a run without IAM
// user-management perms. It cannot mint or self-heal, so a missing credential just warns
// (env fallback for the state backend; a loud pushSpec error later for git) and a persisted
// CSK is probed to surface a key revoked out of band.
func (o *oracleService) consumeCredentials(ctx context.Context, resources *Resources, cskAccess, cskSecret string, git agentGitAuth, needGit bool) (agentGitAuth, error) {
	if cskAccess != "" {
		if err := s3CredentialsUsable(ctx, resources.S3Endpoint, o.region, resources.BucketName, cskAccess, cskSecret); err != nil {
			return agentGitAuth{}, fmt.Errorf("persisted Customer Secret Key no longer authenticates to the s3-compatible endpoint: %w; "+
				"re-run the bootstrap with an admin (user-management) principal to reseed it", err)
		}
		resources.AccessKey, resources.SecretKey = cskAccess, cskSecret
	} else {
		slog.Info(common.PrefixWarning("no persisted Customer Secret Key and could not provision the agent service " +
			"account; the terraform s3 backend will use AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from the environment. " +
			"Run the agent once as an admin to seed and persist one automatically."))
	}
	if needGit && (git.token == "" || git.username == "") {
		slog.Info(common.PrefixWarning("no persisted DevOps git credentials and could not provision the agent service " +
			"account; a build-spec push would fail — run the agent once as an admin to bootstrap them."))
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
	return agentGitAuth{username: username, token: token}, nil
}

// ensureStateCredentials resolves the state-backend CSK on an admin run.
// EnsureCustomerSecretKey reuses the persisted key when it's still active on the SA
// user, or (re)creates it when the key — or the whole user — was deleted out of band. A
// freshly created key must propagate to the bucket region before it's broadly usable
// (OCI does this asynchronously, slower cross-region), so we wait for a stable streak; a
// reused key is already propagated, so a single probe validates it and surfaces a break.
func (o *oracleService) ensureStateCredentials(ctx context.Context, resources *Resources, secrets secretPersistence, iam *IAM, saUserId string) error {
	access, secret, created, err := EnsureCustomerSecretKey(iam, secrets, saUserId, fmt.Sprintf("entigo-infralib-%s-state", o.cloudPrefix))
	if err != nil {
		return err
	}
	if created {
		log.Println("Waiting for the new Customer Secret Key to propagate to the state backend (can take a few minutes)")
		if err = waitForS3Credentials(ctx, resources.S3Endpoint, o.region, resources.BucketName, access, secret); err != nil {
			return err
		}
	} else if err = s3CredentialsUsable(ctx, resources.S3Endpoint, o.region, resources.BucketName, access, secret); err != nil {
		// A reused key is normally already propagated, but it may have been seeded by an
		// earlier run interrupted mid-propagation, so a single probe can fail on a key
		// that's simply not yet consistent. Wait it out before declaring the key broken.
		log.Println("Persisted Customer Secret Key not yet usable; waiting for it to propagate to the state backend")
		if err = waitForS3Credentials(ctx, resources.S3Endpoint, o.region, resources.BucketName, access, secret); err != nil {
			return fmt.Errorf("persisted Customer Secret Key no longer authenticates to the s3-compatible endpoint: %w; "+
				"delete the %q Vault secret to force a reseed", err, customerSecretKeyObject)
		}
	}
	resources.AccessKey, resources.SecretKey = access, secret
	return nil
}

// ensureGitAuth resolves the DevOps git credentials on an admin run: the username is
// derived once (or kept from the Vault — it's deterministic), and EnsureAuthToken reuses
// a live token or recreates one whose user was deleted out of band (self-heal).
func (o *oracleService) ensureGitAuth(secrets secretPersistence, iam *IAM, saUserId string, git *agentGitAuth) error {
	if git.username == "" {
		username, err := o.deriveGitUsername(iam)
		if err != nil {
			return err
		}
		if err = secrets.PutSecret(gitUsernameObject, username); err != nil {
			return fmt.Errorf("failed to persist git username %q: %w", username, err)
		}
		git.username = username
	}
	log.Println("Provisioning DevOps git auth token for the build-spec push")
	token, fresh, err := iam.EnsureAuthToken(secrets, saUserId, fmt.Sprintf("entigo-infralib-%s-devops", o.cloudPrefix))
	if err != nil {
		return err
	}
	git.token, git.fresh = token, fresh
	return nil
}

// deriveGitUsername builds the OCI code-repository HTTPS username for the build-spec
// push. OCI forms it as `<tenancy-name>/<login>` (the tenancy NAME, not the object-
// storage namespace — the two differ); the login is the agent service account user,
// whose name the agent picks (<prefix>-infralib-agent), so only the tenancy name needs
// a lookup. Identity-domain tenancies instead need `<tenancy>/<domain>/<login>`; if that
// ever needs supporting, change the derivation here rather than via an env override.
func (o *oracleService) deriveGitUsername(iam *IAM) (string, error) {
	tenancy, err := iam.TenancyName()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s-infralib-agent", tenancy, o.cloudPrefix), nil
}

// reconcileAgentServiceAccount ensures the agent SA user + group + resource-scoped
// policy and returns its user OCID, or "" when the principal lacks IAM
// user-management perms — best-effort so a consume/Vault-only run isn't blocked, but
// on an admin run it re-applies the current policy statements (so tightening/scoping
// them in code takes effect without deleting the persisted credentials).
func (o *oracleService) reconcileAgentServiceAccount(iam *IAM, bucketName string) string {
	saUserId, err := iam.EnsureAgentServiceAccount(o.cloudPrefix, bucketName, repositoryName(o.cloudPrefix))
	if err != nil {
		slog.Info(common.PrefixWarning(fmt.Sprintf("could not reconcile the agent service account policy (%v); "+
			"relying on already-persisted credentials", err)))
		return ""
	}
	return saUserId
}

// GetResources returns clients wired to the ALREADY-provisioned resources, for
// read-only, destroy and delete flows. Like the AWS/GCloud implementations it must
// NOT create or enable anything (that is SetupResources' job): a prior version wired
// in the full DevOps setup here, which wrongly re-enabled the build log the moment
// the deleter called GetResources. It only resolves the existing DevOps project (by
// name, no creation) so destroy executions can trigger its pipelines.
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
		slog.Warn(common.PrefixWarning(fmt.Sprintf("could not resolve logging: %s", err)))
	}
	builder := NewBuilder(o.ctx, ssm, o.region, o.compartmentId, resources.BucketName,
		resources.S3Endpoint, resources.AccessKey, resources.SecretKey, false, o.terraformCacheEnabled(), o.cloudPrefix)
	if build, err := NewDevOpsBuilder(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("could not create DevOps builder: %s", err)))
	} else {
		build.Resolve() // find-only: no project/log/IAM/git creation
		builder.devopsBuild = build
	}
	resources.CodeBuild = builder
	// No gate: destroy executions run with ApproveForce and never hit approval.
	resources.Pipeline = NewPipeline(o.ctx, builder, nil, logs, o.cloudPrefix, nil)
	return resources, nil
}

// PrepareDestroy resolves the state-backend Customer Secret Key so a local destroy
// execution can reach the s3-compatible backend — GetResources deliberately skips
// credential provisioning (it must not create/enable anything during teardown), so
// without this the resources it returns carry no AccessKey and terraform destroy
// fails "AWS_ACCESS_KEY_ID is not set". Returns a copy of resources with the CSK
// populated (the concrete Resources is a value boxed in the interface, so it can't
// be mutated in place). needGit is false — destroy never pushes build specs.
func (o *oracleService) PrepareDestroy(resources model.Resources) (model.Resources, error) {
	res := resources.(Resources)
	if _, err := o.provisionBackendCredentials(o.ctx, &res, resources.GetSSM(), false); err != nil {
		return resources, err
	}
	return res, nil
}

// DeleteResources tears down the provider-level resources the agent owns. Per-step
// build pipelines and the git-source/wrapper Vault secrets are already removed by
// the delete command executor (service/delete.go); this covers everything else:
// the shared DevOps project (cascading to its repo and pipelines), the approval
// topic, the service log group, the agent's IAM scaffolding, the state bucket and
// — last, because it encrypts the bucket — the agent-owned KMS vault/key.
//
// Only what OCI cannot remove via the SDK is left to the user: the KMS vault, key
// and Vault secrets have no hard delete (they are SCHEDULED for deletion at the
// earliest allowed time, ~7 days, and can be reverted in the console until then),
// and the bucket is kept when deleteBucket is false.
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
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to create DevOps builder for teardown: %s", err)))
	} else {
		build.DeleteBuildResources()
	}
	// Service log + log group the agent reads plan output back from.
	if logs, err := NewLogging(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to create logging service for teardown: %s", err)))
	} else {
		logs.Delete()
	}
	// The agent's own IAM scaffolding (service account with its state Customer Secret
	// Key and DevOps auth token, group, build-pipeline dynamic group, policies).
	iam.DeleteAgentServiceAccount(o.cloudPrefix)
	if deleteServiceAccount {
		iam.DeleteCICDServiceAccount(o.cloudPrefix)
	}
	// The KMS key encrypts the bucket, so it must outlive it: schedule the vault (and
	// thus the key + secrets) for deletion only after the bucket is gone. If the
	// bucket is kept, keep the key too.
	if !deleteBucket {
		log.Printf("Terraform state bucket %s and the KMS vault/key that encrypts it will not be deleted; "+
			"delete the bucket and schedule the KMS vault deletion manually if needed\n", resources.BucketName)
		return nil
	}
	if err = storage.Delete(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete state bucket %s: %s", resources.BucketName, err)))
		slog.Warn(common.PrefixWarning("state bucket deletion failed, so the KMS vault/key that encrypts it is left " +
			"intact; schedule its deletion manually once the bucket is removed"))
		return nil
	}
	iam.deletePolicyByName(fmt.Sprintf("%s-infralib-kms", o.cloudPrefix))
	kms, err := NewKMS(o.ctx, o.provider, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to create kms service for teardown: %s", err)))
		return nil
	}
	if err = kms.ScheduleDeletion(); err != nil {
		slog.Warn(common.PrefixWarning(err.Error()))
	}
	return nil
}

// CreateServiceAccount provisions a CI/CD user with an OCI Customer Secret Key
// (S3-compatible credentials for terraform state) and a group/policy granting it
// management of the compartment. OCI has no cross-account impersonation, so the
// TrustRole flag is not applicable; RotateCredentials replaces existing keys.
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
	statement := fmt.Sprintf("Allow group %s to manage all-resources in compartment id %s", groupName, o.compartmentId)
	if err = iam.ensurePolicy(username, "Entigo infralib CI/CD policy", []string{statement}); err != nil {
		return err
	}
	if !created && !saFlags.RotateCredentials {
		log.Printf("Service account %s already exists, use rotate-credentials flag to generate new credentials\n", username)
		return nil
	}
	if !created {
		if err = iam.rotateCustomerSecretKeys(userId); err != nil {
			return err
		}
	}
	accessKey, secretKey, err := iam.createCustomerSecretKey(userId, fmt.Sprintf("entigo-infralib-%s-sa", o.cloudPrefix))
	if err != nil {
		return err
	}
	fmt.Printf("Customer Secret Key credentials for service account %s:\nAWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n",
		username, accessKey, secretKey)
	return nil
}

func (o *oracleService) AddEncryption(_ string, _ map[string]model.TFOutput) error {
	// Intentional no-op: Oracle owns its own KMS key (see KMS) and never consumes a
	// module-provided key, so there is no module encryption to wire in. Not called
	// for Oracle today anyway (runner.setupEncryption is AWS-only).
	slog.Warn(common.PrefixWarning("Encryption is not yet supported for Oracle Cloud"))
	return nil
}

func (o *oracleService) IsRunningLocally() bool {
	return os.Getenv(auth.ResourcePrincipalVersionEnvVar) == ""
}
