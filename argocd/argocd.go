package argocd

import (
	_ "embed"
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

// ociChartLineRegex matches the git-style source locator line
// (path: "modules/k8s/{{moduleSource}}") so it can be swapped for an OCI Helm
// chart reference. It keys on the yet-unreplaced {{moduleSource}} token, which
// survives the YAML round-trip in mergeAppFiles regardless of quote style.
var ociChartLineRegex = regexp.MustCompile(`(?m)^(\s*)path:.*\{\{moduleSource\}\}.*$`)

var planRegex = regexp.MustCompile(`ArgoCD Applications: (\d+) has changed objects, (\d+) has RequiredPruning objects`)
var newPlanRegex = regexp.MustCompile(`ArgoCD Applications: (?P<add>\d+) to add, (?P<change>\d+) to change, (?P<destroy>\d+) to destroy`)

type ArgoCD struct {
	provider model.ProviderType
}

func NewArgoCD(providerType model.ProviderType) ArgoCD {
	return ArgoCD{
		provider: providerType,
	}
}

// release is the version used to fetch the module's argo-apps.yaml from storage;
// ociVersion is the reference written as targetRevision (a digest in digest mode,
// else the tag). They differ only for OCI sources in digest mode.
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
	return a.replacePlaceholders(bytes, module, source, ociVersion, values), nil
}

func getBaseApplicationFile() []byte {
	contentCopy := make([]byte, len(appYaml))
	copy(contentCopy, appYaml)
	return contentCopy
}

func (a *ArgoCD) replacePlaceholders(bytes []byte, module model.Module, source, version string, values []byte) []byte {
	file := string(bytes)
	var cloudProvider string
	if a.provider == model.GCLOUD {
		cloudProvider = "google"
	} else {
		cloudProvider = "aws"
	}
	url := source
	if util.IsLocalSource(source) {
		url = "file:///tmp" + source
	} else if util.IsOCISource(source) {
		url = util.TrimOCIScheme(source) + "/k8s"
		version = util.NormalizeOCIVersion(version)
		file = ociChartLineRegex.ReplaceAllString(file, "${1}chart: '{{moduleSource}}'")
	} else if !strings.HasSuffix(url, ".git") && !util.IsAzureDevOps(source) {
		url += ".git"
	}
	replacer := strings.NewReplacer("{{moduleName}}", module.Name, "{{moduleVersion}}", version,
		"{{moduleSource}}", module.Source, "{{moduleValues}}", getValuesString(file, []byte(file), values),
		"{{cloudProvider}}", cloudProvider, "{{moduleSourceURL}}", url)
	return []byte(replacer.Replace(file))
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

func getModuleApplicationFile(storage model.Storage, release, moduleSource string) (map[string]interface{}, error) {
	bytes, err := storage.GetFile(fmt.Sprintf("modules/k8s/%s/argo-apps.yaml", moduleSource), release)
	if err != nil {
		if _, ok := errors.AsType[model.NotFoundError](err); ok {
			return nil, nil
		}
		return nil, err
	}
	return util.YamlBytesToMap(bytes)
}

func mergeAppFiles(baseBytes []byte, moduleFile map[string]interface{}) ([]byte, error) {
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

func mergeFirstSource(base, module map[string]interface{}) error {
	baseSpec, _ := base["spec"].(map[string]interface{})
	modSpec, _ := module["spec"].(map[string]interface{})
	if baseSpec == nil || modSpec == nil {
		return nil
	}
	baseSources, _ := baseSpec["sources"].([]interface{})
	modSources, _ := modSpec["sources"].([]interface{})
	if len(baseSources) == 0 || len(modSources) == 0 {
		return nil
	}
	baseSrc, ok1 := baseSources[0].(map[string]interface{})
	modSrc, ok2 := modSources[0].(map[string]interface{})
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

func deduplicateSyncOptions(app map[string]interface{}) {
	spec, ok := app["spec"].(map[string]interface{})
	if !ok {
		return
	}
	policy, ok := spec["syncPolicy"].(map[string]interface{})
	if !ok {
		return
	}
	optionsRaw, ok := policy["syncOptions"]
	if !ok {
		return
	}
	optionsSlice, ok := optionsRaw.([]interface{})
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

func ParseLogChanges(pipelineName, message string) (*model.PipelineChanges, error) {
	matches := newPlanRegex.FindStringSubmatch(message)
	if matches != nil {
		return util.GetChangesFromMatches(pipelineName, message, matches, newPlanRegex.SubexpNames())
	}
	matches = planRegex.FindStringSubmatch(message)
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
