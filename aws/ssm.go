package aws

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsSSM "github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type ssm struct {
	ctx       context.Context
	ssmClient *awsSSM.Client
}

func NewSSM(ctx context.Context, awsConfig aws.Config) model.SSM {
	return &ssm{
		ctx:       ctx,
		ssmClient: awsSSM.NewFromConfig(awsConfig),
	}
}

func (s *ssm) GetParameter(name string) (*model.Parameter, error) {
	result, err := s.ssmClient.GetParameter(s.ctx, &awsSSM.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		var notFoundErr *types.ParameterNotFound
		if errors.As(err, &notFoundErr) {
			return nil, &model.ParameterNotFoundError{Name: name, Err: err}
		}
		return nil, err
	}
	return &model.Parameter{
		Value: result.Parameter.Value,
		Type:  string(result.Parameter.Type),
	}, nil
}

func (s *ssm) PutParameter(name string, value string) error {
	_, err := s.ssmClient.PutParameter(s.ctx, &awsSSM.PutParameterInput{
		Name:      aws.String(name),
		Value:     aws.String(value),
		Type:      types.ParameterTypeSecureString,
		Overwrite: aws.Bool(true),
	})
	return err
}
