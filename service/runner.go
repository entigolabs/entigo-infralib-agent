package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/notify"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

func RunUpdater(ctx context.Context, command common.Command, flags *common.Flags) error {
	provider, err := GetCloudProvider(ctx, flags)
	if err != nil {
		return err
	}
	resources, err := provider.SetupMinimalResources()
	if err != nil {
		return err
	}
	config, err := GetRootConfig(resources.GetSSM(), resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if err != nil {
		return err
	}
	notifiers, err := createNotifiers(config)
	if err != nil {
		return err
	}
	resources, err = provider.SetupResources()
	if err != nil {
		return notifyReturn(notifiers, fmt.Sprintf("Failed to setup resources: %s", err))
	}
	err = updateAgentJob(command, flags.Pipeline, resources, config, provider.IsRunningLocally())
	if err != nil {
		return notifyReturn(notifiers, fmt.Sprintf("Failed to update agent job: %s", err))
	}
	err = SetupEncryption(config, provider, resources)
	if err != nil {
		return notifyReturn(notifiers, fmt.Sprintf("Failed to set up encryption: %s", err))
	}
	updater, err := NewUpdater(ctx, flags, resources, notifiers)
	if err != nil {
		return notifyReturn(notifiers, fmt.Sprintf("Failed to create updater: %s", err))
	}
	err = updater.Process(command)
	if err != nil {
		return notifyReturn(notifiers, fmt.Sprintf("Failed to process command: %s", err))
	}
	return err
}

func createNotifiers(config model.Config) ([]model.Notifier, error) {
	notifiers := make([]model.Notifier, 0)
	for i, notifier := range config.Notifications {
		if notifier.Name == "" {
			return nil, fmt.Errorf("notifier[%d] name is empty", i)
		}
		if notifier.Slack == nil {
			return nil, fmt.Errorf("notifier %s slack is nil", notifier.Name)
		}
		if notifier.Slack.Token == "" {
			return nil, fmt.Errorf("notifier %s slack token is empty", notifier.Name)
		}
		if notifier.Slack.ChannelId == "" {
			return nil, fmt.Errorf("notifier %s slack channel ID is empty", notifier.Name)
		}
		notifiers = append(notifiers, notify.NewSlackClient(notifier.Name, notifier.Slack.Token, notifier.Slack.ChannelId))
	}
	return notifiers, nil
}

func updateAgentJob(cmd common.Command, pipelineFlags common.Pipeline, resources model.Resources, config model.Config, runningLocally bool) error {
	if pipelineFlags.Type == string(common.PipelineTypeLocal) {
		return nil
	}
	pipelineFlags = ProcessPipelineFlags(pipelineFlags)
	agent := NewAgent(resources, *pipelineFlags.TerraformCache.Value)
	return agent.UpdateProjectImage(config.AgentVersion, cmd, runningLocally)
}

func notifyReturn(notifiers []model.Notifier, message string) error {
	util.Notify(notifiers, message)
	return errors.New(message)
}
