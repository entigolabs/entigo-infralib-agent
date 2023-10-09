package argocd

import (
	"github.com/entigolabs/entigo-infralib-agent/model"
	"os"
	"strings"
)

func GetApplicationFile(module model.Module, repoSSHUrl string, version string, valuesFilePath string) ([]byte, error) {
	file, err := os.ReadFile("app.yaml")
	if err != nil {
		return nil, err
	}
	replacer := strings.NewReplacer("{{moduleName}}", module.Name, "{{codeRepoSSHUrl}}", repoSSHUrl,
		"{{moduleVersion}}", version, "{{moduleSource}}", module.Source, "{{valuesFilePath}}", valuesFilePath)
	return []byte(replacer.Replace(string(file))), nil
}
