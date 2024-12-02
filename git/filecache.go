package git

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log/slog"
	"sync"
)

type FileCache struct {
	cache map[string][]byte
	mu    sync.Mutex
}

func NewFileCache() *FileCache {
	return &FileCache{
		cache: make(map[string][]byte),
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
	data, err := util.GetFileFromUrl(url)
	if err != nil {
		return nil, err
	}

	fc.cache[url] = data
	return data, nil
}
