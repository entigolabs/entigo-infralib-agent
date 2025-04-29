package notify

import (
	"fmt"
	goteamsnotify "github.com/atc0005/go-teams-notify/v2"
	"github.com/atc0005/go-teams-notify/v2/adaptivecard"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"strings"
)

func newTeamsClient(baseNotifier model.BaseNotifier, configTeams model.Teams) *BaseNotifier {
	client := goteamsnotify.NewTeamsClient()
	return &BaseNotifier{
		BaseNotifier: baseNotifier,
		MessageFunc: func(message string) error {
			return teamsMessage(client, configTeams.WebhookUrl, message)
		},
	}
}

func teamsMessage(client *goteamsnotify.TeamsClient, webhookUrl, message string) error {
	var body []adaptivecard.Element
	for _, text := range strings.Split(message, "\n") {
		body = append(body, adaptivecard.Element{
			Type: adaptivecard.TypeElementTextBlock,
			Wrap: true,
			Text: text,
		})
	}
	card := adaptivecard.Card{
		Type:    adaptivecard.TypeAdaptiveCard,
		Schema:  adaptivecard.AdaptiveCardSchema,
		Version: fmt.Sprintf(adaptivecard.AdaptiveCardVersionTmpl, adaptivecard.AdaptiveCardMaxVersion),
		Body:    body,
	}
	msg := adaptivecard.Message{
		Type: adaptivecard.TypeMessage,
	}
	err := msg.Attach(card)
	if err != nil {
		return err
	}
	return client.Send(webhookUrl, &msg)
}
