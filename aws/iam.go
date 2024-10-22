package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"log"
)

type IAM interface {
	AttachRolePolicy(policyArn string, roleName string) error
	DeleteRolePolicyAttachment(policyName string, roleName string) error
	CreatePolicy(policyName string, statement []PolicyStatement) *types.Policy
	DeletePolicy(policyName string, accountId string) error
	UpdatePolicy(policyName string, statement []PolicyStatement) string
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
	ctx       context.Context
	accountId string
	iamClient *iam.Client
}

func NewIAM(ctx context.Context, config aws.Config, accountId string) IAM {
	return &identity{
		ctx:       ctx,
		accountId: accountId,
		iamClient: iam.NewFromConfig(config),
	}
}

func (i *identity) CreateRole(roleName string, statement []PolicyStatement) *types.Role {
	result, err := i.iamClient.CreateRole(i.ctx, &iam.CreateRoleInput{
		AssumeRolePolicyDocument: getPolicy(statement),
		RoleName:                 aws.String(roleName),
	})
	if err != nil {
		var awsError *types.EntityAlreadyExistsException
		if errors.As(err, &awsError) {
			return nil
		} else {
			log.Fatalf("Failed to create role %s: %s", roleName, err)
		}
	}
	log.Printf("Created IAM role: %s\n", roleName)
	return result.Role
}

func (i *identity) GetRole(roleName string) *types.Role {
	role, err := i.iamClient.GetRole(i.ctx, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		log.Fatalf("Failed to get role %s: %s", roleName, err)
	}
	return role.Role
}

func (i *identity) CreatePolicy(policyName string, statement []PolicyStatement) *types.Policy {
	result, err := i.iamClient.CreatePolicy(i.ctx, &iam.CreatePolicyInput{
		PolicyDocument: getPolicy(statement),
		PolicyName:     aws.String(policyName),
	})
	if err != nil {
		var awsError *types.EntityAlreadyExistsException
		if errors.As(err, &awsError) {
			return nil
		} else {
			log.Fatalf("Failed to create policy %s: %s", policyName, err)
		}
	}
	log.Printf("Created IAM policy: %s\n", policyName)
	return result.Policy
}

func (i *identity) UpdatePolicy(policyName string, statement []PolicyStatement) string {
	policyArn := fmt.Sprintf("arn:aws:iam::%s:policy/%s", i.accountId, policyName)

	versionsOutput, err := i.iamClient.ListPolicyVersions(i.ctx, &iam.ListPolicyVersionsInput{
		PolicyArn: aws.String(policyArn),
	})
	if err != nil {
		log.Fatalf("Failed to list policy versions for %s: %s", policyName, err)
	}
	if len(versionsOutput.Versions) >= 5 {
		i.deleteOldestPolicyVersion(policyArn, versionsOutput.Versions)
	}

	_, err = i.iamClient.CreatePolicyVersion(i.ctx, &iam.CreatePolicyVersionInput{
		PolicyArn:      aws.String(policyArn),
		PolicyDocument: getPolicy(statement),
		SetAsDefault:   true,
	})
	if err != nil {
		log.Fatalf("Failed to update policy %s: %s", policyName, err)
	}
	log.Printf("Updated IAM policy: %s\n", policyName)
	return policyArn
}

func getPolicy(statements []PolicyStatement) *string {
	policy := PolicyDocument{
		Version:   "2012-10-17",
		Statement: statements,
	}
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		log.Fatalf("Failed to marshal policy: %s", err)
	}
	return aws.String(string(policyBytes))
}

func (i *identity) AttachRolePolicy(policyArn string, roleName string) error {
	_, err := i.iamClient.AttachRolePolicy(i.ctx, &iam.AttachRolePolicyInput{
		PolicyArn: aws.String(policyArn),
		RoleName:  aws.String(roleName),
	})
	return err
}

func (i *identity) DeleteRolePolicyAttachment(policyArn string, roleName string) error {
	_, err := i.iamClient.DetachRolePolicy(i.ctx, &iam.DetachRolePolicyInput{
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
	_, err := i.iamClient.DeletePolicy(i.ctx, &iam.DeletePolicyInput{PolicyArn: aws.String(policyArn)})
	if err != nil {
		var awsError *types.NoSuchEntityException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	log.Printf("Deleted IAM policy: %s\n", policyName)
	return nil
}

func (i *identity) DeleteRole(roleName string) error {
	_, err := i.iamClient.DeleteRole(i.ctx, &iam.DeleteRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		var awsError *types.NoSuchEntityException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	log.Printf("Deleted IAM role: %s\n", roleName)
	return nil
}

func (i *identity) deleteOldestPolicyVersion(policyArn string, versions []types.PolicyVersion) {
	var oldestVersion *types.PolicyVersion
	for _, version := range versions {
		if oldestVersion == nil || version.CreateDate.Before(*oldestVersion.CreateDate) {
			oldestVersion = &version
		}
	}
	if oldestVersion != nil {
		_, err := i.iamClient.DeletePolicyVersion(i.ctx, &iam.DeletePolicyVersionInput{
			PolicyArn: aws.String(policyArn),
			VersionId: oldestVersion.VersionId,
		})
		if err != nil {
			log.Fatalf("Failed to delete oldest policy version for %s: %s", policyArn, err)
		}
	}
}

func CodeBuildPolicy(logGroupArn string, s3Arn string, dynamodbArn string) []PolicyStatement {
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
	}, CodeBuildS3Policy(s3Arn), {
		Effect:   "Allow",
		Resource: []string{dynamodbArn},
		Action: []string{
			"dynamodb:GetItem",
			"dynamodb:PutItem",
			"dynamodb:DeleteItem",
		},
	}}
}

func CodeBuildS3Policy(s3Arn string) PolicyStatement {
	return PolicyStatement{
		Effect:   "Allow",
		Resource: []string{s3Arn, fmt.Sprintf("%s/*", s3Arn)},
		Action: []string{
			"s3:PutObject",
			"s3:GetObject",
			"s3:GetObjectVersion",
			"s3:GetBucketAcl",
			"s3:GetBucketLocation",
			"s3:ListBucket",
		},
	}
}

func CodePipelinePolicy(s3Arn string) []PolicyStatement {
	return []PolicyStatement{{
		Effect:   "Allow",
		Resource: []string{"arn:aws:s3:::*"},
		Action:   []string{"s3:ListBucket"},
	}, CodePipelineS3Policy(s3Arn),
		{
			Effect:   "Allow",
			Resource: []string{"*"},
			Action: []string{
				"codebuild:StartBuild",
				"codebuild:BatchGetBuilds",
				"codebuild:StopBuild",
			},
		}}
}

func CodePipelineS3Policy(s3Arn string) PolicyStatement {
	return PolicyStatement{
		Effect:   "Allow",
		Resource: []string{s3Arn, fmt.Sprintf("%s/*", s3Arn)},
		Action: []string{
			"s3:*",
		},
	}
}
