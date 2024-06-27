package gcloud

import (
	"context"
	"fmt"
	"google.golang.org/api/serviceusage/v1"
)

type ApiUsage struct {
	ctx       context.Context
	projectId string
	service   *serviceusage.Service
}

func NewApiUsage(ctx context.Context, projectId string) (*ApiUsage, error) {
	service, err := serviceusage.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize service usage: %v", err)
	}
	return &ApiUsage{
		ctx:       ctx,
		projectId: projectId,
		service:   service,
	}, nil
}

func (a *ApiUsage) EnableServices(services []string) error {
	for _, service := range services {
		if err := a.EnableService(service); err != nil {
			return err
		}
	}
	return nil
}

func (a *ApiUsage) EnableService(service string) error {
	_, err := a.service.Services.Enable(fmt.Sprintf("projects/%s/services/%s", a.projectId, service),
		&serviceusage.EnableServiceRequest{}).Do()
	if err != nil {
		return fmt.Errorf("error enabling API %s: %v", service, err)
	}
	fmt.Printf("API %s enabled successfully\n", service)
	return nil
}
