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

func (l *local) GetFile(path, _ string) ([]byte, error) {
	fullPath := filepath.Join(l.source, path)
	file, err := os.ReadFile(fullPath)
	if os.IsNotExist(err) {
		return nil, model.NewFileNotFoundError(path)
	}
	return file, nil
}

func (l *local) FileExists(path, _ string) bool {
	return util.FileExists(l.source, path)
}

func (l *local) PathExists(path, _ string) (bool, error) {
	fullPath := filepath.Join(l.source, path)
	info, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func (l *local) CalculateChecksums(_ string) (map[string][]byte, error) {
	return make(map[string][]byte), nil
}
