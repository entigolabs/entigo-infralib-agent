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
	moduleNames := model.NewSet[string]()
	for _, step := range config.Steps {
		for _, module := range step.Modules {
			if moduleNames.Contains(module.Name) {
				common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module name %s is not unique",
					module.Name)})
			}
			moduleNames.Add(module.Name)
		}
	}
}
