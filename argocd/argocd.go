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

func GetApplicationFile(github github.Github, module model.Module, repoSSHUrl string, version string, valuesFilePath string) ([]byte, error) {
	baseBytes, err := getBaseApplicationFile(module, repoSSHUrl, version, valuesFilePath)
	if err != nil {
		return nil, err
	}
	moduleFile, err := getModuleApplicationFile(github, version, module.Source)
	if err != nil {
		return nil, err
	}
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

func getBaseApplicationFile(module model.Module, repoSSHUrl string, version string, valuesFilePath string) ([]byte, error) {
	file, err := os.ReadFile("app.yaml")
	if err != nil {
		return nil, err
	}
	replacer := strings.NewReplacer("{{moduleName}}", module.Name, "{{codeRepoSSHUrl}}", repoSSHUrl,
		"{{moduleVersion}}", version, "{{moduleSource}}", module.Source, "{{valuesFilePath}}", valuesFilePath)
	return []byte(replacer.Replace(string(file))), nil
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
