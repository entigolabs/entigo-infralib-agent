package gcloud

import (
	"context"
	"errors"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	iamv1 "google.golang.org/api/iam/v1"
)

type iam struct {
	ctx     context.Context
	service *iamv1.Service
}

func NewIAM(ctx context.Context) (model.IAM, error) {
	service, err := iamv1.NewService(ctx)
	if err != nil {
		return nil, err
	}

	return &iam{
		ctx:     ctx,
		service: service,
	}, nil
}

func (i iam) AttachRolePolicy(policyArn string, roleName string) error {
	return errors.New("not implemented")
}

func (i iam) CreatePolicy(policyName string, statement []model.PolicyStatement) *model.Policy {
	panic("implement me")
}

func (i iam) CreateRole(roleName string, statement []model.PolicyStatement) *model.Role {
	// TODO Already exists error handling
	account, err := i.service.Projects.ServiceAccounts.Create(roleName, &iamv1.CreateServiceAccountRequest{
		AccountId:      roleName,
		ServiceAccount: &iamv1.ServiceAccount{},
	}).Do()
	if err != nil {
		common.Logger.Fatalf("Failed to create service account: %s", err)
	}
	return &model.Role{
		RoleName: roleName,
		Arn:      account.UniqueId,
	}
}

func (i iam) GetRole(roleName string) *model.Role {
	account, err := i.service.Projects.ServiceAccounts.Get(roleName).Do()
	if err != nil {
		common.Logger.Fatalf("Failed to get service account: %s", err)
	}
	return &model.Role{
		RoleName: roleName,
		Arn:      account.UniqueId,
	}
}
