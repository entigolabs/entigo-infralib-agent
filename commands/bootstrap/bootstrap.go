package bootstrap

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Bootstrap(flags *common.Flags) {
	provider := service.GetCloudProvider(flags)
	resources := provider.SetupResources(flags.Branch)
	config := service.GetConfig(flags.Config, resources.GetCodeRepo())
	if config.AgentVersion == "" {
		config.AgentVersion = service.LatestAgentImage
	}

	common.Logger.Printf("Agent version: %s\n", config.AgentVersion)
	agent := service.NewAgent(resources)
	err := agent.CreatePipeline(config.AgentVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to create agent pipeline: %s", err)
	}
}
