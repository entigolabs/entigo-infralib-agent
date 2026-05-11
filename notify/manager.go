package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/notify/api"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

type NotificationManager struct {
	notifiers     []model.Notifier
	pipelineIndex atomic.Int32
}

var _ model.NotificationManager = (*NotificationManager)(nil)

func NewNotificationManager(ctx context.Context, configNotifiers []model.ConfigNotification) (model.NotificationManager, error) {
	notifiers, err := createNotifiers(ctx, configNotifiers)
	if err != nil {
		return nil, err
	}
	return &NotificationManager{
		notifiers: notifiers,
	}, nil
}

func createNotifiers(ctx context.Context, configNotifiers []model.ConfigNotification) ([]model.Notifier, error) {
	notifiers := make([]model.Notifier, 0)
	names := model.NewSet[string]()
	for i, configNotifier := range configNotifiers {
		if configNotifier.Name == "" {
			return nil, fmt.Errorf("configNotifier[%d] name is empty", i)
		}
		if names.Contains(configNotifier.Name) {
			return nil, fmt.Errorf("configNotifier %s name must be unique", configNotifier.Name)
		}
		names.Add(configNotifier.Name)
		if (util.BoolToInt(configNotifier.Slack != nil) +
			util.BoolToInt(configNotifier.Api != nil) +
			util.BoolToInt(configNotifier.Teams != nil)) != 1 {
			return nil, fmt.Errorf("configNotifier %s must have exactly 1 subtype specified", configNotifier.Name)
		}
		notifier, err := createNotifier(ctx, configNotifier)
		if err != nil {
			return nil, fmt.Errorf("configNotifier %s: %w", configNotifier.Name, err)
		}
		notifiers = append(notifiers, notifier)
	}
	return notifiers, nil
}

func createNotifier(ctx context.Context, configNotifier model.ConfigNotification) (model.Notifier, error) {
	var messageTypes model.Set[model.MessageType]
	if len(configNotifier.MessageTypes) == 0 {
		messageTypes = model.NewSet(model.MessageTypeApprovals, model.MessageTypeFailure)
	} else {
		messageTypes = model.ToSet(configNotifier.MessageTypes)
	}
	baseNotifier := model.BaseNotifier{
		Name:         configNotifier.Name,
		Context:      configNotifier.Context,
		MessageTypes: messageTypes,
	}
	if configNotifier.Slack != nil {
		return createSlackNotifier(baseNotifier, *configNotifier.Slack)
	}
	if configNotifier.Teams != nil {
		return createTeamsNotifier(baseNotifier, *configNotifier.Teams)
	}
	if configNotifier.Api != nil {
		return createApiNotifier(ctx, baseNotifier, *configNotifier.Api)
	}
	return nil, errors.New("has no subtype specified")
}

func createSlackNotifier(baseNotifier model.BaseNotifier, slack model.Slack) (model.Notifier, error) {
	if slack.Token == "" {
		return nil, errors.New("slack token is empty")
	}
	if slack.ChannelId == "" {
		return nil, errors.New("slack channel id is empty")
	}
	return newSlackClient(baseNotifier, slack), nil
}

func createTeamsNotifier(baseNotifier model.BaseNotifier, teams model.Teams) (model.Notifier, error) {
	if teams.WebhookUrl == "" {
		return nil, errors.New("teams webhook url is empty")
	}
	return newTeamsClient(baseNotifier, teams), nil
}

func createApiNotifier(ctx context.Context, baseNotifier model.BaseNotifier, notificationApi model.NotificationApi) (model.Notifier, error) {
	if notificationApi.URL == "" {
		return nil, errors.New("api url is empty")
	}
	if notificationApi.OAuth != nil {
		if notificationApi.OAuth.ClientId == "" {
			return nil, errors.New("api oauth client id is empty")
		}
		if notificationApi.OAuth.ClientSecret == "" {
			return nil, errors.New("api oauth client secret is empty")
		}
		if notificationApi.OAuth.TokenURL == "" {
			return nil, errors.New("api oauth token url is empty")
		}
	}
	return api.NewApi(ctx, baseNotifier, notificationApi)
}

func (n *NotificationManager) SetCurrentPipelineIndex(index int) {
	n.pipelineIndex.Store(int32(index))
}

func (n *NotificationManager) getPipelineIndex() (int32, bool) {
	index := n.pipelineIndex.Load()
	if index == 0 {
		slog.Error(common.PrefixError(fmt.Errorf("pipeline index accessed before setting a value")))
		return 0, false
	}
	return index, true
}

func (n *NotificationManager) HasNotifier(messageType model.MessageType) bool {
	for _, notifier := range n.notifiers {
		if notifier.Includes(messageType) {
			return true
		}
	}
	return false
}

func (n *NotificationManager) Campaign(ctx context.Context, status model.CampaignStatus, resources model.Resources, command common.Command, err error) {
	n.Notify(model.CampaignMessage{Ctx: ctx, Status: status, Resources: resources, Command: command, Err: err})
}

func (n *NotificationManager) Schedule(command common.Command, action model.ScheduleAction, schedule string) {
	n.Notify(model.ScheduleMessage{Command: command, Action: action, Schedule: schedule})
}

func (n *NotificationManager) Approval(pipelineName, step, approvedBy string) {
	index, ok := n.getPipelineIndex()
	if !ok {
		return
	}
	n.Notify(model.ApprovalMessage{PipelineIndex: index, PipelineName: pipelineName, Step: step, ApprovedBy: approvedBy})
}

func (n *NotificationManager) ManualApproval(pipelineName, step string, changes model.PipelineChanges, link string) {
	index, ok := n.getPipelineIndex()
	if !ok {
		return
	}
	n.Notify(model.ManualApprovalMessage{PipelineIndex: index, PipelineName: pipelineName, Step: step, Changes: changes, Link: link})
}

func (n *NotificationManager) StepState(status model.ApplyStatus, stepState model.StateStep, step *model.Step, err error) {
	index, ok := n.getPipelineIndex()
	if !ok {
		return
	}
	n.Notify(model.StepStateMessage{PipelineIndex: index, Status: status, StateStep: stepState, Step: step, Err: err})
}

func (n *NotificationManager) Modules(resources model.Resources, command common.Command, config model.Config) {
	n.Notify(model.ModulesMessage{Resources: resources, Command: command, Config: config})
}

func (n *NotificationManager) Sources(sources map[model.SourceKey]*model.Source) {
	n.Notify(model.SourcesMessage{Sources: sources})
}

func (n *NotificationManager) PipelineState(status model.ApplyStatus, sourceVersions []model.SourceVersion, err error) {
	index, ok := n.getPipelineIndex()
	if !ok {
		return
	}
	n.Notify(model.PipelineStateMessage{Index: index, Status: status, SourceVersions: sourceVersions, Err: err})
}

func (n *NotificationManager) Notify(msg model.Message) {
	msgType := msg.Type()
	if msgType == model.MessageTypeUnknown {
		slog.Error(common.PrefixError(fmt.Errorf("message type unknown, msg: %v", msg)))
		return
	}
	n.fanout(msgType, msg.Dispatch)
}

func (n *NotificationManager) fanout(kind model.MessageType, dispatch func(model.Notifier) error) {
	var wg sync.WaitGroup
	for _, notifier := range n.notifiers {
		if !notifier.Includes(kind) {
			continue
		}
		wg.Add(1)
		go func(notifier model.Notifier) {
			defer wg.Done()
			slog.Debug(fmt.Sprintf("Sending %s notification to %s notifier", kind, notifier.GetName()))
			if err := dispatch(notifier); err != nil {
				if errors.Is(err, context.Canceled) {
					slog.Debug(fmt.Sprintf("notifier '%s' skipped during shutdown: %v", notifier.GetName(), err))
					return
				}
				slog.Error(common.PrefixError(fmt.Errorf("failed to notify '%s': %v", notifier.GetName(), err)))
			}
		}(notifier)
	}
	wg.Wait()
}
