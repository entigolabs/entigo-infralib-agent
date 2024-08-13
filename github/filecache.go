package github

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/util"
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
		common.Logger.Printf("Using cached file content %s\n", url)
		return data, nil
	}

	common.Logger.Printf("Getting raw file content %s\n", url)
	data, err := util.GetFileFromUrl(url)
	if err != nil {
		return nil, err
	}

	fc.cache[url] = data
	return data, nil
}
