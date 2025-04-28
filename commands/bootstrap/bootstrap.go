package bootstrap

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"log"
)

func Bootstrap(ctx context.Context, flags *common.Flags) error {
	provider, err := service.GetCloudProvider(ctx, flags)
	if err != nil {
		return err
	}
	resources, err := provider.SetupResources(nil)
	if err != nil {
		return fmt.Errorf("failed to setup resources: %v", err)
	}
	config, err := service.GetBaseConfig(resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if err != nil {
		return err
	}
	if config.AgentVersion == "" {
		config.AgentVersion = model.LatestImageVersion
	}

	log.Printf("Agent version: %s\n", config.AgentVersion)
	agent := service.NewAgent(resources, getTerraformCache(flags.Pipeline))
	return agent.CreatePipeline(config.AgentVersion)
}

func getTerraformCache(pipeline common.Pipeline) bool {
	if pipeline.TerraformCache.Value != nil {
		return *pipeline.TerraformCache.Value
	}
	return true
}
