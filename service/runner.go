package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/notify"
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
	manager, err := notify.NewNotificationManager(ctx, config.Notifications)
	if err != nil {
		return err
	}
	manager.Message(model.MessageTypeStarted, fmt.Sprintf("Agent %s started: %s", command, provider.GetIdentifier()))
	resources, err = provider.SetupResources(manager)
	if err != nil {
		return notifyError(manager, fmt.Sprintf("Failed to setup resources: %s", err))
	}
	err = updateAgentJob(command, flags.Pipeline, resources, config, provider.IsRunningLocally())
	if err != nil {
		return notifyError(manager, fmt.Sprintf("Failed to update agent job: %s", err))
	}
	err = SetupEncryption(config, provider, resources)
	if err != nil {
		return notifyError(manager, fmt.Sprintf("Failed to set up encryption: %s", err))
	}
	updater, err := NewUpdater(ctx, flags, resources, manager)
	if err != nil {
		return notifyError(manager, err.Error())
	}
	err = updater.Process(command)
	if err != nil {
		return notifyError(manager, err.Error())
	}
	manager.Message(model.MessageTypeSuccess, fmt.Sprintf("Agent %s finished successfully: %s", command, provider.GetIdentifier()))
	return nil
}

func updateAgentJob(cmd common.Command, pipelineFlags common.Pipeline, resources model.Resources, config model.Config, runningLocally bool) error {
	if pipelineFlags.Type == string(common.PipelineTypeLocal) {
		return nil
	}
	pipelineFlags = ProcessPipelineFlags(pipelineFlags)
	agent := NewAgent(resources, *pipelineFlags.TerraformCache.Value)
	return agent.UpdateProjectImage(config.AgentVersion, cmd, runningLocally)
}

func notifyError(manager model.NotificationManager, message string) error {
	manager.Message(model.MessageTypeFailure, message)
	return errors.New(message)
}
