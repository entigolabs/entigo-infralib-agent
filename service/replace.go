package service

import (
	"encoding/json"
	"errors"
	"fmt"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/entigolabs/entigo-infralib-agent/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"gopkg.in/yaml.v3"
	"log"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type paramCache map[string]map[string]tfOutput

type tfOutput struct {
	Sensitive bool
	Type      interface{}
	Value     interface{}
}

type keyType struct {
	ReplaceKey  string
	ReplaceType string
}

const (
	terraformOutput = "terraform-output.json"
)

var replaceRegex = regexp.MustCompile(`{{((?:\x60{{)*.*?(?:}}\x60)*)}}`)
var parameterIndexRegex = regexp.MustCompile(`([^\[\]]+)(\[(\d+)(-(\d+))?])?`)

func (u *updater) replaceConfigStepValues(step model.Step, index int) (model.Step, error) {
	stepYaml, err := yaml.Marshal(step)
	if err != nil {
		return step, fmt.Errorf("failed to convert step %s to yaml, error: %v", step.Name, err)
	}
	cache := make(paramCache)
	modifiedStepYaml, delayedKeyTypes, err := u.replaceStringValues(step, string(stepYaml), index, cache)
	if err != nil {
		log.Printf("Failed to replace tags in step %s", step.Name)
		return step, err
	}
	var modifiedStep model.Step
	err = yaml.Unmarshal([]byte(modifiedStepYaml), &modifiedStep)
	if err != nil {
		slog.Debug(fmt.Sprintf("broken step yaml %s:\n%s", step.Name, modifiedStepYaml))
		return step, fmt.Errorf("failed to unmarshal modified step %s yaml, error: %v", step.Name, err)
	}
	moduleChecksums, err := calculateModuleChecksums(modifiedStep)
	if err != nil {
		return modifiedStep, err
	}
	modifiedStepYaml, err = u.replaceDelayedStringValues(step, modifiedStepYaml, index, cache, delayedKeyTypes)
	if err != nil {
		log.Printf("Failed to replace delayed tags in step %s", step.Name)
		return step, err
	}
	err = yaml.Unmarshal([]byte(modifiedStepYaml), &modifiedStep)
	if err != nil {
		slog.Debug(fmt.Sprintf("broken step yaml %s:\n%s", step.Name, modifiedStepYaml))
		return step, fmt.Errorf("failed to unmarshal modified step %s yaml, error: %v", step.Name, err)
	}
	for i, module := range modifiedStep.Modules {
		module.InputsChecksum = moduleChecksums[module.Name]
		modifiedStep.Modules[i] = module
	}
	err = u.replaceConfigStepFileValues(step, &modifiedStep, index, cache)
	return modifiedStep, err
}

func calculateModuleChecksums(step model.Step) (map[string][]byte, error) {
	checksums := make(map[string][]byte)
	for i, module := range step.Modules {
		if len(module.Inputs) == 0 {
			continue
		}
		sorted := util.SortKeys(module.Inputs)
		inputsYaml, err := yaml.Marshal(sorted)
		if err != nil {
			return nil, fmt.Errorf("failed to convert step %s module %s inputs to yaml, error: %v", step.Name,
				module.Name, err)
		}
		checksums[module.Name] = util.CalculateHash(inputsYaml)
		step.Modules[i] = module
	}
	return checksums, nil
}

func (u *updater) replaceConfigStepFileValues(step model.Step, modifiedStep *model.Step, index int, cache paramCache) error {
	for _, file := range step.Files {
		if !strings.HasSuffix(file.Name, ".tf") && !strings.HasSuffix(file.Name, ".yaml") &&
			!strings.HasSuffix(file.Name, ".yml") && !strings.HasSuffix(file.Name, ".hcl") {
			modifiedStep.Files = append(modifiedStep.Files, model.File{
				Name:     strings.TrimPrefix(file.Name, fmt.Sprintf(IncludeFormat, step.Name)+"/"),
				Content:  file.Content,
				Checksum: util.CalculateHash(file.Content),
			})
			continue
		}
		newContent, delayedKeyTypes, err := u.replaceStringValues(step, string(file.Content), index, cache)
		if err != nil {
			return fmt.Errorf("failed to replace tags in file %s: %v", file.Name, err)
		}
		checksum := util.CalculateHash([]byte(newContent))
		newContent, err = u.replaceDelayedStringValues(step, newContent, index, cache, delayedKeyTypes)
		if err != nil {
			return fmt.Errorf("failed to replace delayed tags in file %s: %v", file.Name, err)
		}
		content := []byte(newContent)
		err = validateStepFile(file.Name, content)
		if err != nil {
			return err
		}
		modifiedStep.Files = append(modifiedStep.Files, model.File{
			Name:     strings.TrimPrefix(file.Name, fmt.Sprintf(IncludeFormat, step.Name)+"/"),
			Content:  content,
			Checksum: checksum,
		})
	}
	return nil
}

func validateStepFile(file string, content []byte) error {
	if strings.HasSuffix(file, ".tf") || strings.HasSuffix(file, ".hcl") {
		_, diags := hclwrite.ParseConfig(content, file, hcl.InitialPos)
		if diags != nil && diags.HasErrors() {
			slog.Debug(fmt.Sprintf("broken hcl %s:\n%s", file, string(content)))
			return fmt.Errorf("failed to parse hcl file %s: %v", file, diags.Errs())
		}
	} else if strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".yml") {
		var yamlContent map[string]interface{}
		err := yaml.Unmarshal(content, &yamlContent)
		if err != nil {
			slog.Debug(fmt.Sprintf("broken yaml %s:\n%s", file, string(content)))
			return fmt.Errorf("failed to unmarshal yaml file %s: %v", file, err)
		}
	}
	return nil
}

func (u *updater) replaceStringValues(step model.Step, content string, index int, cache paramCache) (string, map[string]keyType, error) {
	matches := replaceRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, nil, nil
	}
	delayedKeyTypes := make(map[string]keyType) // Delay replace tags that dynamically change, e.g. module version
	for _, match := range matches {
		replaceTag := match[0]
		replaceKey := match[1]
		if hasSamePrefixSuffix(replaceKey, "`") {
			content = strings.Replace(content, replaceTag, strings.Trim(replaceKey, "`"), 1)
			continue
		}
		keyTypes, err := parseReplaceTag(match)
		if err != nil {
			return "", nil, err
		}
		var replacement string
		for _, keyType := range keyTypes {
			if strings.HasPrefix(keyType.ReplaceKey, `"`) {
				replacement = strings.Trim(keyType.ReplaceKey, `"`)
				break
			}
			if keyType.ReplaceType == string(model.ReplaceTypeAgent) {
				delayedKeyTypes[replaceTag] = keyType
				continue
			}
			replacement, err = u.getReplacementValue(step, index, keyType.ReplaceKey, keyType.ReplaceType, cache)
			if err != nil {
				return "", nil, err
			}
			if replacement != "" {
				break
			}
		}
		content = strings.Replace(content, replaceTag, replacement, 1)
		if strings.HasPrefix(replacement, "module.") {
			content = strings.Replace(content, fmt.Sprintf(`"%s"`, replacement), replacement, 1)
		}
	}
	return content, delayedKeyTypes, nil
}

func (u *updater) replaceDelayedStringValues(step model.Step, content string, index int, cache paramCache, delayedKeyTypes map[string]keyType) (string, error) {
	for replaceTag, keyType := range delayedKeyTypes {
		replacement, err := u.getReplacementValue(step, index, keyType.ReplaceKey, keyType.ReplaceType, cache)
		if err != nil {
			return "", err
		}
		content = strings.Replace(content, replaceTag, replacement, 1)
	}
	return content, nil
}

func hasSamePrefixSuffix(s, prefixSuffix string) bool {
	return strings.HasPrefix(s, prefixSuffix) && strings.HasSuffix(s, prefixSuffix)
}

func (u *updater) getReplacementValue(step model.Step, index int, replaceKey, replaceType string, cache paramCache) (string, error) {
	switch replaceType {
	case string(model.ReplaceTypeOutput), string(model.ReplaceTypeGCSM), string(model.ReplaceTypeSSM):
		return u.getModuleParameter(step, replaceKey, cache, false)
	case string(model.ReplaceTypeOutputOptional):
		return u.getModuleParameter(step, replaceKey, cache, true)
	case string(model.ReplaceTypeOutputCustom), string(model.ReplaceTypeGCSMCustom), string(model.ReplaceTypeSSMCustom):
		return getSSMCustomParameter(u.resources.GetSSM(), replaceKey)
	case string(model.ReplaceTypeTOutput):
		return u.getTypedModuleParameter(step, replaceKey, cache, false)
	case string(model.ReplaceTypeTOutputOptional):
		return u.getTypedModuleParameter(step, replaceKey, cache, true)
	case string(model.ReplaceTypeConfig):
		return u.getReplacementConfigValue(replaceKey[strings.Index(replaceKey, ".")+1:])
	case string(model.ReplaceTypeAgent):
		return u.getReplacementAgentValue(replaceKey[strings.Index(replaceKey, ".")+1:], index)
	case string(model.ReplaceTypeModuleType):
		return u.getTypedModuleName(step, replaceKey)
	case string(model.ReplaceTypeStepModule):
		return getTypedStepModuleName(step, replaceKey)
	case string(model.ReplaceTypeModule):
		return "", nil // Ignore this replace
	default:
		return "", fmt.Errorf("unknown replace type in tag '%s'", replaceType)
	}
}

func (u *updater) getReplacementConfigValue(configKey string) (string, error) {
	if configKey == "prefix" {
		return u.resources.GetCloudPrefix(), nil
	}
	configValue, err := util.GetValueFromStruct(configKey, u.config)
	if err != nil {
		return "", fmt.Errorf("failed to get config value %s: %s", configKey, err)
	}
	return configValue, nil
}

func (u *updater) getReplacementAgentValue(key string, index int) (string, error) {
	parts := strings.Split(key, ".")
	if parts[0] == string(model.AgentReplaceTypeVersion) {
		_, referencedStep := findStep(parts[1], u.config.Steps)
		if referencedStep == nil {
			return "", fmt.Errorf("failed to find step %s", parts[1])
		}
		stepState := GetStepState(u.state, referencedStep.Name)
		referencedModule := getModule(parts[2], referencedStep.Modules)
		if referencedModule == nil {
			return "", fmt.Errorf("failed to find module %s in step %s", parts[2], parts[1])
		}
		moduleVersion, _, err := u.getModuleVersion(*referencedModule, stepState, index, model.ApproveNever)
		return moduleVersion, err
	} else if parts[0] == string(model.AgentReplaceTypeAccountId) {
		return u.resources.(aws.Resources).AccountId, nil
	}
	return "", fmt.Errorf("unknown agent replace type %s", parts[0])
}

func (u *updater) getModuleParameter(step model.Step, replaceKey string, cache paramCache, optional bool) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 4 {
		return "", fmt.Errorf("failed to parse ssm parameter key %s for step %s, got %d split parts instead of 4",
			replaceKey, step.Name, len(parts))
	}
	foundStep, module := u.findStepModuleByName(parts[1], parts[2])
	if foundStep == nil || module == nil {
		if optional {
			return "", nil
		}
		return "", fmt.Errorf("failed to find module %s in step %s for key %s", parts[1], parts[2], replaceKey)
	}
	match := parameterIndexRegex.FindStringSubmatch(parts[3])
	return u.getParameter(match, replaceKey, step, *foundStep, *module, cache, optional)
}

func getSSMCustomParameter(ssm model.SSM, replaceKey string) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("failed to parse ssm custom parameter key %s, got %d split parts instead of 2", replaceKey, len(parts))
	}
	match := parameterIndexRegex.FindStringSubmatch(parts[1])
	return getSSMParameterValue(ssm, match, replaceKey, match[1])
}

func (u *updater) getTypedModuleParameter(step model.Step, replaceKey string, cache paramCache, optional bool) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("failed to parse toutput key %s for step %s, got %d split parts instead of 3",
			replaceKey, step.Name, len(parts))
	}
	foundStep, module, err := u.findStepModuleByType(parts[1])
	if err != nil {
		return "", fmt.Errorf("failed to find step and module for toutput key %s: %s", replaceKey, err)
	}
	if foundStep == nil || module == nil {
		if optional {
			return "", nil
		} else {
			return "", fmt.Errorf("failed to find module with type %s for toutput key %s", parts[1], replaceKey)
		}
	}
	match := parameterIndexRegex.FindStringSubmatch(parts[2])
	return u.getParameter(match, replaceKey, step, *foundStep, *module, cache, optional)
}

func (u *updater) getParameter(match []string, replaceKey string, step, foundStep model.Step, module model.Module, cache paramCache, optional bool) (string, error) {
	if step.Type == model.StepTypeTerraform && step.Name == foundStep.Name {
		return fmt.Sprintf("module.%s.%s", module.Name, match[1]), nil
	}
	outputs, err := u.getModuleOutputs(foundStep, cache)
	if err != nil {
		return "", err
	}
	if len(outputs) > 0 {
		key := fmt.Sprintf("%s__%s", module.Name, strings.Replace(match[1], "/", "_", -1))
		output, found := outputs[key]
		if found {
			return getOutputValue(output, replaceKey, match)
		}
		slog.Debug(fmt.Sprintf("step %s key %s not found in tf output", foundStep.Name, key))
	}
	parameterName := fmt.Sprintf("%s/%s-%s-%s/%s", ssmPrefix, u.resources.GetCloudPrefix(), foundStep.Name, module.Name, match[1])
	prefix, found := module.Inputs["prefix"]
	if found {
		parameterName = fmt.Sprintf("%s/%s/%s", ssmPrefix, prefix, match[1])
	}
	value, err := getSSMParameterValue(u.resources.GetSSM(), match, replaceKey, parameterName)
	if err != nil {
		var parameterError *model.ParameterNotFoundError
		if optional && errors.As(err, &parameterError) {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

func getOutputValue(output tfOutput, replaceKey string, match []string) (string, error) {
	switch v := output.Value.(type) {
	case string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, bool:
		if match[2] != "" {
			return "", fmt.Errorf("output %s is not a list, but an index was given", replaceKey)
		}
		return strings.Trim(getStringValue(v), "\""), nil
	case []interface{}:
		values := make([]string, 0)
		for _, value := range v {
			values = append(values, getStringValue(value))
		}
		if match[2] == "" {
			return strings.Join(values, ","), nil
		}
		return getSSMParameterValueFromList(match, values, replaceKey, match[1])
	case map[string]interface{}:
		slog.Warn(common.PrefixWarning(fmt.Sprintf("tf output %s is a map, returning as json", replaceKey)))
		bytes, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(bytes), nil
	}
	return "", fmt.Errorf("unsupported type: %s", reflect.TypeOf(output.Value))
}

func getStringValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return fmt.Sprintf(`"%s"`, v)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v)
	case float32, float64:
		return fmt.Sprintf("%f", v)
	case bool:
		return fmt.Sprintf("%t", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func (u *updater) getModuleOutputs(step model.Step, cache paramCache) (map[string]tfOutput, error) {
	filePath := fmt.Sprintf("%s-%s/%s", u.resources.GetCloudPrefix(), step.Name, terraformOutput)
	outputs, found := cache[filePath]
	if found {
		return outputs, nil
	}
	file, err := u.resources.GetBucket().GetFile(filePath)
	if err != nil {
		return nil, err
	}
	if file == nil {
		slog.Debug(fmt.Sprintf("terraform output file %s not found", filePath))
		cache[filePath] = make(map[string]tfOutput)
		return cache[filePath], nil
	}
	err = json.Unmarshal(file, &outputs)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal terraform tfOutput file %s: %s", filePath, err)
	}
	cache[filePath] = outputs
	return outputs, nil
}

func getSSMParameterValue(ssm model.SSM, match []string, replaceKey string, parameterName string) (string, error) {
	parameter, err := ssm.GetParameter(parameterName)
	if err != nil {
		return "", err
	}
	if match[2] == "" {
		return *parameter.Value, nil
	}
	if parameter.Type != string(ssmTypes.ParameterTypeStringList) && parameter.Type != "" {
		return "", fmt.Errorf("parameter index was given, but ssm parameter \"%s\" is not a string list", match[1])
	}
	return getSSMParameterValueFromList(match, strings.Split(*parameter.Value, ","), replaceKey, match[1])
}

func getSSMParameterValueFromList(match []string, values []string, replaceKey string, parameterName string) (string, error) {
	start, err := strconv.Atoi(match[3])
	if err != nil {
		return "", fmt.Errorf("failed to parse start index %s of parameter %s: %s", match[3], replaceKey, err)
	}
	if start+1 > len(values) {
		return "", fmt.Errorf("start index %d of parameter %s is out of range", start, parameterName)
	}
	if match[5] == "" {
		return strings.Trim(values[start], "\""), nil
	}
	end, err := strconv.Atoi(match[5])
	if err != nil {
		return "", fmt.Errorf("failed to parse end index %s of parameter %s: %s", match[5], replaceKey, err)
	}
	if end+1 > len(values) {
		return "", fmt.Errorf("end index %d of parameter %s is out of range", end, parameterName)
	}
	return strings.Join(values[start:end+1], ","), nil
}

func (u *updater) getTypedModuleName(step model.Step, replaceKey string) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("failed to parse tmodule key %s for step %s, got %d split parts instead of 2",
			replaceKey, step.Name, len(parts))
	}
	_, module, err := u.findStepModuleByType(parts[1])
	if err != nil || module == nil {
		return "", fmt.Errorf("failed to find step and module for tmodule key %s: %v", replaceKey, err)
	}
	return module.Name, nil
}

func getTypedStepModuleName(step model.Step, replaceKey string) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("failed to parse tsmodule key %s for step %s, got %d split parts instead of 2",
			replaceKey, step.Name, len(parts))
	}
	var module *model.Module
	for _, stepModule := range step.Modules {
		moduleType := getModuleType(stepModule)
		if moduleType != parts[1] {
			continue
		}
		if module != nil {
			return "", fmt.Errorf("found multiple step modules with type %s for tsmodule key %s", parts[1], replaceKey)
		}
		module = &stepModule
	}
	if module == nil {
		return "", fmt.Errorf("failed to find step module for tsmodule key %s", replaceKey)
	}
	return module.Name, nil
}

func (u *updater) findStepModuleByName(stepName, moduleName string) (*model.Step, *model.Module) {
	for _, step := range u.config.Steps {
		if step.Name != stepName {
			continue
		}
		for _, module := range step.Modules {
			if module.Name == moduleName {
				return &step, &module
			}
		}
	}
	return nil, nil
}

func (u *updater) findStepModuleByType(moduleType string) (*model.Step, *model.Module, error) {
	var foundStep *model.Step
	var foundModule *model.Module
	for _, step := range u.config.Steps {
		for _, module := range step.Modules {
			currentType := getModuleType(module)
			if currentType != moduleType {
				continue
			}
			if foundStep != nil {
				return nil, nil, fmt.Errorf("found multiple modules with type %s", moduleType)
			}
			foundStep = &step
			foundModule = &module
		}
	}
	return foundStep, foundModule, nil
}

func getModuleType(module model.Module) string {
	moduleSource := module.Source
	if util.IsClientModule(module) {
		moduleSource = moduleSource[strings.LastIndex(moduleSource, "//")+2:]
	}
	return moduleSource[strings.Index(module.Source, "/")+1:]
}

func replaceConfigValues(ssm model.SSM, prefix string, config model.Config) model.Config {
	if ssm == nil {
		return config
	}
	steps := config.Steps
	config.Steps = nil
	config = replaceConfigRootValues(ssm, prefix, config)
	steps = replaceConfigStepsValues(prefix, steps)
	config.Steps = steps
	return config
}

func replaceConfigRootValues(ssm model.SSM, prefix string, config model.Config) model.Config {
	configYaml, err := yaml.Marshal(config)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	matches := replaceRegex.FindAllStringSubmatch(string(configYaml), -1)
	if len(matches) == 0 {
		return config
	}
	modifiedConfigYaml, err := replaceConfigTags(prefix, config, string(configYaml), matches)
	if err != nil {
		log.Fatalf("Failed to replace tags in config root, error: %v", err)
	}
	modifiedConfigYaml, err = replaceConfigCustomTags(ssm, modifiedConfigYaml, matches)
	if err != nil {
		log.Fatalf("Failed to replace custom output tags in config root, error: %v", err)
	}
	err = yaml.Unmarshal([]byte(modifiedConfigYaml), &config)
	if err != nil {
		log.Fatalf("Failed to unmarshal modified config: %s", err)
	}
	return config
}

func replaceConfigStepsValues(prefix string, steps []model.Step) []model.Step {
	stepsYaml, err := yaml.Marshal(steps)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	matches := replaceRegex.FindAllStringSubmatch(string(stepsYaml), -1)
	if len(matches) == 0 {
		return steps
	}
	modifiedStepsYaml, err := replaceConfigTags(prefix, model.Config{}, string(stepsYaml), matches)
	if err != nil {
		log.Fatalf("Failed to replace config tags in steps, error: %s", err.Error())
	}
	err = yaml.Unmarshal([]byte(modifiedStepsYaml), &steps)
	if err != nil {
		log.Fatalf("Failed to unmarshal modified steps: %s", err)
	}
	return steps
}

func replaceConfigTags(prefix string, config model.Config, content string, matches [][]string) (string, error) {
	for _, match := range matches {
		replaceTag := match[0]
		replaceKey := match[1]
		if hasSamePrefixSuffix(replaceKey, "`") {
			continue
		}
		keyTypes, err := parseReplaceTag(match)
		if err != nil {
			return "", err
		}
		replaceKey = ""
		for _, keyType := range keyTypes {
			if keyType.ReplaceType == string(model.ReplaceTypeConfig) {
				replaceKey = keyType.ReplaceKey
				break
			}
		}
		if replaceKey == "" {
			continue
		}
		configKey := replaceKey[strings.Index(replaceKey, ".")+1:]
		if configKey == "prefix" {
			content = strings.Replace(content, replaceTag, prefix, 1)
			continue
		}
		configValue, err := util.GetValueFromStruct(configKey, config)
		if err != nil {
			return "", fmt.Errorf("failed to get config value %s: %s", configKey, err)
		}
		content = strings.Replace(content, replaceTag, configValue, 1)
	}
	return content, nil
}

func replaceConfigCustomTags(ssm model.SSM, content string, matches [][]string) (string, error) {
	for _, match := range matches {
		replaceTag := match[0]
		replaceKey := match[1]
		if hasSamePrefixSuffix(replaceKey, "`") {
			continue
		}
		keyTypes, err := parseReplaceTag(match)
		if err != nil {
			return "", err
		}
		replaceKey = ""
		for _, keyType := range keyTypes {
			if keyType.ReplaceType == string(model.ReplaceTypeSSMCustom) || keyType.ReplaceType == string(model.ReplaceTypeGCSMCustom) ||
				keyType.ReplaceType == string(model.ReplaceTypeOutputCustom) {
				replaceKey = keyType.ReplaceKey
				break
			}
		}
		if replaceKey == "" {
			continue
		}
		parameter, err := getSSMCustomParameter(ssm, replaceKey)
		if err != nil {
			return "", err
		}
		content = strings.Replace(content, replaceTag, parameter, 1)
	}
	return content, nil
}

func replaceModuleValues(module model.Module, inputs map[string]interface{}) (map[string]interface{}, error) {
	if inputs == nil {
		return nil, nil
	}
	inputsYaml, err := yaml.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to convert module inputs %s to yaml, error: %v", module.Name, err)
	}
	content := string(inputsYaml)
	matches := replaceRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return inputs, nil
	}
	content, err = replaceModuleInputsValues(module, content, matches)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal([]byte(content), &inputs)
	if err != nil {
		slog.Debug(fmt.Sprintf("broken module inputs yaml %s:\n%s", module.Name, content))
		return nil, fmt.Errorf("failed to unmarshal modified module inputs %s yaml, error: %v", module.Name, err)
	}
	return inputs, nil
}

func replaceModuleInputsValues(module model.Module, content string, matches [][]string) (string, error) {
	for _, match := range matches {
		replaceTag := match[0]
		replaceKey := match[1]
		if hasSamePrefixSuffix(replaceKey, "`") {
			continue
		}
		keyTypes, err := parseReplaceTag(match)
		if err != nil {
			return content, err
		}
		replaceKey = ""
		for _, keyType := range keyTypes {
			if keyType.ReplaceType == string(model.ReplaceTypeModule) {
				replaceKey = keyType.ReplaceKey
				break
			}
		}
		if replaceKey == "" {
			continue
		}
		replacement, err := getReplacementModuleValue(replaceKey, module)
		if err != nil {
			return content, err
		}
		if replacement == "" {
			continue
		}
		content = strings.Replace(content, replaceTag, replacement, 1)
	}
	return content, nil
}

func getReplacementModuleValue(replaceKey string, module model.Module) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("failed to parse module key %s for module %s, got %d split parts instead of 2",
			replaceKey, module.Name, len(parts))
	}
	if parts[1] == "name" {
		return module.Name, nil
	} else if parts[1] == "source" {
		return module.Source, nil
	}
	return "", fmt.Errorf("unknown module replace type %s in tag %s", parts[1], replaceKey)
}

func parseReplaceTag(match []string) ([]keyType, error) {
	if len(match) != 2 {
		return nil, fmt.Errorf("failed to parse replace tag match %s", match[0])
	}
	replaceTags := strings.Split(match[1], "|")
	var keyTypes []keyType
	for _, tag := range replaceTags {
		replaceKey := strings.TrimLeft(strings.Trim(tag, " "), ".")
		if strings.HasPrefix(replaceKey, `"`) {
			keyTypes = append(keyTypes, keyType{ReplaceKey: replaceKey, ReplaceType: ""})
			continue
		}
		splitIndex := strings.Index(replaceKey, ".")
		if splitIndex == -1 || len(replaceKey) <= splitIndex {
			return nil, fmt.Errorf("invalid replace tag format: %s", match[0])
		}
		replaceType := strings.ToLower(replaceKey[:strings.Index(replaceKey, ".")])
		keyTypes = append(keyTypes, keyType{ReplaceKey: replaceKey, ReplaceType: replaceType})
	}
	return keyTypes, nil
}
