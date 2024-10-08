package bootstrap

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Bootstrap(ctx context.Context, flags *common.Flags) {
	provider := service.GetCloudProvider(ctx, flags)
	resources := provider.SetupResources()
	config := service.GetConfig(flags.Config, resources.GetBucket())
	if config.AgentVersion == "" {
		config.AgentVersion = model.LatestImageVersion
	}

	common.Logger.Printf("Agent version: %s\n", config.AgentVersion)
	agent := service.NewAgent(resources)
	err := agent.CreatePipeline(config.AgentVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to create agent pipeline: %s", err)
	}
}
