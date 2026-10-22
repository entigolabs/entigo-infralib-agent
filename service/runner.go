package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/agentstream"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/notify"
	"github.com/google/uuid"
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
	campaignId   uuid.UUID
	finalize     sync.Once
	logTee       *agentstream.LogTee
	backend      *agentstream.Stream
	approvals    *agentstream.Approvals
}

func NewRunner(ctx context.Context, command common.Command, flags *common.Flags) (runner *Runner, err error) {
	logTee := agentstream.InstallLogTee()
	defer func() {
		if err != nil {
			logTee.Uninstall()
		}
	}()
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
	campaignId := uuid.New()
	manager, err := notify.NewNotificationManager(ctx, config.Notifications, campaignId)
	if err != nil {
		return nil, err
	}
	backend, approvals := startBackend(config.Notifications, campaignId, command, flags, logTee)
	return &Runner{
		ctx:          ctx,
		command:      command,
		flags:        flags,
		provider:     provider,
		minResources: resources,
		rootConfig:   config,
		manager:      manager,
		campaignId:   campaignId,
		logTee:       logTee,
		backend:      backend,
		approvals:    approvals,
	}, nil
}

func startBackend(notifications []model.ConfigNotification, campaignId uuid.UUID, command common.Command, flags *common.Flags, logTee *agentstream.LogTee) (*agentstream.Stream, *agentstream.Approvals) {
	api := getWrapperConfig(notifications)
	if api == nil {
		logTee.Uninstall()
		return nil, nil
	}
	var approvals *agentstream.Approvals
	if flags.Pipeline.Type != string(common.PipelineTypeLocal) {
		approvals = agentstream.NewApprovals()
	}
	backend, err := agentstream.New(api, campaignId.String(), command, approvals)
	if err != nil {
		logTee.Uninstall()
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to set up backend connection, agent logs won't be streamed: %s", err)))
		return nil, nil
	}
	logTee.Attach(backend.SendLog)
	backend.Start()
	return backend, approvals
}

func (r *Runner) Run() (err error) {
	defer func() { r.disconnectBackend(err) }()
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
	updater, err := NewUpdater(r.ctx, r.flags, resources, r.manager, r.command, r.campaignId, r.approvals)
	if err != nil {
		return r.notifyError(err)
	}
	campaignRan, err := updater.Process()
	if err != nil {
		return r.notifyError(err)
	}
	r.finalize.Do(func() {
		status := model.CampaignStatusSuccess
		if !campaignRan {
			status = model.CampaignStatusSkipped
		}
		r.manager.Campaign(r.ctx, status, r.minResources, r.command, nil)

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

func (r *Runner) disconnectBackend(err error) {
	r.logTee.Uninstall()
	if r.backend == nil {
		return
	}
	if r.ctx.Err() != nil {
		err = fmt.Errorf("agent was terminated")
	}
	exitCode := 0
	if err != nil {
		exitCode = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminationNotifyTimeout)
	defer cancel()
	if disconnectErr := r.backend.Disconnect(ctx, exitCode, err); disconnectErr != nil {
		slog.Debug(common.PrefixWarning(fmt.Sprintf("Failed to disconnect from backend: %s", disconnectErr)))
	}
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
