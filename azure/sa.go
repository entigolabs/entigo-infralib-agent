package azure

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

const (
	federatedCredentialName = "trust"
	federatedAudience       = "api://AzureADTokenExchange"
)

func serviceAccountName(prefix string) string {
	return prefix + "-sa"
}

// parseTrust splits a trust role "<issuer>|<subject>" into its OIDC issuer and subject.
func parseTrust(trustRole string) (string, string, error) {
	issuer, subject, found := strings.Cut(trustRole, "|")
	issuer, subject = strings.TrimSpace(issuer), strings.TrimSpace(subject)
	if !found || !strings.HasPrefix(issuer, "https://") || subject == "" {
		return "", "", fmt.Errorf("azure trust role must be \"<issuer url>|<subject>\", e.g. " +
			"\"https://token.actions.githubusercontent.com|repo:org/repo:ref:refs/heads/main\"")
	}
	return issuer, subject, nil
}

// serviceAccountRoles are scoped to the agent's resource group, like the AWS service account
// policy is scoped to the agent's resources: enough to run a bootstrapped deployment, but not
// to create role assignments, so bootstrap needs the operator.
var serviceAccountRoles = []string{roleContributor, roleKeyVaultAdministrator, roleStorageBlobDataContributor}

// CreateServiceAccount creates a managed identity for external CI/CD. A managed identity
// has no secrets, so the CI/CD system signs in with its own OIDC token through a
// federated credential, which is why the trust role is required.
func (a *azureService) CreateServiceAccount(SAFlags common.ServiceAccount) error {
	if SAFlags.TrustRole == "" {
		return errors.New("azure service accounts authenticate with OIDC, trust-role \"<issuer url>|<subject>\" must be set")
	}
	issuer, subject, err := parseTrust(SAFlags.TrustRole)
	if err != nil {
		return err
	}
	iam, err := NewIAM(a.ctx, a.credential, a.subscriptionId, a.resourceGroup, a.location)
	if err != nil {
		return err
	}
	if err = iam.EnsureResourceGroup(); err != nil {
		return err
	}
	name := serviceAccountName(a.cloudPrefix)
	account, err := iam.EnsureIdentity(name)
	if err != nil {
		return err
	}
	for _, role := range serviceAccountRoles {
		if err = iam.AssignRole(iam.resourceGroupScope(), account.PrincipalId, "ServicePrincipal", role); err != nil {
			return err
		}
	}
	credentials, err := armmsi.NewFederatedIdentityCredentialsClient(a.subscriptionId, a.credential, nil)
	if err != nil {
		return err
	}
	existing, err := credentials.Get(a.ctx, a.resourceGroup, name, federatedCredentialName, nil)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("failed to get federated credential of %s: %w", name, err)
	}
	if err == nil && existing.Properties != nil {
		if *existing.Properties.Issuer == issuer && *existing.Properties.Subject == subject {
			printServiceAccount(name, account.ClientId, a.subscriptionId, a.tenantId())
			return nil
		}
		if !SAFlags.RotateCredentials {
			slog.Error(common.PrefixError(fmt.Errorf("service account %s trusts a different subject %s, use rotate-credentials flag to update it",
				name, *existing.Properties.Subject)))
			return nil
		}
	}
	_, err = credentials.CreateOrUpdate(a.ctx, a.resourceGroup, name, federatedCredentialName, armmsi.FederatedIdentityCredential{
		Properties: &armmsi.FederatedIdentityCredentialProperties{
			Issuer:    &issuer,
			Subject:   &subject,
			Audiences: []*string{new(federatedAudience)},
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to create federated credential for %s: %w", name, err)
	}
	log.Printf("Service account %s trusts %s from %s\n", name, subject, issuer)
	printServiceAccount(name, account.ClientId, a.subscriptionId, a.tenantId())
	return nil
}

func (a *azureService) tenantId() string {
	executor, err := currentPrincipal(a.ctx, a.credential)
	if err != nil {
		return ""
	}
	return executor.TenantId
}

func printServiceAccount(name, clientId, subscriptionId, tenantId string) {
	fmt.Printf("Service account %s\nClient id:\n%s\nTenant id:\n%s\nSubscription id:\n%s\n", name, clientId, tenantId, subscriptionId)
}

func (a *azureService) deleteServiceAccount(iam *IAM) {
	name := serviceAccountName(a.cloudPrefix)
	account, err := iam.GetIdentity(name)
	if err != nil {
		if !isNotFound(err) {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to get service account %s: %s", name, err)))
		}
		return
	}
	for _, role := range serviceAccountRoles {
		if err = iam.DeleteRoleAssignment(iam.resourceGroupScope(), account.PrincipalId, role); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete role %s assignment of %s: %s", role, name, err)))
		}
	}
	if err = iam.DeleteIdentity(name); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete service account %s: %s", name, err)))
		return
	}
	log.Printf("Deleted service account %s\n", name)
}
