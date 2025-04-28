package notify

import (
	"bytes"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/slack-go/slack"
)

type SlackClient struct {
	model.NotifierType
	client    *slack.Client
	channelId string
}

func newSlackClient(notifierType model.NotifierType, configSlack model.Slack) *SlackClient {
	return &SlackClient{
		NotifierType: notifierType,
		client:       slack.New(configSlack.Token),
		channelId:    configSlack.ChannelId,
	}
}

func (s *SlackClient) Message(message string) error {
	_, _, err := s.client.PostMessage(s.channelId, slack.MsgOptionText(message, false))
	return err
}

func (s *SlackClient) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) error {
	formattedChanges := fmt.Sprintf("Plan: %d to add, %d to change, %d to destroy.", changes.Added,
		changes.Changed, changes.Destroyed)
	message := fmt.Sprintf("Waiting for manual approval of pipeline %s\n%s",
		pipelineName, formattedChanges)
	if link != "" {
		message += fmt.Sprintf("\nPipeline: %s", link)
	}
	return s.Message(message)
}

func (s *SlackClient) StepState(status model.ApplyStatus, stepState model.StateStep, _ *model.Step) error {
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintf("Step '%s' status: %s", stepState.Name, status))
	for _, module := range stepState.Modules {
		buffer.WriteString(fmt.Sprintf("\nModule '%s' version: %s", module.Name, module.Version))
		if module.AppliedVersion != nil {
			buffer.WriteString(fmt.Sprintf(", applied version: %s", *module.AppliedVersion))
		}
	}
	return s.Message(buffer.String())
}
