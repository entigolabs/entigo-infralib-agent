package service

import (
	"dario.cat/mergo"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
	"os"
	"reflect"
	"strings"
)

const StableVersion = "stable"

var ReservedFiles = model.Set[string]{"main.tf": true, "provider.tf": true, "backend.conf": true}

func GetAwsPrefix(flags *common.Flags) string {
	if flags.AWSPrefix != "" {
		return flags.AWSPrefix
	}
	if flags.Config == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("aws prefix or config must be provided")})
	}
	prefix := GetLocalConfig(flags.Config).Prefix
	if prefix == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config prefix is not set")})
	}
	return prefix
}

func GetConfig(configFile string, bucket model.Bucket) model.Config {
	var config model.Config
	if configFile != "" {
		config = GetLocalConfig(configFile)
		PutConfig(bucket, config)
		AddModuleInputFiles(&config, os.ReadFile)
		PutAdditionalFiles(bucket, config.Steps)
	} else {
		config = GetRemoteConfig(bucket)
	}
	if config.Source == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config source is not set")})
	}
	if config.Version == "" {
		config.Version = StableVersion
	}
	return config
}

func MergeBaseConfig(github github.Github, release *version.Version, patchConfig model.Config) model.Config {
	rawBaseConfig, err := github.GetRawFileContent(fmt.Sprintf("profiles/%s.yaml", patchConfig.BaseConfig.Profile),
		release.Original())
	if err != nil {
		common.Logger.Fatalf("Failed to get base config: %s", err)
	}
	var baseConfig model.Config
	err = yaml.Unmarshal(rawBaseConfig, &baseConfig)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal base config: %s", err)
	}
	return MergeConfig(patchConfig, baseConfig)
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
	AddStepsFilesFromFolder(&config)
	return config
}

func AddStepsFilesFromFolder(config *model.Config) {
	if config.Steps == nil {
		return
	}
	for i := range config.Steps {
		step := &config.Steps[i]
		if step.Type != model.StepTypeTerraform {
			continue
		}
		addStepFilesFromFolder(step, fmt.Sprintf("config/%s/include", step.Name))
	}
}

func addStepFilesFromFolder(step *model.Step, folder string) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			addStepFilesFromFolder(step, fmt.Sprintf("%s/%s", folder, entry.Name()))
			continue
		}
		if ReservedFiles.Contains(entry.Name()) {
			common.Logger.Fatalf("Can't include files %s in step %s", ReservedFiles, step.Name)
		}
		filePath := fmt.Sprintf("%s/%s", folder, entry.Name())
		fileBytes, err := os.ReadFile(filePath)
		if err != nil {
			common.Logger.Fatalf("failed to read file %s: %s", filePath, err)
		}
		step.Files = append(step.Files, model.File{
			Name:    filePath,
			Content: fileBytes,
		})
	}
}

func PutConfig(bucket model.Bucket, config model.Config) {
	bytes, err := yaml.Marshal(config)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal config: %s", err)
	}
	err = bucket.PutFile("config.yaml", bytes)
	if err != nil {
		common.Logger.Fatalf("Failed to put config: %s", err)
	}
}

func PutAdditionalFiles(bucket model.Bucket, steps []model.Step) {
	for _, step := range steps {
		if step.Files != nil {
			for _, file := range step.Files {
				err := bucket.PutFile(file.Name, file.Content)
				if err != nil {
					common.Logger.Fatalf("Failed to put step file %s: %s", file.Name, err)
				}
			}
		}
		if step.Modules == nil {
			continue
		}
		for _, module := range step.Modules {
			if module.InputsFile == "" {
				continue
			}
			err := bucket.PutFile(module.InputsFile, module.FileContent)
			if err != nil {
				common.Logger.Fatalf("Failed to put module %s inputs file: %s", module.Name, err)
			}
			module.InputsFile = ""
			module.FileContent = nil
		}
	}
}

func GetRemoteConfig(bucket model.Bucket) model.Config {
	bytes, err := bucket.GetFile("config.yaml")
	if err != nil {
		common.Logger.Fatalf("Failed to get config: %s", err)
	}
	if bytes == nil {
		common.Logger.Fatalf("Config file not found")
	}
	var config model.Config
	err = yaml.Unmarshal(bytes, &config)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal config: %s", err)
	}
	AddStepsFilesFromBucket(&config, bucket)
	AddModuleInputFiles(&config, bucket.GetFile)
	return config
}

func AddStepsFilesFromBucket(config *model.Config, bucket model.Bucket) {
	if config.Steps == nil {
		return
	}
	for i := range config.Steps {
		step := &config.Steps[i]
		if step.Type != model.StepTypeTerraform {
			continue
		}
		addStepFilesFromBucket(step, bucket)
	}
}

func addStepFilesFromBucket(step *model.Step, bucket model.Bucket) {
	folder := fmt.Sprintf("config/%s/include", step.Name)
	files, err := bucket.ListFolderFiles(folder)
	if err != nil {
		common.Logger.Fatalf("Failed to list folder files: %s", err)
	}
	for _, file := range files {
		if ReservedFiles.Contains(strings.TrimPrefix(file, folder+"/")) {
			common.Logger.Fatalf("Can't include files %s in step %s", ReservedFiles, step.Name)
		}
		fileBytes, err := bucket.GetFile(file)
		if err != nil {
			common.Logger.Fatalf("Failed to get file %s: %s", file, err)
		}
		if fileBytes == nil {
			continue
		}
		step.Files = append(step.Files, model.File{
			Name:    file,
			Content: fileBytes,
		})
	}
}

func AddModuleInputFiles(config *model.Config, readFile func(string) ([]byte, error)) {
	if config.Steps == nil {
		return
	}
	for _, step := range config.Steps {
		if step.Modules == nil {
			continue
		}
		if step.Name == "" {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name is not set")})
		}
		for i := range step.Modules {
			module := &step.Modules[i]
			if module.Name == "" {
				common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module name is not set in step %s", step.Name)})
			}
			processModuleInputs(step.Name, module, readFile)
		}
	}
}

func processModuleInputs(stepName string, module *model.Module, readFile func(string) ([]byte, error)) {
	yamlFile := fmt.Sprintf("config/%s/%s.yaml", stepName, module.Name)
	bytes, err := readFile(yamlFile)
	if module.Inputs != nil {
		if err == nil && bytes != nil {
			common.PrintWarning(fmt.Sprintf("module %s/%s has inputs, ignoring file %s", stepName, module.Name, yamlFile))
		}
		return
	}
	if bytes == nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
		return
	}
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("failed to read input file %s: %v", yamlFile, err)})
	}
	module.InputsFile = yamlFile
	module.FileContent = bytes
	err = yaml.Unmarshal(bytes, &module.Inputs)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("failed to unmarshal input file %s: %v",
			yamlFile, err)})
	}
}

func ValidateConfig(config model.Config, state *model.State) {
	stepNames := model.NewSet[string]()
	if config.Prefix == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config prefix is not set")})
	}
	for _, step := range config.Steps {
		validateStep(step, config.Steps)
		if stepNames.Contains(step.Name) {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name %s is not unique", step.Name)})
		}
		stepNames.Add(step.Name)
		stepVersion := step.Version
		if stepVersion == "" {
			stepVersion = config.Version
		}
		validateConfigModules(step, state, stepVersion)
	}
}

func validateStep(step model.Step, steps []model.Step) {
	if step.Name == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name is not set")})
	}
	if step.Type == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step type is not set for step %s", step.Name)})
	}
	if step.VpcId != "" && step.VpcSubnetIds == "" {
		common.Logger.Fatalf("VPC ID is set for step %s but subnet IDs are not", step.Name)
	}
	if (step.VpcSubnetIds != "" || step.VpcSecurityGroupIds != "") && step.VpcId == "" {
		common.Logger.Fatalf("VPC ID is not set for step %s", step.Name)
	}
	if step.Type == model.StepTypeTerraformCustom && step.Approve != "" && step.Approve != model.ApproveAlways && step.Approve != model.ApproveNever {
		common.Logger.Fatalf("custom terraform step %s must have approve 'always' or 'never'", step.Name)
	}
	if step.Before != "" {
		_, referencedStep := findStep(step.Before, steps)
		if referencedStep == nil {
			common.Logger.Fatalf("before step %s does not exist for step %s", step.Before, step.Name)
		} else if referencedStep.Remove {
			common.Logger.Fatalf("before step %s is marked for removal for step %s", step.Before, step.Name)
		}
	}
}

func validateConfigModules(step model.Step, state *model.State, stepVersion string) {
	stepState := GetStepState(state, step.Name)
	for _, module := range step.Modules {
		validateModule(module, step.Name)
		if stepState == nil {
			continue
		}
		stateModule := GetModuleState(stepState, module.Name)
		moduleVersionString := module.Version
		if util.IsClientModule(module) {
			if moduleVersionString == "" {
				common.Logger.Fatalf("module version is not set for client module %s in step %s", module.Name, step.Name)
			}
			continue
		}
		if moduleVersionString == "" {
			moduleVersionString = stepVersion
		}
		if stateModule == nil || stateModule.Version == "" || moduleVersionString == StableVersion {
			continue
		}
		moduleVersion, err := version.NewVersion(moduleVersionString)
		if err != nil {
			common.Logger.Fatalf("failed to parse module version %s for module %s: %s", module.Version, module.Name, err)
		}
		stateModuleVersion, err := version.NewVersion(stateModule.Version)
		if err != nil {
			common.Logger.Fatalf("failed to parse state module version %s for module %s: %s", stateModule.Version, module.Name, err)
		}
		if moduleVersion.LessThan(stateModuleVersion) {
			common.Logger.Fatalf("config module %s version %s is less than state version %s", module.Name,
				moduleVersionString, stateModule.Version)
		}
	}
}

func validateModule(module model.Module, stepName string) {
	if module.Name == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module name is not set in step %s", stepName)})
	}
	if module.Source == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module source is not set for module %s in step %s", module.Name, stepName)})
	}
}

func GetStepState(state *model.State, stepName string) *model.StateStep {
	if state == nil {
		return nil
	}
	for _, stateStep := range state.Steps {
		if stateStep.Name == stepName {
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

func updateState(config model.Config, state *model.State) {
	if len(state.Steps) == 0 {
		createState(config, state)
		return
	}
	removeUnusedSteps(config, state)
	addNewSteps(config, state)
}

func createState(config model.Config, state *model.State) {
	steps := make([]*model.StateStep, 0)
	for _, step := range config.Steps {
		modules := make([]*model.StateModule, 0)
		for _, module := range step.Modules {
			modules = append(modules, createStateModule(module))
		}
		steps = append(steps, &model.StateStep{
			Name:    step.Name,
			Modules: modules,
		})
	}
	state.Steps = steps
}

func createStateModule(module model.Module) *model.StateModule {
	stateModule := model.StateModule{
		Name: module.Name,
	}
	if util.IsClientModule(module) {
		moduleType := model.ModuleTypeCustom
		stateModule.Type = &moduleType
	}
	return &stateModule
}

func addNewSteps(config model.Config, state *model.State) {
	for _, step := range config.Steps {
		stepState := GetStepState(state, step.Name)
		if stepState == nil {
			stepState = &model.StateStep{
				Name:    step.Name,
				Modules: make([]*model.StateModule, 0),
			}
			state.Steps = append(state.Steps, stepState)
		}
		for _, module := range step.Modules {
			stateModule := GetModuleState(stepState, module.Name)
			if stateModule == nil {
				stepState.Modules = append(stepState.Modules, createStateModule(module))
			}
		}
	}
}

func removeUnusedSteps(config model.Config, state *model.State) {
	for i := len(state.Steps) - 1; i >= 0; i-- {
		stepState := state.Steps[i]
		stepFound := false
		for _, step := range config.Steps {
			if stepState.Name == step.Name {
				stepFound = true
				removeUnusedModules(step, stepState)
				break
			}
		}
		if !stepFound {
			state.Steps = append(state.Steps[:i], state.Steps[i+1:]...)
		}
	}
}

func removeUnusedModules(step model.Step, stepState *model.StateStep) {
	for i := len(stepState.Modules) - 1; i >= 0; i-- {
		moduleState := stepState.Modules[i]
		moduleFound := false
		for _, module := range step.Modules {
			if moduleState.Name == module.Name {
				moduleFound = true
				break
			}
		}
		if !moduleFound {
			stepState.Modules = append(stepState.Modules[:i], stepState.Modules[i+1:]...)
		}
	}
}

func hasCustomTFStep(steps []model.Step) bool {
	for _, step := range steps {
		if step.Type == model.StepTypeTerraformCustom {
			return true
		}
	}
	return false
}

func MergeConfig(patchConfig model.Config, baseConfig model.Config) model.Config {
	err := mergo.Merge(&baseConfig, patchConfig, mergo.WithOverride, mergo.WithTransformers(stepsTransformer{}))
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	return baseConfig
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
			patchStep := getPatchStep(step.Name, target)
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

func getPatchStep(dstStep string, patchSteps []model.Step) *model.Step {
	for _, step := range patchSteps {
		if step.Name == dstStep {
			return &step
		}
	}
	return nil
}

func addNewPatchSteps(sourceSteps []model.Step, patchSteps []model.Step, result []model.Step) []model.Step {
	for _, patchStep := range patchSteps {
		_, sourceStep := findStep(patchStep.Name, sourceSteps)
		if sourceStep != nil {
			continue
		}
		if patchStep.Remove {
			common.PrintWarning(fmt.Sprintf("unable to remove the step %s because it does not exist in base config", patchStep.Name))
			continue
		} else if patchStep.Before != "" {
			index, referencedStep := findStep(patchStep.Before, result)
			if referencedStep == nil {
				common.Logger.Fatalf("before step %s does not exist for step %s", patchStep.Before, patchStep.Name)
			}
			result = append(result[:index+1], result[index:]...)
			result[index] = patchStep
		} else {
			result = append(result, patchStep)
		}
	}
	return result
}

func findStep(stepName string, steps []model.Step) (int, *model.Step) {
	for i, s := range steps {
		if s.Name == stepName {
			return i, &s
		}
	}
	return 0, nil
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
			patchModule := getModule(module.Name, target)
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

func getModule(moduleName string, modules []model.Module) *model.Module {
	for _, module := range modules {
		if module.Name == moduleName {
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
			if patchModule.Remove {
				common.Logger.Printf("unable to remove the module %s because it does not exist in base config", patchModule.Name)
				continue
			}
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
