package bootstrap

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"strings"
)

func Bootstrap(flags *common.Flags) {
	prefix := service.GetAwsPrefix(flags)
	awsService := service.NewAWS(strings.ToLower(prefix))
	resources := awsService.SetupAWSResources(flags.Branch)
	config := service.GetConfig(flags.Config, resources.CodeCommit)
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
