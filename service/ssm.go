package service

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	awsSSM "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"strings"
)

type SSM interface {
	GetParameter(name string) (*ssmTypes.Parameter, error)
	GetVpcConfig(prefix string, vpcPrefix string, workspace string) *types.VpcConfig
}

type ssm struct {
	ssmClient *awsSSM.Client
}

func NewSSM(awsConfig aws.Config) SSM {
	return &ssm{
		ssmClient: awsSSM.NewFromConfig(awsConfig),
	}
}

func (s *ssm) GetParameter(name string) (*ssmTypes.Parameter, error) {
	result, err := s.ssmClient.GetParameter(context.Background(), &awsSSM.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	return result.Parameter, nil
}

func (s *ssm) GetVpcConfig(prefix string, vpcPrefix string, workspace string) *types.VpcConfig {
	if vpcPrefix == "" {
		return nil
	}
	vpcPrefix = fmt.Sprintf("%s-%s-%s", prefix, vpcPrefix, workspace)
	common.Logger.Printf("Getting VPC config for %s\n", vpcPrefix)
	vpcId, err := s.GetParameter(fmt.Sprintf("/entigo-infralib/%s/vpc/vpc_id", vpcPrefix))
	if err != nil {
		common.Logger.Fatalf("Failed to get VPC ID: %s", err)
	}
	subnetIds, err := s.GetParameter(fmt.Sprintf("/entigo-infralib/%s/vpc/private_subnets", vpcPrefix))
	if err != nil {
		common.Logger.Fatalf("Failed to get subnet IDs: %s", err)
	}
	securityGroupIds, err := s.GetParameter(fmt.Sprintf("/entigo-infralib/%s/vpc/pipeline_security_group", vpcPrefix))
	if err != nil {
		common.Logger.Fatalf("Failed to get security group IDs: %s", err)
	}
	return &types.VpcConfig{
		SecurityGroupIds: strings.Split(*securityGroupIds.Value, ","),
		Subnets:          strings.Split(*subnetIds.Value, ","),
		VpcId:            aws.String(*vpcId.Value),
	}
}
