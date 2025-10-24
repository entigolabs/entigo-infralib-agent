package notify

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

const urlErrorFormat = "error joining url: %v"

type API struct {
	model.BaseNotifier
	ctx    context.Context
	client *common.HttpClient
	url    string
	key    string
}

func newApi(ctx context.Context, baseNotifier model.BaseNotifier, configApi model.NotificationApi) *API {
	return &API{
		BaseNotifier: baseNotifier,
		ctx:          ctx,
		client:       common.NewHttpClient(30*time.Second, 2),
		url:          configApi.URL,
		key:          configApi.Key,
	}
}

func (a *API) Message(messageType model.MessageType, message string) error {
	fullUrl, err := url.JoinPath(a.url, "message")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	requestType := "INFO"
	if messageType == model.MessageTypeFailure {
		requestType = "ERROR"
	}
	_, err = a.client.Post(a.ctx, fullUrl, a.getHeaders(), model.MessageRequest{Type: requestType, Message: message})
	return err
}

func (a *API) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) error {
	fullUrl, err := url.JoinPath(a.url, "pipeline")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	_, err = a.client.Post(a.ctx, fullUrl, a.getHeaders(), toPipelineRequest(pipelineName, changes, link))
	return err
}

func (a *API) StepState(status model.ApplyStatus, stepState model.StateStep, step *model.Step, stepErr error) error {
	fullUrl, err := url.JoinPath(a.url, "steps", "status")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	_, err = a.client.Post(a.ctx, fullUrl, a.getHeaders(), toModulesRequest(status, stepState, step, stepErr))
	return err
}

func (a *API) getHeaders() http.Header {
	return http.Header{
		"Api-Key":      []string{a.key},
		"Content-Type": []string{"application/json"},
	}
}

func toModulesRequest(status model.ApplyStatus, stepState model.StateStep, step *model.Step, err error) model.ModulesRequest {
	modules := make([]model.ModuleEntity, 0)
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
		modules = append(modules, model.ModuleEntity{
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
	return model.ModulesRequest{
		Status:    status,
		StatusAt:  time.Now().UTC(),
		Step:      stepState.Name,
		Error:     errMsg,
		AppliedAt: stepState.AppliedAt,
		Modules:   modules,
	}
}

func toPipelineRequest(pipelineName string, changes model.PipelineChanges, link string) model.PipelineRequest {
	return model.PipelineRequest{
		Name: pipelineName,
		Plan: model.PlanEntity{
			Import:  changes.Imported,
			Add:     changes.Added,
			Change:  changes.Changed,
			Destroy: changes.Destroyed,
		},
		Link: link,
	}
}
