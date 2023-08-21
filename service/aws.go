package service

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

func NewAWSConfig() aws.Config {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		common.Logger.Fatalf("Failed to initialize AWS session: %s", err)
	}
	common.Logger.Printf("AWS session initialized with region: %s\n", cfg.Region)
	return cfg
}
