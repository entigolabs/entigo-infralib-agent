package service

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

type callback struct {
	ctx     context.Context
	client  *http.Client
	retries int
	url     string
	key     string
}

type Callback interface {
	PostStepState(status model.ApplyStatus, stepState model.StateStep) error
}

func NewCallback(ctx context.Context, config model.Callback) Callback {
	if config.URL == "" {
		return nil
	}
	return &callback{
		ctx: ctx,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		retries: 2,
		url:     config.URL,
		key:     config.Key,
	}
}

func (c *callback) PostStepState(status model.ApplyStatus, stepState model.StateStep) error {
	fullUrl, err := url.JoinPath(c.url, "steps", "status")
	if err != nil {
		return fmt.Errorf("error joining url: %v", err)
	}
	headers := c.getHeaders()
	headers.Add("Content-Type", "application/json")
	request := model.ToModulesRequest(status, stepState)
	_, err = c.Post(c.ctx, fullUrl, headers, request)
	return err
}

func (c *callback) Post(ctx context.Context, url string, headers http.Header, object any) (*http.Response, error) {
	return c.PostWithParams(ctx, url, headers, object, nil)
}

func (c *callback) PostWithParams(ctx context.Context, url string, headers http.Header, object any, params map[string]string) (*http.Response, error) {
	return c.Do(ctx, http.MethodPost, url, object, headers, params)
}

func (c *callback) Do(ctx context.Context, method string, url string, object any, headers http.Header, params map[string]string) (*http.Response, error) {
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
	return c.DoWithRetry(ctx, req)
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

func (c *callback) DoWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	var resp *http.Response
	var err error
	req = req.WithContext(ctx)
	for i := 0; i < c.retries; i++ {
		resp, err = c.client.Do(req)
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

func (c *callback) getHeaders() http.Header {
	return http.Header{
		"Api-Key": []string{c.key},
	}
}
