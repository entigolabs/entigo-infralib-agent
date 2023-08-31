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
	return config
}

func validateConfig(config model.Config, state *model.State) {
	stepWorkspaces := model.NewSet[string]()
	for _, step := range config.Steps {
		stepWorkspace := fmt.Sprintf("%s-%s", step.Name, step.Workspace)
		if stepWorkspaces.Contains(stepWorkspace) {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step workspace combination %s is not unique",
				stepWorkspace)})
		}
		stepWorkspaces.Add(stepWorkspace)
		validateConfigVersions(step, state)
	}
}

func validateConfigVersions(step model.Step, state *model.State) {
	stepState := GetStepState(state, step)
	if stepState == nil {
		return
	}
	for _, module := range step.Modules {
		stateModule := GetModuleState(stepState, module.Name)
		if stateModule == nil || stateModule.Version == nil || module.Version == "" || module.Version == StableVersion {
			continue
		}
		moduleVersion, err := version.NewVersion(module.Version)
		if err != nil {
			common.Logger.Fatalf("failed to parse module version %s: %s\n", module.Version, err)
		}
		if moduleVersion.LessThan(stateModule.Version) {
			common.Logger.Fatalf("config module %s version %s is less than state version %s\n", module.Name,
				module.Version, stateModule.Version)
		}
	}
}

func GetStepState(state *model.State, step model.Step) *model.StateStep {
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

func AddNewSteps(config model.Config, state *model.State) {
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
