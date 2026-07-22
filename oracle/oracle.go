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

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
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
// read natively by the backend (AWS_ENDPOINT_URL_S3 / AWS_REGION). Credentials
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) come from the environment for now;
// Phase 3 will provision them from an OCI Customer Secret Key.
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

func (o *oracleService) bucketResources() (Resources, *Storage, *Storage, error) {
	bucket := getBucketName(o.cloudPrefix, o.region)
	storage, err := NewStorage(o.ctx, o.provider, o.region, o.compartmentId, bucket)
	if err != nil {
		return Resources{}, nil, nil, fmt.Errorf("failed to create object storage service: %w", err)
	}
	configStorage, err := NewStorage(o.ctx, o.provider, o.region, o.compartmentId, getConfigBucketName(o.cloudPrefix, o.region))
	if err != nil {
		return Resources{}, nil, nil, fmt.Errorf("failed to create config storage service: %w", err)
	}
	return Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.ORACLE,
			Bucket:       storage,
			SSM:          NewSSM(configStorage),
			BucketName:   bucket,
			CloudPrefix:  o.cloudPrefix,
			Region:       o.region,
			Account:      o.compartmentId,
		},
		Namespace:  storage.Namespace(),
		S3Endpoint: s3Endpoint(storage.Namespace(), o.region),
	}, storage, configStorage, nil
}

func (o *oracleService) SetupMinimalResources() (model.Resources, error) {
	resources, storage, _, err := o.bucketResources()
	if err != nil {
		return nil, err
	}
	if err = storage.CreateBucket(o.skipDelay); err != nil {
		return nil, fmt.Errorf("failed to create object storage bucket: %w", err)
	}
	return resources, nil
}

func (o *oracleService) SetupResources(manager model.NotificationManager, config model.Config) (model.Resources, error) {
	resources, storage, configStorage, err := o.bucketResources()
	if err != nil {
		return nil, err
	}
	if err = storage.CreateBucket(o.skipDelay); err != nil {
		return nil, fmt.Errorf("failed to create object storage bucket: %w", err)
	}
	if err = o.provisionBackendCredentials(&resources, configStorage); err != nil {
		return nil, err
	}
	if o.pipeline.Type == string(common.PipelineTypeLocal) {
		return resources, nil
	}
	logs, err := o.ensureLogging()
	if err != nil {
		return nil, err
	}
	builder := NewBuilder(o.ctx, configStorage, o.region, o.compartmentId, resources.BucketName,
		resources.S3Endpoint, resources.AccessKey, resources.SecretKey, config.IsOpenTofuEnabled(),
		o.terraformCacheEnabled(), o.cloudPrefix)
	gate, err := NewGate(o.ctx, o.provider, o.region, o.cloudPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create approval gate: %w", err)
	}
	// Must run before gate.Ensure: it points the gate at the shared <prefix>-infralib
	// project so approval and build pipelines co-locate.
	if err = o.attachDevOpsBuild(builder, gate, configStorage, logs); err != nil {
		return nil, fmt.Errorf("failed to set up DevOps build execution: %w", err)
	}
	if err = gate.Ensure(); err != nil {
		return nil, fmt.Errorf("failed to set up approval gate: %w", err)
	}
	resources.CodeBuild = builder
	resources.Pipeline = NewPipeline(o.ctx, builder, gate, logs, o.cloudPrefix, manager)
	o.warnScheduleUnsupported(config.Schedule)
	return resources, nil
}

// attachDevOpsBuild wires the DevOps build-pipeline execution backend onto the
// builder. It provisions the shared <prefix>-infralib project (build pipelines +
// hosted build-spec repo), grants the build pipeline's dynamic group access,
// enables the project's service logs, and — so approval and build pipelines
// share one project — points the gate at it. The gate may be nil
// (destroy/read-only flows), which skips the co-location.
func (o *oracleService) attachDevOpsBuild(builder *Builder, gate *Gate, configStorage *Storage, logs *Logging) error {
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return err
	}
	build, err := NewDevOpsBuilder(o.ctx, o.provider, iam, configStorage, o.region, o.compartmentId, o.cloudPrefix)
	if err != nil {
		return err
	}
	if err = build.Ensure(o.userId()); err != nil {
		return err
	}
	if err = iam.EnsureDevOpsBuildAccess(o.cloudPrefix); err != nil {
		return err
	}
	if logs != nil {
		if err = logs.EnsureDevOpsBuildLog(build.ProjectId()); err != nil {
			return err
		}
	}
	builder.devopsBuild = build
	if gate != nil {
		gate.UseProject(build.ProjectId())
	}
	return nil
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

// provisionBackendCredentials attaches S3-compatible credentials (an OCI Customer
// Secret Key) to the resources so the terraform s3 backend can authenticate. The
// key is user-scoped. When a user OCID is available (API-key or session-token
// auth) it is created on that user and persisted to the config bucket. Under
// resource principals (in-container) there is no user, so a CSK a prior local run
// persisted is reused; failing that, the operator supplies the credentials via env.
func (o *oracleService) provisionBackendCredentials(resources *Resources, configStorage *Storage) error {
	userId := o.userId()
	if userId == "" {
		accessKey, secretKey, err := loadPersistedCustomerSecretKey(configStorage)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("could not read persisted Customer Secret Key: %v", err)))
			return nil
		}
		if accessKey == "" {
			slog.Info(common.PrefixWarning("no user OCID and no persisted Customer Secret Key; the terraform s3 " +
				"backend will use AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from the environment. Run the agent " +
				"once with session-token or API-key auth to provision and persist one automatically."))
			return nil
		}
		// No user to regenerate with (resource principal in-container); fail fast
		// with a clear message rather than letting terraform hit SignatureDoesNotMatch.
		if err = s3CredentialsUsable(o.ctx, resources.S3Endpoint, o.region, resources.BucketName, accessKey, secretKey); err != nil {
			return fmt.Errorf("persisted Customer Secret Key no longer authenticates to the s3-compatible endpoint: %w; "+
				"re-run the agent locally with session-token or API-key auth to regenerate it", err)
		}
		resources.AccessKey = accessKey
		resources.SecretKey = secretKey
		return nil
	}
	if err := configStorage.CreateBucket(true); err != nil {
		return fmt.Errorf("failed to create config bucket: %w", err)
	}
	iam, err := NewIAM(o.ctx, o.provider, o.region, o.compartmentId)
	if err != nil {
		return err
	}
	accessKey, secretKey, created, err := EnsureCustomerSecretKey(iam, configStorage, userId,
		fmt.Sprintf("entigo-infralib-%s-state", o.cloudPrefix))
	if err != nil {
		return err
	}
	// A newly created key must propagate to the bucket region before it's broadly
	// usable (OCI does this asynchronously, slower cross-region) — wait for a stable
	// streak of successes. A reused key is normally already propagated, so try a
	// single quick probe first and only fall back to the full wait if it's not
	// (e.g. seeded by an earlier run that was interrupted mid-propagation). We never
	// regenerate here: a failing probe can't be told apart from propagation and would
	// reset the clock; a genuinely deleted key is recreated by EnsureCustomerSecretKey.
	if !created && s3CredentialsUsable(o.ctx, resources.S3Endpoint, o.region, resources.BucketName, accessKey, secretKey) == nil {
		resources.AccessKey = accessKey
		resources.SecretKey = secretKey
		return nil
	}
	if err = waitForS3Credentials(o.ctx, resources.S3Endpoint, o.region, resources.BucketName, accessKey, secretKey); err != nil {
		return err
	}
	resources.AccessKey = accessKey
	resources.SecretKey = secretKey
	return nil
}

// userId returns the OCID of the authenticated user, or "" when there is none
// (resource principals in-container). API-key auth exposes it directly; session
// tokens (UPST) carry no user in the config file, so the OCID is read from the
// token's `sub` claim, which the SDK surfaces via KeyID() as "ST$<jwt>".
func (o *oracleService) userId() string {
	if user, err := o.provider.UserOCID(); err == nil && user != "" {
		return user
	}
	if keyID, err := o.provider.KeyID(); err == nil {
		if token, ok := strings.CutPrefix(keyID, "ST$"); ok {
			return subjectFromJWT(token)
		}
	}
	return ""
}

// subjectFromJWT extracts the `sub` claim (the user OCID for an OCI session token)
// from an unverified JWT. The signature is not checked: the token only names the
// user a CSK is attached to, and the SDK still authenticates every API call with
// the token itself.
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
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Sub
}

func (o *oracleService) GetResources() (model.Resources, error) {
	resources, _, configStorage, err := o.bucketResources()
	if err != nil {
		return nil, err
	}
	// Best-effort so a destroy execution can authenticate to state; read-only
	// callers ignore the pipeline/builder.
	_ = o.provisionBackendCredentials(&resources, configStorage)
	logs, err := o.ensureLogging()
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("could not resolve logging: %s", err)))
	}
	builder := NewBuilder(o.ctx, configStorage, o.region, o.compartmentId, resources.BucketName,
		resources.S3Endpoint, resources.AccessKey, resources.SecretKey, false, o.terraformCacheEnabled(), o.cloudPrefix)
	// No gate: destroy executions run with ApproveForce and never hit approval.
	if err = o.attachDevOpsBuild(builder, nil, configStorage, logs); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("could not set up DevOps build execution: %s", err)))
	}
	resources.CodeBuild = builder
	resources.Pipeline = NewPipeline(o.ctx, builder, nil, logs, o.cloudPrefix, nil)
	return resources, nil
}

func (o *oracleService) DeleteResources(deleteBucket, deleteServiceAccount bool) error {
	resources, storage, configStorage, err := o.bucketResources()
	if err != nil {
		return err
	}
	if deleteServiceAccount {
		slog.Warn(common.PrefixWarning("Oracle IAM teardown is not automated; remove the service-account user, " +
			"group, policy, build-pipeline dynamic group, <prefix>-infralib devops project and notification topic manually"))
	}
	if !deleteBucket {
		log.Printf("Terraform state bucket %s will not be deleted, delete it manually if needed\n", resources.BucketName)
		return nil
	}
	if err = storage.Delete(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete state bucket %s: %s", resources.BucketName, err)))
	}
	if err = configStorage.Delete(); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete config bucket: %s", err)))
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
	// Not called for Oracle today (runner.setupEncryption is AWS-only); Phase 5.
	slog.Warn(common.PrefixWarning("Encryption is not yet supported for Oracle Cloud"))
	return nil
}

func (o *oracleService) IsRunningLocally() bool {
	return os.Getenv("OCI_RESOURCE_PRINCIPAL_VERSION") == ""
}
