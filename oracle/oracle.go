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

// agentGitAuth carries the DevOps build-spec push credentials for the agent service
// account, fed to the builder via DevOpsBuilder.SetGitAuth. Both strings are empty on a
// consume run that never bootstrapped them. fresh reports a just-created token that must
// propagate to the git endpoint before it authenticates.
type agentGitAuth struct {
	username string
	token    string
	fresh    bool
}

// provisionBackendCredentials resolves the agent service account's credentials: the
// S3-compatible Customer Secret Key for the terraform state backend (always) and, when
// needGit is set, the DevOps git auth token + username. Both belong to the agent's
// dedicated service account, not to whoever runs the agent. Two regimes, decided by
// whether the caller can reconcile the agent SA (has IAM user-management perms):
//   - Admin/seed-capable: resolve both through their Ensure* funcs, which reuse a valid
//     credential or recreate one deleted out of band (self-heal). Both propagate after
//     creation, so they're resolved concurrently to overlap the waits.
//   - Consume (CI/CD SA or in-container RP, Vault-read only): trust whatever is persisted
//     (probing the CSK to surface a revoked key); a missing credential can't be minted,
//     so it warns and falls back (env credentials for state; a loud pushSpec error for git).
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
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return agentGitAuth{}, err
	}
	// Reconcile the agent SA + its policy on every run with IAM perms (not just seeding)
	// so policy changes take effect without deleting the persisted credentials. On a
	// first-run seed in a non-home region this waits minutes on IAM replication, so
	// announce it. Best-effort: a Vault-read-only principal can't, and its credentials work.
	log.Println("Reconciling the agent service account (user, group and access policy)")
	saUserId := o.reconcileAgentServiceAccount(iam, resources.BucketName)

	// No IAM user-management perms: trust whatever is persisted — can't mint or self-heal.
	if saUserId == "" {
		return o.consumeCredentials(ctx, resources, cskAccess, cskSecret, git, needGit)
	}

	// Admin: resolve both credentials concurrently (each propagates asynchronously, so
	// overlap the waits). WithContext so a fast git-auth error isn't masked behind the
	// up-to-10-min CSK propagation wait.
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
// user-management perms. It cannot mint or self-heal, so a missing credential just warns;
// a persisted CSK is probed to surface a key revoked out of band.
func (o *oracleService) consumeCredentials(ctx context.Context, resources *Resources, cskAccess, cskSecret string, git agentGitAuth, needGit bool) (agentGitAuth, error) {
	if cskAccess != "" {
		if err := s3CredentialsUsable(ctx, resources.S3Endpoint, o.region, resources.BucketName, cskAccess, cskSecret); err != nil {
			return agentGitAuth{}, fmt.Errorf("persisted Customer Secret Key no longer authenticates to the s3-compatible endpoint: %w; "+
				"re-run the bootstrap with an admin (user-management) principal to reseed it", err)
		}
		resources.AccessKey, resources.SecretKey = cskAccess, cskSecret
	} else {
		slog.Warn(common.PrefixWarning("No persisted Customer Secret Key and could not provision the agent service " +
			"account; the terraform s3 backend will use AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from the environment. " +
			"Run the agent once as an admin to seed and persist one automatically."))
	}
	if needGit && (git.token == "" || git.username == "") {
		slog.Warn(common.PrefixWarning("No persisted DevOps git credentials and could not provision the agent service " +
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
// EnsureCustomerSecretKey reuses the persisted key or (re)creates one deleted out of
// band. A freshly created key must propagate before it's broadly usable, so wait for a
// stable streak; a reused key is validated with a single probe.
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
		// A reused key seeded by an earlier run interrupted mid-propagation can still be
		// inconsistent, so wait it out before declaring it broken.
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
// derived once (deterministic), and EnsureAuthToken reuses a live token or recreates one
// whose user was deleted out of band.
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
// push: `<tenancy-name>/<login>` (the tenancy NAME, not the object-storage namespace).
// The login is the agent SA user, whose name the agent picks, so only the tenancy name
// is looked up. Identity-domain tenancies need `<tenancy>/<domain>/<login>` — change the
// derivation here if that's ever required.
func (o *oracleService) deriveGitUsername(iam *IAM) (string, error) {
	tenancy, err := iam.TenancyName()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s-infralib-agent", tenancy, o.cloudPrefix), nil
}

// reconcileAgentServiceAccount ensures the agent SA user + group + resource-scoped
// policy and returns its user OCID, or "" when the principal lacks IAM user-management
// perms (best-effort so a consume/Vault-only run isn't blocked). On an admin run it
// re-applies the current policy statements, so scoping them in code takes effect.
func (o *oracleService) reconcileAgentServiceAccount(iam *IAM, bucketName string) string {
	saUserId, err := iam.EnsureAgentServiceAccount(o.cloudPrefix, bucketName, repositoryName(o.cloudPrefix))
	if err != nil {
		log.Printf("Skipping agent service account reconcile (no IAM permissions — expected on a "+
			"non-admin run); using the already-persisted credentials (%s)\n", errSummary(err))
		return ""
	}
	return saUserId
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
// group, the agent's IAM scaffolding, the state bucket and — last, because it encrypts
// the bucket — the agent-owned KMS vault/key. The KMS vault/key/secrets have no hard
// delete: they are scheduled for deletion (~7 days, revertible in the console).
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
	// The agent's own IAM scaffolding (service account with its state Customer Secret
	// Key and DevOps auth token, group, policies).
	iam.DeleteAgentServiceAccount(o.cloudPrefix)
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
	return nil
}

// CreateServiceAccount provisions the external CI/CD service account a gitops engineer
// uses to run the agent from their pipeline AFTER an admin bootstrap. It gets an API
// signing key and a group/policy scoped to only what a steady-state run needs
// (cicdServiceAccountStatements) — NOT the bootstrap's identity-management or
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
	if err = iam.ensurePolicy(username, "Entigo infralib CI/CD policy", statements); err != nil {
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
	// this SA; it never expires. The state-backend CSK belongs to the agent's OWN service
	// account and is read from the Vault at run time, so this SA needs none.
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
