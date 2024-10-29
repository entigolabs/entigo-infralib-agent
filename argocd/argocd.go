package argocd

import (
	"dario.cat/mergo"
	_ "embed"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log"
	"regexp"
	"strconv"
	"strings"
)

//go:embed app.yaml
var appYaml []byte

var planRegex = regexp.MustCompile(`ArgoCD Applications: (\d+) has changed objects, (\d+) has RequiredPruning objects`)

func GetApplicationFile(github github.Github, module model.Module, sourceURL, repoSSHUrl, version string, values []byte, provider model.ProviderType) ([]byte, error) {
	baseBytes := getBaseApplicationFile()
	moduleFile, err := getModuleApplicationFile(github, version, module.Source, sourceURL)
	if err != nil {
		return nil, err
	}
	bytes, err := mergeAppFiles(baseBytes, moduleFile)
	if err != nil {
		return nil, err
	}
	return replacePlaceholders(bytes, module, sourceURL, repoSSHUrl, version, values, provider), nil
}

func getBaseApplicationFile() []byte {
	contentCopy := make([]byte, len(appYaml))
	copy(contentCopy, appYaml)
	return contentCopy
}

func replacePlaceholders(bytes []byte, module model.Module, sourceURL string, repoSSHUrl string, version string, values []byte, provider model.ProviderType) []byte {
	file := string(bytes)
	var cloudProvider string
	if provider == model.GCLOUD {
		cloudProvider = "google"
	} else {
		cloudProvider = "aws"
	}
	url := sourceURL
	if !strings.HasSuffix(url, ".git") {
		url += ".git"
	}
	replacer := strings.NewReplacer("{{moduleName}}", module.Name, "{{codeRepoSSHUrl}}", repoSSHUrl,
		"{{moduleVersion}}", version, "{{moduleSource}}", module.Source, "{{moduleValues}}",
		getValuesString(file, bytes, values), "{{cloudProvider}}", cloudProvider, "{{moduleSourceURL}}", url)
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

func getModuleApplicationFile(git github.Github, release, moduleSource, sourceURL string) (map[string]interface{}, error) {
	bytes, err := git.GetRawFileContent(sourceURL, fmt.Sprintf("modules/k8s/%s/argo-apps.yaml", moduleSource), release)
	if err != nil {
		var fileError model.FileNotFoundError
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
	err = mergo.Merge(&baseFile, moduleFile)
	if err != nil {
		return nil, err
	}
	return util.MapToYamlBytes(baseFile)
}

func ParseLogChanges(pipelineName, message string) (*model.PipelineChanges, error) {
	matches := planRegex.FindStringSubmatch(message)
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
