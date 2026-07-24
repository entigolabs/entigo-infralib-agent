package oracle

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/secrets"
	"github.com/oracle/oci-go-sdk/v65/vault"
)

const (
	// OCI secret mutations (create/update/cancel-deletion) are ASYNCHRONOUS: the
	// secret sits in a transient state (UPDATING/CREATING) after the call returns and
	// a second mutation before it settles is rejected 409 IncorrectState. So every
	// write waits for ACTIVE first (up to secretActiveTimeout) and retries the write a
	// few times if it still races the transition.
	secretActiveTimeout = 2 * time.Minute
	defaultSecretPoll   = 3 * time.Second
	secretUpdateRetries = 5
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
	secretPoll    time.Duration // poll interval while waiting for a secret to settle; overridable in tests
}

// vaultSecretsAPI / secretsReadAPI are the subsets of the OCI Vault management and
// secret-retrieval clients the SSM uses, extracted so it can be unit tested with
// a fake in-memory vault.
type vaultSecretsAPI interface {
	ListSecrets(context.Context, vault.ListSecretsRequest) (vault.ListSecretsResponse, error)
	CreateSecret(context.Context, vault.CreateSecretRequest) (vault.CreateSecretResponse, error)
	UpdateSecret(context.Context, vault.UpdateSecretRequest) (vault.UpdateSecretResponse, error)
	ScheduleSecretDeletion(context.Context, vault.ScheduleSecretDeletionRequest) (vault.ScheduleSecretDeletionResponse, error)
	CancelSecretDeletion(context.Context, vault.CancelSecretDeletionRequest) (vault.CancelSecretDeletionResponse, error)
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
		secretPoll:    defaultSecretPoll,
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
//
// A secret scheduled for deletion still occupies its name — OCI rejects a fresh
// CreateSecret with "name already exists" and does not free the name until the
// scheduled time — so ensureSecret must recognise that state (which secretOCID
// hides) and REVIVE the secret by cancelling the deletion, then write the new value.
func (s *SSM) ensureSecret(name, value string) (string, error) {
	key := sanitizeKey(name)
	id, state, found, err := s.findSecret(key)
	if err != nil {
		return "", err
	}
	content := base64.StdEncoding.EncodeToString([]byte(value))
	if !found {
		return s.createSecret(key, content)
	}
	if secretPendingDeletion(state) {
		if err = s.cancelDeletion(key, id); err != nil {
			return "", err
		}
		// A just-revived secret can't be read back yet; always write the new value.
		return id, s.applyUpdate(key, id, content)
	}
	// Skip a no-op write only when the secret is settled and already holds the value;
	// a transient state (e.g. UPDATING from a prior write) can't be trusted to read.
	if state == vault.SecretSummaryLifecycleStateActive {
		current, ok, err := s.readSecret(key)
		if err != nil {
			return "", err
		}
		if ok && current == value {
			return id, nil
		}
	}
	return id, s.applyUpdate(key, id, content)
}

// applyUpdate writes the secret content, tolerating OCI's asynchronous state
// machine: it waits for the secret to settle into ACTIVE before writing, and retries
// if a still-in-flight mutation makes UpdateSecret fail 409 IncorrectState (e.g. the
// secret was created/revived moments earlier and is still UPDATING).
func (s *SSM) applyUpdate(name, id, content string) error {
	var lastErr error
	for attempt := 0; attempt < secretUpdateRetries; attempt++ {
		if err := s.waitForSecretActive(name); err != nil {
			return err
		}
		err := s.updateSecret(id, content)
		if err == nil {
			return nil
		}
		if !isIncorrectState(err) {
			return err
		}
		lastErr = err
		if waitErr := s.sleep(); waitErr != nil {
			return waitErr
		}
	}
	return lastErr
}

// waitForSecretActive polls until the named secret is ACTIVE (no in-flight mutation).
func (s *SSM) waitForSecretActive(name string) error {
	deadline := time.Now().Add(secretActiveTimeout)
	logged := false
	for {
		_, state, found, err := s.findSecret(name)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("secret %s disappeared while waiting for it to become active", name)
		}
		if state == vault.SecretSummaryLifecycleStateActive {
			return nil
		}
		if !logged {
			log.Printf("Waiting for Vault secret %s to settle (state %s) before writing it\n", name, state)
			logged = true
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for secret %s to become active (state %s)", name, state)
		}
		if err = s.sleep(); err != nil {
			return err
		}
	}
}

func (s *SSM) sleep() error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-time.After(s.secretPoll):
		return nil
	}
}

// findSecret locates the secret with the given (already sanitized) name regardless
// of lifecycle state — unlike secretOCID, which hides any secret scheduled for
// deletion. It returns the OCID, the lifecycle state and whether such a secret
// exists at all; a fully deleted secret has freed its name, so it counts as absent.
func (s *SSM) findSecret(name string) (string, vault.SecretSummaryLifecycleStateEnum, bool, error) {
	response, err := s.vaultClient.ListSecrets(s.ctx, vault.ListSecretsRequest{
		CompartmentId: &s.compartmentId,
		VaultId:       &s.vaultId,
		Name:          &name,
	})
	if err != nil {
		return "", "", false, fmt.Errorf("failed to list secret %s: %w", name, err)
	}
	for _, item := range response.Items {
		if item.Id == nil || item.SecretName == nil || *item.SecretName != name {
			continue
		}
		// Deleting is a one-way transient toward Deleted (the name frees, the deletion
		// can't be cancelled), so treat it as absent like Deleted — otherwise ensureSecret
		// would spin waitForSecretActive to its timeout on a secret that can never settle.
		// PendingDeletion/SchedulingDeletion are kept: they still occupy the name and are
		// revivable, which ensureSecret relies on.
		switch item.LifecycleState {
		case vault.SecretSummaryLifecycleStateDeleted,
			vault.SecretSummaryLifecycleStateDeleting:
			continue
		}
		return *item.Id, item.LifecycleState, true, nil
	}
	return "", "", false, nil
}

// cancelDeletion reverses a scheduled deletion and waits for the secret to return
// to ACTIVE so a subsequent update doesn't race the state transition.
func (s *SSM) cancelDeletion(name, id string) error {
	_, err := s.vaultClient.CancelSecretDeletion(s.ctx, vault.CancelSecretDeletionRequest{SecretId: &id})
	if err != nil {
		return fmt.Errorf("failed to cancel scheduled deletion of secret %s: %w", name, err)
	}
	return s.waitForSecretActive(name)
}

// isIncorrectState reports the OCI 409 "IncorrectState" a mutation returns while the
// secret is still transitioning from a previous (asynchronous) mutation. errors.As
// unwraps the fmt-wrapped error updateSecret returns.
func isIncorrectState(err error) bool {
	var failure ocicommon.ServiceError
	return errors.As(err, &failure) &&
		failure.GetHTTPStatusCode() == http.StatusConflict && failure.GetCode() == "IncorrectState"
}

// secretPendingDeletion reports whether a scheduled deletion can still be cancelled
// (the secret keeps occupying its name until the deletion actually happens).
func secretPendingDeletion(state vault.SecretSummaryLifecycleStateEnum) bool {
	switch state {
	case vault.SecretSummaryLifecycleStatePendingDeletion,
		vault.SecretSummaryLifecycleStateSchedulingDeletion:
		return true
	}
	return false
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
	// OCI requires ScheduledTimeOfDeletion at least 1 day out; scheduling exactly now+24h
	// lands just under that once request latency is counted (400 InvalidParameter, "invalid
	// range"), so leave a day of margin. It's a soft delete — the secret is recoverable
	// until this time — and the agent never relies on the actual removal happening sooner.
	when := ocicommon.SDKTime{Time: time.Now().Add(48 * time.Hour)}
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
