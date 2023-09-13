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
	GetVpcConfig(prefix string, vpcPrefix string, workspace string) *types.VpcConfig
	GetArgoCDRepoUrl(prefix string, argoCDPrefix string, workspace string) string
}

type ssm struct {
	ssmClient *awsSSM.Client
}

func NewSSM(awsConfig aws.Config) SSM {
	return &ssm{
		ssmClient: awsSSM.NewFromConfig(awsConfig),
	}
}

func (s *ssm) GetParameter(name string) (string, error) {
	result, err := s.ssmClient.GetParameter(context.Background(), &awsSSM.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	return *result.Parameter.Value, nil
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
		SecurityGroupIds: strings.Split(securityGroupIds, ","),
		Subnets:          strings.Split(subnetIds, ","),
		VpcId:            aws.String(vpcId),
	}
}

func (s *ssm) GetArgoCDRepoUrl(prefix string, argoCDPrefix string, workspace string) string {
	if argoCDPrefix == "" {
		common.Logger.Fatalf("argoCDPrefix is missing")
	}
	argoCDPrefix = fmt.Sprintf("%s-%s-%s", prefix, argoCDPrefix, workspace)
	repoUrl, err := s.GetParameter(fmt.Sprintf("/entigo-infralib/%s/argocd/repo_url", argoCDPrefix))
	if err != nil {
		common.Logger.Fatalf("failed to get argoCD repourl: %s", err)
	}
	return repoUrl
}
