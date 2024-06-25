package gcloud

import (
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
	"net/http"
	"strings"
)

type IAM struct {
	ctx             context.Context
	service         *iamv1.Service
	resourceManager *cloudresourcemanager.Service
	projectId       string
}

func NewIAM(ctx context.Context, projectId string) (*IAM, error) {
	service, err := iamv1.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IAM service: %w", err)
	}
	resourceManager, err := cloudresourcemanager.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize Cloud Resource Manager service: %w", err)
	}
	return &IAM{
		ctx:             ctx,
		service:         service,
		resourceManager: resourceManager,
		projectId:       projectId,
	}, nil
}

func (iam *IAM) GetServiceAccount(serviceAccountName string) (*iamv1.ServiceAccount, error) {
	account, err := iam.service.Projects.ServiceAccounts.Get(serviceAccountName).Do()
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get service account: %w", err)
	}
	return account, nil
}

func (iam *IAM) GetOrCreateServiceAccount(name, displayName string) (*iamv1.ServiceAccount, error) {
	account, err := iam.GetServiceAccount(fmt.Sprintf("projects/%s/serviceAccounts/%s@%s.iam.gserviceaccount.com",
		iam.projectId, name, iam.projectId))
	if err != nil {
		return nil, err
	}
	if account != nil {
		return account, nil
	}
	account, err = iam.service.Projects.ServiceAccounts.Create(fmt.Sprintf("projects/%s", iam.projectId),
		&iamv1.CreateServiceAccountRequest{
			AccountId: name,
			ServiceAccount: &iamv1.ServiceAccount{
				DisplayName: displayName,
			},
		}).Do()
	if err != nil {
		return nil, err
	}
	common.Logger.Printf("Created new service account: %v", account.Name)
	return account, nil
}

func (iam *IAM) AddRolesToServiceAccount(serviceAccountName string, roles []string) error {
	policy, err := iam.service.Projects.ServiceAccounts.GetIamPolicy(serviceAccountName).Do()
	if err != nil {
		return fmt.Errorf("failed to get IAM policy: %v", err)
	}
	parts := strings.Split(serviceAccountName, "/")
	if len(parts) < 4 {
		return fmt.Errorf("invalid service account name format")
	}
	account := parts[len(parts)-1]
	for _, role := range roles {
		policy.Bindings = append(policy.Bindings, &iamv1.Binding{
			Role:    role,
			Members: []string{"serviceAccount:" + account},
		})
	}
	_, err = iam.service.Projects.ServiceAccounts.SetIamPolicy(serviceAccountName, &iamv1.SetIamPolicyRequest{
		Policy: policy,
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to set IAM policy: %v", err)
	}
	return nil
}

func (iam *IAM) AddRolesToProject(serviceAccountName string, roles []string) error {
	parts := strings.Split(serviceAccountName, "/")
	if len(parts) < 4 {
		return fmt.Errorf("invalid service account name format")
	}
	account := parts[len(parts)-1]

	policy, err := iam.resourceManager.Projects.GetIamPolicy(iam.projectId, &cloudresourcemanager.GetIamPolicyRequest{}).Do()
	if err != nil {
		return fmt.Errorf("failed to get IAM policy: %w", err)
	}
	for _, role := range roles {
		policy.Bindings = append(policy.Bindings, &cloudresourcemanager.Binding{
			Role:    role,
			Members: []string{"serviceAccount:" + account},
		})
	}
	_, err = iam.resourceManager.Projects.SetIamPolicy(iam.projectId, &cloudresourcemanager.SetIamPolicyRequest{
		Policy: policy,
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to set IAM policy: %w", err)
	}
	return nil
}
