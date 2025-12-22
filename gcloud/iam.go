package gcloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
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

func (iam *IAM) GetOrCreateServiceAccount(name, displayName string) (*iamv1.ServiceAccount, bool, error) {
	account, err := iam.GetServiceAccount(fmt.Sprintf("projects/%s/serviceAccounts/%s@%s.iam.gserviceaccount.com",
		iam.projectId, name, iam.projectId))
	if err != nil {
		return nil, false, err
	}
	if account != nil {
		return account, false, nil
	}
	account, err = iam.service.Projects.ServiceAccounts.Create(fmt.Sprintf("projects/%s", iam.projectId),
		&iamv1.CreateServiceAccountRequest{
			AccountId: name,
			ServiceAccount: &iamv1.ServiceAccount{
				DisplayName: displayName,
			},
		}).Do()
	if err != nil {
		return nil, false, err
	}
	log.Printf("Created new service account: %s@%s.iam.gserviceaccount.com", name, iam.projectId)
	return account, true, nil
}

func (iam *IAM) AddRolesToServiceAccount(serviceAccountName string, roles []string) error {
	parts := strings.Split(serviceAccountName, "/")
	if len(parts) < 4 {
		return fmt.Errorf("invalid service account name format")
	}
	account := parts[len(parts)-1]
	maxRetries := 6
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		err := iam.tryAddRoles(serviceAccountName, account, roles)
		if err == nil {
			return nil
		}
		lastErr = err
		var gErr *googleapi.Error
		if !errors.As(err, &gErr) || (gErr.Code != http.StatusBadRequest && gErr.Code != http.StatusNotFound) {
			return fmt.Errorf("failed to set IAM policy: %v", err)
		}
		wait := time.Duration(1<<i) * time.Second //  2 to the power of i seconds
		log.Printf("Service Account not yet propagated, retrying in %v... (Attempt %d/%d)", wait, i+1, maxRetries)
		time.Sleep(wait)
	}
	return fmt.Errorf("failed to set IAM policy: %v", lastErr)
}

func (iam *IAM) tryAddRoles(accountName, accountEmail string, roles []string) error {
	policy, err := iam.service.Projects.ServiceAccounts.GetIamPolicy(accountName).Do()
	if err != nil {
		return err
	}
	updateIAMPolicy(policy, accountEmail, roles)
	_, err = iam.service.Projects.ServiceAccounts.SetIamPolicy(accountName, &iamv1.SetIamPolicyRequest{
		Policy: policy,
	}).Do()
	return err
}

func updateIAMPolicy(policy *iamv1.Policy, accountEmail string, roles []string) {
	memberString := "serviceAccount:" + accountEmail
	for _, role := range roles {
		foundRole := false
		for _, binding := range policy.Bindings {
			if binding.Role != role {
				continue
			}
			foundRole = true
			foundMember := false
			for _, member := range binding.Members {
				if member != memberString {
					continue
				}
				foundMember = true
				break
			}
			if !foundMember {
				binding.Members = append(binding.Members, memberString)
			}
			break
		}
		if !foundRole {
			policy.Bindings = append(policy.Bindings, &iamv1.Binding{
				Role:    role,
				Members: []string{memberString},
			})
		}
	}
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
	updateResourcePolicy(policy, account, roles)
	_, err = iam.resourceManager.Projects.SetIamPolicy(iam.projectId, &cloudresourcemanager.SetIamPolicyRequest{
		Policy: policy,
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to set IAM policy: %w", err)
	}
	return nil
}

func updateResourcePolicy(policy *cloudresourcemanager.Policy, accountEmail string, roles []string) {
	memberString := "serviceAccount:" + accountEmail
	for _, role := range roles {
		foundRole := false
		for _, binding := range policy.Bindings {
			if binding.Role != role {
				continue
			}
			foundRole = true
			foundMember := false
			for _, member := range binding.Members {
				if member != memberString {
					continue
				}
				foundMember = true
				break
			}
			if !foundMember {
				binding.Members = append(binding.Members, memberString)
			}
			break
		}
		if !foundRole {
			policy.Bindings = append(policy.Bindings, &cloudresourcemanager.Binding{
				Role:    role,
				Members: []string{memberString},
			})
		}
	}
}

func (iam *IAM) DeleteServiceAccount(name string) error {
	_, err := iam.service.Projects.ServiceAccounts.Delete(fmt.Sprintf("projects/%s/serviceAccounts/%s@%s.iam.gserviceaccount.com",
		iam.projectId, name, iam.projectId)).Do()
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
			return nil
		}
		return err
	}
	log.Printf("Deleted service account: %s", name)
	return nil
}

func (iam *IAM) CreateServiceAccountKey(serviceAccountName string) (*iamv1.ServiceAccountKey, error) {
	return iam.service.Projects.ServiceAccounts.Keys.Create(serviceAccountName, &iamv1.CreateServiceAccountKeyRequest{}).Do()
}
