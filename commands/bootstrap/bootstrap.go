package bootstrap

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"log"
)

func Bootstrap(ctx context.Context, flags *common.Flags) {
	provider := service.GetCloudProvider(ctx, flags)
	resources := provider.SetupResources()
	config := service.GetBaseConfig(resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if config.AgentVersion == "" {
		config.AgentVersion = model.LatestImageVersion
	}

	log.Printf("Agent version: %s\n", config.AgentVersion)
	agent := service.NewAgent(resources, getTerraformCache(flags.Pipeline))
	err := agent.CreatePipeline(config.AgentVersion)
	if err != nil {
		log.Fatalf("Failed to create agent pipeline: %s", err)
	}
}

func getTerraformCache(pipeline common.Pipeline) bool {
	if pipeline.TerraformCache.Value != nil {
		return *pipeline.TerraformCache.Value
	}
	return true
}
