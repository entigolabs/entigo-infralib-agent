package gcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"google.golang.org/api/option"
)

type gcloudService struct {
	ctx         context.Context
	cloudPrefix string
	projectId   string
	location    string
	zone        string
	resources   Resources
	pipeline    common.Pipeline
	skipDelay   bool
	options     []option.ClientOption
}

type Resources struct {
	model.CloudResources
	Logging *Logging
}

func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"prefix": key,
		"bucket": r.BucketName,
	}
}

func NewGCloud(ctx context.Context, cloudPrefix string, gCloud common.GCloud, pipeline common.Pipeline, skipBucketDelay bool) (model.CloudProvider, error) {
	options, err := getClientOptions(gCloud)
	if err != nil {
		return nil, err
	}
	return &gcloudService{
		ctx:         ctx,
		cloudPrefix: cloudPrefix,
		projectId:   gCloud.ProjectId,
		location:    gCloud.Location,
		zone:        gCloud.Zone,
		pipeline:    pipeline,
		skipDelay:   skipBucketDelay,
		options:     options,
	}, nil
}

func getClientOptions(gCloud common.GCloud) ([]option.ClientOption, error) {
	if gCloud.CredentialsJson == "" {
		return nil, nil
	}
	err := validateServiceAccountJSON([]byte(gCloud.CredentialsJson))
	if err != nil {
		return nil, fmt.Errorf("invalid service account JSON: %v", err)
	}
	return []option.ClientOption{
		option.WithAuthCredentialsJSON(option.ServiceAccount, []byte(gCloud.CredentialsJson)),
	}, nil
}

func validateServiceAccountJSON(credJSON []byte) error {
	var cred struct {
		Type     string `json:"type"`
		TokenURI string `json:"token_uri"`
	}
	if err := json.Unmarshal(credJSON, &cred); err != nil {
		return err
	}
	if cred.Type != string(option.ServiceAccount) {
		return fmt.Errorf("only service_account type is allowed, got %q", cred.Type)
	}
	if cred.TokenURI != "" && cred.TokenURI != "https://oauth2.googleapis.com/token" {
		return fmt.Errorf("invalid token_uri")
	}
	return nil
}

func (g *gcloudService) GetIdentifier() string {
	return fmt.Sprintf("prefix %s, Google project Id %s, location %s", g.cloudPrefix, g.projectId, g.location)
}

func (g *gcloudService) SetupMinimalResources() (model.Resources, error) {
	err := g.enableApiServices([]string{"secretmanager.googleapis.com"})
	if err != nil {
		return nil, err
	}
	bucket := g.getBucketName()
	storage, err := NewStorage(g.ctx, g.options, g.projectId, g.location, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage service: %s", err)
	}
	err = storage.CreateBucket(g.skipDelay)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage bucket: %s", err)
	}
	sm, err := NewSM(g.ctx, g.options, g.projectId)
	if err != nil {
		return nil, fmt.Errorf("failed to create secret manager: %s", err)
	}
	return Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			Bucket:       storage,
			SSM:          sm,
			BucketName:   bucket,
			CloudPrefix:  g.cloudPrefix,
			Region:       g.location,
			Account:      g.projectId,
		},
	}, nil
}

func (g *gcloudService) SetupResources(manager model.NotificationManager, config model.Config) (model.Resources, error) {
	// TODO Default clients use gRPC, connections must be closed before exiting
	err := g.enableApiServices([]string{"compute.googleapis.com", "cloudresourcemanager.googleapis.com",
		"secretmanager.googleapis.com", "run.googleapis.com", "container.googleapis.com", "dns.googleapis.com",
		"clouddeploy.googleapis.com", "certificatemanager.googleapis.com", "cloudscheduler.googleapis.com"})
	if err != nil {
		return nil, err
	}
	bucket := g.getBucketName()
	storage, err := NewStorage(g.ctx, g.options, g.projectId, g.location, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage service: %s", err)
	}
	err = storage.CreateBucket(g.skipDelay)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage bucket: %s", err)
	}
	sm, err := NewSM(g.ctx, g.options, g.projectId)
	if err != nil {
		return nil, fmt.Errorf("failed to create secret manager: %s", err)
	}
	g.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			Bucket:       storage,
			SSM:          sm,
			BucketName:   bucket,
			CloudPrefix:  g.cloudPrefix,
			Region:       g.location,
			Account:      g.projectId,
		},
	}
	if g.pipeline.Type == string(common.PipelineTypeLocal) {
		return g.resources, nil
	}

	logging, err := NewLogging(g.ctx, g.options, g.projectId, g.location)
	if err != nil {
		return nil, fmt.Errorf("failed to create logging client: %s", err)
	}
	err = g.createLogResources(logging, g.getLogBucketId(), "")
	if err != nil {
		return nil, err
	}
	g.resources.Logging = logging
	iam, err := NewIAM(g.ctx, g.options, g.projectId)
	if err != nil {
		return nil, fmt.Errorf("failed to create IAM service: %s", err)
	}
	serviceAccount, err := g.createServiceAccount(iam)
	if err != nil {
		return nil, err
	}
	builder, err := NewBuilder(g.ctx, g.options, g.projectId, g.location, g.zone, serviceAccount, *g.pipeline.TerraformCache.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to create builder: %s", err)
	}
	pipeline, err := NewPipeline(g.ctx, g.options, g.projectId, g.location, g.cloudPrefix, serviceAccount, storage, builder, logging, manager)
	if err != nil {
		return nil, fmt.Errorf("failed to create pipeline: %s", err)
	}
	g.resources.CodeBuild = builder
	g.resources.Pipeline = pipeline
	err = g.createSchedule(config.Schedule, serviceAccount)
	if err != nil {
		return nil, err
	}
	return g.resources, nil
}

func (g *gcloudService) GetResources() (model.Resources, error) {
	bucket := g.getBucketName()
	codeStorage, err := NewStorage(g.ctx, g.options, g.projectId, g.location, bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage service: %s", err)
	}
	builder, err := NewBuilder(g.ctx, g.options, g.projectId, g.location, g.zone, "", true)
	if err != nil {
		return nil, fmt.Errorf("failed to create builder: %s", err)
	}
	pipeline, err := NewPipeline(g.ctx, g.options, g.projectId, g.location, g.cloudPrefix, "", codeStorage, builder, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create pipeline: %s", err)
	}
	sm, err := NewSM(g.ctx, g.options, g.projectId)
	if err != nil {
		return nil, fmt.Errorf("failed to create secret manager: %s", err)
	}
	logging, err := NewLogging(g.ctx, g.options, g.projectId, g.location)
	if err != nil {
		return nil, fmt.Errorf("failed to create logging client: %s", err)
	}
	logging.logBucketId = g.resolveLogBucketId(logging)
	g.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.GCLOUD,
			Bucket:       codeStorage,
			CodeBuild:    builder,
			Pipeline:     pipeline,
			CloudPrefix:  g.cloudPrefix,
			BucketName:   bucket,
			SSM:          sm,
			Region:       g.location,
			Account:      g.projectId,
		},
		Logging: logging,
	}
	return g.resources, nil
}

func (g *gcloudService) DeleteResources(deleteBucket, deleteServiceAccount bool) error {
	scheduler, err := NewScheduler(g.ctx, g.options, g.projectId, g.location, g.cloudPrefix)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to create scheduler service: %s", err)))
	} else {
		err = scheduler.deleteUpdateSchedule()
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete update schedule: %s", err)))
		}
	}
	agentPrefix := model.GetAgentPrefix(g.cloudPrefix)
	agentJob := model.GetAgentProjectName(agentPrefix, common.RunCommand)
	err = g.resources.GetBuilder().(*Builder).deleteJob(agentJob)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent job %s: %s", agentJob, err)))
	}
	agentJob = model.GetAgentProjectName(agentPrefix, common.UpdateCommand)
	err = g.resources.GetBuilder().(*Builder).deleteJob(agentJob)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent job %s: %s", agentJob, err)))
	}
	err = g.resources.GetPipeline().(*Pipeline).deleteTargets()
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete pipeline targets: %s", err)))
	}
	g.resources.Logging.DeleteLogResources([]string{g.getLogBucketId(), g.getLogBucketKmsId()}, g.getLogSinkName(), g.getLogExclusionName())
	iam, err := NewIAM(g.ctx, g.options, g.projectId)
	if err != nil {
		return fmt.Errorf("failed to create IAM service: %s", err)
	}
	accountName := getAgentSAName(g.cloudPrefix, g.location)
	err = iam.DeleteServiceAccount(accountName)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete service account %s: %s", accountName, err)))
	}
	if deleteServiceAccount {
		g.DeleteServiceAccount(iam)
	}
	if !deleteBucket {
		log.Printf("Terraform state bucket %s will not be deleted, delete it manually if needed\n", g.resources.GetBucketName())
		return nil
	}
	err = g.resources.GetBucket().Delete()
	if err != nil {
		bucket := fmt.Sprintf("%s-%s", g.cloudPrefix, g.projectId)
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete storage bucket %s: %s", bucket, err)))
	}
	return nil
}

func (g *gcloudService) IsRunningLocally() bool {
	return os.Getenv("CLOUD_RUN_JOB") == ""
}

func (g *gcloudService) enableApiServices(services []string) error {
	apiUsage, err := NewApiUsage(g.ctx, g.options, g.projectId)
	if err != nil {
		return fmt.Errorf("failed to create API usage service: %s", err)
	}
	if len(services) == 1 {
		err = apiUsage.EnableService(services[0])
	} else {
		err = apiUsage.EnableServices(services)
	}
	if err != nil {
		return fmt.Errorf("failed to enable services: %s", err)
	}
	return nil
}

func getAgentSAName(cloudPrefix, location string) string {
	accountName := fmt.Sprintf("%s-agent-%s", cloudPrefix, location)
	if len(accountName) > 30 {
		accountName = accountName[:30]
	}
	return accountName
}

func (g *gcloudService) createServiceAccount(iam *IAM) (string, error) {
	accountName := getAgentSAName(g.cloudPrefix, g.location)
	account, created, err := iam.GetOrCreateServiceAccount(accountName, "Entigo infralib service account")
	if err != nil {
		return "", fmt.Errorf("failed to create service account: %s", err)
	}
	err = iam.AddRolesToServiceAccount(account.Name, []string{"roles/editor", "roles/iam.securityAdmin",
		"roles/iam.serviceAccountAdmin"})
	if err != nil {
		return "", fmt.Errorf("failed to add roles to service account: %s", err)
	}
	err = iam.AddRolesToProject(account.Name, []string{"roles/editor", "roles/iam.securityAdmin",
		"roles/iam.serviceAccountAdmin", "roles/container.admin", "roles/secretmanager.secretAccessor",
		"roles/iam.roleAdmin", "roles/servicenetworking.networksAdmin", "roles/logging.viewAccessor"})
	if err != nil {
		return "", fmt.Errorf("failed to add roles to project: %s", err)
	}
	if created {
		log.Println("Waiting 60 seconds for service account permissions to be applied...")
		time.Sleep(60 * time.Second)
	}
	nameParts := strings.Split(account.Name, "/")
	return nameParts[len(nameParts)-1], nil
}

func (g *gcloudService) createSchedule(schedule model.Schedule, serviceAccount string) error {
	scheduler, err := NewScheduler(g.ctx, g.options, g.projectId, g.location, g.cloudPrefix)
	if err != nil {
		return fmt.Errorf("failed to create scheduler service: %s", err)
	}
	updateSchedule, err := scheduler.getUpdateSchedule()
	if err != nil {
		if schedule.UpdateCron == "" {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to get schedule %s: %s",
				getScheduleName(g.cloudPrefix, common.UpdateCommand), err)))
			return nil
		}
		return err
	}
	if schedule.UpdateCron == "" {
		if updateSchedule != nil {
			return scheduler.deleteUpdateSchedule()
		}
		return nil
	}
	agentJob := model.GetAgentProjectName(model.GetAgentPrefix(g.cloudPrefix), common.UpdateCommand)
	if updateSchedule == nil {
		return scheduler.createUpdateSchedule(schedule.UpdateCron, agentJob, serviceAccount)
	}
	if updateSchedule.Schedule != schedule.UpdateCron {
		return scheduler.updateUpdateSchedule(schedule.UpdateCron, agentJob, serviceAccount)
	}
	return nil
}

func (g *gcloudService) createLogResources(logging *Logging, bucketId, kmsKeyName string) error {
	err := logging.CreateLogBucket(bucketId, kmsKeyName)
	if err != nil {
		return err
	}
	filter := fmt.Sprintf(`resource.type = "cloud_run_job" AND resource.labels.job_name =~ "^%s-"`, g.cloudPrefix)
	err = logging.CreateLogSink(g.getLogSinkName(), bucketId, filter)
	if err != nil {
		return err
	}
	return logging.CreateDefaultSinkExclusion(g.getLogExclusionName(), filter)
}

func (g *gcloudService) getLogBucketId() string {
	return fmt.Sprintf("%s-log", g.cloudPrefix)
}

func (g *gcloudService) getLogBucketKmsId() string {
	return fmt.Sprintf("%s-log-kms", g.cloudPrefix)
}

func (g *gcloudService) resolveLogBucketId(logging *Logging) string {
	kmsBucketId := g.getLogBucketKmsId()
	bucket, err := logging.getLogBucket(kmsBucketId)
	if err == nil && bucket != nil {
		return kmsBucketId
	}
	return g.getLogBucketId()
}

func (g *gcloudService) getLogSinkName() string {
	return fmt.Sprintf("%s-log-sink", g.cloudPrefix)
}

func (g *gcloudService) getLogExclusionName() string {
	return fmt.Sprintf("%s-log-exclusion", g.cloudPrefix)
}

func (g *gcloudService) getBucketName() string {
	return getBucketName(g.cloudPrefix, g.projectId, g.location)
}

func getBucketName(cloudPrefix, projectId, location string) string {
	return fmt.Sprintf("%s-%s-%s", cloudPrefix, projectId, location)
}

func getServiceAccountName(cloudPrefix, location string) string {
	return fmt.Sprintf("%s-sa-%s", cloudPrefix, location)
}

func (g *gcloudService) CreateServiceAccount() error {
	username := getServiceAccountName(g.cloudPrefix, g.location)
	if len(username) > 30 {
		return fmt.Errorf("service account name %s is too long, must be fewer than 30 characters", username)
	}
	iam, err := NewIAM(g.ctx, g.options, g.projectId)
	if err != nil {
		return fmt.Errorf("failed to create IAM service: %s", err)
	}
	secrets, err := NewSM(g.ctx, g.options, g.projectId)
	if err != nil {
		return fmt.Errorf("failed to create secret manager: %s", err)
	}
	apiUsage, err := NewApiUsage(g.ctx, g.options, g.projectId)
	if err != nil {
		return fmt.Errorf("failed to create API usage service: %s", err)
	}
	err = apiUsage.EnableService("secretmanager.googleapis.com")
	if err != nil {
		return fmt.Errorf("failed to enable services: %s", err)
	}
	account, created, err := iam.GetOrCreateServiceAccount(username, "Entigo infralib CI/CD service account")
	if err != nil {
		return fmt.Errorf("failed to create service account: %s", err)
	}
	roleName := strings.ReplaceAll(username, "-", "_")
	err = iam.GetOrCreateRole(roleName, "Entigo Infralib CI/CD", "Entigo Infralib CI/CD", ServiceAccountPermissions())
	if err != nil {
		return fmt.Errorf("failed to create custom IAM role: %s", err)
	}
	customRole := fmt.Sprintf("projects/%s/roles/%s", g.projectId, roleName)
	err = iam.AddRolesToProject(account.Name, []string{customRole})
	if err != nil {
		return fmt.Errorf("failed to add roles to project: %s", err)
	}

	keyParam := fmt.Sprintf("entigo-infralib-%s-key", username)
	if !created {
		_, err = secrets.GetParameter(keyParam)
		if err == nil {
			log.Printf("Service account secret %s stored in SM", keyParam)
			return nil
		}
	}
	time.Sleep(1 * time.Second) // Creating key immediately after account creation may fail with 404
	key, err := iam.CreateServiceAccountKey(account.Name)
	if err != nil {
		return fmt.Errorf("failed to create service account key: %v", err)
	}
	err = secrets.PutParameter(keyParam, key.PrivateKeyData)
	if err != nil {
		return fmt.Errorf("failed to create secret %s: %v", keyParam, err)
	}
	log.Printf("Service account secret %s stored in SM", keyParam)
	return nil
}

func (g *gcloudService) DeleteServiceAccount(iam *IAM) {
	username := getServiceAccountName(g.cloudPrefix, g.location)
	err := iam.DeleteServiceAccount(username)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete service account %s: %s", username, err)))
	}
	roleName := strings.ReplaceAll(username, "-", "_")
	err = iam.DeleteRole(roleName)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete custom IAM role %s: %s", roleName, err)))
	}
	keyParam := fmt.Sprintf("entigo-infralib-%s-key", username)
	err = g.resources.SSM.DeleteParameter(keyParam)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete secret %s: %s", keyParam, err)))
	}
}

func GetConfigEncryptionKey(moduleName string, outputs map[string]model.TFOutput) (string, error) {
	return util.GetOutputStringValue(outputs, fmt.Sprintf("%s__config_key_id", moduleName))
}

func (g *gcloudService) AddEncryption(moduleName string, outputs map[string]model.TFOutput) error {
	err := g.setupConfigEncryption(moduleName, outputs)
	if err != nil {
		return err
	}
	if g.pipeline.Type == string(common.PipelineTypeLocal) {
		return nil
	}
	return g.setupTelemetryEncryption(moduleName, outputs)
}

func (g *gcloudService) setupConfigEncryption(moduleName string, outputs map[string]model.TFOutput) error {
	keyName, err := GetConfigEncryptionKey(moduleName, outputs)
	if err != nil {
		return err
	}
	if keyName == "" {
		return nil
	}
	g.resources.GetSSM().(*sm).AddEncryptionKeyId(keyName)
	err = g.resources.GetBucket().(*GStorage).addEncryption(keyName)
	if err != nil {
		return fmt.Errorf("failed to add encryption to bucket: %v", err)
	}
	return nil
}

func (g *gcloudService) setupTelemetryEncryption(moduleName string, outputs map[string]model.TFOutput) error {
	keyName, err := util.GetOutputStringValue(outputs, fmt.Sprintf("%s__telemetry_key_id", moduleName))
	if err != nil {
		return err
	}
	if keyName == "" {
		return nil
	}
	logging := g.resources.Logging
	if logging == nil {
		slog.Warn(common.PrefixWarning("Logging not initialized, skipping telemetry encryption"))
		return nil
	}
	kmsBucketId := g.getLogBucketKmsId()
	bucket, err := logging.getLogBucket(kmsBucketId)
	if err != nil {
		return err
	}
	if bucket != nil && bucket.CmekSettings != nil && bucket.CmekSettings.KmsKeyName != "" {
		logging.logBucketId = kmsBucketId
		return nil
	}
	log.Printf("Creating CMEK log bucket %s\n", kmsBucketId)
	err = logging.CreateLogBucket(kmsBucketId, keyName)
	if err != nil {
		return fmt.Errorf("failed to create CMEK log bucket: %v", err)
	}
	return logging.UpdateLogSinkDestination(g.getLogSinkName(), kmsBucketId)
}
