package api

import (
	"context"
	"fmt"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// Generation requires dependency tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
//go:generate go tool oapi-codegen --config=../../openapi/oapi-config.yaml -o client.gen.go ../../openapi/notification-api.yaml

var _ model.Notifier = (*API)(nil)

type API struct {
	model.BaseNotifier
	ctx        context.Context
	client     *Client
	campaignId CampaignId
}

func NewApi(ctx context.Context, baseNotifier model.BaseNotifier, configApi model.NotificationApi) (*API, error) {
	tokenSource, err := getTokenSource(ctx, configApi.OAuth)
	if err != nil {
		return nil, err
	}
	httpClient := NewHttpClient(ctx, 30*time.Second, 3, tokenSource, configApi.Headers)
	apiClient, err := NewClient(configApi.URL, WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	return &API{
		BaseNotifier: baseNotifier,
		ctx:          ctx,
		client:       apiClient,
		campaignId:   uuid.New(),
	}, nil
}

func getTokenSource(ctx context.Context, auth *model.ApiOauth) (oauth2.TokenSource, error) {
	if auth == nil {
		return nil, nil
	}
	config := clientcredentials.Config{
		ClientID:     auth.ClientId,
		ClientSecret: auth.ClientSecret,
		TokenURL:     auth.TokenURL,
		Scopes:       auth.Scopes,
	}
	tokenSource := oauth2.ReuseTokenSourceWithExpiry(nil, config.TokenSource(ctx), 5*time.Minute)
	_, err := tokenSource.Token() // Validate token source
	if err != nil {
		return nil, fmt.Errorf("failed to get oauth2 token: %w", err)
	}
	return tokenSource, nil
}

func (a *API) HandleCampaign(msg model.CampaignMessage) error {
	notification, err := toCampaignNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(msg.Ctx, notification)
}

func (a *API) HandleSchedule(msg model.ScheduleMessage) error {
	notification, err := toScheduleNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(a.ctx, notification)
}

func (a *API) HandleApproval(msg model.ApprovalMessage) error {
	notification, err := toApprovalNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(a.ctx, notification)
}

func (a *API) HandleManualApproval(msg model.ManualApprovalMessage) error {
	notification, err := toManualApprovalNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(a.ctx, notification)
}

func (a *API) HandleStepState(msg model.StepStateMessage) error {
	notification, err := toStepStateNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(a.ctx, notification)
}

func (a *API) HandleModules(msg model.ModulesMessage) error {
	notification, err := toModulesNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(a.ctx, notification)
}

func (a *API) HandleSources(msg model.SourcesMessage) error {
	notification, err := toSourcesNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(a.ctx, notification)
}

func (a *API) HandlePipelineState(msg model.PipelineStateMessage) error {
	notification, err := toPipelineStateNotification(a.Context, msg)
	if err != nil {
		return err
	}
	return a.post(a.ctx, notification)
}

func (a *API) post(ctx context.Context, notification Notification) error {
	_, err := a.client.PostNotification(ctx, a.campaignId, notification)
	return err
}
