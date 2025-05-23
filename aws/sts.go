package aws

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type Account interface {
	GetAccountID() (string, error)
}

type account struct {
	ctx context.Context
	sts *sts.Client
}

func NewSTS(ctx context.Context, config aws.Config) Account {
	return &account{
		ctx: ctx,
		sts: sts.NewFromConfig(config),
	}
}

func (a *account) GetAccountID() (string, error) {
	stsOutput, err := a.sts.GetCallerIdentity(a.ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get CloudProvider account number: %w", err)
	}
	return *stsOutput.Account, nil
}
