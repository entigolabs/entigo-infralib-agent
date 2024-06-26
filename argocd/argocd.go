package argocd

import (
	"dario.cat/mergo"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"os"
	"strings"
)

func GetApplicationFile(github github.Github, module model.Module, repoSSHUrl string, version string, values []byte) ([]byte, error) {
	baseBytes, err := getBaseApplicationFile()
	if err != nil {
		return nil, err
	}
	moduleFile, err := getModuleApplicationFile(github, version, module.Source)
	if err != nil {
		return nil, err
	}
	bytes, err := mergeAppFiles(baseBytes, moduleFile)
	if err != nil {
		return nil, err
	}
	return replacePlaceholders(bytes, module, repoSSHUrl, version, values), nil
}

func getBaseApplicationFile() ([]byte, error) {
	return os.ReadFile("app.yaml")
}

func replacePlaceholders(bytes []byte, module model.Module, repoSSHUrl string, version string, values []byte) []byte {
	file := string(bytes)
	replacer := strings.NewReplacer("{{moduleName}}", module.Name, "{{codeRepoSSHUrl}}", repoSSHUrl,
		"{{moduleVersion}}", version, "{{moduleSource}}", module.Source, "{{moduleValues}}",
		getValuesString(file, bytes, values))
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

func getModuleApplicationFile(git github.Github, release string, moduleSource string) (map[string]interface{}, error) {
	bytes, err := git.GetRawFileContent(fmt.Sprintf("modules/k8s/%s/argo-apps.yaml", moduleSource), release)
	if err != nil {
		var fileError github.FileNotFoundError
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
