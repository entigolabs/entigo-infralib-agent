package service

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
	sts *sts.Client
}

func NewSTS(config aws.Config) Account {
	return &account{
		sts: sts.NewFromConfig(config),
	}
}

func (a *account) GetAccountID() (string, error) {
	stsOutput, err := a.sts.GetCallerIdentity(context.Background(), nil)
	if err != nil {
		return "", fmt.Errorf("failed to get CloudProvider account number: %w", err)
	}
	return *stsOutput.Account, nil
}
