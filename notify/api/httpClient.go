package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/oauth2"
)

type HttpClient struct {
	client  *http.Client
	timeout time.Duration
	retries int
	headers map[string]string
}

func NewHttpClient(ctx context.Context, timeout time.Duration, retries int, source oauth2.TokenSource, headers map[string]string) *HttpClient {
	var client *http.Client
	if source == nil {
		client = &http.Client{}
	} else {
		client = oauth2.NewClient(ctx, source)
	}
	return &HttpClient{
		client:  client,
		timeout: timeout,
		retries: retries,
		headers: headers,
	}
}

func (c *HttpClient) Do(req *http.Request) (*http.Response, error) {
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}
	if c.timeout > 0 {
		// Ctx based timeout is required instead of client timeout to avoid oauth2 CancelRequest func from logging once
		// deprecated: golang.org/x/oauth2: Transport.CancelRequest no longer does anything; use contexts
		ctx, cancel := context.WithTimeout(req.Context(), c.timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}
	if req.Body == nil {
		return c.doWithRetry(req, nil)
	}
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	return c.doWithRetry(req, bytes.NewReader(bodyBytes))
}

func (c *HttpClient) doWithRetry(req *http.Request, body *bytes.Reader) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < c.retries; attempt++ {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if body != nil {
			_, _ = body.Seek(0, io.SeekStart)
			req.Body = io.NopCloser(body)
		}
		resp, err = c.client.Do(req)
		if err == nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		if err != nil {
			slog.Error(fmt.Sprintf("http request failed: %v", err))
		} else if !isRetryable(resp.StatusCode) {
			return resp, getFailedResponseError(resp)
		}
		if attempt == c.retries-1 {
			break
		}
		if err := sleep(req.Context(), retryDelay(resp, attempt)); err != nil {
			return nil, err
		}
	}
	if err == nil && resp != nil && resp.StatusCode/100 != 2 {
		err = getFailedResponseError(resp)
	}
	return resp, err
}

func isRetryable(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= 500
}

func retryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return time.Duration(attempt*2) * time.Second
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func getFailedResponseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewBuffer(body))
	if len(body) != 0 {
		slog.Error(fmt.Sprintf("failed request body: %s", string(body)))
	}
	return fmt.Errorf("http request failed with status code %d", resp.StatusCode)
}
