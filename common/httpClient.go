package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type HttpClient struct {
	client  *http.Client
	retries int
}

func NewHttpClient(timeout time.Duration, retries int) *HttpClient {
	return &HttpClient{
		client: &http.Client{
			Timeout: timeout,
		},
		retries: retries,
	}
}

func (c *HttpClient) Post(ctx context.Context, url string, headers http.Header, object any) (*http.Response, error) {
	return c.PostWithParams(ctx, url, headers, object, nil)
}

func (c *HttpClient) PostWithParams(ctx context.Context, url string, headers http.Header, object any, params map[string]string) (*http.Response, error) {
	return c.Do(ctx, http.MethodPost, url, object, headers, params)
}

func (c *HttpClient) Do(ctx context.Context, method string, url string, object any, headers http.Header, params map[string]string) (*http.Response, error) {
	var body *bytes.Reader
	if object != nil {
		jsonObject, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(jsonObject)
	}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	addParams(req, params)
	return c.DoWithRetry(ctx, req, body)
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

func (c *HttpClient) DoWithRetry(ctx context.Context, req *http.Request, body *bytes.Reader) (*http.Response, error) {
	var resp *http.Response
	var err error
	req = req.WithContext(ctx)
	if body != nil {
		req.Body = io.NopCloser(body)
	}
	for i := 0; i < c.retries; i++ {
		if body != nil {
			_, _ = body.Seek(0, io.SeekStart)
		}
		resp, err = c.client.Do(req)
		if err == nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		message := fmt.Sprintf("http request failed, error: %v", err)
		if resp != nil {
			message += fmt.Sprintf(", status code: %d", resp.StatusCode)
		}
		slog.Error(message)
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
