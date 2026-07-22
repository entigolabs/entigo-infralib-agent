package oracle

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/secrets"
	"github.com/oracle/oci-go-sdk/v65/vault"
)

// SSM implements model.SSM over OCI Vault secrets, encrypted with the agent-owned
// master key (see KMS). Both parameters and secrets are stored as Vault secrets —
// custom parameters are few, and terraform module outputs are read bucket-first
// (service/replace.go), so the parameter path is effectively a rarely-hit
// fallback. Storing everything in the Vault keeps all sensitive material off the
// (now single) bucket and under the customer-managed key.
//
// Caveat inherited from OCI: a Vault secret cannot be hard-deleted — deletion is
// scheduled — so Delete* schedules deletion at the earliest allowed time. Updates
// create a new secret version, so rewriting an existing value never deletes.
type SSM struct {
	ctx           context.Context
	vaultClient   vaultSecretsAPI
	secretsClient secretsReadAPI
	compartmentId string
	vaultId       string
	keyId         string
}

// vaultSecretsAPI / secretsReadAPI are the subsets of the OCI Vault management and
// secret-retrieval clients the SSM uses, extracted so it can be unit tested with
// a fake in-memory vault.
type vaultSecretsAPI interface {
	ListSecrets(context.Context, vault.ListSecretsRequest) (vault.ListSecretsResponse, error)
	CreateSecret(context.Context, vault.CreateSecretRequest) (vault.CreateSecretResponse, error)
	UpdateSecret(context.Context, vault.UpdateSecretRequest) (vault.UpdateSecretResponse, error)
	ScheduleSecretDeletion(context.Context, vault.ScheduleSecretDeletionRequest) (vault.ScheduleSecretDeletionResponse, error)
}

type secretsReadAPI interface {
	GetSecretBundleByName(context.Context, secrets.GetSecretBundleByNameRequest) (secrets.GetSecretBundleByNameResponse, error)
}

// secretResolver is the subset of SSM the Builder uses to turn an env var into a
// Vault secret OCID (for build-spec vaultVariables) — kept small so the Builder
// doesn't depend on the whole model.SSM surface.
type secretResolver interface {
	secretOCID(name string) (string, error)
	ensureSecret(name, value string) (string, error)
}

func NewSSM(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, vaultId, keyId string) (*SSM, error) {
	vaultClient, err := vault.NewVaultsClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	secretsClient, err := secrets.NewSecretsClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		vaultClient.SetRegion(region)
		secretsClient.SetRegion(region)
	}
	return &SSM{
		ctx:           ctx,
		vaultClient:   vaultClient,
		secretsClient: secretsClient,
		compartmentId: compartmentId,
		vaultId:       vaultId,
		keyId:         keyId,
	}, nil
}

// sanitizeKey maps an SSM key to a legal OCI Vault secret name (letters, digits,
// hyphens, underscores, periods); every other char becomes a hyphen.
func sanitizeKey(name string) string {
	name = strings.TrimLeft(name, "/")
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// AddEncryptionKeyId is a no-op: the store is always encrypted with the agent's
// own key, injected at construction. Kept to satisfy model.SSM.
func (s *SSM) AddEncryptionKeyId(_ string) {}

func (s *SSM) GetParameter(name string) (*model.Parameter, error) {
	value, found, err := s.readSecret(sanitizeKey(name))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &model.ParameterNotFoundError{Name: name}
	}
	return &model.Parameter{Value: &value}, nil
}

func (s *SSM) ParameterExists(name string) (bool, error) {
	id, err := s.secretOCID(sanitizeKey(name))
	if err != nil {
		return false, err
	}
	return id != "", nil
}

func (s *SSM) PutParameter(name string, value string) error {
	_, err := s.ensureSecret(name, value)
	return err
}

func (s *SSM) PutSecret(name string, value string) error {
	_, err := s.ensureSecret(name, value)
	return err
}

func (s *SSM) DeleteParameter(name string) error {
	return s.scheduleDeletion(sanitizeKey(name))
}

func (s *SSM) DeleteSecret(name string) error {
	return s.scheduleDeletion(sanitizeKey(name))
}

// ListParameters returns the names of the agent's Vault secrets (those tagged by
// the agent), used by the list-custom CLI. Secret names are the sanitized keys.
func (s *SSM) ListParameters() ([]string, error) {
	var names []string
	var page *string
	for {
		response, err := s.vaultClient.ListSecrets(s.ctx, vault.ListSecretsRequest{
			CompartmentId: &s.compartmentId,
			VaultId:       &s.vaultId,
			Page:          page,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list secrets: %w", err)
		}
		for _, item := range response.Items {
			if item.SecretName == nil || secretDeleted(item.LifecycleState) {
				continue
			}
			if item.FreeformTags[model.ResourceTagKey] != model.ResourceTagValue {
				continue
			}
			names = append(names, *item.SecretName)
		}
		if response.OpcNextPage == nil {
			break
		}
		page = response.OpcNextPage
	}
	return names, nil
}

// ensureSecret upserts a Vault secret and returns its OCID. A no-op write (value
// unchanged) skips the update but still returns the OCID, so callers can use it as
// a resolve-or-create for build-spec vaultVariables.
func (s *SSM) ensureSecret(name, value string) (string, error) {
	key := sanitizeKey(name)
	id, err := s.secretOCID(key)
	if err != nil {
		return "", err
	}
	content := base64.StdEncoding.EncodeToString([]byte(value))
	if id == "" {
		return s.createSecret(key, content)
	}
	current, found, err := s.readSecret(key)
	if err != nil {
		return "", err
	}
	if found && current == value {
		return id, nil
	}
	if err = s.updateSecret(id, content); err != nil {
		return "", err
	}
	return id, nil
}

func (s *SSM) createSecret(name, base64Content string) (string, error) {
	response, err := s.vaultClient.CreateSecret(s.ctx, vault.CreateSecretRequest{
		CreateSecretDetails: vault.CreateSecretDetails{
			CompartmentId: &s.compartmentId,
			VaultId:       &s.vaultId,
			KeyId:         &s.keyId,
			SecretName:    &name,
			SecretContent: vault.Base64SecretContentDetails{Content: &base64Content},
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create secret %s: %w", name, err)
	}
	return *response.Id, nil
}

func (s *SSM) updateSecret(id, base64Content string) error {
	_, err := s.vaultClient.UpdateSecret(s.ctx, vault.UpdateSecretRequest{
		SecretId: &id,
		UpdateSecretDetails: vault.UpdateSecretDetails{
			SecretContent: vault.Base64SecretContentDetails{Content: &base64Content},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update secret %s: %w", id, err)
	}
	return nil
}

// secretOCID returns the OCID of the live secret with the given (already
// sanitized) name, or "" if none exists.
func (s *SSM) secretOCID(name string) (string, error) {
	response, err := s.vaultClient.ListSecrets(s.ctx, vault.ListSecretsRequest{
		CompartmentId: &s.compartmentId,
		VaultId:       &s.vaultId,
		Name:          &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list secret %s: %w", name, err)
	}
	for _, item := range response.Items {
		if item.SecretName != nil && *item.SecretName == name && !secretDeleted(item.LifecycleState) {
			return *item.Id, nil
		}
	}
	return "", nil
}

func (s *SSM) readSecret(name string) (string, bool, error) {
	response, err := s.secretsClient.GetSecretBundleByName(s.ctx, secrets.GetSecretBundleByNameRequest{
		SecretName: &name,
		VaultId:    &s.vaultId,
	})
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("failed to read secret %s: %w", name, err)
	}
	content, ok := response.SecretBundleContent.(secrets.Base64SecretBundleContentDetails)
	if !ok || content.Content == nil {
		return "", false, fmt.Errorf("secret %s has no base64 content", name)
	}
	decoded, err := base64.StdEncoding.DecodeString(*content.Content)
	if err != nil {
		return "", false, fmt.Errorf("failed to decode secret %s: %w", name, err)
	}
	return string(decoded), true, nil
}

// scheduleDeletion schedules the earliest-allowed deletion of the named secret; a
// missing secret is not an error. Hard delete is not possible on OCI.
func (s *SSM) scheduleDeletion(name string) error {
	id, err := s.secretOCID(name)
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	when := ocicommon.SDKTime{Time: time.Now().Add(24 * time.Hour)}
	_, err = s.vaultClient.ScheduleSecretDeletion(s.ctx, vault.ScheduleSecretDeletionRequest{
		SecretId: &id,
		ScheduleSecretDeletionDetails: vault.ScheduleSecretDeletionDetails{
			TimeOfDeletion: &when,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to schedule deletion of secret %s: %w", name, err)
	}
	return nil
}

func secretDeleted(state vault.SecretSummaryLifecycleStateEnum) bool {
	switch state {
	case vault.SecretSummaryLifecycleStateDeleting,
		vault.SecretSummaryLifecycleStateDeleted,
		vault.SecretSummaryLifecycleStatePendingDeletion,
		vault.SecretSummaryLifecycleStateSchedulingDeletion:
		return true
	}
	return false
}
