package service

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsSSM "github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type SSM interface {
	GetParameter(name string) (*ssmTypes.Parameter, error)
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
