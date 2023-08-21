package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

type IAM interface {
	AttachRolePolicy(policyArn string, roleName string) error
	CreatePolicy(policyName string, statement []PolicyStatement) *types.Policy
	CreateRole(roleName string, statement []PolicyStatement) *types.Role
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
