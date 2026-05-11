package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/notify"
)

const terminationNotifyTimeout = 10 * time.Second

type Runner struct {
	ctx          context.Context
	command      common.Command
	flags        *common.Flags
	provider     model.CloudProvider
	minResources model.Resources
	rootConfig   model.Config
	manager      model.NotificationManager
	finalize     sync.Once
}

func NewRunner(ctx context.Context, command common.Command, flags *common.Flags) (*Runner, error) {
	provider, err := GetCloudProvider(ctx, flags)
	if err != nil {
		return nil, err
	}
	resources, err := provider.SetupMinimalResources()
	if err != nil {
		return nil, err
	}
	config, err := GetRootConfig(resources.GetSSM(), resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if err != nil {
		return nil, err
	}
	manager, err := notify.NewNotificationManager(ctx, config.Notifications)
	if err != nil {
		return nil, err
	}
	return &Runner{
		ctx:          ctx,
		command:      command,
		flags:        flags,
		provider:     provider,
		minResources: resources,
		rootConfig:   config,
		manager:      manager,
	}, nil
}

func (r *Runner) Run() error {
	r.manager.Modules(r.minResources, r.command, r.rootConfig)
	defer r.notifyTerminationIfCanceled()
	r.manager.Campaign(r.ctx, model.CampaignStatusStarted, r.minResources, r.command, nil)

	resources, err := r.provider.SetupResources(r.manager, r.rootConfig)
	if err != nil {
		return r.notifyError(fmt.Errorf("failed to setup resources: %s", err))
	}
	err = r.updateAgentJob(resources)
	if err != nil {
		return r.notifyError(fmt.Errorf("failed to update agent job: %s", err))
	}
	err = r.setupEncryption(resources)
	if err != nil {
		return r.notifyError(fmt.Errorf("failed to set up encryption: %s", err))
	}
	updater, err := NewUpdater(r.ctx, r.flags, resources, r.manager, r.command)
	if err != nil {
		return r.notifyError(err)
	}
	err = updater.Process()
	if err != nil {
		return r.notifyError(err)
	}
	r.finalize.Do(func() {
		r.manager.Campaign(r.ctx, model.CampaignStatusSuccess, r.minResources, r.command, nil)

	})
	return nil
}

func (r *Runner) notifyTerminationIfCanceled() {
	if r.ctx.Err() == nil {
		return
	}
	if !r.manager.HasNotifier(model.MessageTypeFailure) {
		return
	}
	r.finalize.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), terminationNotifyTimeout)
		defer cancel()
		r.manager.Campaign(shutdownCtx, model.CampaignStatusTerminated, r.minResources, r.command,
			fmt.Errorf("agent was terminated"))
	})
}

func (r *Runner) updateAgentJob(resources model.Resources) error {
	if r.flags.Pipeline.Type == string(common.PipelineTypeLocal) {
		return nil
	}
	pipelineFlags := ProcessPipelineFlags(r.flags.Pipeline)
	agent := NewAgent(resources, *pipelineFlags.TerraformCache.Value)
	return agent.UpdateProjectImage(r.rootConfig.AgentVersion, r.command, r.provider.IsRunningLocally())
}

func (r *Runner) notifyError(err error) error {
	if r.ctx.Err() != nil {
		return err
	}
	r.finalize.Do(func() {
		r.manager.Campaign(r.ctx, model.CampaignStatusFailure, r.minResources, r.command, err)
	})
	return err
}

func (r *Runner) setupEncryption(resources model.Resources) error {
	if resources.GetProviderType() != model.AWS {
		return nil // TODO Remove when GCP encryption is implemented
	}
	moduleName, outputs, err := GetEncryptionOutputs(r.rootConfig, resources.GetCloudPrefix(), resources.GetBucket())
	if err != nil {
		return fmt.Errorf("failed to get outputs for %s: %v", moduleName, err)
	}
	if outputs == nil {
		return nil
	}
	return r.provider.AddEncryption(moduleName, outputs)
}
