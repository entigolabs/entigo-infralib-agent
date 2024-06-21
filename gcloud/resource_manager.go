package gcloud

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iam/v1"
)

type iamv1 struct {
	ctx     context.Context
	service *iam.Service
}

type csmv1 struct {
	ctx     context.Context
	service *cloudresourcemanager.Service
}

type resourceManager struct {
	iam *iamv1
	csm *csmv1
}

func NewResourceManager(ctx context.Context) (*resourceManager, error) {
	rm := &resourceManager{}
	err := rm.NewIAM(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IAM service")
	}
	err = rm.NewCSM(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Cloud Resource Manager service")
	}
	return rm, nil
}

func (rm *resourceManager) NewIAM(ctx context.Context) error {
	service, err := iam.NewService(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize IAM service: %w", err)
	}
	rm.iam = &iamv1{
		ctx:     ctx,
		service: service,
	}
	return nil
}

func (rm *resourceManager) NewCSM(ctx context.Context) error {
	service, err := cloudresourcemanager.NewService(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize Cloud Resource Manager service: %w", err)
	}
	rm.csm = &csmv1{
		ctx:     ctx,
		service: service,
	}
	return nil
}

func (rm *resourceManager) GetServiceAccount(serviceAccountName string) (*iam.ServiceAccount, error) {
	account, err := rm.iam.service.Projects.ServiceAccounts.Get(serviceAccountName).Do()
	if err != nil {
		if gerr, ok := err.(*googleapi.Error); ok && gerr.Code == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get service account: %w", err)
	}
	return account, nil
}

func (rm *resourceManager) createServiceAccount(projectID, name, displayName string) (*iam.ServiceAccount, error) {
	account, err := rm.iam.service.Projects.ServiceAccounts.Create("projects/"+projectID, &iam.CreateServiceAccountRequest{
		AccountId: name,
		ServiceAccount: &iam.ServiceAccount{
			DisplayName: displayName,
		},
	}).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to create service account: %w", err)
	}
	return account, nil
}

func (rm *resourceManager) GetOrCreateServiceAccount(projectID, name, displayName string) (*iam.ServiceAccount, error) {
	foundSA, err := rm.GetServiceAccount("projects/" + projectID + "/serviceAccounts/" + name + "@" + projectID + ".iam.gserviceaccount.com")
	if err != nil {
		return nil, fmt.Errorf("failed to check if service account exists: %w", err)
	}
	if foundSA == nil {
		createdSA, err := rm.createServiceAccount(projectID, name, displayName)
		if err != nil {
			return nil, fmt.Errorf("failed to create service account: %w", err)
		}
		common.Logger.Printf("Created new service account: %v", createdSA)
		return createdSA, nil
	}
	common.Logger.Printf("Found existing service account: %v", foundSA)
	return foundSA, nil
}

func (rm *resourceManager) AddRolesToServiceAccount(serviceAccountName string, roles []string) error {
	policy, err := rm.iam.service.Projects.ServiceAccounts.GetIamPolicy(serviceAccountName).Do()
	if err != nil {
		return fmt.Errorf("failed to get IAM policy: %w", err)
	}
	parts := strings.Split(serviceAccountName, "/")
	if len(parts) < 2 {
		return fmt.Errorf("invalid service account name format")
	}
	emailAddress := parts[len(parts)-1]
	for _, role := range roles {
		policy.Bindings = append(policy.Bindings, &iam.Binding{
			Role:    role,
			Members: []string{"serviceAccount:" + emailAddress},
		})
	}
	_, err = rm.iam.service.Projects.ServiceAccounts.SetIamPolicy(serviceAccountName, &iam.SetIamPolicyRequest{
		Policy: policy,
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to set IAM policy: %w", err)
	}
	return nil
}

func (rm *resourceManager) AddRolesToProject(serviceAccountName string, roles []string) error {
	parts := strings.Split(serviceAccountName, "/")
	if len(parts) < 4 {
		return fmt.Errorf("invalid service account name format")
	}
	projectID := parts[1]
	emailAddress := parts[len(parts)-1]

	policy, err := rm.csm.service.Projects.GetIamPolicy(projectID, &cloudresourcemanager.GetIamPolicyRequest{}).Do()
	if err != nil {
		return fmt.Errorf("failed to get IAM policy: %w", err)
	}
	for _, role := range roles {
		policy.Bindings = append(policy.Bindings, &cloudresourcemanager.Binding{
			Role:    role,
			Members: []string{"serviceAccount:" + emailAddress},
		})
	}
	_, err = rm.csm.service.Projects.SetIamPolicy(projectID, &cloudresourcemanager.SetIamPolicyRequest{
		Policy: policy,
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to set IAM policy: %w", err)
	}
	return nil
}
