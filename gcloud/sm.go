package gcloud

import (
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
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
		return nil, err
	}
	if result.Payload == nil {
		return nil, fmt.Errorf("secret %s not found", name)
	}
	value := string(result.Payload.Data)
	return &model.Parameter{
		Value: &value,
	}, nil
}
