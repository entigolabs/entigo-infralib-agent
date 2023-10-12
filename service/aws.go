package service

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"time"
)

type AWS interface {
	SetupAWSResources(branch string) AWSResources
	SetupCustomCodeCommit(branch string) CodeCommit
}

type awsService struct {
	awsConfig aws.Config
	awsPrefix string
	accountId string
	resources AWSResources
}

type AWSResources struct {
	CodeCommit    CodeCommit
	CodePipeline  Pipeline
	CodeBuild     Builder
	SSM           SSM
	IAM           IAM
	AwsPrefix     string
	Bucket        string
	DynamoDBTable string
	AccountId     string
	Region        string
}

func NewAWS(awsPrefix string) AWS {
	awsConfig := GetAWSConfig()
	return &awsService{
		awsConfig: awsConfig,
		awsPrefix: awsPrefix,
		accountId: getAccountId(awsConfig),
	}
}

func GetAWSConfig() aws.Config {
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRetryer(func() aws.Retryer {
		return retry.AddWithMaxAttempts(retry.NewStandard(), 10)
	}))
	if err != nil {
		common.Logger.Fatalf("Failed to initialize AWS session: %s", err)
	}
	common.Logger.Printf("AWS session initialized with region: %s\n", cfg.Region)
	return cfg
}

func (a *awsService) SetupAWSResources(branch string) AWSResources {
	codeCommit := a.setupCodeCommit(branch)
	repoMetadata := codeCommit.GetRepoMetadata()

	bucket, s3Arn := a.createBucket()
	dynamoDBTable := a.createDynamoDBTable()
	logGroup, logGroupArn, logStream, cloudwatch := a.createCloudWatchLogs()
	iam, buildRoleArn, pipelineRoleArn := a.createIAMRoles(logGroupArn, s3Arn, *repoMetadata.Arn, *dynamoDBTable.TableArn)

	codeBuild := NewBuilder(a.awsConfig, buildRoleArn, logGroup, logStream, s3Arn)
	codePipeline := NewPipeline(a.awsConfig, *repoMetadata.RepositoryName, branch, pipelineRoleArn, bucket, cloudwatch, logGroup, logStream)

	a.resources = AWSResources{
		CodeCommit:    codeCommit,
		CodePipeline:  codePipeline,
		CodeBuild:     codeBuild,
		SSM:           NewSSM(a.awsConfig),
		IAM:           iam,
		AwsPrefix:     a.awsPrefix,
		Bucket:        bucket,
		DynamoDBTable: *dynamoDBTable.TableName,
		AccountId:     a.accountId,
		Region:        a.awsConfig.Region,
	}
	return a.resources
}

func getAccountId(awsConfig aws.Config) string {
	stsService := NewSTS(awsConfig)
	return stsService.GetAccountID()
}

func (a *awsService) setupCodeCommit(branch string) CodeCommit {
	repoName := fmt.Sprintf("%s-%s", a.awsPrefix, a.accountId)
	codeCommit := NewCodeCommit(a.awsConfig, repoName, branch)
	created, err := codeCommit.CreateRepository()
	if err != nil {
		common.Logger.Fatalf("Failed to create CodeCommit repository: %s", err)
	}
	if created {
		codeCommit.PutFile("README.md", []byte("# Entigo infralib repository\nThis repository is used to apply Entigo infralib modules."))
	}
	return codeCommit
}

func (a *awsService) SetupCustomCodeCommit(branch string) CodeCommit {
	repoName := fmt.Sprintf("%s-custom-%s", a.awsPrefix, a.accountId)
	codeCommit := NewCodeCommit(a.awsConfig, repoName, branch)
	created, err := codeCommit.CreateRepository()
	if err != nil {
		common.Logger.Fatalf("Failed to create custom CodeCommit repository: %s", err)
	}
	if created {
		codeCommit.PutFile("README.md", []byte("# Entigo infralib custom repository\nThis repository is used to apply client modules."))
		a.attachRolePolicies(*codeCommit.GetRepoMetadata().Arn)
	}
	return codeCommit
}

func (a *awsService) createBucket() (string, string) {
	s3 := NewS3(a.awsConfig)
	bucket := fmt.Sprintf("%s-%s", a.awsPrefix, a.accountId)
	s3Arn, err := s3.CreateBucket(bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create S3 Bucket: %s", err)
	}
	return bucket, s3Arn
}

func (a *awsService) createDynamoDBTable() *types.TableDescription {
	dynamoDBTable, err := CreateDynamoDBTable(a.awsConfig, fmt.Sprintf("%s-%s", a.awsPrefix, a.accountId))
	if err != nil {
		common.Logger.Fatalf("Failed to create DynamoDB table: %s", err)
	}
	return dynamoDBTable
}

func (a *awsService) createCloudWatchLogs() (string, string, string, CloudWatch) {
	cloudwatch := NewCloudWatch(a.awsConfig)
	logGroup := fmt.Sprintf("%s-log", a.awsPrefix)
	logGroupArn, err := cloudwatch.CreateLogGroup(logGroup)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log group: %s", err)
	}
	logStream := fmt.Sprintf("%s-log", a.awsPrefix)
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}
	return logGroup, logGroupArn, logStream, cloudwatch
}

func (a *awsService) createIAMRoles(logGroupArn string, s3Arn string, repoArn string, dynamoDBTableArn string) (IAM, string, string) {
	iam := NewIAM(a.awsConfig)
	buildRoleArn, buildRoleCreated := createBuildRole(iam, a.awsPrefix, logGroupArn, s3Arn, repoArn, dynamoDBTableArn)
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

	buildRoleName := getBuildRoleName(a.awsPrefix)
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
	return fmt.Sprintf("%s-pipeline", a.awsPrefix)
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
