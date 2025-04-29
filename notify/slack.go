package notify

import (
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/slack-go/slack"
)

func newSlackClient(baseNotifier model.BaseNotifier, configSlack model.Slack) *BaseNotifier {
	client := slack.New(configSlack.Token)
	return &BaseNotifier{
		BaseNotifier: baseNotifier,
		MessageFunc: func(message string) error {
			return slackMessage(client, configSlack.ChannelId, message)
		},
	}
}

func slackMessage(client *slack.Client, channelId, message string) error {
	_, _, err := client.PostMessage(channelId, slack.MsgOptionText(message, false))
	return err
}
