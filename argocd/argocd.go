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

var planRegex = regexp.MustCompile(`ArgoCD Applications: (\d+) has changed objects, (\d+) has RequiredPruning objects`)
var newPlanRegex = regexp.MustCompile(`ArgoCD Applications: (?P<add>\d+) to add, (?P<change>\d+) to change, (?P<destroy>\d+) to destroy`)

func GetApplicationFile(storage model.Storage, module model.Module, source, version string, values []byte, provider model.ProviderType) ([]byte, error) {
	baseBytes := getBaseApplicationFile()
	moduleFile, err := getModuleApplicationFile(storage, version, module.Source)
	if err != nil {
		return nil, err
	}
	bytes, err := mergeAppFiles(baseBytes, moduleFile)
	if err != nil {
		return nil, err
	}
	return replacePlaceholders(bytes, module, source, version, values, provider), nil
}

func getBaseApplicationFile() []byte {
	contentCopy := make([]byte, len(appYaml))
	copy(contentCopy, appYaml)
	return contentCopy
}

func replacePlaceholders(bytes []byte, module model.Module, source, version string, values []byte, provider model.ProviderType) []byte {
	file := string(bytes)
	var cloudProvider string
	if provider == model.GCLOUD {
		cloudProvider = "google"
	} else {
		cloudProvider = "aws"
	}
	url := source
	if util.IsLocalSource(source) {
		url = "file:///tmp" + source
	} else if !strings.HasSuffix(url, ".git") {
		url += ".git"
	}
	replacer := strings.NewReplacer("{{moduleName}}", module.Name, "{{moduleVersion}}", version,
		"{{moduleSource}}", module.Source, "{{moduleValues}}", getValuesString(file, bytes, values),
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
		var fileError model.NotFoundError
		if errors.As(err, &fileError) {
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
	err = mergo.Merge(&baseFile, moduleFile, mergo.WithAppendSlice)
	if err != nil {
		return nil, err
	}
	return util.MapToYamlBytes(baseFile)
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
