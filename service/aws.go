package service

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"time"
)

type AWSResources struct {
	CodeCommit    CodeCommit
	CodePipeline  Pipeline
	CodeBuild     Builder
	SSM           SSM
	AwsPrefix     string
	Bucket        string
	DynamoDBTable string
}

func GetAWSConfig() aws.Config {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		common.Logger.Fatalf("Failed to initialize AWS session: %s", err)
	}
	common.Logger.Printf("AWS session initialized with region: %s\n", cfg.Region)
	return cfg
}

func SetupAWSResources(awsPrefix string, branch string) AWSResources {
	awsConfig := GetAWSConfig()
	accountId := getAccountId(awsConfig)

	codeCommit := setupCodeCommit(awsConfig, accountId, awsPrefix, branch)
	repoMetadata := codeCommit.GetRepoMetadata()

	bucket, s3Arn := createBucket(awsConfig, awsPrefix, accountId)
	dynamoDBTable := createDynamoDBTable(awsConfig, awsPrefix, accountId)
	logGroup, logGroupArn, logStream, cloudwatch := createCloudWatchLogs(awsConfig, awsPrefix)
	buildRoleArn, pipelineRoleArn := createIAMRoles(awsConfig, awsPrefix, logGroupArn, s3Arn, *repoMetadata.Arn, *dynamoDBTable.TableArn)

	codeBuild := NewBuilder(awsConfig, buildRoleArn, logGroup, logStream, s3Arn)
	codePipeline := NewPipeline(awsConfig, *repoMetadata.RepositoryName, branch, pipelineRoleArn, bucket, cloudwatch, logGroup, logStream)

	return AWSResources{
		CodeCommit:    codeCommit,
		CodePipeline:  codePipeline,
		CodeBuild:     codeBuild,
		SSM:           NewSSM(awsConfig),
		AwsPrefix:     awsPrefix,
		Bucket:        bucket,
		DynamoDBTable: *dynamoDBTable.TableName,
	}
}

func getAccountId(awsConfig aws.Config) string {
	stsService := NewSTS(awsConfig)
	return stsService.GetAccountID()
}

func setupCodeCommit(awsConfig aws.Config, accountID string, prefix string, branch string) CodeCommit {
	repoName := fmt.Sprintf("%s-%s", prefix, accountID)
	codeCommit := NewCodeCommit(awsConfig, repoName, branch)
	created, err := codeCommit.CreateRepository()
	if err != nil {
		common.Logger.Fatalf("Failed to create CodeCommit repository: %s", err)
	}
	if created {
		codeCommit.PutFile("README.md", []byte("# Entigo infralib repository\nThis repository is used to apply Entigo infralib modules."))
	}
	return codeCommit
}

func createBucket(awsConfig aws.Config, prefix string, accountId string) (string, string) {
	s3 := NewS3(awsConfig)
	bucket := fmt.Sprintf("%s-%s", prefix, accountId)
	s3Arn, err := s3.CreateBucket(bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create S3 Bucket: %s", err)
	}
	return bucket, s3Arn
}

func createDynamoDBTable(awsConfig aws.Config, prefix string, accountId string) *types.TableDescription {
	dynamoDBTable, err := CreateDynamoDBTable(awsConfig, fmt.Sprintf("%s-%s", prefix, accountId))
	if err != nil {
		common.Logger.Fatalf("Failed to create DynamoDB table: %s", err)
	}
	return dynamoDBTable
}

func createCloudWatchLogs(awsConfig aws.Config, prefix string) (string, string, string, CloudWatch) {
	cloudwatch := NewCloudWatch(awsConfig)
	logGroup := fmt.Sprintf("log-%s", prefix)
	logGroupArn, err := cloudwatch.CreateLogGroup(logGroup)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log group: %s", err)
	}
	logStream := fmt.Sprintf("log-%s", prefix)
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}
	return logGroup, logGroupArn, logStream, cloudwatch
}

func createIAMRoles(awsConfig aws.Config, prefix string, logGroupArn string, s3Arn string, repoArn string, dynamoDBTableArn string) (string, string) {
	iam := NewIAM(awsConfig)
	buildRoleArn, buildRoleCreated := createBuildRole(iam, prefix, logGroupArn, s3Arn, repoArn, dynamoDBTableArn)
	pipelineRoleArn, pipelineRoleCreated := createPipelineRole(iam, prefix, s3Arn, repoArn)

	if buildRoleCreated || pipelineRoleCreated {
		common.Logger.Println("Waiting for roles to be available...")
		time.Sleep(10 * time.Second)
	}

	return buildRoleArn, pipelineRoleArn
}

func createPipelineRole(iam IAM, prefix string, s3Arn string, repoArn string) (string, bool) {
	pipelineRoleName := fmt.Sprintf("%s-pipeline", prefix)
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

func createBuildRole(iam IAM, prefix string, logGroupArn string, s3Arn string, repoArn string, dynamoDBTableArn string) (string, bool) {
	buildRoleName := fmt.Sprintf("%s-build", prefix)
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
