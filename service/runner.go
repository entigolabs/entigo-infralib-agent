package service

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/notify"
	"log"
)

func RunUpdater(ctx context.Context, command common.Command, flags *common.Flags) {
	provider := GetCloudProvider(ctx, flags)
	resources := provider.SetupResources()
	config := GetFullConfig(resources.GetSSM(), resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	notifiers := createNotifiers(config)
	err := updateAgentJob(command, flags.Pipeline, resources, config, provider.IsRunningLocally())
	// TODO Notify on all failed exits
	if err != nil {
		log.Fatalf("Failed to update agent job: %v", err)
	}
	err = SetupEncryption(config, provider, resources)
	if err != nil {
		log.Fatalf("Failed to set up encryption: %v", err)
	}
	updater := NewUpdater(ctx, flags, resources, notifiers)
	// TODO Return error and notify if failed
	updater.Process(command)
}

func createNotifiers(config model.Config) []model.Notifier {
	notifiers := make([]model.Notifier, 0)
	for i, notifier := range config.Notifications {
		if notifier.Name == "" {
			log.Fatalf("Notifier[%d] name is empty", i)
		}
		if notifier.Slack == nil {
			log.Fatalf("Notifier %s slack is nil", notifier.Name)
		}
		if notifier.Slack.Token == "" {
			log.Fatalf("Notifier %s slack token is empty", notifier.Name)
		}
		if notifier.Slack.ChannelId == "" {
			log.Fatalf("Notifier %s slack channel ID is empty", notifier.Name)
		}
		notifiers = append(notifiers, notify.NewSlackClient(notifier.Name, notifier.Slack.Token, notifier.Slack.ChannelId))
	}
	return notifiers
}

func updateAgentJob(cmd common.Command, pipelineFlags common.Pipeline, resources model.Resources, config model.Config, runningLocally bool) error {
	if pipelineFlags.Type == string(common.PipelineTypeLocal) {
		return nil
	}
	pipelineFlags = ProcessPipelineFlags(pipelineFlags)
	agent := NewAgent(resources, *pipelineFlags.TerraformCache.Value)
	return agent.UpdateProjectImage(config.AgentVersion, cmd, runningLocally)
}
