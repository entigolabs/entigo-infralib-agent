package updater

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(flags *common.Flags) {
	config := service.GetConfig(flags.Config)

	awsConfig := service.NewAWSConfig()
	stsService := service.NewSTS(awsConfig)

	accountID := stsService.GetAccountID()
	codeCommit := setupCodeCommit(awsConfig, accountID, flags.AWSPrefix, flags.Branch)
	repoMetadata := codeCommit.GetRepoMetadata()

	service.CreateStepFiles(config, codeCommit)

	s3 := service.NewS3(awsConfig)

	s3Arn, err := s3.CreateBucket(fmt.Sprintf("%s-%s", flags.AWSPrefix, "pipeline"))
	if err != nil {
		common.Logger.Fatalf("Failed to create S3 bucket: %s", err)
	}
	dynamoDBArn, err := service.CreateDynamoDBTable(awsConfig, fmt.Sprintf("%s-%s", flags.AWSPrefix, "pipeline"))
	if err != nil {
		common.Logger.Fatalf("Failed to create DynamoDB table: %s", err)
	}
	cloudwatch := service.NewCloudWatch(awsConfig)
	logGroup := fmt.Sprintf("log-%s", flags.AWSPrefix)
	logGroupArn, err := cloudwatch.CreateLogGroup(logGroup)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log group: %s", err)
	}
	logStream := fmt.Sprintf("log-%s", flags.AWSPrefix)
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}

	iam := service.NewIAM(awsConfig)

	buildRole := iam.CreateRole(fmt.Sprintf("%s-build", flags.AWSPrefix), []service.PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codebuild.amazonaws.com"},
	}})
	if buildRole != nil {
		err = iam.AttachRolePolicy("arn:aws:iam::aws:policy/AdministratorAccess", *buildRole.RoleName)
		if err != nil {
			common.Logger.Fatalf("Failed to attach admin policy to role %s: %s", *buildRole.RoleName, err)
		}
		buildPolicy := iam.CreatePolicy(fmt.Sprintf("%s-build", flags.AWSPrefix),
			codeBuildPolicy(logGroupArn, s3Arn, *repoMetadata.Arn, dynamoDBArn))
		err = iam.AttachRolePolicy(*buildPolicy.Arn, *buildRole.RoleName)
		if err != nil {
			common.Logger.Fatalf("Failed to attach build policy to role %s: %s", *buildRole.RoleName, err)
		}
	} else {
		buildRole = iam.GetRole(fmt.Sprintf("%s-build", flags.AWSPrefix))
	}

	codeBuild := service.NewBuilder(awsConfig)
	err = codeBuild.CreateProject(fmt.Sprintf("%s-build", flags.AWSPrefix), *buildRole.Arn, logGroup, logStream, s3Arn, *repoMetadata.CloneUrlHttp)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodeBuild project: %s", err)
	}

	pipelineRole := iam.CreateRole(fmt.Sprintf("%s-pipeline", flags.AWSPrefix), []service.PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codepipeline.amazonaws.com"},
	}})
	if pipelineRole != nil {
		pipelinePolicy := iam.CreatePolicy(fmt.Sprintf("%s-pipeline", flags.AWSPrefix), codePipelinePolicy(s3Arn))
		err = iam.AttachRolePolicy(*pipelinePolicy.Arn, *pipelineRole.RoleName)
		if err != nil {
			common.Logger.Fatalf("Failed to attach pipeline policy to role %s: %s", *pipelineRole.RoleName, err)
		}
	}
}

func setupCodeCommit(awsConfig aws.Config, accountID string, prefix string, branch string) service.CodeCommit {
	repoName := fmt.Sprintf("%s-%s", prefix, accountID)
	codeCommit := service.NewCodeCommit(awsConfig, repoName, branch)
	err := codeCommit.CreateRepository()
	if err != nil {
		common.Logger.Fatalf("Failed to create CodeCommit repository: %s", err)
	}
	codeCommit.PutFile("README.md", []byte("# Entigo infralib repository\nThis is the README file."))
	return codeCommit
}

func codePipelinePolicy(s3Arn string) []service.PolicyStatement {
	return []service.PolicyStatement{{
		Effect:   "Allow",
		Resource: []string{s3Arn},
		Action: []string{
			"s3:*",
		},
	}, {
		Effect:   "Allow",
		Resource: []string{"*"},
		Action: []string{
			"codebuild:BatchGetBuilds",
			"codebuild:StartBuild",
		},
	}}
}

func codeBuildPolicy(logGroupArn string, s3Arn string, repoArn string, dynamodbArn string) []service.PolicyStatement {
	return []service.PolicyStatement{{
		Effect:   "Allow",
		Resource: []string{logGroupArn, fmt.Sprintf("%s:*", logGroupArn)},
		Action: []string{
			"logs:CreateLogGroup",
			"logs:CreateLogStream",
			"logs:PutLogEvents",
		},
	}, {
		Effect:   "Allow",
		Resource: []string{"arn:aws:s3:::*"},
		Action:   []string{"s3:ListBucket"},
	}, {
		Effect:   "Allow",
		Resource: []string{s3Arn},
		Action: []string{
			"s3:PutObject",
			"s3:GetObject",
			"s3:GetObjectVersion",
			"s3:GetBucketAcl",
			"s3:GetBucketLocation",
			"s3:ListBucket",
		},
	}, {
		Effect:   "Allow",
		Resource: []string{repoArn},
		Action: []string{
			"codecommit:GetCommit",
			"codecommit:ListBranches",
			"codecommit:GetRepository",
			"codecommit:GetBranch",
			"codecommit:GitPull",
		},
	}, {
		Effect:   "Allow",
		Resource: []string{dynamodbArn},
		Action: []string{
			"dynamodb:GetItem",
			"dynamodb:PutItem",
			"dynamodb:DeleteItem",
		},
	}}
}
