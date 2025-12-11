package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

type NotificationManager struct {
	notifiers []model.Notifier
}

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
			return nil, fmt.Errorf("configNotifier %s %s", configNotifier.Name, err)
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
	return newApi(ctx, baseNotifier, notificationApi)
}

func (n *NotificationManager) HasNotifier(messageType model.MessageType) bool {
	for _, notifier := range n.notifiers {
		if notifier.Includes(messageType) {
			return true
		}
	}
	return false
}

func (n *NotificationManager) Message(messageType model.MessageType, message string) {
	n.notify(messageType, func(notifier model.Notifier) error {
		return notifier.Message(messageType, message)
	})
}

func (n *NotificationManager) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) {
	n.notify(model.MessageTypeApprovals, func(notifier model.Notifier) error {
		return notifier.ManualApproval(pipelineName, changes, link)
	})
}

func (n *NotificationManager) StepState(status model.ApplyStatus, stepState model.StateStep, step *model.Step, err error) {
	n.notify(model.MessageTypeProgress, func(notifier model.Notifier) error {
		return notifier.StepState(status, stepState, step, err)
	})
}

func (n *NotificationManager) Modules(accountId, region string, provider model.ProviderType, config model.Config) {
	n.notify(model.MessageTypeModules, func(notifier model.Notifier) error {
		return notifier.Modules(accountId, region, provider, config)
	})
}

func (n *NotificationManager) notify(messageType model.MessageType, action func(notifier model.Notifier) error) {
	var wg sync.WaitGroup
	for _, notifier := range n.notifiers {
		if !notifier.Includes(messageType) {
			continue
		}
		wg.Add(1)
		go func(notifier model.Notifier) {
			defer wg.Done()
			slog.Debug(fmt.Sprintf("Sending %s notification to %s notifier", messageType, notifier.GetName()))
			err := action(notifier)
			if err != nil {
				slog.Error(common.PrefixError(fmt.Errorf("failed to notify '%s': %v", notifier.GetName(), err)))
			}
		}(notifier)
	}
	wg.Wait()
}
