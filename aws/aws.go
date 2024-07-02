package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"time"
)

type awsService struct {
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
		"bucket":         r.Bucket,
		"dynamodb_table": r.DynamoDBTable,
		"encrypt":        "true",
	}
}

func NewAWS(ctx context.Context, cloudPrefix string) model.CloudProvider {
	awsConfig := GetAWSConfig(ctx)
	accountId, err := getAccountId(awsConfig)
	if err != nil {
		common.Logger.Fatalf(fmt.Sprintf("%s", err))
	}
	return &awsService{
		awsConfig:   awsConfig,
		cloudPrefix: cloudPrefix,
		accountId:   accountId,
	}
}

func GetAWSConfig(ctx context.Context) aws.Config {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRetryer(func() aws.Retryer {
		return retry.AddWithMaxAttempts(retry.NewStandard(), 10)
	}))
	if err != nil {
		common.Logger.Fatalf("Failed to initialize AWS session: %s", err)
	}
	common.Logger.Printf("AWS session initialized with region: %s\n", cfg.Region)
	return cfg
}

func (a *awsService) SetupResources(branch string) model.Resources {
	codeCommit, err := a.setupCodeCommit(branch)
	if err != nil {
		common.Logger.Fatalf(fmt.Sprintf("%s", err))
	}
	repoMetadata, err := codeCommit.GetAWSRepoMetadata()
	if err != nil {
		common.Logger.Fatalf(fmt.Sprintf("%s", err))
	}

	bucket, s3Arn := a.createBucket()
	dynamoDBTable := a.createDynamoDBTable()
	logGroup, logGroupArn, logStream, cloudwatch := a.createCloudWatchLogs()
	iam, buildRoleArn, pipelineRoleArn := a.createIAMRoles(logGroupArn, s3Arn, *repoMetadata.Arn, *dynamoDBTable.TableArn)

	codeBuild := NewBuilder(a.awsConfig, buildRoleArn, logGroup, logStream, s3Arn)
	codePipeline := NewPipeline(a.awsConfig, branch, pipelineRoleArn, bucket, cloudwatch, logGroup, logStream)

	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AWS,
			CodeRepo:     codeCommit,
			Pipeline:     codePipeline,
			CodeBuild:    codeBuild,
			SSM:          NewSSM(a.awsConfig),
			CloudPrefix:  a.cloudPrefix,
			Bucket:       bucket,
		},
		IAM:           iam,
		DynamoDBTable: *dynamoDBTable.TableName,
		Region:        a.awsConfig.Region,
		AccountId:     a.accountId,
	}
	return a.resources
}

func (a *awsService) GetResources(branch string) model.Resources {
	repoName := fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId)
	a.resources = Resources{
		CloudResources: model.CloudResources{
			ProviderType: model.AWS,
			CodeRepo:     NewCodeCommit(a.awsConfig, repoName, branch),
			CodeBuild:    NewBuilder(a.awsConfig, "", "", "", ""),
			Pipeline:     NewPipeline(a.awsConfig, branch, "", "", nil, "", ""),
			CloudPrefix:  a.cloudPrefix,
			Bucket:       fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId),
		},
		IAM:       NewIAM(a.awsConfig),
		Region:    a.awsConfig.Region,
		AccountId: a.accountId,
	}
	return a.resources
}

func (a *awsService) DeleteResources(deleteBucket bool, hasCustomTFStep bool) {
	agentProjectName := fmt.Sprintf("%s-agent", a.cloudPrefix)
	err := a.resources.GetPipeline().(*Pipeline).deletePipeline(agentProjectName)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent pipeline: %s", err))
	}
	err = a.resources.GetBuilder().DeleteProject(agentProjectName, model.Step{})
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete agent project: %s", err))
	}
	err = DeleteDynamoDBTable(a.awsConfig, fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId))
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete DynamoDB table: %s", err))
	}
	a.deleteCloudWatchLogs()
	err = a.resources.GetCodeRepo().Delete()
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete CodeCommit repository: %s", err))
	}
	a.deleteIAMRoles()
	if hasCustomTFStep {
		common.PrintWarning(fmt.Sprintf("Custom terraform state bucket %s-custom-%s will not be deleted, delete it manually if needed\n",
			a.cloudPrefix, a.accountId))
	}
	if !deleteBucket {
		common.Logger.Printf("Terraform state bucket %s will not be deleted, delete it manually if needed\n", a.resources.GetBucket())
		return
	}
	s3 := NewS3(a.awsConfig)
	err = s3.DeleteBucket(fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId))
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete S3 bucket: %s", err))
	}
}

func getAccountId(awsConfig aws.Config) (string, error) {
	stsService := NewSTS(awsConfig)
	return stsService.GetAccountID()
}

func (a *awsService) setupCodeCommit(branch string) (*CodeCommit, error) {
	repoName := fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId)
	codeCommit := NewCodeCommit(a.awsConfig, repoName, branch)
	created, err := codeCommit.CreateRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to create CodeRepo repository: %w", err)
	}
	if created {
		err := codeCommit.PutFile("README.md", []byte("# Entigo infralib repository\nThis repository is used to apply Entigo infralib modules."))
		if err != nil {
			return nil, err
		}
	}
	return codeCommit, nil
}

func (a *awsService) SetupCustomCodeRepo(branch string) (model.CodeRepo, error) {
	repoName := fmt.Sprintf("%s-custom-%s", a.cloudPrefix, a.accountId)
	codeCommit := NewCodeCommit(a.awsConfig, repoName, branch)
	created, err := codeCommit.CreateRepository()
	if err != nil {
		common.Logger.Fatalf("Failed to create custom CodeRepo repository: %s", err)
	}
	if created {
		err := codeCommit.PutFile("README.md", []byte("# Entigo infralib custom repository\nThis repository is used to apply client modules."))
		if err != nil {
			return nil, err
		}
		repoMetadata, err := codeCommit.GetAWSRepoMetadata()
		if err != nil {
			return nil, err
		}
		a.attachRolePolicies(*repoMetadata.Arn)
	}
	return codeCommit, nil
}

func (a *awsService) createBucket() (string, string) {
	s3 := NewS3(a.awsConfig)
	bucket := fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId)
	s3Arn, err := s3.CreateBucket(bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create S3 Bucket: %s", err)
	}
	return bucket, s3Arn
}

func (a *awsService) createDynamoDBTable() *types.TableDescription {
	dynamoDBTable, err := CreateDynamoDBTable(a.awsConfig, fmt.Sprintf("%s-%s", a.cloudPrefix, a.accountId))
	if err != nil {
		common.Logger.Fatalf("Failed to create DynamoDB table: %s", err)
	}
	return dynamoDBTable
}

func (a *awsService) createCloudWatchLogs() (string, string, string, CloudWatch) {
	cloudwatch := NewCloudWatch(a.awsConfig)
	logGroup := fmt.Sprintf("%s-log", a.cloudPrefix)
	logGroupArn, err := cloudwatch.CreateLogGroup(logGroup)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log group: %s", err)
	}
	logStream := fmt.Sprintf("%s-log", a.cloudPrefix)
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}
	return logGroup, logGroupArn, logStream, cloudwatch
}

func (a *awsService) createIAMRoles(logGroupArn string, s3Arn string, repoArn string, dynamoDBTableArn string) (IAM, string, string) {
	iam := NewIAM(a.awsConfig)
	buildRoleArn, buildRoleCreated := createBuildRole(iam, a.cloudPrefix, logGroupArn, s3Arn, repoArn, dynamoDBTableArn)
	pipelineRoleArn, pipelineRoleCreated := a.createPipelineRole(iam, s3Arn, repoArn)

	if buildRoleCreated || pipelineRoleCreated {
		common.Logger.Println("Waiting for roles to be available...")
		time.Sleep(10 * time.Second)
	}

	return iam, buildRoleArn, pipelineRoleArn
}

func (a *awsService) attachRolePolicies(roleArn string) {
	pipelineRoleName := a.getPipelineRoleName()
	pipelinePolicyName := fmt.Sprintf("%s-custom", pipelineRoleName)
	pipelinePolicy := a.resources.IAM.CreatePolicy(pipelinePolicyName, []PolicyStatement{CodePipelineRepoPolicy(roleArn)})
	err := a.resources.IAM.AttachRolePolicy(*pipelinePolicy.Arn, pipelineRoleName)
	if err != nil {
		common.Logger.Fatalf("Failed to attach pipeline policy to role %s: %s", pipelineRoleName, err)
	}

	buildRoleName := getBuildRoleName(a.cloudPrefix)
	buildPolicyName := fmt.Sprintf("%s-custom", buildRoleName)
	buildPolicy := a.resources.IAM.CreatePolicy(buildPolicyName, []PolicyStatement{CodeBuildRepoPolicy(roleArn)})
	err = a.resources.IAM.AttachRolePolicy(*buildPolicy.Arn, buildRoleName)
	if err != nil {
		common.Logger.Fatalf("Failed to attach build policy to role %s: %s", buildRoleName, err)
	}
}

func (a *awsService) createPipelineRole(iam IAM, s3Arn string, repoArn string) (string, bool) {
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
	pipelinePolicy := iam.CreatePolicy(pipelineRoleName, CodePipelinePolicy(s3Arn, repoArn))
	err := iam.AttachRolePolicy(*pipelinePolicy.Arn, *pipelineRole.RoleName)
	if err != nil {
		common.Logger.Fatalf("Failed to attach pipeline policy to role %s: %s", *pipelineRole.RoleName, err)
	}
	return *pipelineRole.Arn, true
}

func (a *awsService) getPipelineRoleName() string {
	return fmt.Sprintf("%s-pipeline", a.cloudPrefix)
}

func createBuildRole(iam IAM, prefix string, logGroupArn string, s3Arn string, repoArn string, dynamoDBTableArn string) (string, bool) {
	buildRoleName := getBuildRoleName(prefix)
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
		common.Logger.Fatalf("Failed to attach admin policy to role %s: %s", *buildRole.RoleName, err)
	}
	buildPolicy := iam.CreatePolicy(buildRoleName,
		CodeBuildPolicy(logGroupArn, s3Arn, repoArn, dynamoDBTableArn))
	err = iam.AttachRolePolicy(*buildPolicy.Arn, *buildRole.RoleName)
	if err != nil {
		common.Logger.Fatalf("Failed to attach build policy to role %s: %s", *buildRole.RoleName, err)
	}
	return *buildRole.Arn, true
}

func getBuildRoleName(prefix string) string {
	return fmt.Sprintf("%s-build", prefix)
}

func (a *awsService) deleteCloudWatchLogs() {
	cloudwatch := NewCloudWatch(a.awsConfig)
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
	buildRole := getBuildRoleName(a.cloudPrefix)
	policyArn := fmt.Sprintf("arn:aws:iam::%s:policy/%s", a.accountId, buildRole)
	err := a.resources.IAM.DeleteRolePolicyAttachment(policyArn, buildRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to detach IAM policy %s: %s", buildRole, err))
	}
	policyArn = fmt.Sprintf("arn:aws:iam::%s:policy/%s-custom", a.accountId, buildRole)
	err = a.resources.IAM.DeleteRolePolicyAttachment(policyArn, buildRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to detach IAM policy %s-custom: %s", buildRole, err))
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
	err = a.resources.IAM.DeletePolicy(fmt.Sprintf("%s-custom", buildRole), a.accountId)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM policy %s-custom: %s", buildRole, err))
	}
	pipelineRole := a.getPipelineRoleName()
	policyArn = fmt.Sprintf("arn:aws:iam::%s:policy/%s", a.accountId, pipelineRole)
	err = a.resources.IAM.DeleteRolePolicyAttachment(policyArn, pipelineRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to detach IAM policy %s: %s", pipelineRole, err))
	}
	policyArn = fmt.Sprintf("arn:aws:iam::%s:policy/%s-custom", a.accountId, pipelineRole)
	err = a.resources.IAM.DeleteRolePolicyAttachment(policyArn, pipelineRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to detach IAM policy %s-custom: %s", pipelineRole, err))
	}
	err = a.resources.IAM.DeleteRole(pipelineRole)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM role %s: %s", pipelineRole, err))
	}
	err = a.resources.IAM.DeletePolicy(pipelineRole, a.accountId)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM policy %s: %s", pipelineRole, err))
	}
	err = a.resources.IAM.DeletePolicy(fmt.Sprintf("%s-custom", pipelineRole), a.accountId)
	if err != nil {
		common.PrintWarning(fmt.Sprintf("Failed to delete IAM policy %s-custom: %s", pipelineRole, err))
	}
}
