package git

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const maxRetries = 3

type FileCache struct {
	ctx   context.Context
	cache map[string][]byte
	mu    sync.Mutex
}

func NewFileCache(ctx context.Context) *FileCache {
	return &FileCache{
		cache: make(map[string][]byte),
		ctx:   ctx,
	}
}

func (fc *FileCache) GetFile(url string) ([]byte, error) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if data, found := fc.cache[url]; found {
		slog.Debug(fmt.Sprintf("Using cached file content %s\n", url))
		return data, nil
	}

	slog.Debug(fmt.Sprintf("Getting raw file content %s", url))
	data, err := fc.GetFileFromUrl(url)
	if err != nil {
		return nil, err
	}

	fc.cache[url] = data
	return data, nil
}

func (fc *FileCache) GetFileFromUrl(fileUrl string) ([]byte, error) {
	resp, err := fc.GetFileWithRetry(fileUrl)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			log.Printf("Failed to close response body: %s", err)
		}
	}(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (fc *FileCache) GetFileWithRetry(fileUrl string) (*http.Response, error) {
	var resp *http.Response
	var err error
	request, err := http.NewRequestWithContext(fc.ctx, http.MethodGet, fileUrl, nil)
	if err != nil {
		return nil, err
	}
	for i := 0; i < maxRetries; i++ {
		resp, err = http.DefaultClient.Do(request)
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			return resp, nil
		}
		if i < maxRetries-1 {
			time.Sleep(time.Second * time.Duration(i*2))
			slog.Debug(fmt.Sprintf("Retrying file request %s: %s", fileUrl, err))
		}
	}
	if err == nil && resp == nil {
		return nil, fmt.Errorf("empty response from %s", fileUrl)
	}
	return resp, err
}
