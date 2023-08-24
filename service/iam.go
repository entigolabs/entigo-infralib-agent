package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

type IAM interface {
	AttachRolePolicy(policyArn string, roleName string) error
	CreatePolicy(policyName string, statement []PolicyStatement) *types.Policy
	CreateRole(roleName string, statement []PolicyStatement) *types.Role
	GetRole(roleName string) *types.Role
}

type PolicyDocument struct {
	Version   string
	Statement []PolicyStatement
}

type PolicyStatement struct {
	Effect    string
	Action    []string
	Principal map[string]string `json:",omitempty"`
	Resource  []string          `json:",omitempty"`
}

type identity struct {
	iamClient *iam.Client
}

func NewIAM(config aws.Config) IAM {
	return &identity{
		iamClient: iam.NewFromConfig(config),
	}
}

func (i *identity) CreateRole(roleName string, statement []PolicyStatement) *types.Role {
	common.Logger.Printf("Creating IAM role: %s\n", roleName)
	result, err := i.iamClient.CreateRole(context.Background(), &iam.CreateRoleInput{
		AssumeRolePolicyDocument: getPolicy(statement),
		RoleName:                 aws.String(roleName),
	})
	if err != nil {
		var awsError *types.EntityAlreadyExistsException
		if errors.As(err, &awsError) {
			common.Logger.Printf("Role %s already exists. Continuing...\n", roleName)
			return nil
		} else {
			common.Logger.Fatalf("Failed to create role %s: %s", roleName, err)
		}
	}
	return result.Role
}

func (i *identity) GetRole(roleName string) *types.Role {
	role, err := i.iamClient.GetRole(context.Background(), &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		common.Logger.Fatalf("Failed to get role %s: %s", roleName, err)
	}
	return role.Role
}

func (i *identity) CreatePolicy(policyName string, statement []PolicyStatement) *types.Policy {
	common.Logger.Printf("Creating IAM policy: %s\n", policyName)
	result, err := i.iamClient.CreatePolicy(context.Background(), &iam.CreatePolicyInput{
		PolicyDocument: getPolicy(statement),
		PolicyName:     aws.String(policyName),
	})
	if err != nil {
		var awsError *types.EntityAlreadyExistsException
		if errors.As(err, &awsError) {
			common.Logger.Printf("Policy %s already exists. Continuing...\n", policyName)
			return nil
		} else {
			common.Logger.Fatalf("Failed to create policy %s: %s", policyName, err)
		}
	}
	return result.Policy
}

func getPolicy(statements []PolicyStatement) *string {
	policy := PolicyDocument{
		Version:   "2012-10-17",
		Statement: statements,
	}
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal policy: %s", err)
	}
	return aws.String(string(policyBytes))
}

func (i *identity) AttachRolePolicy(policyArn string, roleName string) error {
	_, err := i.iamClient.AttachRolePolicy(context.Background(), &iam.AttachRolePolicyInput{
		PolicyArn: aws.String(policyArn),
		RoleName:  aws.String(roleName),
	})
	return err
}

func CodeBuildPolicy(logGroupArn string, s3Arn string, repoArn string, dynamodbArn string) []PolicyStatement {
	return []PolicyStatement{{
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

func CodePipelinePolicy(s3Arn string, repoArn string) []PolicyStatement {
	return []PolicyStatement{{
		Effect:   "Allow",
		Resource: []string{s3Arn, fmt.Sprintf("%s/*", s3Arn)},
		Action: []string{
			"s3:*",
		},
	}, {
		Effect:   "Allow",
		Resource: []string{repoArn, fmt.Sprintf("%s/*", repoArn)},
		Action: []string{
			"codecommit:GetCommit",
			"codecommit:GetBranch",
			"codecommit:GetRepository",
			"codecommit:UploadArchive",
			"codecommit:GetUploadArchiveStatus",
			"codecommit:CancelUploadArchive",
		},
	}, {
		Effect:   "Allow",
		Resource: []string{"*"},
		Action: []string{
			"codebuild:StartBuild",
			"codebuild:BatchGetBuilds",
			"codebuild:StopBuild",
		},
	}}
}
