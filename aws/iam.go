package aws

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
	DeleteRolePolicyAttachment(policyName string, roleName string) error
	CreatePolicy(policyName string, statement []PolicyStatement) *types.Policy
	DeletePolicy(policyName string, accountId string) error
	CreateRole(roleName string, statement []PolicyStatement) *types.Role
	DeleteRole(roleName string) error
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
	result, err := i.iamClient.CreateRole(context.Background(), &iam.CreateRoleInput{
		AssumeRolePolicyDocument: getPolicy(statement),
		RoleName:                 aws.String(roleName),
	})
	if err != nil {
		var awsError *types.EntityAlreadyExistsException
		if errors.As(err, &awsError) {
			return nil
		} else {
			common.Logger.Fatalf("Failed to create role %s: %s", roleName, err)
		}
	}
	common.Logger.Printf("Created IAM role: %s\n", roleName)
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
	result, err := i.iamClient.CreatePolicy(context.Background(), &iam.CreatePolicyInput{
		PolicyDocument: getPolicy(statement),
		PolicyName:     aws.String(policyName),
	})
	if err != nil {
		var awsError *types.EntityAlreadyExistsException
		if errors.As(err, &awsError) {
			return nil
		} else {
			common.Logger.Fatalf("Failed to create policy %s: %s", policyName, err)
		}
	}
	common.Logger.Printf("Created IAM policy: %s\n", policyName)
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

func (i *identity) DeleteRolePolicyAttachment(policyArn string, roleName string) error {
	_, err := i.iamClient.DetachRolePolicy(context.Background(), &iam.DetachRolePolicyInput{
		PolicyArn: aws.String(policyArn),
		RoleName:  aws.String(roleName),
	})
	if err != nil {
		var awsError *types.NoSuchEntityException
		if errors.As(err, &awsError) {
			return nil
		}
	}
	return err
}

func (i *identity) DeletePolicy(policyName string, accountId string) error {
	policyArn := fmt.Sprintf("arn:aws:iam::%s:policy/%s", accountId, policyName)
	_, err := i.iamClient.DeletePolicy(context.Background(), &iam.DeletePolicyInput{PolicyArn: aws.String(policyArn)})
	if err != nil {
		var awsError *types.NoSuchEntityException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	common.Logger.Printf("Deleted IAM policy: %s\n", policyName)
	return nil
}

func (i *identity) DeleteRole(roleName string) error {
	_, err := i.iamClient.DeleteRole(context.Background(), &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		var awsError *types.NoSuchEntityException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	common.Logger.Printf("Deleted IAM role: %s\n", roleName)
	return nil
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
	}, CodeBuildRepoPolicy(repoArn), {
		Effect:   "Allow",
		Resource: []string{dynamodbArn},
		Action: []string{
			"dynamodb:GetItem",
			"dynamodb:PutItem",
			"dynamodb:DeleteItem",
		},
	}}
}

func CodeBuildRepoPolicy(repoArn string) PolicyStatement {
	return PolicyStatement{
		Effect:   "Allow",
		Resource: []string{repoArn},
		Action: []string{
			"codecommit:GetCommit",
			"codecommit:ListBranches",
			"codecommit:GetRepository",
			"codecommit:GetBranch",
			"codecommit:GitPull",
		},
	}
}

func CodePipelinePolicy(s3Arn string, repoArn string) []PolicyStatement {
	return []PolicyStatement{{
		Effect:   "Allow",
		Resource: []string{s3Arn, fmt.Sprintf("%s/*", s3Arn)},
		Action: []string{
			"s3:*",
		},
	}, CodePipelineRepoPolicy(repoArn), {
		Effect:   "Allow",
		Resource: []string{"*"},
		Action: []string{
			"codebuild:StartBuild",
			"codebuild:BatchGetBuilds",
			"codebuild:StopBuild",
		},
	}}
}

func CodePipelineRepoPolicy(repoArn string) PolicyStatement {
	return PolicyStatement{
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
	}
}
