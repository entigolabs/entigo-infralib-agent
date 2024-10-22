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
	"log"
	"time"
)

type awsService struct {
	ctx         context.Context
	awsConfig   aws.Config
	cloudPrefix string
	accountId   string
	resources   Resources
}

type Resources struct {
	model.CloudResources
	IAM           IAM
	DynamoDBTable string
	Region        string
	AccountId     string
}

func (r Resources) GetBackendConfigVars(key string) map[string]string {
	return map[string]string{
		"key":            key,
		"bucket":         r.BucketName,
		"dynamodb_table": r.DynamoDBTable,
		"encrypt":        "true",
	}
}

func NewAWS(ctx context.Context, cloudPrefix string, awsFlags common.AWS) model.CloudProvider {
	awsConfig := GetAWSConfig(ctx, awsFlags.RoleArn)
	accountId, err := getAccountId(awsConfig)
	if err != nil {
		log.Fatal(err.Error())
	}
	return &awsService{
		ctx:         ctx,
		awsConfig:   awsConfig,
		cloudPrefix: cloudPrefix,
		accountId:   accountId,
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
	bucket := fmt.Sprintf("%s-%s-%s", a.cloudPrefix, a.accountId, a.awsConfig.Region)
	s3, s3Arn := a.createBucket(bucket)
	dynamoDBTable := a.createDynamoDBTable()
	logGroup, logGroupArn, logStream, cloudwatch := a.createCloudWatchLogs()
	iam, buildRoleArn, pipelineRoleArn := a.createIAMRoles(logGroupArn, s3Arn, *dynamoDBTable.TableArn)

	codeBuild := NewBuilder(a.ctx, a.awsConfig, buildRoleArn, logGroup, logStream, s3Arn)
	codePipeline := NewPipeline(a.ctx, a.awsConfig, pipelineRoleArn, cloudwatch, logGroup, logStream)

	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AWS,
			Bucket:       s3,
			Pipeline:     codePipeline,
			CodeBuild:    codeBuild,
			SSM:          NewSSM(a.ctx, a.awsConfig),
			CloudPrefix:  a.cloudPrefix,
			BucketName:   bucket,
		},
		IAM:           iam,
		DynamoDBTable: *dynamoDBTable.TableName,
		Region:        a.awsConfig.Region,
		AccountId:     a.accountId,
	}
	return a.resources
}

func (a *awsService) GetResources() model.Resources {
	bucket := fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId)
	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AWS,
			Bucket:       NewS3(a.ctx, a.awsConfig, bucket),
			CodeBuild:    NewBuilder(a.ctx, a.awsConfig, "", "", "", ""),
			Pipeline:     NewPipeline(a.ctx, a.awsConfig, "", nil, "", ""),
			CloudPrefix:  a.cloudPrefix,
			BucketName:   bucket,
		},
		IAM:       NewIAM(a.ctx, a.awsConfig, a.accountId),
		Region:    a.awsConfig.Region,
		AccountId: a.accountId,
	}
	return a.resources
}

func (a *awsService) DeleteResources(deleteBucket bool) {
	agentProjectName := fmt.Sprintf("%s-agent-%s", a.cloudPrefix, common.RunCommand)
	err := a.resources.GetPipeline().(*Pipeline).deletePipeline(agentProjectName)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent run pipeline: %s", err))
	}
	err = a.resources.GetBuilder().DeleteProject(agentProjectName, model.Step{})
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent run project: %s", err))
	}

	agentProjectName = fmt.Sprintf("%s-agent-%s", a.cloudPrefix, common.UpdateCommand)
	err = a.resources.GetPipeline().(*Pipeline).deletePipeline(agentProjectName)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent update pipeline: %s", err))
	}
	err = a.resources.GetBuilder().DeleteProject(agentProjectName, model.Step{})
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent update project: %s", err))
	}

	err = DeleteDynamoDBTable(a.ctx, a.awsConfig, fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId))
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete DynamoDB table: %s", err))
	}
	a.deleteCloudWatchLogs()
	a.deleteIAMRoles()
	if !deleteBucket {
		log.Printf("Terraform state bucket %s will not be deleted, delete it manually if needed\n", a.resources.GetBucketName())
		return
	}
	err = a.resources.GetBucket().Delete()
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete S3 bucket: %s", err))
	}
}

func getAccountId(awsConfig aws.Config) (string, error) {
	stsService := NewSTS(awsConfig)
	return stsService.GetAccountID()
}

func (a *awsService) createBucket(bucket string) (*S3, string) {
	s3 := NewS3(a.ctx, a.awsConfig, bucket)
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
	dynamoDBTable, err := CreateDynamoDBTable(a.ctx, a.awsConfig, fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId))
	if err != nil {
		log.Fatalf("Failed to create DynamoDB table: %s", err)
	}
	return dynamoDBTable
}

func (a *awsService) createCloudWatchLogs() (string, string, string, CloudWatch) {
	cloudwatch := NewCloudWatch(a.ctx, a.awsConfig)
	logGroup := fmt.Sprintf("%s-log", a.cloudPrefix)
	logGroupArn, err := cloudwatch.CreateLogGroup(logGroup)
	if err != nil {
		log.Fatalf("Failed to create CloudWatch log group: %s", err)
	}
	logStream := fmt.Sprintf("%s-log", a.cloudPrefix)
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		log.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}
	return logGroup, logGroupArn, logStream, cloudwatch
}

func (a *awsService) createIAMRoles(logGroupArn string, s3Arn string, dynamoDBTableArn string) (IAM, string, string) {
	iam := NewIAM(a.ctx, a.awsConfig, a.accountId)
	buildRoleArn, buildRoleCreated := a.createBuildRole(iam, logGroupArn, s3Arn, dynamoDBTableArn)
	pipelineRoleArn, pipelineRoleCreated := a.createPipelineRole(iam, s3Arn)

	if buildRoleCreated || pipelineRoleCreated {
		log.Println("Waiting for roles to be available...")
		time.Sleep(10 * time.Second)
	}

	return iam, buildRoleArn, pipelineRoleArn
}

func (a *awsService) createPipelineRole(iam IAM, s3Arn string) (string, bool) {
	pipelineRoleName := a.getPipelineRoleName()
	pipelineRole := iam.CreateRole(pipelineRoleName, []PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codepipeline.amazonaws.com"},
	}})
	if pipelineRole == nil {
		pipelineRole = iam.GetRole(pipelineRoleName)
		return *pipelineRole.Arn, false
	}
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
	buildRole := iam.CreateRole(buildRoleName, []PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codebuild.amazonaws.com"},
	}})
	if buildRole == nil {
		buildRole = iam.GetRole(buildRoleName)
		return *buildRole.Arn, false
	}

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
	logGroup := fmt.Sprintf("%s-log", a.cloudPrefix)
	logStream := fmt.Sprintf("%s-log", a.cloudPrefix)
	err := cloudwatch.DeleteLogStream(logGroup, logStream)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete CloudWatch log stream: %s", err))
	}
	err = cloudwatch.DeleteLogGroup(logGroup)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete CloudWatch log group: %s", err))
	}
}

func (a *awsService) deleteIAMRoles() {
	buildRole := a.getBuildRoleName()
	policyArn := fmt.Sprintf("arn:aws:iam::%s:policy/%s", a.accountId, buildRole)
	err := a.resources.IAM.DeleteRolePolicyAttachment(policyArn, buildRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to detach IAM policy %s: %s", buildRole, err))
	}
	err = a.resources.IAM.DeleteRolePolicyAttachment("arn:aws:iam::aws:policy/AdministratorAccess", buildRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to detach IAM policy AdministratorAccess: %s", err))
	}
	err = a.resources.IAM.DeleteRole(buildRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM role %s: %s", buildRole, err))
	}
	err = a.resources.IAM.DeletePolicy(buildRole, a.accountId)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM policy %s: %s", buildRole, err))
	}
	pipelineRole := a.getPipelineRoleName()
	policyArn = fmt.Sprintf("arn:aws:iam::%s:policy/%s", a.accountId, pipelineRole)
	err = a.resources.IAM.DeleteRolePolicyAttachment(policyArn, pipelineRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to detach IAM policy %s: %s", pipelineRole, err))
	}
	err = a.resources.IAM.DeleteRole(pipelineRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM role %s: %s", pipelineRole, err))
	}
	err = a.resources.IAM.DeletePolicy(pipelineRole, a.accountId)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM policy %s: %s", pipelineRole, err))
	}
}
