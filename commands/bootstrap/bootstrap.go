package bootstrap

import (
	"context"
	"fmt"
	"log"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/notify"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Bootstrap(ctx context.Context, flags *common.Flags) error {
	provider, err := service.GetCloudProvider(ctx, flags)
	if err != nil {
		return err
	}
	resources, err := provider.SetupMinimalResources()
	if err != nil {
		return err
	}
	config, err := service.GetBaseConfig(resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if err != nil {
		return err
	}
	manager, err := notify.NewNotificationManager(ctx, config.Notifications)
	if err != nil {
		return err
	}
	resources, err = provider.SetupResources(manager, config)
	if err != nil {
		return fmt.Errorf("failed to setup resources: %v", err)
	}
	agentVersion := model.LatestImageVersion
	if config.AgentVersion != "" {
		agentVersion = config.AgentVersion
	}

	log.Printf("Agent version: %s\n", agentVersion)
	agent := service.NewAgent(resources, getTerraformCache(flags.Pipeline))
	return agent.CreatePipeline(agentVersion, flags.Start)
}

func getTerraformCache(pipeline common.Pipeline) bool {
	if pipeline.TerraformCache.Value != nil {
		return *pipeline.TerraformCache.Value
	}
	return true
}
