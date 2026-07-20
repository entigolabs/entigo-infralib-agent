package oracle

import (
	"errors"
	"strings"
	"sync"

	"github.com/entigolabs/entigo-infralib-agent/model"
)

const (
	paramPrefix  = "params/"
	secretPrefix = "secrets/"
)

// SSM implements model.SSM over an Object Storage bucket. OCI has no direct
// Parameter Store / Secrets Manager equivalent that fits the agent's frequent
// read/write/delete access pattern: Vault secrets are created asynchronously
// and cannot be hard-deleted (deletion is scheduled 1-30 days out), which makes
// them unfit for high-traffic config parameters. Objects in a private,
// at-rest-encrypted bucket give immediate, consistent CRUD instead. Parameters
// and secrets live under separate key prefixes in the same bucket.
//
// TODO(phase-5): move sensitive secrets to OCI Vault once encryption lands.
type SSM struct {
	storage    objectStore
	kmsKeyId   string
	ensureOnce sync.Once
	ensureErr  error
}

// objectStore is the subset of Storage the SSM relies on, extracted so the
// parameter/secret logic can be unit tested without a live Object Storage client.
type objectStore interface {
	CreateBucket(skipDelay bool) error
	GetFile(file string) ([]byte, error)
	PutFile(file string, content []byte) error
	DeleteFile(file string) error
	ListFolderFiles(folder string) ([]string, error)
}

func NewSSM(storage objectStore) model.SSM {
	return &SSM{storage: storage}
}

func (s *SSM) ensureBucket() error {
	s.ensureOnce.Do(func() {
		s.ensureErr = s.storage.CreateBucket(true)
	})
	return s.ensureErr
}

func paramKey(name string) string {
	return paramPrefix + sanitizeKey(name)
}

func secretKey(name string) string {
	return secretPrefix + sanitizeKey(name)
}

func sanitizeKey(name string) string {
	return strings.ReplaceAll(strings.TrimLeft(name, "/"), "/", "-")
}

func (s *SSM) AddEncryptionKeyId(keyId string) {
	s.kmsKeyId = keyId
}

func (s *SSM) GetParameter(name string) (*model.Parameter, error) {
	content, err := s.storage.GetFile(paramKey(name))
	if err != nil {
		return nil, err
	}
	if content == nil {
		return nil, &model.ParameterNotFoundError{Name: name}
	}
	value := string(content)
	return &model.Parameter{Value: &value}, nil
}

func (s *SSM) ParameterExists(name string) (bool, error) {
	_, err := s.GetParameter(name)
	if err != nil {
		var notFoundErr *model.ParameterNotFoundError
		if errors.As(err, &notFoundErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *SSM) PutParameter(name string, value string) error {
	current, err := s.GetParameter(name)
	if err != nil {
		var notFoundErr *model.ParameterNotFoundError
		if !errors.As(err, &notFoundErr) {
			return err
		}
	}
	if current != nil && *current.Value == value {
		return nil
	}
	if err = s.ensureBucket(); err != nil {
		return err
	}
	return s.storage.PutFile(paramKey(name), []byte(value))
}

func (s *SSM) DeleteParameter(name string) error {
	return s.storage.DeleteFile(paramKey(name))
}

func (s *SSM) ListParameters() ([]string, error) {
	files, err := s.storage.ListFolderFiles(paramPrefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(files))
	for _, file := range files {
		keys = append(keys, strings.TrimPrefix(file, paramPrefix))
	}
	return keys, nil
}

func (s *SSM) PutSecret(name string, value string) error {
	if err := s.ensureBucket(); err != nil {
		return err
	}
	return s.storage.PutFile(secretKey(name), []byte(value))
}

func (s *SSM) DeleteSecret(name string) error {
	return s.storage.DeleteFile(secretKey(name))
}
