package gcloud

import (
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
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

func (s *sm) PutParameter(name string, value string) error {
	secret, err := s.client.CreateSecret(s.ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", s.projectId),
		SecretId: name,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{},
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = s.client.AddSecretVersion(s.ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent: secret.Name,
		Payload: &secretmanagerpb.SecretPayload{
			Data: []byte(value),
		},
	})
	return err
}
