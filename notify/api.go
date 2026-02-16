package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const urlErrorFormat = "error joining url: %v"

type API struct {
	model.BaseNotifier
	ctx         context.Context
	client      *common.HttpClient
	url         string
	headers     http.Header
	tokenSource oauth2.TokenSource
}

func newApi(ctx context.Context, baseNotifier model.BaseNotifier, configApi model.NotificationApi) (*API, error) {
	tokenSource, err := getTokenSource(ctx, configApi.OAuth)
	if err != nil {
		return nil, err
	}
	return &API{
		BaseNotifier: baseNotifier,
		ctx:          ctx,
		client:       common.NewHttpClient(30*time.Second, 2),
		url:          configApi.URL,
		headers:      getHeaders(configApi.Headers),
		tokenSource:  tokenSource,
	}, nil
}

func getHeaders(additionalHeaders map[string]string) http.Header {
	headers := http.Header{
		"Content-Type": []string{"application/json"},
	}
	for k, v := range additionalHeaders {
		headers.Add(k, v)
	}
	return headers
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
		return nil, fmt.Errorf("failed to get oauth2 token: %v", err)
	}
	return tokenSource, nil
}

func (a *API) Message(messageType model.MessageType, message string, params map[string]string) error {
	fullUrl, err := url.JoinPath(a.url, "agent", "status")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	headers, err := a.getHeaders()
	if err != nil {
		return err
	}
	_, err = a.client.Post(a.ctx, fullUrl, headers, model.MessageRequest{
		Type:    string(messageType),
		Message: message,
		Params:  params,
	})
	return err
}

func (a *API) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) error {
	fullUrl, err := url.JoinPath(a.url, "agent", "approval")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	headers, err := a.getHeaders()
	if err != nil {
		return err
	}
	_, err = a.client.Post(a.ctx, fullUrl, headers, toApprovalRequest(pipelineName, changes, link))
	return err
}

func (a *API) StepState(status model.ApplyStatus, stepState model.StateStep, step *model.Step, stepErr error) error {
	fullUrl, err := url.JoinPath(a.url, "steps", "status")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	headers, err := a.getHeaders()
	if err != nil {
		return err
	}
	_, err = a.client.Post(a.ctx, fullUrl, headers, toStatusRequest(status, stepState, step, stepErr))
	return err
}

func (a *API) Modules(accountId string, region string, provider model.ProviderType, config model.Config) error {
	fullUrl, err := url.JoinPath(a.url, "steps", "modules")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	headers, err := a.getHeaders()
	if err != nil {
		return err
	}
	_, err = a.client.Post(a.ctx, fullUrl, headers, toModulesRequest(accountId, region, provider, config))
	return err
}

func (a *API) getHeaders() (http.Header, error) {
	headers := a.headers.Clone()
	if a.tokenSource == nil {
		return headers, nil
	}
	token, err := a.tokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %v", err)
	}
	headers.Set("Authorization", token.Type()+" "+token.AccessToken)
	return headers, nil
}

func toStatusRequest(status model.ApplyStatus, stepState model.StateStep, step *model.Step, err error) model.StepStatusRequest {
	modules := make([]model.ModuleStatusEntity, 0)
	for _, module := range stepState.Modules {
		var metadata map[string]string
		if step != nil {
			for _, m := range step.Modules {
				if m.Name == module.Name {
					metadata = m.Metadata
					break
				}
			}
		}
		modules = append(modules, model.ModuleStatusEntity{
			Name:           module.Name,
			AppliedVersion: module.AppliedVersion,
			Version:        module.Version,
			Metadata:       metadata,
		})
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	return model.StepStatusRequest{
		Status:    status,
		StatusAt:  time.Now().UTC(),
		Step:      stepState.Name,
		Error:     errMsg,
		AppliedAt: stepState.AppliedAt,
		Modules:   modules,
	}
}

func toApprovalRequest(pipelineName string, changes model.PipelineChanges, link string) model.ApprovalRequest {
	return model.ApprovalRequest{
		Name: pipelineName,
		Plan: model.PlanEntity{
			Imported:  changes.Imported,
			Added:     changes.Added,
			Changed:   changes.Changed,
			Destroyed: changes.Destroyed,
		},
		Link: link,
	}
}

func toModulesRequest(accountId string, region string, provider model.ProviderType, config model.Config) model.ModulesRequest {
	var steps []model.StepEntity
	for _, step := range config.Steps {
		var modules []model.ModuleEntity
		for _, module := range step.Modules {
			modules = append(modules, model.ModuleEntity{
				Name:   module.Name,
				Source: module.Source,
			})
		}
		steps = append(steps, model.StepEntity{
			Name:    step.Name,
			Modules: modules,
		})
	}
	return model.ModulesRequest{
		Id:             accountId,
		Region:         region,
		UpdateSchedule: config.Schedule.UpdateCron,
		Provider:       provider,
		Steps:          steps,
	}
}
