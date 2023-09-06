package service

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	awsSSM "github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"strings"
)

type SSM interface {
	GetParameter(name string) (string, error)
	GetVpcConfig(vpcPrefix string, workspace string) *types.VpcConfig
}

type ssm struct {
	ssmClient *awsSSM.Client
}

func NewSSM(awsConfig aws.Config) SSM {
	return &ssm{
		ssmClient: awsSSM.NewFromConfig(awsConfig),
	}
}

func (s ssm) GetParameter(name string) (string, error) {
	result, err := s.ssmClient.GetParameter(context.Background(), &awsSSM.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	return *result.Parameter.Value, nil
}

func (s ssm) GetVpcConfig(vpcPrefix string, workspace string) *types.VpcConfig {
	if vpcPrefix == "" {
		return nil
	}
	common.Logger.Printf("Getting VPC config for %s-%s\n", vpcPrefix, workspace)
	vpcId, err := s.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/vpc_id", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get VPC ID: %s", err)
	}
	subnetIds, err := s.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/private_subnets", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get subnet IDs: %s", err)
	}
	securityGroupIds, err := s.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/pipeline_security_group", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get security group IDs: %s", err)
	}
	return &types.VpcConfig{
		SecurityGroupIds: strings.Split(securityGroupIds, ","),
		Subnets:          strings.Split(subnetIds, ","),
		VpcId:            aws.String(vpcId),
	}
}
