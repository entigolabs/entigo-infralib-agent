package gcloud

import (
	"context"
	"fmt"
	"log"
	"strings"

	"google.golang.org/api/option"
	"google.golang.org/api/serviceusage/v1"
)

type ApiUsage struct {
	ctx       context.Context
	projectId string
	service   *serviceusage.Service
}

func NewApiUsage(ctx context.Context, options []option.ClientOption, projectId string) (*ApiUsage, error) {
	service, err := serviceusage.NewService(ctx, options...)
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
	_, err := a.service.Services.BatchEnable(fmt.Sprintf("projects/%s", a.projectId), &serviceusage.BatchEnableServicesRequest{
		ServiceIds: services,
	}).Do()
	if err != nil {
		return err
	}
	log.Printf("APIs enabled successfully: %s\n", strings.Join(services, ", "))
	return nil
}

func (a *ApiUsage) EnableService(service string) error {
	name := fmt.Sprintf("projects/%s/services/%s", a.projectId, service)
	apiService, err := a.service.Services.Get(name).Do()
	if err != nil {
		return err
	}
	if apiService.State == "ENABLED" {
		return nil
	}
	_, err = a.service.Services.Enable(name, &serviceusage.EnableServiceRequest{}).Do()
	if err != nil {
		return fmt.Errorf("error enabling API %s: %v", service, err)
	}
	log.Printf("API %s enabled successfully\n", service)
	return nil
}
