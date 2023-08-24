package service

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"gopkg.in/yaml.v3"
	"os"
)

func GetConfig(configFile string) model.Config {
	fileBytes, err := os.ReadFile(configFile)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	var config model.Config
	err = yaml.Unmarshal(fileBytes, &config)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	validateConfig(config)
	return config
}

func validateConfig(config model.Config) {
	stepWorkspaces := model.NewSet[string]()
	for _, step := range config.Steps {
		stepWorkspace := fmt.Sprintf("%s-%s", step.Name, step.Workspace)
		if stepWorkspaces.Contains(stepWorkspace) {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step workspace combination %s is not unique",
				stepWorkspace)})
		}
		stepWorkspaces.Add(stepWorkspace)
	}
}
