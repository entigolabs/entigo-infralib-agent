package service

import (
	"dario.cat/mergo"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
	"os"
	"reflect"
)

const StableVersion = "stable"

func GetConfig(configFile string, codeCommit CodeCommit) model.Config {
	var config model.Config
	if configFile != "" {
		config = GetLocalConfig(configFile)
		bytes, err := yaml.Marshal(config)
		if err != nil {
			common.Logger.Fatalf("Failed to marshal config: %s", err)
		}
		codeCommit.PutFile("config.yaml", bytes)
	} else {
		bytes := codeCommit.GetFile("config.yaml")
		if bytes == nil {
			common.Logger.Fatalf("Config file not found")
		}
		err := yaml.Unmarshal(bytes, &config)
		if err != nil {
			common.Logger.Fatalf("Failed to unmarshal config: %s", err)
		}
	}
	if config.Source == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config source is not set")})
	}
	if config.Version == "" {
		config.Version = StableVersion
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

func ValidateConfig(config model.Config, state *model.State) {
	stepWorkspaces := model.NewSet[string]()
	if config.Prefix == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config prefix is not set")})
	}
	for _, step := range config.Steps {
		validateStep(step)
		stepWorkspace := fmt.Sprintf("%s-%s", step.Name, step.Workspace)
		if stepWorkspaces.Contains(stepWorkspace) {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step workspace combination %s is not unique",
				stepWorkspace)})
		}
		stepWorkspaces.Add(stepWorkspace)
		stepVersion := step.Version
		if stepVersion == "" {
			stepVersion = config.Version
		}
		validateConfigVersions(step, state, stepVersion)
	}
}

func validateStep(step model.Step) {
	if step.Name == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name is not set")})
	}
	if step.Workspace == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step workspace is not set")})
	}
	if step.Type == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step type is not set")})
	}
}

func validateConfigVersions(step model.Step, state *model.State, stepVersion string) {
	stepState := GetStepState(state, step)
	if stepState == nil {
		return
	}
	for _, module := range step.Modules {
		validateModule(module)
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

func validateModule(module model.Module) {
	if module.Name == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module name is not set")})
	}
	if module.Source == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module source is not set")})
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

func createState(config model.Config) *model.State {
	steps := make([]*model.StateStep, 0)
	for _, step := range config.Steps {
		modules := make([]*model.StateModule, 0)
		for _, module := range step.Modules {
			modules = append(modules, &model.StateModule{
				Name: module.Name,
			})
		}
		steps = append(steps, &model.StateStep{
			Name:      step.Name,
			Workspace: step.Workspace,
			Modules:   modules,
		})
	}
	return &model.State{
		Steps: steps,
	}
}

func updateSteps(config model.Config, state *model.State) {
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

func MergeConfig(baseConfig model.Config, patchConfig model.Config) model.Config {
	err := mergo.Merge(&patchConfig, baseConfig, mergo.WithOverride, mergo.WithTransformers(stepsTransformer{}))
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	return patchConfig
}

type stepsTransformer struct {
}

func (st stepsTransformer) Transformer(typ reflect.Type) func(dst, src reflect.Value) error {
	if typ != reflect.TypeOf([]model.Step{}) {
		return nil
	}
	return func(dst, src reflect.Value) error {
		target := src.Interface().([]model.Step)
		source := dst.Interface().([]model.Step)
		if len(target) == 0 {
			dst.Set(reflect.ValueOf(source))
			return nil
		}
		result := make([]model.Step, 0)
		for _, step := range source {
			patchStep := getPatchStep(step, target)
			if patchStep == nil {
				result = append(result, step)
				continue
			}
			if patchStep.Remove {
				continue
			}
			err := mergo.Merge(&step, *patchStep, mergo.WithOverride, mergo.WithTransformers(modulesTransformer{}))
			if err != nil {
				return err
			}
			result = append(result, step)
		}
		result = addNewPatchSteps(source, target, result)
		dst.Set(reflect.ValueOf(result))
		return nil
	}
}

func getPatchStep(dstStep model.Step, patchSteps []model.Step) *model.Step {
	for _, step := range patchSteps {
		if step.Name == dstStep.Name && step.Workspace == dstStep.Workspace {
			return &step
		}
	}
	return nil
}

func addNewPatchSteps(sourceSteps []model.Step, patchSteps []model.Step, result []model.Step) []model.Step {
	for _, patchStep := range patchSteps {
		found := false
		for _, sourceStep := range sourceSteps {
			if patchStep.Name == sourceStep.Name && patchStep.Workspace == sourceStep.Workspace {
				found = true
				break
			}
		}
		if !found {
			result = append(result, patchStep)
		}
	}
	return result
}

type modulesTransformer struct {
}

func (mt modulesTransformer) Transformer(typ reflect.Type) func(dst, src reflect.Value) error {
	if typ != reflect.TypeOf([]model.Module{}) {
		return nil
	}
	return func(dst, src reflect.Value) error {
		target := src.Interface().([]model.Module)
		source := dst.Interface().([]model.Module)
		if len(target) == 0 {
			dst.Set(reflect.ValueOf(source))
			return nil
		}
		result := make([]model.Module, 0)
		for _, module := range source {
			patchModule := getPatchModule(module, target)
			if patchModule == nil {
				result = append(result, module)
				continue
			}
			if patchModule.Remove {
				continue
			}
			err := mergo.Merge(&module, *patchModule, mergo.WithOverride, mergo.WithTransformers(inputsTransformer{}))
			if err != nil {
				return err
			}
			result = append(result, module)
		}
		result = addNewPatchModules(source, target, result)
		dst.Set(reflect.ValueOf(result))
		return nil
	}
}

func getPatchModule(dstModule model.Module, patchModules []model.Module) *model.Module {
	for _, module := range patchModules {
		if module.Name == dstModule.Name {
			return &module
		}
	}
	return nil
}

func addNewPatchModules(sourceModules []model.Module, patchModules []model.Module, result []model.Module) []model.Module {
	for _, patchModule := range patchModules {
		found := false
		for _, sourceModule := range sourceModules {
			if patchModule.Name == sourceModule.Name {
				found = true
				break
			}
		}
		if !found {
			result = append(result, patchModule)
		}
	}
	return result
}

type inputsTransformer struct {
}

func (it inputsTransformer) Transformer(typ reflect.Type) func(dst, src reflect.Value) error {
	if typ != reflect.TypeOf(map[string]interface{}{}) {
		return nil
	}
	return func(dst, src reflect.Value) error {
		target := src.Interface().(map[string]interface{})
		source := dst.Interface().(map[string]interface{})
		if len(target) == 0 {
			dst.Set(reflect.ValueOf(source))
			return nil
		}
		if len(source) == 0 {
			dst.Set(reflect.ValueOf(target))
			return nil
		}
		for key, value := range target {
			source[key] = value
		}
		dst.Set(reflect.ValueOf(source))
		return nil
	}
}
