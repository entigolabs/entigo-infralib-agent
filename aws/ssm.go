package aws

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagTypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	awsSSM "github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"strings"
)

type ssm struct {
	ctx       context.Context
	ssmClient *awsSSM.Client
	smClient  *secretsmanager.Client
	tagClient *resourcegroupstaggingapi.Client
}

func NewSSM(ctx context.Context, awsConfig aws.Config) model.SSM {
	return &ssm{
		ctx:       ctx,
		ssmClient: awsSSM.NewFromConfig(awsConfig),
		smClient:  secretsmanager.NewFromConfig(awsConfig),
		tagClient: resourcegroupstaggingapi.NewFromConfig(awsConfig),
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

func (s *ssm) ParameterExists(name string) (bool, error) {
	_, err := s.GetParameter(name)
	if err != nil {
		var notFoundErr *model.ParameterNotFoundError
		if errors.As(err, &notFoundErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *ssm) PutParameter(name string, value string) error {
	param, err := s.GetParameter(name)
	if err != nil {
		var notFoundErr *model.ParameterNotFoundError
		if !errors.As(err, &notFoundErr) {
			return err
		}
	}
	if param != nil && *param.Value == value {
		return nil
	}
	input := &awsSSM.PutParameterInput{
		Name:  aws.String(name),
		Value: aws.String(value),
		Type:  types.ParameterTypeSecureString,
	}
	if param == nil {
		input.Overwrite = aws.Bool(true)
	} else {
		input.Tags = []types.Tag{{
			Key:   aws.String(model.ResourceTagKey),
			Value: aws.String(model.ResourceTagValue),
		}}
	}
	_, err = s.ssmClient.PutParameter(s.ctx, input)
	return err
}

func (s *ssm) DeleteParameter(name string) error {
	_, err := s.ssmClient.DeleteParameter(s.ctx, &awsSSM.DeleteParameterInput{
		Name: aws.String(name),
	})
	if err != nil {
		var notFoundErr *types.ParameterNotFound
		if errors.As(err, &notFoundErr) {
			return nil
		}
		return err
	}
	return nil
}

func (s *ssm) ListParameters() ([]string, error) {
	var keys []string
	input := &resourcegroupstaggingapi.GetResourcesInput{
		ResourceTypeFilters: []string{"ssm:parameter"},
		TagFilters: []tagTypes.TagFilter{
			{
				Key:    aws.String(model.ResourceTagKey),
				Values: []string{model.ResourceTagValue},
			},
		},
	}
	paginator := resourcegroupstaggingapi.NewGetResourcesPaginator(s.tagClient, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(s.ctx)
		if err != nil {
			return nil, err
		}
		for _, resourceTagMapping := range page.ResourceTagMappingList {
			parts := strings.Split(*resourceTagMapping.ResourceARN, ":")
			key := parts[len(parts)-1]
			slashIndex := strings.Index(key, "/")
			if slashIndex != -1 {
				key = key[slashIndex+1:]
			}
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func (s *ssm) PutSecret(name string, value string) error {
	secret, err := s.getSecret(name)
	if err != nil {
		return err
	}
	if secret == nil {
		return s.createSecret(name, value)
	}
	if *secret != value {
		return nil
	}
	return s.updateSecret(name, value)
}

func (s *ssm) getSecret(name string) (*string, error) {
	secret, err := s.smClient.GetSecretValue(s.ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(name),
	})
	if err == nil {
		return secret.SecretString, nil
	}
	var notFoundError *types.ResourceNotFoundException
	if errors.As(err, &notFoundError) {
		return nil, nil
	}
	return nil, err
}

func (s *ssm) createSecret(name, value string) error {
	_, err := s.smClient.CreateSecret(s.ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(name),
		SecretString: aws.String(value),
	})
	return err
}

func (s *ssm) updateSecret(name, value string) error {
	_, err := s.smClient.PutSecretValue(s.ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     aws.String(name),
		SecretString: aws.String(value),
	})
	return err
}

func (s *ssm) DeleteSecret(name string) error {
	_, err := s.smClient.DeleteSecret(s.ctx, &secretsmanager.DeleteSecretInput{
		SecretId: aws.String(name),
	})
	if err == nil {
		return nil
	}
	var notFoundError *types.ResourceNotFoundException
	if errors.As(err, &notFoundError) {
		return nil
	}
	return err
}
