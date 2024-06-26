package aws

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsSSM "github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type ssm struct {
	ssmClient *awsSSM.Client
}

func NewSSM(awsConfig aws.Config) model.SSM {
	return &ssm{
		ssmClient: awsSSM.NewFromConfig(awsConfig),
	}
}

func (s *ssm) GetParameter(name string) (*model.Parameter, error) {
	result, err := s.ssmClient.GetParameter(context.Background(), &awsSSM.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	return &model.Parameter{
		Value: result.Parameter.Value,
		Type:  string(result.Parameter.Type),
	}, nil
}
