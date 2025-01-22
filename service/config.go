package service

import (
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	StableVersion = "stable"
	IncludeFormat = "config/%s/include"
	ConfigFile    = "config.yaml"
	EntigoSource  = "github.com/entigolabs/entigo-infralib-release"

	terraformCache = ".terraform"
)

var ReservedTFFiles = model.ToSet([]string{"main.tf", "provider.tf", "backend.conf"})
var ReservedAppsFiles = model.ToSet([]string{"argocd.yaml"})

func GetProviderPrefix(flags *common.Flags) string {
	prefix := flags.Prefix
	if prefix == "" {
		if flags.Config == "" {
			log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("prefix or config must be provided")})
		}
		prefix = getLocalConfigFile(flags.Config).Prefix
	}
	if prefix == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("config prefix is not set")})
	}
	if len(prefix) > 10 {
		slog.Warn(common.PrefixWarning("prefix longer than 10 characters, trimming to fit"))
		prefix = prefix[:10]
	}
	return prefix
}

func GetFullConfig(ssm model.SSM, prefix, configFile string, bucket model.Bucket) model.Config {
	return getConfig(ssm, prefix, configFile, bucket, true)
}

func GetBaseConfig(prefix, configFile string, bucket model.Bucket) model.Config {
	return getConfig(nil, prefix, configFile, bucket, false)
}

func getConfig(ssm model.SSM, prefix, configFile string, bucket model.Bucket, addInputs bool) model.Config {
	var config model.Config
	if configFile != "" {
		config = GetLocalConfig(ssm, prefix, configFile, bucket, addInputs)
	} else {
		config = GetRemoteConfig(ssm, prefix, bucket, addInputs)
	}
	return config
}

func GetLocalConfig(ssm model.SSM, prefix, configFile string, bucket model.Bucket, addInputs bool) model.Config {
	config := getLocalConfigFile(configFile)
	PutConfig(bucket, config)
	config = replaceConfigValues(ssm, prefix, config)
	reserveAppsFiles(config)
	basePath := filepath.Dir(configFile) + "/"
	AddStepsFilesFromFolder(&config, basePath)
	AddModuleInputFiles(&config, basePath, os.ReadFile, addInputs)
	PutAdditionalFiles(bucket, config.Steps)
	return config
}

func getLocalConfigFile(configFile string) model.Config {
	fileBytes, err := os.ReadFile(configFile)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	var config model.Config
	err = yaml.Unmarshal(fileBytes, &config)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	return config
}

func reserveAppsFiles(config model.Config) {
	for _, step := range config.Steps {
		if step.Type != model.StepTypeArgoCD {
			continue
		}
		for _, module := range step.Modules {
			ReservedAppsFiles.Add(fmt.Sprintf("%s.yaml", module.Name))
		}
	}
}

func AddStepsFilesFromFolder(config *model.Config, basePath string) {
	if config.Steps == nil {
		return
	}
	for i := range config.Steps {
		step := &config.Steps[i]
		addStepFilesFromFolder(step, basePath, fmt.Sprintf(IncludeFormat, step.Name))
	}
}

func addStepFilesFromFolder(step *model.Step, basePath, folder string) {
	entries, err := os.ReadDir(fmt.Sprintf("%s%s", basePath, folder))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			addStepFilesFromFolder(step, basePath, fmt.Sprintf("%s/%s", folder, entry.Name()))
			continue
		}
		if step.Type == model.StepTypeTerraform && ReservedTFFiles.Contains(entry.Name()) {
			log.Fatalf("Can't include files %s in step %s", ReservedTFFiles, step.Name)
		} else if step.Type == model.StepTypeArgoCD && ReservedAppsFiles.Contains(entry.Name()) {
			log.Fatalf("Can't include files %s in step %s", ReservedAppsFiles, step.Name)
		}
		filePath := fmt.Sprintf("%s/%s", folder, entry.Name())
		fileBytes, err := os.ReadFile(basePath + filePath)
		if err != nil {
			log.Fatalf("failed to read file %s: %v", filePath, err)
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
		log.Fatalf("Failed to marshal config: %s", err)
	}
	err = bucket.PutFile(ConfigFile, bytes)
	if err != nil {
		log.Fatalf("Failed to put config: %s", err)
	}
}

func PutAdditionalFiles(bucket model.Bucket, steps []model.Step) {
	for _, step := range steps {
		if len(step.Files) == 0 {
			removeStepIncludeFolder(bucket, step.Name)
		} else if step.Files != nil {
			putStepFiles(bucket, step)
		}
		if step.Modules == nil {
			continue
		}
		for _, module := range step.Modules {
			if module.InputsFile == "" {
				inputsFile := fmt.Sprintf("config/%s/%s.yaml", step.Name, module.Name)
				bytes, err := bucket.GetFile(inputsFile)
				if err != nil {
					log.Fatalf("Failed to get module %s inputs file: %s", module.Name, err)
				}
				if bytes != nil {
					err = bucket.DeleteFile(inputsFile)
					if err != nil {
						log.Fatalf("Failed to delete module %s inputs file: %s", module.Name, err)
					}
				}
			} else {
				err := bucket.PutFile(module.InputsFile, module.FileContent)
				if err != nil {
					log.Fatalf("Failed to put module %s inputs file: %s", module.Name, err)
				}
				module.InputsFile = ""
				module.FileContent = nil
			}
		}
	}
}

func removeStepIncludeFolder(bucket model.Bucket, name string) {
	files, err := bucket.ListFolderFiles(fmt.Sprintf(IncludeFormat, name))
	if err != nil {
		log.Fatalf("Failed to list folder files: %s", err)
	}
	if len(files) == 0 {
		return
	}
	log.Printf("Removing included files for step %s", name)
	for _, file := range files {
		err = bucket.DeleteFile(file)
		if err != nil {
			log.Fatalf("Failed to delete file %s: %s", file, err)
		}
	}
}

func putStepFiles(bucket model.Bucket, step model.Step) {
	files := model.NewSet[string]()
	for _, file := range step.Files {
		err := bucket.PutFile(file.Name, file.Content)
		if err != nil {
			log.Fatalf("Failed to put step file %s: %s", file.Name, err)
		}
		files.Add(file.Name)
	}
	bucketFiles, err := bucket.ListFolderFiles(fmt.Sprintf(IncludeFormat, step.Name))
	if err != nil {
		log.Fatalf("Failed to list folder files: %s", err)
	}
	for _, bucketFile := range bucketFiles {
		if files.Contains(bucketFile) {
			continue
		}
		err = bucket.DeleteFile(bucketFile)
		if err != nil {
			log.Fatalf("Failed to delete file %s: %s", bucketFile, err)
		}
	}
}

func GetRemoteConfig(ssm model.SSM, prefix string, bucket model.Bucket, addInputs bool) model.Config {
	config := replaceConfigValues(ssm, prefix, getRemoteConfigFile(bucket))
	reserveAppsFiles(config)
	AddStepsFilesFromBucket(&config, bucket)
	AddModuleInputFiles(&config, "", bucket.GetFile, addInputs)
	return config
}

func getRemoteConfigFile(bucket model.Bucket) model.Config {
	bytes, err := bucket.GetFile(ConfigFile)
	if err != nil {
		log.Fatalf("Failed to get config: %s", err)
	}
	if bytes == nil {
		log.Fatalf("Config file not found")
	}
	var config model.Config
	err = yaml.Unmarshal(bytes, &config)
	if err != nil {
		log.Fatalf("Failed to unmarshal config: %s", err)
	}
	return config
}

func AddStepsFilesFromBucket(config *model.Config, bucket model.Bucket) {
	if config.Steps == nil {
		return
	}
	for i := range config.Steps {
		step := &config.Steps[i]
		addStepFilesFromBucket(step, bucket)
	}
}

func addStepFilesFromBucket(step *model.Step, bucket model.Bucket) {
	folder := fmt.Sprintf(IncludeFormat, step.Name)
	files, err := bucket.ListFolderFiles(folder)
	if err != nil {
		log.Fatalf("Failed to list folder files: %s", err)
	}
	for _, file := range files {
		if step.Type == model.StepTypeTerraform && ReservedTFFiles.Contains(strings.TrimPrefix(file, folder+"/")) {
			log.Fatalf("Can't include files %s in step %s", ReservedTFFiles, step.Name)
		} else if step.Type == model.StepTypeArgoCD && ReservedAppsFiles.Contains(strings.TrimPrefix(file, folder+"/")) {
			log.Fatalf("Can't include files %s in step %s", ReservedAppsFiles, step.Name)
		}
		fileBytes, err := bucket.GetFile(file)
		if err != nil {
			log.Fatalf("Failed to get file %s: %s", file, err)
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

func AddModuleInputFiles(config *model.Config, basePath string, readFile func(string) ([]byte, error), addInputs bool) {
	if config.Steps == nil {
		return
	}
	for _, step := range config.Steps {
		if step.Modules == nil {
			continue
		}
		if step.Name == "" {
			log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name is not set")})
		}
		for i := range step.Modules {
			module := &step.Modules[i]
			if module.Name == "" {
				log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module name is not set in step %s", step.Name)})
			}
			processModuleInputs(step.Name, module, basePath, readFile, addInputs)
		}
	}
}

func processModuleInputs(stepName string, module *model.Module, basePath string, readFile func(string) ([]byte, error), addInputs bool) {
	yamlFile := fmt.Sprintf("%sconfig/%s/%s.yaml", basePath, stepName, module.Name)
	bytes, err := readFile(yamlFile)
	if module.Inputs != nil {
		if err == nil && bytes != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("module %s/%s has inputs, ignoring file %s", stepName, module.Name, yamlFile)))
		}
		return
	}
	if bytes == nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
		return
	}
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("failed to read input file %s: %v", yamlFile, err)})
	}
	module.InputsFile = strings.TrimPrefix(yamlFile, basePath)
	module.FileContent = bytes
	if !addInputs {
		return
	}
	err = yaml.Unmarshal(bytes, &module.Inputs)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("failed to unmarshal input file %s: %v",
			yamlFile, err)})
	}
}

func ProcessConfig(config *model.Config, providerType model.ProviderType) {
	processSources(config)
	processSteps(config, providerType)
}

func processSources(config *model.Config) {
	for i, source := range config.Sources {
		if !util.IsLocalSource(source.URL) {
			continue
		}
		source.ForceVersion = true
		source.Version = "local"
		config.Sources[i] = source
	}
}

func processSteps(config *model.Config, providerType model.ProviderType) {
	for i, step := range config.Steps {
		processStepVpcAttach(&step, providerType)
		processStepVpcIds(&step, providerType)
		config.Steps[i] = step
	}
}

func processStepVpcAttach(step *model.Step, providerType model.ProviderType) {
	if step.Vpc.Attach == nil {
		attach := step.Type == model.StepTypeArgoCD
		if step.Type == model.StepTypeArgoCD && step.KubernetesClusterName == "" {
			step.KubernetesClusterName = getKubernetesClusterName(providerType)
		}
		step.Vpc.Attach = &attach
	}
}

func getKubernetesClusterName(providerType model.ProviderType) string {
	if providerType == model.GCLOUD {
		return "{{ .toutput.gke.cluster_name }}"
	}
	return "{{ .toutput.eks.cluster_name }}"
}

func processStepVpcIds(step *model.Step, providerType model.ProviderType) {
	if !*step.Vpc.Attach || step.Vpc.Id != "" {
		return
	}
	if providerType == model.AWS {
		step.Vpc.Id = "{{ .toutput.vpc.vpc_id }}"
		step.Vpc.SubnetIds = "[{{ .toutput.vpc.private_subnets }}]"
		step.Vpc.SecurityGroupIds = "[{{ .toutput.vpc.pipeline_security_group }}]"
	} else if providerType == model.GCLOUD {
		step.Vpc.Id = "{{ .toutput.vpc.vpc_name }}"
		step.Vpc.SubnetIds = "[{{ .toutput.vpc.private_subnets[0] }}]"
	}
}

func ValidateConfig(config model.Config, state *model.State) {
	if len(config.Sources) == 0 {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("at least one source must be provided")})
	}
	for index, source := range config.Sources {
		validateSource(index, source)
	}
	destinations := model.NewSet[string]()
	for index, destination := range config.Destinations {
		validateDestination(index, destination)
		if destinations.Contains(destination.Name) {
			log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("destination name %s is not unique", destination.Name)})
		}
		destinations.Add(destination.Name)
	}
	stepNames := model.NewSet[string]()
	for _, step := range config.Steps {
		validateStep(step)
		if stepNames.Contains(step.Name) {
			log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name %s is not unique", step.Name)})
		}
		stepNames.Add(step.Name)
		validateConfigModules(step, state)
	}
}

func validateSource(index int, source model.ConfigSource) {
	if source.URL == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("%d. source URL is not set", index+1)})
	}
	if source.Include != nil && source.Exclude != nil {
		log.Fatalf("source %s can't have both include and exclude", source.URL)
	}
	if source.Version == "" && source.ForceVersion {
		log.Fatalf("source %s force version is set but version is not", source.URL)
	}
	if source.ForceVersion {
		return
	}
	if source.Version != "" && source.Version != StableVersion {
		_, err := version.NewVersion(source.Version)
		if err != nil {
			log.Fatalf("source %s version must follow semantic versioning: %s", source.URL, err)
		}
	}
}

func validateDestination(index int, destination model.ConfigDestination) {
	if destination.Name == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("%d. destination name is not set", index+1)})
	}
	if destination.Git == nil {
		return
	}
	if destination.Git.URL == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("%d. destination git URL is not set", index+1)})
	}
	if destination.Git.Key != "" {
		if destination.Git.Username != "" || destination.Git.Password != "" {
			log.Fatalf("%d. destination git key and username/password can't be set together", index+1)
		}
	}
	if destination.Git.Username != "" && destination.Git.Password == "" {
		log.Fatalf("%d. destination git password is required when using basic auth", index+1)
	}
	if destination.Git.Password != "" && destination.Git.Username == "" {
		log.Fatalf("%d. destination git username is required when using basic auth", index+1)
	}
}

func validateStep(step model.Step) {
	if step.Name == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step name is not set")})
	}
	if step.Type == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("step type is not set for step %s", step.Name)})
	}
	if step.Vpc.Id != "" && step.Vpc.SubnetIds == "" {
		log.Fatalf("VPC ID is set for step %s but subnet IDs are not", step.Name)
	}
	if (step.Vpc.SubnetIds != "" || step.Vpc.SecurityGroupIds != "") && step.Vpc.Id == "" {
		log.Fatalf("VPC ID is not set for step %s", step.Name)
	}
}

func validateConfigModules(step model.Step, state *model.State) {
	stepState := GetStepState(state, step.Name)
	moduleNames := model.NewSet[string]()
	for _, module := range step.Modules {
		validateModule(module, step.Name)
		if moduleNames.Contains(module.Name) {
			log.Fatalf("module name %s is not unique in step %s", module.Name, step.Name)
		}
		moduleNames.Add(module.Name)
		if stepState == nil {
			continue
		}
		validateModuleVersioning(step, stepState, module)
	}
}

func validateModule(module model.Module, stepName string) {
	if module.Name == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module name is not set in step %s", stepName)})
	}
	if module.Source == "" {
		log.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module Source is not set for module %s in step %s", module.Name, stepName)})
	}
}

func validateModuleVersioning(step model.Step, stepState *model.StateStep, module model.Module) {
	stateModule := GetModuleState(stepState, module.Name)
	moduleVersionString := module.Version
	if util.IsClientModule(module) {
		if moduleVersionString == "" {
			log.Fatalf("module version is not set for client module %s in step %s", module.Name, step.Name)
		}
		return
	}
	if moduleVersionString == "" || moduleVersionString == StableVersion {
		return
	}
	moduleVersion, err := version.NewVersion(moduleVersionString)
	if err != nil {
		log.Fatalf("failed to parse module version %s for module %s: %s", module.Version, module.Name, err)
	}
	if stateModule == nil || stateModule.Version == "" {
		return
	}
	stateModuleVersion, err := version.NewVersion(stateModule.Version)
	if err != nil {
		log.Fatalf("failed to parse state module version %s for module %s: %s", stateModule.Version, module.Name, err)
	}
	if moduleVersion.LessThan(stateModuleVersion) {
		log.Fatalf("config module %s version %s is less than state version %s", module.Name,
			moduleVersionString, stateModule.Version)
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

func removeUnusedSteps(prefix string, config model.Config, state *model.State, bucket model.Bucket) {
	for i := len(state.Steps) - 1; i >= 0; i-- {
		stepState := state.Steps[i]
		stepFound := false
		for _, step := range config.Steps {
			if stepState.Name == step.Name {
				stepFound = true
				removeUnusedModules(step, stepState, bucket)
				break
			}
		}
		if !stepFound {
			state.Steps = append(state.Steps[:i], state.Steps[i+1:]...)
			log.Printf("Removing unused step %s files", stepState.Name)
			removeUnusedFiles(bucket, fmt.Sprintf("steps/%s-%s", prefix, stepState.Name))
			removeUnusedFiles(bucket, fmt.Sprintf("config/%s", stepState.Name))
		}
	}
}

func removeUnusedModules(step model.Step, stepState *model.StateStep, bucket model.Bucket) {
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
			inputsFile := fmt.Sprintf("config/%s/%s.yaml", step.Name, moduleState.Name)
			_ = bucket.DeleteFile(inputsFile)
		}
	}
}

func removeUnusedFiles(bucket model.Bucket, folder string) {
	stepFiles, err := bucket.ListFolderFiles(folder)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to list files in unused folder %s: %v", folder, err)))
		return
	}
	err = bucket.DeleteFiles(stepFiles)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete unused files in folder %s: %v", folder, err)))
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
