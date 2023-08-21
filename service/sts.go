package service

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

type Account interface {
	GetAccountID() string
}

type account struct {
	sts *sts.Client
}

func NewSTS(config aws.Config) Account {
	return &account{
		sts: sts.NewFromConfig(config),
	}
}

func (a *account) GetAccountID() string {
	stsOutput, err := a.sts.GetCallerIdentity(context.Background(), nil)
	if err != nil {
		common.Logger.Fatalf("Failed to get AWS account number: %s", err)
	}
	return *stsOutput.Account
}
