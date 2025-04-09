package notify

import (
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/slack-go/slack"
)

type SlackClient struct {
	model.NotifierType
	client    *slack.Client
	channelId string
}

func NewSlackClient(name, token, channelId string) *SlackClient {
	return &SlackClient{
		NotifierType: model.NotifierType{Name: name},
		client:       slack.New(token),
		channelId:    channelId,
	}
}

func (s *SlackClient) Notify(message string) error {
	_, _, err := s.client.PostMessage(s.channelId, slack.MsgOptionText(message, false))
	return err
}
