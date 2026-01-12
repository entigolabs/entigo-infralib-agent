package service

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
)

const (
	StableVersion = "stable"
	IncludeFormat = "config/%s/include"
	ConfigFile    = "config.yaml"
	EntigoSource  = "github.com/entigolabs/entigo-infralib-release"

	certsFolder    = "ca-certificates"
	terraformCache = ".terraform"
)

var ReservedTFFiles = model.NewSet("main.tf", "provider.tf", "backend.conf")
var ReservedAppsFiles = model.NewSet("argocd.yaml")

func GetProviderPrefix(flags *common.Flags) (string, error) {
	prefix := flags.Prefix
	if prefix == "" {
		if flags.Config == "" {
			return "", errors.New("prefix or config must be provided")
		}
		config, err := getLocalConfigFile(flags.Config)
		if err != nil {
			return "", err
		}
		prefix = config.Prefix
	}
	if prefix == "" {
		return "", errors.New("config prefix is not set")
	}
	if len(prefix) > 10 {
		slog.Warn(common.PrefixWarning("prefix longer than 10 characters, trimming to fit"))
		prefix = prefix[:10]
	}
	return prefix, nil
}

func GetFullConfig(ssm model.SSM, prefix, configFile string, bucket model.Bucket) (model.Config, error) {
	return getConfig(ssm, prefix, configFile, bucket, true)
}

func GetRootConfig(ssm model.SSM, prefix, configFile string, bucket model.Bucket) (model.Config, error) {
	var config model.Config
	var err error
	if configFile != "" {
		config, err = getLocalConfigFile(configFile)
	} else {
		config, err = getRemoteConfigFile(bucket)
	}
	if err != nil {
		return config, err
	}
	return replaceConfigValues(ssm, prefix, config)
}

func GetBaseConfig(prefix, configFile string, bucket model.Bucket) (model.Config, error) {
	return getConfig(nil, prefix, configFile, bucket, false)
}

func getConfig(ssm model.SSM, prefix, configFile string, bucket model.Bucket, addInputs bool) (model.Config, error) {
	if configFile != "" {
		return GetLocalConfig(ssm, prefix, configFile, bucket, addInputs)
	}
	return GetRemoteConfig(ssm, prefix, bucket, addInputs)
}

func GetLocalConfig(ssm model.SSM, prefix, configFile string, bucket model.Bucket, addInputs bool) (model.Config, error) {
	config, err := getLocalConfigFile(configFile)
	if err != nil {
		return config, err
	}
	if err = PutConfig(bucket, config); err != nil {
		return config, err
	}
	config, err = replaceConfigValues(ssm, prefix, config)
	if err != nil {
		return config, err
	}
	reserveAppsFiles(config)
	basePath := filepath.Dir(configFile) + "/"
	if err = AddCertFilesFromFolder(&config, basePath); err != nil {
		return config, err
	}
	if err = AddStepsFilesFromFolder(&config, basePath); err != nil {
		return config, err
	}
	if err = AddModuleInputFiles(&config, basePath, os.ReadFile, addInputs); err != nil {
		return config, err
	}
	if err = PutAdditionalFiles(bucket, config); err != nil {
		return config, err
	}
	return config, nil
}

func getLocalConfigFile(configFile string) (model.Config, error) {
	fileBytes, err := os.ReadFile(configFile)
	if err != nil {
		return model.Config{}, err
	}
	var config model.Config
	err = yaml.Unmarshal(fileBytes, &config)
	if err != nil {
		return model.Config{}, err
	}
	return config, nil
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

func AddCertFilesFromFolder(config *model.Config, basePath string) error {
	entries, err := os.ReadDir(fmt.Sprintf("%s%s", basePath, certsFolder))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		filePath := fmt.Sprintf("%s/%s", certsFolder, entry.Name())
		fileBytes, err := os.ReadFile(basePath + filePath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %v", filePath, err)
		}
		config.Certs = append(config.Certs, model.File{
			Name:    filePath,
			Content: fileBytes,
		})
	}
	return nil
}

func AddStepsFilesFromFolder(config *model.Config, basePath string) error {
	for i := range config.Steps {
		step := &config.Steps[i]
		err := addStepFilesFromFolder(step, basePath, fmt.Sprintf(IncludeFormat, step.Name))
		if err != nil {
			return err
		}
	}
	return nil
}

func addStepFilesFromFolder(step *model.Step, basePath, folder string) error {
	entries, err := os.ReadDir(fmt.Sprintf("%s%s", basePath, folder))
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			err = addStepFilesFromFolder(step, basePath, fmt.Sprintf("%s/%s", folder, entry.Name()))
			if err != nil {
				return err
			}
			continue
		}
		if step.Type == model.StepTypeTerraform && ReservedTFFiles.Contains(entry.Name()) {
			return fmt.Errorf("can't include files %s in step %s", ReservedTFFiles, step.Name)
		} else if step.Type == model.StepTypeArgoCD && ReservedAppsFiles.Contains(entry.Name()) {
			return fmt.Errorf("can't include files %s in step %s", ReservedAppsFiles, step.Name)
		}
		filePath := fmt.Sprintf("%s/%s", folder, entry.Name())
		fileBytes, err := os.ReadFile(basePath + filePath)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %v", filePath, err)
		}
		step.Files = append(step.Files, model.File{
			Name:    filePath,
			Content: fileBytes,
		})
	}
	return nil
}

func PutConfig(bucket model.Bucket, config model.Config) error {
	bytes, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %s", err)
	}
	err = bucket.PutFile(ConfigFile, bytes)
	if err != nil {
		return fmt.Errorf("failed to put config: %s", err)
	}
	return nil
}

func PutAdditionalFiles(bucket model.Bucket, config model.Config) error {
	if len(config.Certs) == 0 {
		if err := removeFolder(bucket, certsFolder); err != nil {
			return err
		}
	} else {
		if err := putFolderFiles(bucket, certsFolder, config.Certs); err != nil {
			return err
		}
	}
	for _, step := range config.Steps {
		if len(step.Files) == 0 {
			if err := removeFolder(bucket, fmt.Sprintf(IncludeFormat, step.Name)); err != nil {
				return err
			}
		} else if step.Files != nil {
			if err := putFolderFiles(bucket, fmt.Sprintf(IncludeFormat, step.Name), step.Files); err != nil {
				return err
			}
		}
		if step.Modules == nil {
			continue
		}
		for _, module := range step.Modules {
			if err := putModuleFiles(step, module, bucket); err != nil {
				return err
			}
		}
	}
	return nil
}

func putModuleFiles(step model.Step, module model.Module, bucket model.Bucket) error {
	if module.InputsFile == "" {
		inputsFile := fmt.Sprintf("config/%s/%s.yaml", step.Name, module.Name)
		bytes, err := bucket.GetFile(inputsFile)
		if err != nil {
			return fmt.Errorf("failed to get module %s inputs file: %s", module.Name, err)
		}
		if bytes != nil {
			err = bucket.DeleteFile(inputsFile)
			if err != nil {
				return fmt.Errorf("failed to delete module %s inputs file: %s", module.Name, err)
			}
		}
	} else {
		err := bucket.PutFile(module.InputsFile, module.FileContent)
		if err != nil {
			return fmt.Errorf("failed to put module %s inputs file: %s", module.Name, err)
		}
		module.InputsFile = ""
		module.FileContent = nil
	}
	return nil
}

func removeFolder(bucket model.Bucket, folder string) error {
	files, err := bucket.ListFolderFiles(folder)
	if err != nil {
		return fmt.Errorf("failed to list folder %s files: %s", folder, err)
	}
	if len(files) == 0 {
		return nil
	}
	log.Printf("Removing bucket folder %s", folder)
	for _, file := range files {
		err = bucket.DeleteFile(file)
		if err != nil {
			return fmt.Errorf("failed to delete file %s: %s", file, err)
		}
	}
	return nil
}

func putFolderFiles(bucket model.Bucket, folder string, files []model.File) error {
	allFiles := model.NewSet[string]()
	for _, file := range files {

		err := bucket.PutFile(file.Name, file.Content)
		if err != nil {
			return fmt.Errorf("failed to put step file %s: %s", file.Name, err)
		}
		allFiles.Add(file.Name)
	}
	bucketFiles, err := bucket.ListFolderFiles(folder)
	if err != nil {
		return fmt.Errorf("failed to list folder allFiles: %s", err)
	}
	for _, bucketFile := range bucketFiles {
		if allFiles.Contains(bucketFile) {
			continue
		}
		err = bucket.DeleteFile(bucketFile)
		if err != nil {
			return fmt.Errorf("failed to delete file %s: %s", bucketFile, err)
		}
	}
	return nil
}

func GetRemoteConfig(ssm model.SSM, prefix string, bucket model.Bucket, addInputs bool) (model.Config, error) {
	config, err := getRemoteConfigFile(bucket)
	if err != nil {
		return config, err
	}
	config, err = replaceConfigValues(ssm, prefix, config)
	if err != nil {
		return config, err
	}
	reserveAppsFiles(config)
	if err = AddCertFilesFromBucket(&config, bucket); err != nil {
		return config, err
	}
	if err = AddStepsFilesFromBucket(&config, bucket); err != nil {
		return config, err
	}
	if err = AddModuleInputFiles(&config, "", bucket.GetFile, addInputs); err != nil {
		return config, err
	}
	return config, nil
}

func getRemoteConfigFile(bucket model.Bucket) (model.Config, error) {
	bytes, err := bucket.GetFile(ConfigFile)
	if err != nil {
		return model.Config{}, fmt.Errorf("failed to get config: %s", err)
	}
	if bytes == nil {
		return model.Config{}, errors.New("config file not found")
	}
	var config model.Config
	err = yaml.Unmarshal(bytes, &config)
	if err != nil {
		return model.Config{}, fmt.Errorf("failed to unmarshal config: %s", err)
	}
	return config, nil
}

func AddCertFilesFromBucket(config *model.Config, bucket model.Bucket) error {
	files, err := bucket.ListFolderFiles(certsFolder)
	if err != nil {
		return fmt.Errorf("failed to list %s folder files: %s", certsFolder, err)
	}
	for _, file := range files {
		fileBytes, err := bucket.GetFile(file)
		if err != nil {
			return fmt.Errorf("failed to get file %s: %s", file, err)
		}
		if fileBytes == nil {
			continue
		}
		config.Certs = append(config.Certs, model.File{
			Name:    file,
			Content: fileBytes,
		})
	}
	return nil
}

func AddStepsFilesFromBucket(config *model.Config, bucket model.Bucket) error {
	for i := range config.Steps {
		step := &config.Steps[i]
		if err := addStepFilesFromBucket(step, bucket); err != nil {
			return err
		}
	}
	return nil
}

func addStepFilesFromBucket(step *model.Step, bucket model.Bucket) error {
	folder := fmt.Sprintf(IncludeFormat, step.Name)
	files, err := bucket.ListFolderFiles(folder)
	if err != nil {
		return fmt.Errorf("failed to list folder files: %s", err)
	}
	for _, file := range files {
		if step.Type == model.StepTypeTerraform && ReservedTFFiles.Contains(strings.TrimPrefix(file, folder+"/")) {
			return fmt.Errorf("can't include files %s in step %s", ReservedTFFiles, step.Name)
		} else if step.Type == model.StepTypeArgoCD && ReservedAppsFiles.Contains(strings.TrimPrefix(file, folder+"/")) {
			return fmt.Errorf("can't include files %s in step %s", ReservedAppsFiles, step.Name)
		}
		fileBytes, err := bucket.GetFile(file)
		if err != nil {
			return fmt.Errorf("failed to get file %s: %s", file, err)
		}
		if fileBytes == nil {
			continue
		}
		step.Files = append(step.Files, model.File{
			Name:    file,
			Content: fileBytes,
		})
	}
	return nil
}

func AddModuleInputFiles(config *model.Config, basePath string, readFile func(string) ([]byte, error), addInputs bool) error {
	for _, step := range config.Steps {
		if step.Modules == nil {
			continue
		}
		if step.Name == "" {
			return errors.New("step name is not set")
		}
		for i := range step.Modules {
			module := &step.Modules[i]
			if module.Name == "" {
				return fmt.Errorf("module name is not set in step %s", step.Name)
			}
			if err := processModuleInputs(step.Name, module, basePath, readFile, addInputs); err != nil {
				return err
			}
		}
	}
	return nil
}

func processModuleInputs(stepName string, module *model.Module, basePath string, readFile func(string) ([]byte, error), addInputs bool) error {
	yamlFile := fmt.Sprintf("%sconfig/%s/%s.yaml", basePath, stepName, module.Name)
	bytes, err := readFile(yamlFile)
	if module.Inputs != nil {
		if err == nil && bytes != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("module %s/%s has inputs, ignoring file %s", stepName, module.Name, yamlFile)))
		}
		module.ConfigInputs, err = util.DeepCopyYAML(module.Inputs) // Make a copy for merging base inputs later
		return err
	}
	if bytes == nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read input file %s: %v", yamlFile, err)
	}
	module.InputsFile = strings.TrimPrefix(yamlFile, basePath)
	module.FileContent = bytes
	if !addInputs {
		return nil
	}
	err = yaml.Unmarshal(bytes, &module.Inputs)
	if err != nil {
		return fmt.Errorf("failed to unmarshal input file %s: %v", yamlFile, err)
	}
	err = yaml.Unmarshal(bytes, &module.ConfigInputs)
	if err != nil {
		return fmt.Errorf("failed to unmarshal input file %s: %v", yamlFile, err)
	}
	return nil
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
	if step.Type == model.StepTypeArgoCD && step.KubernetesClusterName == "" {
		step.KubernetesClusterName = getKubernetesClusterName(providerType)
	}
	if step.Vpc.Attach == nil {
		attach := step.Type == model.StepTypeArgoCD
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
	if !*step.Vpc.Attach {
		return
	}
	switch providerType {
	case model.AWS:
		if step.Vpc.Id == "" {
			step.Vpc.Id = "{{ .toutput.vpc.vpc_id }}"
		}
		if step.Vpc.SubnetIds == "" {
			step.Vpc.SubnetIds = "[{{ .toptout.vpc.control_subnets | .toutput.vpc.private_subnets }}]"
		}
		if step.Vpc.SecurityGroupIds == "" {
			step.Vpc.SecurityGroupIds = "[{{ .toutput.vpc.pipeline_security_group }}]"
		}
	case model.GCLOUD:
		if step.Vpc.Id == "" {
			step.Vpc.Id = "{{ .toutput.vpc.vpc_name }}"
		}
		if step.Vpc.SubnetIds == "" {
			step.Vpc.SubnetIds = "[{{ .toutput.vpc.private_subnets[0] }}]"
		}
	}
}

func ValidateConfig(config model.Config, state *model.State) error {
	if len(config.Sources) == 0 {
		return fmt.Errorf("at least one source must be provided")
	}
	for index, source := range config.Sources {
		if err := validateSource(index, source); err != nil {
			return err
		}
	}
	destinations := model.NewSet[string]()
	for index, destination := range config.Destinations {
		if err := validateDestination(index, destination); err != nil {
			return err
		}
		if destinations.Contains(destination.Name) {
			return fmt.Errorf("destination name %s is not unique", destination.Name)
		}
		destinations.Add(destination.Name)
	}
	err := validateSteps(config, state)
	if err != nil {
		return err
	}
	return nil
}

func validateSource(index int, source model.ConfigSource) error {
	if source.URL == "" {
		return fmt.Errorf("%d. source URL is not set", index+1)
	}
	if source.Include != nil && source.Exclude != nil {
		return fmt.Errorf("source %s can't have both include and exclude", source.URL)
	}
	if source.Version == "" && source.ForceVersion {
		return fmt.Errorf("source %s force version is set but version is not", source.URL)
	}
	if source.ForceVersion {
		return nil
	}
	if source.Version != "" && source.Version != StableVersion {
		_, err := version.NewVersion(source.Version)
		if err != nil {
			return fmt.Errorf("source %s version must follow semantic versioning: %s", source.URL, err)
		}
	}
	if source.Username != "" && source.Password == "" {
		return fmt.Errorf("source %s username given but password is empty", source.URL)
	}
	if source.Password != "" && source.Username == "" {
		return fmt.Errorf("source %s password given but username is empty", source.URL)
	}
	return nil
}

func validateDestination(index int, destination model.ConfigDestination) error {
	if destination.Name == "" {
		return fmt.Errorf("%d. destination name is not set", index+1)
	}
	if destination.Git == nil {
		return nil
	}
	if destination.Git.URL == "" {
		return fmt.Errorf("%d. destination git URL is not set", index+1)
	}
	if destination.Git.Key != "" {
		if destination.Git.Username != "" || destination.Git.Password != "" {
			return fmt.Errorf("%d. destination git key and username/password can't be set together", index+1)
		}
	}
	if destination.Git.Username != "" && destination.Git.Password == "" {
		return fmt.Errorf("%d. destination git password is required when using basic auth", index+1)
	}
	if destination.Git.Password != "" && destination.Git.Username == "" {
		return fmt.Errorf("%d. destination git username is required when using basic auth", index+1)
	}
	return nil
}

func validateSteps(config model.Config, state *model.State) error {
	stepNames := model.NewSet[string]()
	defaultModules := model.NewSet[string]()
	for _, step := range config.Steps {
		if err := validateStep(step); err != nil {
			return err
		}
		if stepNames.Contains(step.Name) {
			return fmt.Errorf("step name %s is not unique", step.Name)
		}
		stepNames.Add(step.Name)
		if err := validateConfigModules(step, state); err != nil {
			return err
		}
		for _, module := range step.Modules {
			if !module.DefaultModule {
				continue
			}
			moduleType := getModuleType(module)
			if defaultModules.Contains(moduleType) {
				return fmt.Errorf("multiple default modules found with type %s", moduleType)
			}
			defaultModules.Add(moduleType)
		}
	}
	return nil
}

func validateStep(step model.Step) error {
	if step.Name == "" {
		return errors.New("step name is not set")
	}
	if step.Type == "" {
		return fmt.Errorf("step type is not set for step %s", step.Name)
	}
	if step.Approve != "" {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Step %s uses deprecated 'approve' property, use 'manual_approve_run' and 'manual_approve_update'", step.Name)))
	}
	if step.Approve != "" && (step.UpdateApprove != "" || step.RunApprove != "") {
		return fmt.Errorf("step %s can't have both approve and manual_approve set", step.Name)
	}
	return nil
}

func validateConfigModules(step model.Step, state *model.State) error {
	stepState := GetStepState(state, step.Name)
	moduleNames := model.NewSet[string]()
	for _, module := range step.Modules {
		if err := validateModule(module, step.Name); err != nil {
			return err
		}
		if moduleNames.Contains(module.Name) {
			return fmt.Errorf("module name %s is not unique in step %s", module.Name, step.Name)
		}
		moduleNames.Add(module.Name)
		if stepState == nil {
			continue
		}
		if err := validateModuleVersioning(step, stepState, module); err != nil {
			return err
		}
	}
	return nil
}

func validateModule(module model.Module, stepName string) error {
	if module.Name == "" {
		return fmt.Errorf("module name is not set in step %s", stepName)
	}
	if module.Source == "" {
		return fmt.Errorf("module Source is not set for module %s in step %s", module.Name, stepName)
	}
	return nil
}

func validateModuleVersioning(step model.Step, stepState *model.StateStep, module model.Module) error {
	stateModule := GetModuleState(stepState, module.Name)
	moduleVersionString := module.Version
	if util.IsClientModule(module) {
		if moduleVersionString == "" {
			return fmt.Errorf("module version is not set for client module %s in step %s", module.Name, step.Name)
		}
		return nil
	}
	if moduleVersionString == "" || moduleVersionString == StableVersion {
		return nil
	}
	moduleVersion, err := version.NewVersion(moduleVersionString)
	if err != nil {
		return fmt.Errorf("failed to parse module version %s for module %s: %s", module.Version, module.Name, err)
	}
	if stateModule == nil || stateModule.Version == "" {
		return nil
	}
	stateModuleVersion, err := version.NewVersion(stateModule.Version)
	if err != nil {
		return fmt.Errorf("failed to parse state module version %s for module %s: %s", stateModule.Version, module.Name, err)
	}
	if moduleVersion.LessThan(stateModuleVersion) {
		return fmt.Errorf("config module %s version %s is less than state version %s", module.Name,
			moduleVersionString, stateModule.Version)
	}
	return nil
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
	if stepState == nil {
		return nil
	}
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
