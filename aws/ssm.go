package aws

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagTypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smTypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	awsSSM "github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log"
	"strings"
	"time"
)

type ssm struct {
	ctx       context.Context
	ssmClient *awsSSM.Client
	smClient  *secretsmanager.Client
	tagClient *resourcegroupstaggingapi.Client
	kmsKeyId  string
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
	output, err := s.ssmClient.GetParameter(s.ctx, &awsSSM.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		var notFoundErr *types.ParameterNotFound
		if !errors.As(err, &notFoundErr) {
			return err
		}
		output = nil
	}
	if output != nil && *output.Parameter.Value == value {
		return nil
	}
	input := &awsSSM.PutParameterInput{
		Name:  aws.String(name),
		Value: aws.String(value),
		Type:  types.ParameterTypeSecureString,
	}
	if s.kmsKeyId != "" {
		input.KeyId = &s.kmsKeyId
	}
	if output != nil {
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

func (s *ssm) AddEncryptionKeyId(keyId string) {
	s.kmsKeyId = keyId
}

func (s *ssm) PutSecret(name string, value string) error {
	secret, kmsKeyId, err := s.getSecret(name)
	if err != nil {
		return err
	}
	if secret == nil {
		return s.createSecret(name, value)
	}
	if (kmsKeyId == nil && s.kmsKeyId != "") || (kmsKeyId != nil && s.kmsKeyId != "" && *kmsKeyId != s.kmsKeyId) {
		return s.updateKmsKey(name, value)
	}
	if *secret == value {
		return nil
	}
	return s.updateSecret(name, value)
}

func (s *ssm) getSecret(name string) (*string, *string, error) {
	described, err := s.smClient.DescribeSecret(s.ctx, &secretsmanager.DescribeSecretInput{
		SecretId: aws.String(name),
	})
	if err != nil {
		var notFoundError *smTypes.ResourceNotFoundException
		if errors.As(err, &notFoundError) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if described.DeletedDate != nil {
		_, err = s.smClient.RestoreSecret(s.ctx, &secretsmanager.RestoreSecretInput{
			SecretId: aws.String(name),
		})
		if err != nil {
			return nil, nil, err
		}
	}
	secret, err := s.smClient.GetSecretValue(s.ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(name),
	})
	if err == nil {
		return secret.SecretString, described.KmsKeyId, nil
	}
	var notFoundError *smTypes.ResourceNotFoundException
	if errors.As(err, &notFoundError) {
		return nil, nil, nil
	}
	return nil, nil, err
}

func (s *ssm) createSecret(name, value string) error {
	input := secretsmanager.CreateSecretInput{
		Name:         aws.String(name),
		SecretString: aws.String(value),
		Tags: []smTypes.Tag{
			{
				Key:   aws.String(model.ResourceTagKey),
				Value: aws.String(model.ResourceTagValue),
			},
		},
	}
	if s.kmsKeyId != "" {
		input.KmsKeyId = aws.String(s.kmsKeyId)
	}
	_, err := s.smClient.CreateSecret(s.ctx, &input)
	return err
}

func (s *ssm) updateSecret(name, value string) error {
	_, err := s.smClient.PutSecretValue(s.ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     aws.String(name),
		SecretString: aws.String(value),
	})
	return err
}

func (s *ssm) updateKmsKey(name string, value string) error {
	err := s.deleteSecret(name, true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	delay := 1
	log.Printf("updating kms key for secret %s, timeout %s\n", name, 2*time.Minute)
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("failed to update kms key for secret %s", name)
			}
			return ctx.Err()
		default:
			err = s.createSecret(name, value)
			if err == nil {
				return nil
			}
			var invalidError *smTypes.InvalidRequestException
			if !errors.As(err, &invalidError) {
				return err
			}
			time.Sleep(time.Duration(delay) * time.Second)
			delay = util.MinInt(2*delay, 8)
		}
	}
}

func (s *ssm) DeleteSecret(name string) error {
	return s.deleteSecret(name, false)
}

func (s *ssm) deleteSecret(name string, force bool) error {
	input := secretsmanager.DeleteSecretInput{
		SecretId:                   aws.String(name),
		ForceDeleteWithoutRecovery: &force,
	}
	_, err := s.smClient.DeleteSecret(s.ctx, &input)
	if err == nil {
		log.Printf("Deleted secret: %s\n", name)
		return nil
	}
	var notFoundError *smTypes.ResourceNotFoundException
	if errors.As(err, &notFoundError) {
		return nil
	}
	return err
}
