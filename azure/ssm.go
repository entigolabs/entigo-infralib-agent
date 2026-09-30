package azure

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const secretRecoveryTimeout = 2 * time.Minute

// SSM backs both parameters and secrets with Key Vault secrets. Key Vault names allow
// only letters, digits and hyphens, so the original key is kept in a tag for listing.
// Without a client the vault doesn't exist: nothing is found and deleting is a no-op.
type SSM struct {
	ctx      context.Context
	client   *azsecrets.Client
	vaultURI string
}

func NewSSM(ctx context.Context, credential azcore.TokenCredential, vaultURI string) (*SSM, error) {
	client, err := azsecrets.NewClient(vaultURI, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create key vault secrets client: %w", err)
	}
	return &SSM{ctx: ctx, client: client, vaultURI: vaultURI}, nil
}

func newMissingSSM(ctx context.Context) *SSM {
	return &SSM{ctx: ctx}
}

func (s *SSM) AddEncryptionKeyId(_ string) {}

func (s *SSM) GetParameter(name string) (*model.Parameter, error) {
	value, found, err := s.readSecret(secretName(name))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &model.ParameterNotFoundError{Name: name}
	}
	return &model.Parameter{Value: &value}, nil
}

func (s *SSM) ParameterExists(name string) (bool, error) {
	_, found, err := s.readSecret(secretName(name))
	return found, err
}

func (s *SSM) PutParameter(name string, value string) error {
	return s.ensureSecret(name, value)
}

func (s *SSM) PutSecret(name string, value string) error {
	return s.ensureSecret(name, value)
}

func (s *SSM) DeleteParameter(name string) error {
	return s.deleteSecret(secretName(name))
}

func (s *SSM) DeleteSecret(name string) error {
	return s.deleteSecret(secretName(name))
}

func (s *SSM) ListParameters() ([]string, error) {
	if s.client == nil {
		return nil, nil
	}
	var names []string
	pager := s.client.NewListSecretPropertiesPager(nil)
	for pager.More() {
		page, err := pager.NextPage(s.ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list secrets: %w", err)
		}
		for _, secret := range page.Value {
			if secret.Tags == nil || secret.Tags[model.ResourceTagKey] == nil ||
				*secret.Tags[model.ResourceTagKey] != model.ResourceTagValue {
				continue
			}
			if original := secret.Tags[secretNameTag]; original != nil {
				names = append(names, *original)
			} else if secret.ID != nil {
				names = append(names, secret.ID.Name())
			}
		}
	}
	return names, nil
}

// secretURI is the versionless secret URI Container Apps resolves to the latest version.
func (s *SSM) secretURI(name string) string {
	return fmt.Sprintf("%s/secrets/%s", strings.TrimSuffix(s.vaultURI, "/"), secretName(name))
}

func (s *SSM) secretExists(name string) (bool, error) {
	_, found, err := s.readSecret(secretName(name))
	return found, err
}

func (s *SSM) readSecret(key string) (string, bool, error) {
	if s.client == nil {
		return "", false, nil
	}
	var value string
	var found bool
	err := retryUntilAuthorized(s.ctx, "reading key vault secrets", func() error {
		response, err := s.client.GetSecret(s.ctx, key, "", nil)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		found = true
		if response.Value != nil {
			value = *response.Value
		}
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("failed to get secret %s: %w", key, err)
	}
	return value, found, nil
}

// ensureSecret skips unchanged values, since every write adds a secret version. A
// soft-deleted secret still occupies its name, so it's recovered before writing.
func (s *SSM) ensureSecret(name, value string) error {
	if s.client == nil {
		return errors.New("key vault not found, run bootstrap first")
	}
	key := secretName(name)
	current, found, err := s.readSecret(key)
	if err != nil {
		return err
	}
	if found && current == value {
		return nil
	}
	tags := resourceTags()
	tags[secretNameTag] = &name
	parameters := azsecrets.SetSecretParameters{Value: &value, Tags: tags}
	_, err = s.client.SetSecret(s.ctx, key, parameters, nil)
	if isStatus(err, http.StatusConflict) {
		if err = s.recoverSecret(key); err != nil {
			return err
		}
		_, err = s.client.SetSecret(s.ctx, key, parameters, nil)
	}
	if err != nil {
		return fmt.Errorf("failed to set secret %s: %w", key, err)
	}
	return nil
}

func (s *SSM) recoverSecret(key string) error {
	log.Printf("Recovering soft-deleted secret %s\n", key)
	_, err := s.client.RecoverDeletedSecret(s.ctx, key, nil)
	if err != nil {
		return fmt.Errorf("failed to recover secret %s: %w", key, err)
	}
	deadline := time.Now().Add(secretRecoveryTimeout)
	for {
		_, err = s.client.GetSecret(s.ctx, key, "", nil)
		if err == nil {
			return nil
		}
		if !isNotFound(err) || time.Now().After(deadline) {
			return fmt.Errorf("failed to wait for recovered secret %s: %w", key, err)
		}
		if err = util.Sleep(s.ctx, 5*time.Second); err != nil {
			return err
		}
	}
}

func (s *SSM) deleteSecret(key string) error {
	if s.client == nil {
		return nil
	}
	_, err := s.client.DeleteSecret(s.ctx, key, nil)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("failed to delete secret %s: %w", key, err)
	}
	return nil
}
