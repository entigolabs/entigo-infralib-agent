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
	"google.golang.org/api/option"
)

type IAM struct {
	ctx             context.Context
	service         *iamv1.Service
	resourceManager *cloudresourcemanager.Service
	projectId       string
}

func NewIAM(ctx context.Context, options []option.ClientOption, projectId string) (*IAM, error) {
	service, err := iamv1.NewService(ctx, options...)
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
	err := retry(func() error {
		return iam.tryAddIAMRoles(serviceAccountName, account, roles)
	})
	if err != nil {
		return fmt.Errorf("failed to add roles to service account: %w", err)
	}
	return nil
}

func (iam *IAM) tryAddIAMRoles(accountName, accountEmail string, roles []string) error {
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
	err := retry(func() error {
		return iam.tryAddProjectRoles(account, roles)
	})
	if err != nil {
		return fmt.Errorf("failed to add roles to project: %w", err)
	}
	return nil
}

func (iam *IAM) tryAddProjectRoles(account string, roles []string) error {
	policy, err := iam.resourceManager.Projects.GetIamPolicy(iam.projectId, &cloudresourcemanager.GetIamPolicyRequest{}).Do()
	if err != nil {
		return fmt.Errorf("failed to get IAM policy: %w", err)
	}
	updateResourcePolicy(policy, account, roles)
	_, err = iam.resourceManager.Projects.SetIamPolicy(iam.projectId, &cloudresourcemanager.SetIamPolicyRequest{
		Policy: policy,
	}).Do()
	return err
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

func (iam *IAM) GetOrCreateCustomRole(roleId, title, description string, permissions []string) error {
	roleName := fmt.Sprintf("projects/%s/roles/%s", iam.projectId, roleId)
	existing, err := iam.service.Projects.Roles.Get(roleName).Do()
	if err != nil {
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) || gerr.Code != http.StatusNotFound {
			return fmt.Errorf("failed to get custom role %s: %w", roleId, err)
		}
		return iam.createRole(roleId, title, description, permissions)
	}
	if existing.Deleted {
		_, err = iam.service.Projects.Roles.Undelete(roleName, &iamv1.UndeleteRoleRequest{}).Do()
		if err != nil {
			return fmt.Errorf("failed to undelete custom role %s: %w", roleId, err)
		}
		log.Printf("Undeleted custom IAM role %s\n", roleId)
	}
	if !existing.Deleted && permissionsMatch(existing.IncludedPermissions, permissions) {
		return nil
	}
	existing.IncludedPermissions = permissions
	_, err = iam.service.Projects.Roles.Patch(roleName, existing).Do()
	if err != nil {
		return fmt.Errorf("failed to update custom role %s: %w", roleId, err)
	}
	log.Printf("Updated custom IAM role %s\n", roleId)
	return nil
}

func (iam *IAM) createRole(roleId, title, description string, permissions []string) error {
	_, err := iam.service.Projects.Roles.Create(fmt.Sprintf("projects/%s", iam.projectId), &iamv1.CreateRoleRequest{
		RoleId: roleId,
		Role: &iamv1.Role{
			Title:               title,
			Description:         description,
			IncludedPermissions: permissions,
			Stage:               "GA",
		},
	}).Do()
	if err != nil {
		return fmt.Errorf("failed to create custom role %s: %w", roleId, err)
	}
	log.Printf("Created custom IAM role %s\n", roleId)
	return nil
}

func permissionsMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, p := range a {
		set[p] = struct{}{}
	}
	for _, p := range b {
		if _, ok := set[p]; !ok {
			return false
		}
	}
	return true
}

func (iam *IAM) DeleteRole(roleId string) error {
	roleName := fmt.Sprintf("projects/%s/roles/%s", iam.projectId, roleId)
	_, err := iam.service.Projects.Roles.Delete(roleName).Do()
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
			return nil
		}
		return err
	}
	log.Printf("Deleted custom IAM role %s\n", roleId)
	return nil
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

func retry(execute func() error) error {
	maxRetries := 6
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		err := execute()
		if err == nil {
			return nil
		}
		lastErr = err
		var gErr *googleapi.Error
		if !errors.As(err, &gErr) || (gErr.Code != http.StatusBadRequest && gErr.Code != http.StatusNotFound) {
			return err
		}
		wait := time.Duration(1<<i) * time.Second //  2 to the power of i seconds
		log.Printf("Service Account not yet propagated, retrying in %v... (Attempt %d/%d)", wait, i+1, maxRetries)
		time.Sleep(wait)
	}
	return lastErr
}
