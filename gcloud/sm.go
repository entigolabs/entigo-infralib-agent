package gcloud

import (
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"log/slog"
	"strings"
)

type sm struct {
	ctx       context.Context
	client    *secretmanager.Client
	projectId string
}

func NewSM(ctx context.Context, projectId string) (model.SSM, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &sm{
		ctx:       ctx,
		client:    client,
		projectId: projectId,
	}, nil
}

func (s *sm) AddEncryptionKeyId(_ string) {
	slog.Warn("AddEncryptionKeyId is not supported for GCP")
}

func (s *sm) GetParameter(name string) (*model.Parameter, error) {
	name = strings.Replace(strings.TrimLeft(name, "/"), "/", "-", -1)
	result, err := s.client.AccessSecretVersion(s.ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s/versions/latest", s.projectId, name),
	})
	if err != nil {
		var apiError *apierror.APIError
		if errors.As(err, &apiError) && apiError.GRPCStatus().Code() == codes.NotFound {
			return nil, &model.ParameterNotFoundError{Name: name, Err: err}
		}
		return nil, err
	}
	if result.Payload == nil {
		return nil, &model.ParameterNotFoundError{Name: name}
	}
	value := string(result.Payload.Data)
	return &model.Parameter{
		Value: &value,
	}, nil
}

func (s *sm) ParameterExists(name string) (bool, error) {
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

func (s *sm) PutParameter(name string, value string) error {
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
	if param == nil {
		err = s.createSecret(name)
		if err != nil {
			return err
		}
	}
	_, err = s.client.AddSecretVersion(s.ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: fmt.Sprintf("projects/%s/secrets/%s", s.projectId, name),
		Payload: &secretmanagerpb.SecretPayload{
			Data: []byte(value),
		},
	})
	return err
}

func (s *sm) createSecret(name string) error {
	_, err := s.client.CreateSecret(s.ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", s.projectId),
		SecretId: name,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{},
			},
			Labels: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	return err
}

func (s *sm) DeleteParameter(name string) error {
	err := s.client.DeleteSecret(s.ctx, &secretmanagerpb.DeleteSecretRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s", s.projectId, name),
	})
	if err != nil {
		var apiError *apierror.APIError
		if errors.As(err, &apiError) && apiError.GRPCStatus().Code() == codes.NotFound {
			return nil
		}
		return err
	}
	return nil
}

func (s *sm) ListParameters() ([]string, error) {
	var keys []string
	secrets := s.client.ListSecrets(s.ctx, &secretmanagerpb.ListSecretsRequest{
		Parent: fmt.Sprintf("projects/%s", s.projectId),
		Filter: fmt.Sprintf("labels.%s:%s", model.ResourceTagKey, model.ResourceTagValue),
	})
	for {
		secret, err := secrets.Next()
		if err != nil {
			if errors.Is(err, iterator.Done) {
				break
			}
			return nil, err
		}
		key := secret.Name[strings.LastIndex(secret.Name, "/")+1:]
		keys = append(keys, key)
	}
	return keys, nil
}

func (s *sm) PutSecret(name string, value string) error {
	return s.PutParameter(name, value)
}

func (s *sm) DeleteSecret(name string) error {
	return s.DeleteParameter(name)
}
