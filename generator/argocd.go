package generator

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"dario.cat/mergo"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

//go:embed app.yaml
var appYaml []byte

// moduleInfoLineRegex matches the spec.info placeholder line regardless of the
// quote style the YAML round-trip in mergeAppFiles produces.
var moduleInfoLineRegex = regexp.MustCompile(`(?m)^([ \t]*)info:.*\{\{moduleInfo\}\}.*\n?`)

var argoCDPlanRegex = regexp.MustCompile(`ArgoCD Applications: (\d+) has changed objects, (\d+) has RequiredPruning objects`)
var argoCDNewPlanRegex = regexp.MustCompile(`ArgoCD Applications: (?P<add>\d+) to add, (?P<change>\d+) to change, (?P<destroy>\d+) to destroy`)

type planFile struct {
	Type    string `json:"type"`
	Add     int    `json:"add"`
	Change  int    `json:"change"`
	Destroy int    `json:"destroy"`
}

type ArgoCD struct {
	provider model.ProviderType
}

func NewArgoCD(providerType model.ProviderType) ArgoCD {
	return ArgoCD{
		provider: providerType,
	}
}

// release is the version used to fetch the module's argo-apps.yaml from storage;
// ociVersion is the module's OCI reference (a digest in digest mode, else the
// release). They differ only for OCI sources in digest mode.
func (a *ArgoCD) GetApplicationFile(storage model.Storage, module model.Module, source, release, ociVersion string, values []byte) ([]byte, error) {
	baseBytes := getBaseApplicationFile()
	moduleFile, err := getModuleApplicationFile(storage, release, module.Source)
	if err != nil {
		return nil, err
	}
	bytes, err := mergeAppFiles(baseBytes, moduleFile)
	if err != nil {
		return nil, err
	}
	return a.replacePlaceholders(bytes, module, source, release, ociVersion, values), nil
}

func getBaseApplicationFile() []byte {
	contentCopy := make([]byte, len(appYaml))
	copy(contentCopy, appYaml)
	return contentCopy
}

func (a *ArgoCD) replacePlaceholders(bytes []byte, module model.Module, source, release, ociVersion string, values []byte) []byte {
	file := string(bytes)
	var cloudProvider string
	switch a.provider {
	case model.GCLOUD:
		cloudProvider = "google"
	case model.ORACLE:
		cloudProvider = "oracle"
	case model.AZURE:
		cloudProvider = "azure"
	default:
		cloudProvider = "aws"
	}
	url, path, version := source, "modules/k8s/"+module.Source, release
	if util.IsLocalSource(source) {
		url = "file:///tmp" + source
	} else if util.IsOCISource(source) {
		url = fmt.Sprintf("oci://%s/k8s/%s", util.TrimOCIScheme(source), module.Source)
		path = "."
		version = ociTargetRevision(release, ociVersion)
	} else if !strings.HasSuffix(url, ".git") && !util.IsAzureDevOps(source) {
		url += ".git"
	}
	file = replaceModuleInfo(file, release, ociVersion)
	replacer := strings.NewReplacer("{{moduleName}}", module.Name, "{{moduleVersion}}", version,
		"{{modulePath}}", path, "{{moduleValues}}", getValuesString(file, []byte(file), values),
		"{{cloudProvider}}", cloudProvider, "{{moduleSourceURL}}", url)
	return []byte(replacer.Replace(file))
}

// ociTargetRevision pins the tag to the module digest (tag@sha256:...) in digest
// mode, keeping the tag readable in the ArgoCD UI.
func ociTargetRevision(release, ociVersion string) string {
	if !util.IsOCIDigest(ociVersion) || util.IsOCIDigest(release) {
		return util.NormalizeOCIVersion(ociVersion)
	}
	return util.NormalizeOCIVersion(release) + "@" + ociVersion
}

// replaceModuleInfo shows the Infralib version in the ArgoCD UI when the source
// is pinned to a digest, otherwise drops the info placeholder.
func replaceModuleInfo(file, release, ociVersion string) string {
	if !util.IsOCIDigest(ociVersion) {
		return moduleInfoLineRegex.ReplaceAllString(file, "")
	}
	info := fmt.Sprintf("${1}info:\n${1}  - name: 'Infralib:'\n${1}    value: '%s'\n", util.NormalizeOCIVersion(release))
	return moduleInfoLineRegex.ReplaceAllString(file, info)
}

func getValuesString(file string, bytes []byte, values []byte) string {
	index := strings.Index(file, "{{moduleValues}}")
	if index == -1 {
		return string(values)
	}
	spaceCount := 0
	for i := index - 1; i >= 0; i-- {
		if bytes[i] == '\n' {
			break
		}
		spaceCount++
	}
	replaceLines := strings.Split(string(values), "\n")
	for i := 1; i < len(replaceLines); i++ {
		replaceLines[i] = strings.Repeat(" ", spaceCount) + replaceLines[i]
	}
	return strings.Join(replaceLines, "\n")
}

func getModuleApplicationFile(storage model.Storage, release, moduleSource string) (map[string]any, error) {
	bytes, err := storage.GetFile(fmt.Sprintf("modules/k8s/%s/argo-apps.yaml", moduleSource), release)
	if err != nil {
		if _, ok := errors.AsType[model.NotFoundError](err); ok {
			return nil, nil
		}
		return nil, err
	}
	return util.YamlBytesToMap(bytes)
}

func mergeAppFiles(baseBytes []byte, moduleFile map[string]any) ([]byte, error) {
	if moduleFile == nil {
		return baseBytes, nil
	}
	baseFile, err := util.YamlBytesToMap(baseBytes)
	if err != nil {
		return nil, err
	}
	if err := mergeFirstSource(baseFile, moduleFile); err != nil {
		return nil, err
	}
	if err := mergo.Merge(&baseFile, moduleFile, mergo.WithAppendSlice); err != nil {
		return nil, err
	}
	deduplicateSyncOptions(baseFile)
	return util.MapToYamlBytes(baseFile)
}

func mergeFirstSource(base, module map[string]any) error {
	baseSpec, _ := base["spec"].(map[string]any)
	modSpec, _ := module["spec"].(map[string]any)
	if baseSpec == nil || modSpec == nil {
		return nil
	}
	baseSources, _ := baseSpec["sources"].([]any)
	modSources, _ := modSpec["sources"].([]any)
	if len(baseSources) == 0 || len(modSources) == 0 {
		return nil
	}
	baseSrc, ok1 := baseSources[0].(map[string]any)
	modSrc, ok2 := modSources[0].(map[string]any)
	if !ok1 || !ok2 {
		return nil
	}
	if err := mergo.Merge(&baseSrc, modSrc, mergo.WithAppendSlice, mergo.WithOverride); err != nil {
		return err
	}
	baseSources[0] = baseSrc
	modSpec["sources"] = modSources[1:]
	return nil
}

func deduplicateSyncOptions(app map[string]any) {
	spec, ok := app["spec"].(map[string]any)
	if !ok {
		return
	}
	policy, ok := spec["syncPolicy"].(map[string]any)
	if !ok {
		return
	}
	optionsRaw, ok := policy["syncOptions"]
	if !ok {
		return
	}
	optionsSlice, ok := optionsRaw.([]any)
	if !ok {
		return
	}
	dedupedMap := make(map[string]string)
	var orderedKeys []string
	for _, item := range optionsSlice {
		str, ok := item.(string)
		if !ok {
			continue
		}
		parts := strings.SplitN(str, "=", 2)
		key := parts[0]
		if _, exists := dedupedMap[key]; !exists {
			orderedKeys = append(orderedKeys, key)
		}
		dedupedMap[key] = str
	}
	newOptions := make([]string, 0, len(orderedKeys))
	for _, key := range orderedKeys {
		newOptions = append(newOptions, dedupedMap[key])
	}
	policy["syncOptions"] = newOptions
}

func ParseArgoCDPlan(pipelineName string, data []byte) (*model.PipelineChanges, error) {
	var plan planFile
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("failed to parse argocd plan json for %s: %w", pipelineName, err)
	}
	log.Printf("Pipeline %s: %d to add, %d to change, %d to destroy", pipelineName, plan.Add, plan.Change, plan.Destroy)
	changes := model.PipelineChanges{Added: plan.Add, Changed: plan.Change, Destroyed: plan.Destroy}
	if plan.Add == 0 && plan.Change == 0 && plan.Destroy == 0 {
		changes.NoChanges = true
	}
	return &changes, nil
}

func ParseArgoCDLog(pipelineName, message string) (*model.PipelineChanges, error) {
	matches := argoCDNewPlanRegex.FindStringSubmatch(message)
	if matches != nil {
		return util.GetChangesFromMatches(pipelineName, message, matches, argoCDNewPlanRegex.SubexpNames())
	}
	matches = argoCDPlanRegex.FindStringSubmatch(message)
	if matches == nil {
		return nil, nil
	}
	log.Printf("Pipeline %s: %s", pipelineName, message)
	changed := matches[1]
	destroyed := matches[2]
	argoChanges := model.PipelineChanges{}
	if changed == "0" && destroyed == "0" {
		argoChanges.NoChanges = true
		return &argoChanges, nil
	}
	var err error
	argoChanges.Changed, err = strconv.Atoi(changed)
	if err != nil {
		return nil, err
	}
	argoChanges.Destroyed, err = strconv.Atoi(destroyed)
	if err != nil {
		return nil, err
	}
	return &argoChanges, nil
}
