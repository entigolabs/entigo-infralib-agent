package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log"
	"log/slog"
	"os"
	"time"
)

type awsService struct {
	ctx         context.Context
	awsConfig   aws.Config
	cloudPrefix string
	accountId   string
	resources   Resources
	pipeline    common.Pipeline
	skipDelay   bool
}

type Resources struct {
	model.CloudResources
	IAM           IAM
	DynamoDBTable string
	AccountId     string
	CloudWatch    CloudWatch
}

func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"key":            key,
		"bucket":         r.BucketName,
		"dynamodb_table": r.DynamoDBTable,
		"encrypt":        "true",
	}
}

func NewAWS(ctx context.Context, cloudPrefix string, awsFlags common.AWS, pipeline common.Pipeline, skipBucketDelay bool) model.CloudProvider {
	awsConfig := GetAWSConfig(ctx, awsFlags.RoleArn)
	accountId, err := getAccountId(awsConfig)
	if err != nil {
		log.Fatal(err.Error())
	}
	log.Printf("AWS account id: %s\n", accountId)
	return &awsService{
		ctx:         ctx,
		awsConfig:   awsConfig,
		cloudPrefix: cloudPrefix,
		accountId:   accountId,
		pipeline:    pipeline,
		skipDelay:   skipBucketDelay,
	}
}

func GetAWSConfig(ctx context.Context, roleArn string) aws.Config {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRetryer(func() aws.Retryer {
		return retry.AddWithMaxAttempts(retry.NewStandard(), 10)
	}))
	if err != nil {
		log.Fatalf("Failed to initialize AWS session: %s", err)
	}
	log.Printf("AWS session initialized with region: %s\n", cfg.Region)
	if roleArn != "" {
		return GetAssumedConfig(ctx, cfg, roleArn)
	}
	return cfg
}

func GetAssumedConfig(ctx context.Context, baseConfig aws.Config, roleArn string) aws.Config {
	stsClient := sts.NewFromConfig(baseConfig)
	assumedRole, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         aws.String(roleArn),
		RoleSessionName: aws.String("entigo-infralib-agent"),
		DurationSeconds: aws.Int32(3600),
	})
	if err != nil {
		log.Fatalf("Failed to assume role %s: %s", roleArn, err)
	}
	assumedConfig, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			*assumedRole.Credentials.AccessKeyId,
			*assumedRole.Credentials.SecretAccessKey,
			*assumedRole.Credentials.SessionToken,
		)),
		config.WithRegion(baseConfig.Region),
	)
	if err != nil {
		log.Fatalf("Failed to initialize assumed AWS session: %s", err)
	}
	return assumedConfig
}

func (a *awsService) SetupResources() model.Resources {
	bucket := a.getBucketName()
	s3, s3Arn := a.createBucket(bucket)
	dynamoDBTable := a.createDynamoDBTable()
	iam := NewIAM(a.ctx, a.awsConfig, a.accountId)
	iam.CreateServiceLinkedRole("autoscaling.amazonaws.com")

	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AWS,
			Bucket:       s3,
			SSM:          NewSSM(a.ctx, a.awsConfig),
			CloudPrefix:  a.cloudPrefix,
			BucketName:   bucket,
			Region:       a.awsConfig.Region,
		},
		DynamoDBTable: *dynamoDBTable.TableName,
		AccountId:     a.accountId,
		IAM:           iam,
	}
	if a.pipeline.Type == string(common.PipelineTypeLocal) {
		return a.resources
	}

	logGroup, logGroupArn, logStream, cloudwatch := a.createCloudWatchLogs()
	buildRoleArn, pipelineRoleArn := a.createIAMRoles(logGroupArn, s3Arn, *dynamoDBTable.TableArn)

	codeBuild := NewBuilder(a.ctx, a.awsConfig, buildRoleArn, logGroup, logStream, s3Arn, *a.pipeline.TerraformCache.Value)
	codePipeline := NewPipeline(a.ctx, a.awsConfig, pipelineRoleArn, cloudwatch, logGroup, logStream,
		*a.pipeline.TerraformCache.Value)
	a.resources.CloudWatch = cloudwatch
	a.resources.CodeBuild = codeBuild
	a.resources.Pipeline = codePipeline
	return a.resources
}

func (a *awsService) GetResources() model.Resources {
	bucket := a.getBucketName()
	cloudwatch := NewCloudWatch(a.ctx, a.awsConfig)
	logGroup := a.getLogGroup()
	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AWS,
			Bucket:       NewS3(a.ctx, a.awsConfig, bucket),
			CodeBuild:    NewBuilder(a.ctx, a.awsConfig, "", "", "", "", true),
			Pipeline:     NewPipeline(a.ctx, a.awsConfig, "", cloudwatch, logGroup, logGroup, true),
			CloudPrefix:  a.cloudPrefix,
			BucketName:   bucket,
			SSM:          NewSSM(a.ctx, a.awsConfig),
			Region:       a.awsConfig.Region,
		},
		IAM:        NewIAM(a.ctx, a.awsConfig, a.accountId),
		AccountId:  a.accountId,
		CloudWatch: cloudwatch,
	}
	return a.resources
}

func (a *awsService) DeleteResources(deleteBucket, deleteServiceAccount bool) {
	agentProjectName := fmt.Sprintf("%s-agent-%s", a.cloudPrefix, common.RunCommand)
	err := a.resources.GetPipeline().(*Pipeline).deletePipeline(agentProjectName)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent run pipeline: %s", err)))
	}
	err = a.resources.GetBuilder().DeleteProject(agentProjectName, model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent run project: %s", err)))
	}

	agentProjectName = fmt.Sprintf("%s-agent-%s", a.cloudPrefix, common.UpdateCommand)
	err = a.resources.GetPipeline().(*Pipeline).deletePipeline(agentProjectName)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent update pipeline: %s", err)))
	}
	err = a.resources.GetBuilder().DeleteProject(agentProjectName, model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete agent update project: %s", err)))
	}

	err = DeleteDynamoDBTable(a.ctx, a.awsConfig, fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId))
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete DynamoDB table: %s", err)))
	}
	a.deleteCloudWatchLogs()
	a.deleteIAMRoles()
	if deleteServiceAccount {
		a.DeleteServiceAccount()
	}
	if !deleteBucket {
		log.Printf("Terraform state bucket %s will not be deleted, delete it manually if needed\n", a.resources.GetBucketName())
		return
	}
	err = a.resources.GetBucket().Delete()
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete S3 bucket: %s", err)))
	}
}

func (a *awsService) IsRunningLocally() bool {
	return os.Getenv("CODEBUILD_BUILD_ID") == ""
}

func getAccountId(awsConfig aws.Config) (string, error) {
	stsService := NewSTS(awsConfig)
	return stsService.GetAccountID()
}

func (a *awsService) getBucketName() string {
	return getBucketName(a.cloudPrefix, a.accountId, a.awsConfig.Region)
}

func getBucketName(cloudPrefix, accountId, region string) string {
	return fmt.Sprintf("%s-%s-%s", cloudPrefix, accountId, region)
}

func (a *awsService) createBucket(bucket string) (*S3, string) {
	s3 := NewS3(a.ctx, a.awsConfig, bucket)
	exists, err := s3.BucketExists()
	if err != nil {
		log.Fatalf("Failed to check if S3 Bucket %s exists: %s", bucket, err)
	}
	if exists {
		log.Printf("S3 Bucket %s already exists\n", bucket)
		return s3, fmt.Sprintf(bucketArnFormat, bucket)
	}
	util.DelayBucketCreation(bucket, a.skipDelay) // This allows users to react if they ran the agent with wrong credentials
	s3Arn, _, err := s3.CreateBucket()
	if err != nil {
		log.Fatalf("Failed to create S3 Bucket %s: %s", bucket, err)
	}
	err = s3.addDummyZip()
	if err != nil {
		log.Fatalf("Failed to add dummy zip to S3 Bucket %s: %s", bucket, err)
	}
	return s3, s3Arn
}

func (a *awsService) createDynamoDBTable() *types.TableDescription {
	tableName := fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId)
	dynamoDBTable, err := GetDynamoDBTable(a.ctx, a.awsConfig, tableName)
	if err != nil {
		log.Fatalf("Failed to get DynamoDB table: %s", err)
	}
	if dynamoDBTable != nil {
		return dynamoDBTable
	}
	dynamoDBTable, err = CreateDynamoDBTable(a.ctx, a.awsConfig, tableName)
	if err != nil {
		log.Fatalf("Failed to create DynamoDB table: %s", err)
	}
	return dynamoDBTable
}

func (a *awsService) createCloudWatchLogs() (string, string, string, CloudWatch) {
	cloudwatch := NewCloudWatch(a.ctx, a.awsConfig)
	logGroup := a.getLogGroup()
	logGroupArn, err := cloudwatch.GetLogGroup(logGroup)
	if err != nil {
		log.Fatalf("Failed to get CloudWatch log group: %s", err)
	}
	if logGroupArn == "" {
		logGroupArn, err = cloudwatch.CreateLogGroup(logGroup)
		if err != nil {
			log.Fatalf("Failed to create CloudWatch log group: %s", err)
		}
	}
	logStream := a.getLogGroup()
	exists, err := cloudwatch.LogStreamExists(logGroup, logStream)
	if err != nil {
		log.Fatalf("Failed to get CloudWatch log stream exists: %s", err)
	}
	if exists {
		return logGroup, logGroupArn, logStream, cloudwatch
	}
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		log.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}
	return logGroup, logGroupArn, logStream, cloudwatch
}

func (a *awsService) getLogGroup() string {
	return fmt.Sprintf("%s-log", a.cloudPrefix)
}

func (a *awsService) createIAMRoles(logGroupArn string, s3Arn string, dynamoDBTableArn string) (string, string) {
	buildRoleArn, buildRoleCreated := a.createBuildRole(a.resources.IAM, logGroupArn, s3Arn, dynamoDBTableArn)
	pipelineRoleArn, pipelineRoleCreated := a.createPipelineRole(a.resources.IAM, s3Arn)

	if buildRoleCreated || pipelineRoleCreated {
		log.Println("Waiting for roles to be available...")
		time.Sleep(15 * time.Second)
	}

	return buildRoleArn, pipelineRoleArn
}

func (a *awsService) createPipelineRole(iam IAM, s3Arn string) (string, bool) {
	pipelineRoleName := a.getPipelineRoleName()
	pipelineRole := iam.GetRole(pipelineRoleName)
	if pipelineRole != nil {
		return *pipelineRole.Arn, false
	}
	pipelineRole = iam.CreateRole(pipelineRoleName, []PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codepipeline.amazonaws.com"},
	}})
	pipelinePolicy := iam.CreatePolicy(pipelineRoleName, CodePipelinePolicy(s3Arn))
	err := iam.AttachRolePolicy(*pipelinePolicy.Arn, *pipelineRole.RoleName)
	if err != nil {
		log.Fatalf("Failed to attach pipeline policy to role %s: %s", *pipelineRole.RoleName, err)
	}
	return *pipelineRole.Arn, true
}

func (a *awsService) getPipelineRoleName() string {
	return fmt.Sprintf("%s-pipeline-%s", a.cloudPrefix, a.awsConfig.Region)
}

func (a *awsService) createBuildRole(iam IAM, logGroupArn string, s3Arn string, dynamoDBTableArn string) (string, bool) {
	buildRoleName := a.getBuildRoleName()
	buildRole := iam.GetRole(buildRoleName)
	if buildRole != nil {
		return *buildRole.Arn, false
	}
	buildRole = iam.CreateRole(buildRoleName, []PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codebuild.amazonaws.com"},
	}})

	err := iam.AttachRolePolicy("arn:aws:iam::aws:policy/AdministratorAccess", *buildRole.RoleName)
	if err != nil {
		log.Fatalf("Failed to attach admin policy to role %s: %s", *buildRole.RoleName, err)
	}
	buildPolicy := iam.CreatePolicy(buildRoleName,
		CodeBuildPolicy(logGroupArn, s3Arn, dynamoDBTableArn))
	err = iam.AttachRolePolicy(*buildPolicy.Arn, *buildRole.RoleName)
	if err != nil {
		log.Fatalf("Failed to attach build policy to role %s: %s", *buildRole.RoleName, err)
	}
	return *buildRole.Arn, true
}

func (a *awsService) getBuildRoleName() string {
	return fmt.Sprintf("%s-build-%s", a.cloudPrefix, a.awsConfig.Region)
}

func (a *awsService) deleteCloudWatchLogs() {
	cloudwatch := NewCloudWatch(a.ctx, a.awsConfig)
	logGroup := a.getLogGroup()
	logStream := a.getLogGroup()
	err := cloudwatch.DeleteLogStream(logGroup, logStream)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete CloudWatch log stream: %s", err)))
	}
	err = cloudwatch.DeleteLogGroup(logGroup)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete CloudWatch log group: %s", err)))
	}
}

func (a *awsService) deleteIAMRoles() {
	buildRole := a.getBuildRoleName()
	err := a.resources.IAM.DeleteRolePolicyAttachments(buildRole)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to detach IAM policies %s: %s", buildRole, err)))
	}
	err = a.resources.IAM.DeleteRole(buildRole)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM role %s: %s", buildRole, err)))
	}
	err = a.resources.IAM.DeletePolicy(buildRole, a.accountId)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM policy %s: %s", buildRole, err)))
	}
	pipelineRole := a.getPipelineRoleName()
	err = a.resources.IAM.DeleteRolePolicyAttachments(pipelineRole)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to detach IAM policy %s: %s", pipelineRole, err)))
	}
	err = a.resources.IAM.DeleteRole(pipelineRole)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM role %s: %s", pipelineRole, err)))
	}
	err = a.resources.IAM.DeletePolicy(pipelineRole, a.accountId)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM policy %s: %s", pipelineRole, err)))
	}
}

func (a *awsService) CreateServiceAccount() {
	username := fmt.Sprintf("%s-service-account-%s", a.cloudPrefix, a.awsConfig.Region)
	bucket := a.getBucketName()
	bucketArn := fmt.Sprintf(bucketArnFormat, bucket)
	policyStatement := ServiceAccountPolicy(bucketArn, a.accountId, a.getBuildRoleName(), a.getPipelineRoleName())

	user := a.resources.IAM.GetUser(username)
	if user != nil {
		log.Printf("Service account %s already exists\n", username)
		a.updateServiceAccountPolicy(username, policyStatement)
		return
	}

	user = a.resources.IAM.CreateUser(username)
	policy := a.resources.IAM.CreatePolicy(username, policyStatement)
	a.resources.IAM.AttachUserPolicy(*policy.Arn, *user.UserName)
	accessKey := a.resources.IAM.CreateAccessKey(*user.UserName)

	accessKeyIdParam := fmt.Sprintf("/entigo-infralib/%s/access_key_id", username)
	err := a.resources.SSM.PutParameter(accessKeyIdParam, *accessKey.AccessKeyId)
	if err != nil {
		log.Printf("Access key id: %s\nSecret access key: %s\n", *accessKey.AccessKeyId, *accessKey.SecretAccessKey)
		log.Fatalf("Failed to store access key id: %s", err)
	}
	secretAccessKeyParam := fmt.Sprintf("/entigo-infralib/%s/secret_access_key", username)
	err = a.resources.SSM.PutParameter(secretAccessKeyParam, *accessKey.SecretAccessKey)
	if err != nil {
		log.Printf("Access key id: %s\nSecret access key: %s\n", *accessKey.AccessKeyId, *accessKey.SecretAccessKey)
		log.Fatalf("Failed to store secret access key: %s", err)
	}

	log.Printf("Service account secrets %s and %s saved into ssm\n", accessKeyIdParam, secretAccessKeyParam)
}

func (a *awsService) updateServiceAccountPolicy(username string, statement []PolicyStatement) {
	policy, err := a.resources.IAM.GetPolicy(username)
	if err != nil {
		log.Fatalf("Failed to get policy for user %s: %s", username, err)
	}
	if policy != nil {
		a.resources.IAM.UpdatePolicy(*policy.Arn, statement)
		return
	}
	policy = a.resources.IAM.CreatePolicy(username, statement)
	a.resources.IAM.AttachUserPolicy(*policy.Arn, username)
}

func (a *awsService) DeleteServiceAccount() {
	username := fmt.Sprintf("%s-service-account-%s", a.cloudPrefix, a.awsConfig.Region)
	policyArn := fmt.Sprintf("arn:aws:iam::%s:policy/%s", a.accountId, username)
	err := a.resources.IAM.DetachUserPolicy(policyArn, username)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to detach IAM policy %s: %s", username, err)))
	}
	err = a.resources.IAM.DeleteAccessKeys(username)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM access keys for %s: %s", username, err)))
	}
	err = a.resources.IAM.DeleteUser(username)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM user %s: %s", username, err)))
	}
	err = a.resources.IAM.DeletePolicy(username, a.accountId)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete IAM policy %s: %s", username, err)))
	}
	accessKeyIdParam := fmt.Sprintf("/entigo-infralib/%s/access_key_id", username)
	err = a.resources.SSM.DeleteParameter(accessKeyIdParam)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete SSM parameter %s: %s", accessKeyIdParam, err)))
	}
	secretAccessKeyParam := fmt.Sprintf("/entigo-infralib/%s/secret_access_key", username)
	err = a.resources.SSM.DeleteParameter(secretAccessKeyParam)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete SSM parameter %s: %s", secretAccessKeyParam, err)))
	}
}

func (a *awsService) AddEncryption(moduleName string, outputs map[string]model.TFOutput) error {
	err := a.setupConfigEncryption(moduleName, outputs)
	if err != nil {
		return err
	}
	if a.pipeline.Type == string(common.PipelineTypeLocal) {
		return nil
	}
	return a.setupTelemetryEncryption(moduleName, outputs)
}

func GetConfigEncryptionKey(moduleName string, outputs map[string]model.TFOutput) (string, error) {
	return util.GetOutputStringValue(outputs, fmt.Sprintf("%s__config_alias_arn", moduleName))
}

func (a *awsService) setupConfigEncryption(moduleName string, outputs map[string]model.TFOutput) error {
	arn, err := GetConfigEncryptionKey(moduleName, outputs)
	if err != nil {
		return err
	}
	if arn == "" {
		return nil
	}
	a.resources.GetSSM().(*ssm).AddEncryptionKeyId(arn)
	err = a.resources.GetBucket().(*S3).addEncryption(arn)
	if err != nil {
		return fmt.Errorf("failed to add encryption to bucket: %v", err)
	}
	return nil
}

func (a *awsService) setupTelemetryEncryption(moduleName string, outputs map[string]model.TFOutput) error {
	arn, err := util.GetOutputStringValue(outputs, fmt.Sprintf("%s__telemetry_alias_arn", moduleName))
	if err != nil {
		return err
	}
	if arn == "" {
		return nil
	}
	err = a.resources.CloudWatch.addEncryption(a.getLogGroup(), arn)
	if err != nil {
		return fmt.Errorf("failed to add encryption to log group: %v", err)
	}
	return nil
}
