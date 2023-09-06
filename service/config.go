package service

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
	"os"
)

const StableVersion = "stable"

func GetConfig(configFile string, codeCommit CodeCommit) model.Config {
	if configFile != "" {
		config := GetLocalConfig(configFile)
		bytes, err := yaml.Marshal(config)
		if err != nil {
			common.Logger.Fatalf("Failed to marshal config: %s", err)
		}
		codeCommit.PutFile("config.yaml", bytes)
		return config
	}
	bytes := codeCommit.GetFile("config.yaml")
	if bytes == nil {
		common.Logger.Fatalf("Config file not found")
	}
	var config model.Config
	err := yaml.Unmarshal(bytes, &config)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal config: %s", err)
	}
	return config
}

func GetLocalConfig(configFile string) model.Config {
	fileBytes, err := os.ReadFile(configFile)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	var config model.Config
	err = yaml.Unmarshal(fileBytes, &config)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	return config
}

func validateConfig(config model.Config, state *model.State) {
	stepWorkspaces := model.NewSet[string]()
	configVersion := config.Version
	if configVersion == "" {
		configVersion = StableVersion
	}
	for _, step := range config.Steps {
		stepWorkspace := fmt.Sprintf("%s-%s", step.Name, step.Workspace)
		if stepWorkspaces.Contains(stepWorkspace) {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step workspace combination %s is not unique",
				stepWorkspace)})
		}
		stepWorkspaces.Add(stepWorkspace)
		stepVersion := step.Version
		if stepVersion == "" {
			stepVersion = configVersion
		}
		validateConfigVersions(step, state, stepVersion)
	}
}

func validateConfigVersions(step model.Step, state *model.State, stepVersion string) {
	stepState := GetStepState(state, step)
	if stepState == nil {
		return
	}
	for _, module := range step.Modules {
		stateModule := GetModuleState(stepState, module.Name)
		moduleVersionString := module.Version
		if moduleVersionString == "" {
			moduleVersionString = stepVersion
		}
		if stateModule == nil || stateModule.Version == nil || moduleVersionString == StableVersion {
			continue
		}
		moduleVersion, err := version.NewVersion(moduleVersionString)
		if err != nil {
			common.Logger.Fatalf("failed to parse module version %s: %s\n", module.Version, err)
		}
		if moduleVersion.LessThan(stateModule.Version) {
			common.Logger.Fatalf("config module %s version %s is less than state version %s\n", module.Name,
				moduleVersionString, stateModule.Version)
		}
	}
}

func GetStepState(state *model.State, step model.Step) *model.StateStep {
	if state == nil {
		return nil
	}
	for _, stateStep := range state.Steps {
		if stateStep.Name == step.Name && stateStep.Workspace == step.Workspace {
			return stateStep
		}
	}
	return nil
}

func GetModuleState(stepState *model.StateStep, moduleName string) *model.StateModule {
	for _, module := range stepState.Modules {
		if module.Name == moduleName {
			return module
		}
	}
	return nil
}

func UpdateSteps(config model.Config, state *model.State) {
	removeUnusedSteps(config, state)
	addNewSteps(config, state)
}

func addNewSteps(config model.Config, state *model.State) {
	for _, step := range config.Steps {
		stepState := GetStepState(state, step)
		if stepState == nil {
			stepState = &model.StateStep{
				Name:      step.Name,
				Workspace: step.Workspace,
				Modules:   make([]*model.StateModule, 0),
			}
			state.Steps = append(state.Steps, stepState)
		}
		for _, module := range step.Modules {
			stateModule := GetModuleState(stepState, module.Name)
			if stateModule == nil {
				stateModule = &model.StateModule{
					Name: module.Name,
				}
				stepState.Modules = append(stepState.Modules, stateModule)
			}
		}
	}
}

func removeUnusedSteps(config model.Config, state *model.State) {
	for i := len(state.Steps) - 1; i >= 0; i-- {
		stepState := state.Steps[i]
		stepFound := false
		for _, step := range config.Steps {
			if stepState.Name == step.Name && stepState.Workspace == step.Workspace {
				stepFound = true
				break
			}
		}
		if !stepFound {
			state.Steps = append(state.Steps[:i], state.Steps[i+1:]...)
		}
	}
}
