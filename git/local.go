package git

import (
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"os"
	"path/filepath"
)

type local struct {
	source string
}

func NewLocalPath(source string) model.Storage {
	return &local{
		source: source,
	}
}

func (s *local) GetFile(path, _ string) ([]byte, error) {
	fullPath := filepath.Join(s.source, path)
	file, err := os.ReadFile(fullPath)
	if os.IsNotExist(err) {
		return nil, model.NewFileNotFoundError(path)
	}
	return file, nil
}

func (s *local) FileExists(path, _ string) bool {
	return util.FileExists(s.source, path)
}

func (s *local) PathExists(path, _ string) bool {
	fullPath := filepath.Join(s.source, path)
	info, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}
