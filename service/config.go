package service

import (
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
)

const (
	StableVersion = "stable"
	IncludeFormat = "config/%s/include"

	EntigoSource   = "github.com/entigolabs/entigo-infralib-release"
	terraformCache = ".terraform"
)

var ReservedFiles = model.ToSet([]string{"main.tf", "provider.tf", "backend.conf"})

func GetProviderPrefix(flags *common.Flags) string {
	if flags.Prefix != "" {
		return flags.Prefix
	}
	if flags.Config == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("prefix or config must be provided")})
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
		addStepFilesFromFolder(step, fmt.Sprintf(IncludeFormat, step.Name))
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
			common.Logger.Fatalf("failed to read file %s: %v", filePath, err)
		}
		validateStepFile(filePath, fileBytes)
		step.Files = append(step.Files, model.File{
			Name:    filePath,
			Content: fileBytes,
		})
	}
}

func validateStepFile(file string, content []byte) {
	if strings.HasSuffix(file, ".tf") || strings.HasSuffix(file, ".hcl") {
		_, diags := hclwrite.ParseConfig(content, file, hcl.InitialPos)
		if diags.HasErrors() {
			common.Logger.Fatalf("failed to parse hcl file %s: %v", file, diags.Errs())
		}
	} else if strings.HasSuffix(file, ".yaml") {
		var yamlContent map[string]interface{}
		err := yaml.Unmarshal(content, &yamlContent)
		if err != nil {
			common.Logger.Fatalf("failed to unmarshal yaml file %s: %v", file, err)
		}
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
			putStepFiles(bucket, step)
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

func putStepFiles(bucket model.Bucket, step model.Step) {
	files := model.NewSet[string]()
	for _, file := range step.Files {
		err := bucket.PutFile(file.Name, file.Content)
		if err != nil {
			common.Logger.Fatalf("Failed to put step file %s: %s", file.Name, err)
		}
		files.Add(file.Name)
	}
	bucketFiles, err := bucket.ListFolderFiles(fmt.Sprintf(IncludeFormat, step.Name))
	if err != nil {
		common.Logger.Fatalf("Failed to list folder files: %s", err)
	}
	for _, bucketFile := range bucketFiles {
		if files.Contains(bucketFile) {
			continue
		}
		err = bucket.DeleteFile(bucketFile)
		if err != nil {
			common.Logger.Fatalf("Failed to delete file %s: %s", bucketFile, err)
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
	folder := fmt.Sprintf(IncludeFormat, step.Name)
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
		validateStepFile(file, fileBytes)
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

func ProcessSteps(config *model.Config, providerType model.ProviderType) {
	for i, step := range config.Steps {
		if step.Vpc.Attach == nil {
			attach := step.Type == model.StepTypeArgoCD
			if step.Type == model.StepTypeArgoCD && step.KubernetesClusterName == "" {
				step.KubernetesClusterName = "{{ .toutput.eks.cluster_name }}"
			}
			step.Vpc.Attach = &attach
			config.Steps[i] = step
		}
		if !*step.Vpc.Attach || step.Vpc.Id != "" {
			continue
		}
		if providerType == model.AWS {
			step.Vpc.Id = "{{ .toutput.vpc.vpc_id }}"
			step.Vpc.SubnetIds = "[{{ .toutput.vpc.private_subnets }}]"
			step.Vpc.SecurityGroupIds = "[{{ .toutput.vpc.pipeline_security_group }}]"
		} else if providerType == model.GCLOUD {
			step.Vpc.Id = "{{ .toutput.vpc.vpc_name }}"
			step.Vpc.SubnetIds = "[{{ .toutput.vpc.private_subnets[0] }}]"
		}
		config.Steps[i] = step
	}
}

func ValidateConfig(config *model.Config, state *model.State) {
	stepNames := model.NewSet[string]()
	if config.Prefix == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config prefix is not set")})
	}
	if len(config.Prefix) > 10 {
		common.PrintWarning("config prefix longer than 10 characters, trimming to fit")
		config.Prefix = config.Prefix[:10]
	}
	if config.Source != "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config source is deprecated, use sources instead")})
	}
	if config.Sources == nil || len(config.Sources) == 0 {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("sources are not set")})

	}
	for i, source := range config.Sources {
		if source.URL == "" {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("Source[%d] is missing the repository url", i)})
		}
		if source.Version != "" && source.Version != StableVersion {
			_, err := version.NewVersion(source.Version)
			if err != nil {
				common.Logger.Fatalf("Source[%d] version must follow semantic versioning: %s", i, err)
			}
		}
	}
	for _, step := range config.Steps {
		validateStep(step)
		if stepNames.Contains(step.Name) {
			common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name %s is not unique", step.Name)})
		}
		stepNames.Add(step.Name)
		validateConfigModules(step, state)
	}
}

func validateStep(step model.Step) {
	if step.Name == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name is not set")})
	}
	if step.Type == "" {
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step type is not set for step %s", step.Name)})
	}
	if step.Vpc.Id != "" && step.Vpc.SubnetIds == "" {
		common.Logger.Fatalf("VPC ID is set for step %s but subnet IDs are not", step.Name)
	}
	if (step.Vpc.SubnetIds != "" || step.Vpc.SecurityGroupIds != "") && step.Vpc.Id == "" {
		common.Logger.Fatalf("VPC ID is not set for step %s", step.Name)
	}
}

func validateConfigModules(step model.Step, state *model.State) {
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
		if moduleVersionString == "" || moduleVersionString == StableVersion {
			continue
		}
		moduleVersion, err := version.NewVersion(moduleVersionString)
		if err != nil {
			common.Logger.Fatalf("failed to parse module version %s for module %s: %s", module.Version, module.Name, err)
		}
		if stateModule == nil || stateModule.Version == "" {
			continue
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
		common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module Source is not set for module %s in step %s", module.Name, stepName)})
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

func findStep(stepName string, steps []model.Step) (int, *model.Step) {
	for i, s := range steps {
		if s.Name == stepName {
			return i, &s
		}
	}
	return 0, nil
}

func getModule(moduleName string, modules []model.Module) *model.Module {
	for _, module := range modules {
		if module.Name == moduleName {
			return &module
		}
	}
	return nil
}
