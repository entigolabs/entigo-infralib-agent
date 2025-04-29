package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"io"
	"net/http"
	"net/url"
	"time"
)

const urlErrorFormat = "error joining url: %v"

type API struct {
	model.NotifierType
	ctx     context.Context
	client  *http.Client
	retries int
	url     string
	key     string
}

func newApi(ctx context.Context, notifierType model.NotifierType, configApi model.NotificationApi) *API {
	return &API{
		NotifierType: notifierType,
		ctx:          ctx,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		retries: 2,
		url:     configApi.URL,
		key:     configApi.Key,
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
	_, err = a.Post(a.ctx, fullUrl, a.getHeaders(), model.MessageRequest{Type: requestType, Message: message})
	return err
}

func (a *API) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) error {
	fullUrl, err := url.JoinPath(a.url, "pipeline")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	_, err = a.Post(a.ctx, fullUrl, a.getHeaders(), toPipelineRequest(pipelineName, changes, link))
	return err
}

func (a *API) StepState(status model.ApplyStatus, stepState model.StateStep, step *model.Step) error {
	fullUrl, err := url.JoinPath(a.url, "steps", "status")
	if err != nil {
		return fmt.Errorf(urlErrorFormat, err)
	}
	_, err = a.Post(a.ctx, fullUrl, a.getHeaders(), toModulesRequest(status, stepState, step))
	return err
}

func (a *API) Post(ctx context.Context, url string, headers http.Header, object any) (*http.Response, error) {
	return a.PostWithParams(ctx, url, headers, object, nil)
}

func (a *API) PostWithParams(ctx context.Context, url string, headers http.Header, object any, params map[string]string) (*http.Response, error) {
	return a.Do(ctx, http.MethodPost, url, object, headers, params)
}

func (a *API) Do(ctx context.Context, method string, url string, object any, headers http.Header, params map[string]string) (*http.Response, error) {
	var body io.Reader
	if object != nil {
		jsonObject, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(jsonObject)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	addParams(req, params)
	return a.DoWithRetry(ctx, req)
}

func addParams(req *http.Request, params map[string]string) {
	if len(params) > 0 {
		q := req.URL.Query()
		for k, v := range params {
			q.Add(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}
}

func (a *API) DoWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error
	req = req.WithContext(ctx)
	for i := 0; i < a.retries; i++ {
		resp, err = a.client.Do(req)
		if err == nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		time.Sleep(time.Second * time.Duration(i*2))
	}
	if resp != nil && resp.StatusCode/100 != 2 {
		err = getFailedResponseError(resp)
	}
	return resp, err
}

func getFailedResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("request failed with status code %d, body: %s", resp.StatusCode, body)
}

func (a *API) getHeaders() http.Header {
	return http.Header{
		"NotificationApi-Key": []string{a.key},
		"Content-Type":        []string{"application/json"},
	}
}

func toModulesRequest(status model.ApplyStatus, stepState model.StateStep, step *model.Step) model.ModulesRequest {
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
	return model.ModulesRequest{
		Status:    status,
		StatusAt:  time.Now().UTC(),
		Step:      stepState.Name,
		AppliedAt: stepState.AppliedAt,
		Modules:   modules,
	}
}

func toPipelineRequest(pipelineName string, changes model.PipelineChanges, link string) model.PipelineRequest {
	return model.PipelineRequest{
		Name: pipelineName,
		Plan: model.PlanEntity{
			Add:     changes.Added,
			Change:  changes.Changed,
			Destroy: changes.Destroyed,
		},
		Link: link,
	}
}
