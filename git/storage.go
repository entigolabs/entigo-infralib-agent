package git

import (
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"os"
	"path/filepath"
)

type Storage interface {
	GetFile(source string, path string, release string) ([]byte, error)
}

type storage struct {
	github Github
}

func NewStorage(github Github) Storage {
	return &storage{
		github: github,
	}
}

func (s *storage) GetFile(source string, path string, release string) ([]byte, error) {
	if util.IsLocalSource(source) {
		return readFile(source, path)
	}
	return s.github.GetRawFileContent(source, path, release)
}

func readFile(source string, path string) ([]byte, error) {
	fullPath := filepath.Join(source, path)
	file, err := os.ReadFile(fullPath)
	if os.IsNotExist(err) {
		return nil, model.NewFileNotFoundError(path)
	}
	return file, nil
}
