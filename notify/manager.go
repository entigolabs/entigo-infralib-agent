package notify

import (
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log/slog"
	"sync"
)

const warningFormat = "failed to notify '%s': %v"

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
	for i, configNotifier := range configNotifiers {
		if configNotifier.Name == "" {
			return nil, fmt.Errorf("configNotifier[%d] name is empty", i)
		}
		if configNotifier.Slack != nil && configNotifier.Api != nil {
			return nil, fmt.Errorf("configNotifier %s can have only 1 subtype specified", configNotifier.Name)
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
		messageTypes = model.ToSet([]model.MessageType{model.MessageTypeApprovals, model.MessageTypeFailure})
	} else {
		messageTypes = model.ToSet(configNotifier.MessageTypes)
	}
	notifierType := model.NotifierType{
		Name:         configNotifier.Name,
		MessageTypes: messageTypes,
	}
	if configNotifier.Slack != nil {
		return createSlackNotifier(notifierType, *configNotifier.Slack)
	}
	if configNotifier.Api != nil {
		return createApiNotifier(ctx, notifierType, *configNotifier.Api)
	}
	return nil, errors.New("has no subtype specified")
}

func createSlackNotifier(notifierType model.NotifierType, slack model.Slack) (model.Notifier, error) {
	if slack.Token == "" {
		return nil, errors.New("slack token is empty")
	}
	if slack.ChannelId == "" {
		return nil, errors.New("slack channel id is empty")
	}
	return newSlackClient(notifierType, slack), nil
}

func createApiNotifier(ctx context.Context, notifierType model.NotifierType, notificationApi model.NotificationApi) (model.Notifier, error) {
	if notificationApi.URL == "" {
		return nil, errors.New("api url is empty")
	}
	if notificationApi.Key == "" {
		return nil, errors.New("api key is empty")
	}
	return newApi(ctx, notifierType, notificationApi), nil
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
	n.notify(messageType, func(notifier model.Notifier) {
		err := notifier.Message(message)
		if err != nil {
			slog.Error(common.PrefixError(fmt.Errorf(warningFormat, notifier.GetName(), err)))
		}
	})
}

func (n *NotificationManager) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) {
	n.notify(model.MessageTypeApprovals, func(notifier model.Notifier) {
		err := notifier.ManualApproval(pipelineName, changes, link)
		if err != nil {
			slog.Error(common.PrefixError(fmt.Errorf(warningFormat, notifier.GetName(), err)))
		}
	})
}

func (n *NotificationManager) StepState(status model.ApplyStatus, stepState model.StateStep, step *model.Step) {
	n.notify(model.MessageTypeProgress, func(notifier model.Notifier) {
		err := notifier.StepState(status, stepState, step)
		if err != nil {
			slog.Error(common.PrefixError(fmt.Errorf(warningFormat, notifier.GetName(), err)))
		}
	})
}

func (n *NotificationManager) notify(messageType model.MessageType, action func(notifier model.Notifier)) {
	var wg sync.WaitGroup
	for _, notifier := range n.notifiers {
		if !notifier.Includes(messageType) {
			continue
		}
		wg.Add(1)
		go func(notifier model.Notifier) {
			defer wg.Done()
			action(notifier)
		}(notifier)
	}
	wg.Wait()
}
