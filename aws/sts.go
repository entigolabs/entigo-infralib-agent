package aws

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/organizations"
	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type Account interface {
	GetAccountID() (string, error)
	GetOrganizationID() (string, error)
}

type account struct {
	ctx           context.Context
	sts           *sts.Client
	organizations *organizations.Client
}

func NewSTS(ctx context.Context, config aws.Config) Account {
	return &account{
		ctx:           ctx,
		sts:           sts.NewFromConfig(config),
		organizations: organizations.NewFromConfig(config),
	}
}

func (a *account) GetAccountID() (string, error) {
	stsOutput, err := a.sts.GetCallerIdentity(a.ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get CloudProvider account number: %w", err)
	}
	return *stsOutput.Account, nil
}

// GetOrganizationID returns an empty id when the account isn't in an organization.
func (a *account) GetOrganizationID() (string, error) {
	output, err := a.organizations.DescribeOrganization(a.ctx, &organizations.DescribeOrganizationInput{})
	if err != nil {
		if _, ok := errors.AsType[*orgtypes.AWSOrganizationsNotInUseException](err); ok {
			return "", nil
		}
		return "", fmt.Errorf("failed to describe organization: %w", err)
	}
	if output.Organization == nil || output.Organization.Id == nil {
		return "", nil
	}
	return *output.Organization.Id, nil
}
