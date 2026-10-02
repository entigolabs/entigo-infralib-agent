package azure

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const (
	containerName     = "tfstate"
	maxJobNameLen     = 32
	maxVaultLen       = 24
	maxStorageLen     = 24
	nameHashLen       = 8
	longestJobCommand = "apply-destroy"
	secretNameTag     = "infralib-name"
	approvalTagKey    = "infralib-approval"
	executionTagKey   = "infralib-execution"
)

var jobNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

func resourceGroup(azure common.Azure, prefix string) string {
	if azure.ResourceGroup != "" {
		return azure.ResourceGroup
	}
	return fmt.Sprintf("%s-infralib-%s", prefix, azure.Location)
}

func identityName(prefix string) string {
	return prefix + "-infralib"
}

func storageIdentityName(prefix string) string {
	return prefix + "-infralib-storage"
}

func environmentName(prefix string) string {
	return truncate(prefix+"-infralib", 60)
}

func workspaceName(prefix string) string {
	return truncate(prefix+"-infralib", 63)
}

func agentKeyName(prefix string) string {
	return prefix + "-agent"
}

func nameHash(parts ...string) string {
	return util.HashCode(strings.Join(parts, "/"))
}

// storageAccountName is globally unique, 3-24 lowercase letters and digits.
func storageAccountName(prefix, subscriptionId, location string) string {
	base := truncate(strings.ReplaceAll(slug(prefix), "-", ""), maxStorageLen-nameHashLen)
	return base + nameHash(prefix, subscriptionId, location)
}

// vaultName is globally unique, 3-24 chars, starts with a letter, no consecutive hyphens.
func vaultName(prefix, subscriptionId, location string) string {
	base := strings.TrimRight(truncate(slug(prefix), maxVaultLen-nameHashLen-1), "-")
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = "ei" + base
		base = strings.TrimRight(truncate(base, maxVaultLen-nameHashLen-1), "-")
	}
	return base + "-" + nameHash(prefix, subscriptionId, location)
}

// jobName is a Container Apps job name: lowercase letters, digits and single hyphens. A step
// has one type, so ArgoCD commands drop their argocd- prefix. Names are never shortened, since
// running the wrong job must not be possible by mistake; validateJobNames rejects long ones.
func jobName(projectName string, command model.ActionCommand) string {
	name := projectName
	if command != "" {
		name += "-" + strings.TrimPrefix(string(command), "argocd-")
	}
	return slug(name)
}

func checkJobName(name string) error {
	if len(name) > maxJobNameLen || !jobNamePattern.MatchString(name) {
		return fmt.Errorf("container apps job name %s must be at most %d lowercase letters, digits and hyphens, starting with a letter",
			name, maxJobNameLen)
	}
	return nil
}

// validateJobNames fails before any resource is created when a step's job names don't fit.
func validateJobNames(prefix string, steps []model.Step) error {
	owners := map[string]string{}
	for _, step := range steps {
		for _, command := range model.GetStepCommands(step.Type) {
			name := jobName(fmt.Sprintf("%s-%s", prefix, step.Name), command)
			if len(name) > maxJobNameLen {
				return fmt.Errorf("step %s name is too long for azure, job name %s is longer than %d characters, use a step name of at most %d characters",
					step.Name, name, maxJobNameLen, maxJobNameLen-len(prefix)-len(longestJobCommand)-2)
			}
			if err := checkJobName(name); err != nil {
				return err
			}
			if owner, ok := owners[name]; ok && owner != step.Name {
				return fmt.Errorf("steps %s and %s have the same container apps job name %s", owner, step.Name, name)
			}
			owners[name] = step.Name
		}
	}
	return nil
}

// secretName maps an SSM key to a Key Vault secret name: letters, digits and hyphens.
func secretName(name string) string {
	name = strings.Map(func(r rune) rune {
		if isAlnum(r) || r == '-' {
			return r
		}
		return '-'
	}, strings.TrimLeft(name, "/"))
	return truncate(name, 127)
}

// appSecretName maps an env var name to a Container Apps secret name: lowercase letters, digits and hyphens.
func appSecretName(envName string) string {
	return slug(envName)
}

// slug joins the runs of letters and digits in value with single hyphens, lowercased.
func slug(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !isAlnum(r) }), "-")
}

func isAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func truncate(value string, length int) string {
	if len(value) > length {
		return value[:length]
	}
	return value
}

func resourceTags() map[string]*string {
	value := model.ResourceTagValue
	return map[string]*string{model.ResourceTagKey: &value}
}

// clientModuleSecretName hashes step and module apart, since both may contain hyphens.
func clientModuleSecretName(stepName, moduleName string) string {
	return fmt.Sprintf("git-%s-%s-%s-password", stepName, moduleName, nameHash(stepName, moduleName))
}
