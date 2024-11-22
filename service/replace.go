package service

import (
	"encoding/json"
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

const (
	terraformOutput = "terraform-output.json"
)

var replaceRegex = regexp.MustCompile(`{{(.*?)}}`)

func (u *updater) replaceConfigStepValues(step model.Step, index int) (model.Step, error) {
	stepYaml, err := yaml.Marshal(step)
	if err != nil {
		return step, fmt.Errorf("failed to convert step %s to yaml, error: %v", step.Name, err)
	}
	cache := make(paramCache)
	modifiedStepYaml, err := u.replaceStringValues(step, string(stepYaml), index, cache)
	if err != nil {
		log.Printf("Failed to replace tags in step %s", step.Name)
		return step, err
	}
	var modifiedStep model.Step
	err = yaml.Unmarshal([]byte(modifiedStepYaml), &modifiedStep)
	if err != nil {
		return step, fmt.Errorf("failed to unmarshal modified step %s yaml, error: %v", step.Name, err)
	}
	if step.Files == nil {
		return modifiedStep, nil
	}
	for _, file := range step.Files {
		if !strings.HasSuffix(file.Name, ".tf") && !strings.HasSuffix(file.Name, ".yaml") &&
			!strings.HasSuffix(file.Name, ".yml") && !strings.HasSuffix(file.Name, ".hcl") {
			modifiedStep.Files = append(modifiedStep.Files, model.File{
				Name:    strings.TrimPrefix(file.Name, fmt.Sprintf(IncludeFormat, step.Name)+"/"),
				Content: file.Content,
			})
			continue
		}
		newContent, err := u.replaceStringValues(step, string(file.Content), index, cache)
		content := []byte(newContent)
		if err != nil {
			return modifiedStep, fmt.Errorf("failed to replace tags in file %s: %v", file.Name, err)
		}
		err = validateStepFile(file.Name, content)
		if err != nil {
			return modifiedStep, err
		}
		modifiedStep.Files = append(modifiedStep.Files, model.File{
			Name:    strings.TrimPrefix(file.Name, fmt.Sprintf(IncludeFormat, step.Name)+"/"),
			Content: content,
		})
	}
	return modifiedStep, nil
}

func validateStepFile(file string, content []byte) error {
	if strings.HasSuffix(file, ".tf") || strings.HasSuffix(file, ".hcl") {
		_, diags := hclwrite.ParseConfig(content, file, hcl.InitialPos)
		if diags.HasErrors() {
			return fmt.Errorf("failed to parse hcl file %s: %v", file, diags.Errs())
		}
	} else if strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".yml") {
		var yamlContent map[string]interface{}
		err := yaml.Unmarshal(content, &yamlContent)
		if err != nil {
			return fmt.Errorf("failed to unmarshal yaml file %s: %v", file, err)
		}
	}
	return nil
}

func (u *updater) replaceStringValues(step model.Step, content string, index int, cache paramCache) (string, error) {
	matches := replaceRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, nil
	}
	for _, match := range matches {
		if len(match) != 2 {
			return "", fmt.Errorf("failed to parse replace tag match %s", match[0])
		}
		replaceTag := match[0]
		replaceKey := strings.TrimLeft(strings.Trim(match[1], " "), ".")
		replaceType := strings.ToLower(replaceKey[:strings.Index(replaceKey, ".")])
		switch replaceType {
		case string(model.ReplaceTypeOutput):
			fallthrough
		case string(model.ReplaceTypeGCSM):
			fallthrough
		case string(model.ReplaceTypeSSM):
			parameter, err := u.getModuleParameter(step, replaceKey, cache)
			if err != nil {
				return "", err
			}
			content = strings.Replace(content, replaceTag, parameter, 1)
		case string(model.ReplaceTypeOutputCustom):
			fallthrough
		case string(model.ReplaceTypeGCSMCustom):
			fallthrough
		case string(model.ReplaceTypeSSMCustom):
			parameter, err := u.getSSMCustomParameter(replaceKey)
			if err != nil {
				return "", err
			}
			content = strings.Replace(content, replaceTag, parameter, 1)
		case string(model.ReplaceTypeTOutput):
			parameter, err := u.getTypedModuleParameter(step, replaceKey, cache)
			if err != nil {
				return "", err
			}
			content = strings.Replace(content, replaceTag, parameter, 1)
		case string(model.ReplaceTypeConfig):
			configKey := replaceKey[strings.Index(replaceKey, ".")+1:]
			if configKey == "prefix" {
				content = strings.Replace(content, replaceTag, u.resources.GetCloudPrefix(), 1)
				break
			}
			configValue, err := util.GetValueFromStruct(configKey, u.config)
			if err != nil {
				return "", fmt.Errorf("failed to get config value %s: %s", configKey, err)
			}
			content = strings.Replace(content, replaceTag, configValue, 1)
		case string(model.ReplaceTypeAgent):
			key := replaceKey[strings.Index(replaceKey, ".")+1:]
			agentValue, err := u.getReplacementAgentValue(key, index)
			if err != nil {
				return "", fmt.Errorf("failed to get agent value %s: %s", key, err)
			}
			content = strings.Replace(content, replaceTag, agentValue, 1)
		case string(model.ReplaceTypeModule):
			moduleName, err := u.getTypedModuleName(step, replaceKey)
			if err != nil {
				return "", err
			}
			content = strings.Replace(content, replaceTag, moduleName, 1)
		default:
			return "", fmt.Errorf("unknown replace type in tag %s", match[0])
		}
	}
	return content, nil
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

func (u *updater) getModuleParameter(step model.Step, replaceKey string, cache paramCache) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 4 {
		return "", fmt.Errorf("failed to parse ssm parameter key %s for step %s, got %d split parts instead of 4",
			replaceKey, step.Name, len(parts))
	}
	foundStep, module, err := u.findStepModuleByName(parts[1], parts[2])
	if err != nil {
		return "", fmt.Errorf("failed to find step and module for ssm parameter key %s: %s", replaceKey, err)
	}
	match := parameterIndexRegex.FindStringSubmatch(parts[3])
	return u.getParameter(match, replaceKey, step, foundStep, module, cache)
}

func (u *updater) getSSMCustomParameter(replaceKey string) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("failed to parse ssm custom parameter key %s, got %d split parts instead of 2", replaceKey, len(parts))
	}
	match := parameterIndexRegex.FindStringSubmatch(parts[1])
	return u.getSSMParameterValue(match, replaceKey, match[1])
}

func (u *updater) getTypedModuleParameter(step model.Step, replaceKey string, cache paramCache) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("failed to parse toutput key %s for step %s, got %d split parts instead of 3",
			replaceKey, step.Name, len(parts))
	}
	foundStep, module, err := u.findStepModuleByType(parts[1])
	if err != nil {
		return "", fmt.Errorf("failed to find step and module for toutput key %s: %s", replaceKey, err)
	}
	match := parameterIndexRegex.FindStringSubmatch(parts[2])
	return u.getParameter(match, replaceKey, step, foundStep, module, cache)
}

func (u *updater) getParameter(match []string, replaceKey string, step, foundStep model.Step, module model.Module, cache paramCache) (string, error) {
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
	return u.getSSMParameterValue(match, replaceKey, parameterName)
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
		slog.Warn(fmt.Sprintf("tf output %s is a map, returning as json", replaceKey))
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

func (u *updater) getSSMParameterValue(match []string, replaceKey string, parameterName string) (string, error) {
	parameter, err := u.resources.GetSSM().GetParameter(parameterName)
	if err != nil {
		return "", fmt.Errorf("ssm parameter %s %s", parameterName, err)
	}
	if match[2] == "" {
		return *parameter.Value, nil
	}
	if parameter.Type != string(ssmTypes.ParameterTypeStringList) && parameter.Type != "" {
		return "", fmt.Errorf("parameter index was given, but ssm parameter %s is not a string list", match[1])
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
	if err != nil {
		return "", fmt.Errorf("failed to find step and module for tmodule key %s: %s", replaceKey, err)
	}
	return module.Name, nil
}

func (u *updater) findStepModuleByName(stepName, moduleName string) (model.Step, model.Module, error) {
	for _, step := range u.config.Steps {
		if step.Name != stepName {
			continue
		}
		for _, module := range step.Modules {
			if module.Name == moduleName {
				return step, module, nil
			}
		}
	}
	return model.Step{}, model.Module{}, fmt.Errorf("failed to find module %s in step %s", moduleName, stepName)
}

func (u *updater) findStepModuleByType(moduleType string) (model.Step, model.Module, error) {
	var foundStep *model.Step
	var foundModule model.Module
	for _, step := range u.config.Steps {
		for _, module := range step.Modules {
			moduleSource := module.Source
			if util.IsClientModule(module) {
				moduleSource = moduleSource[strings.LastIndex(moduleSource, "//")+2:]
			}
			currentType := moduleSource[strings.Index(module.Source, "/")+1:]
			if currentType != moduleType {
				continue
			}
			if foundStep != nil {
				return model.Step{}, model.Module{}, fmt.Errorf("found multiple modules with type %s", moduleType)
			}
			foundStep = &step
			foundModule = module
		}
	}
	if foundStep == nil {
		return model.Step{}, model.Module{}, fmt.Errorf("no module found with type %s", moduleType)
	}
	return *foundStep, foundModule, nil
}

func replaceConfigValues(prefix string, config *model.Config) {
	configYaml, err := yaml.Marshal(config)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	modifiedConfigYaml, err := replaceConfigTags(prefix, *config, string(configYaml))
	if err != nil {
		log.Fatalf("Failed to replace tags in config")
	}
	err = yaml.Unmarshal([]byte(modifiedConfigYaml), config)
	if err != nil {
		log.Fatalf("Failed to unmarshal modified config: %s", err)
	}
}

func replaceConfigTags(prefix string, config model.Config, content string) (string, error) {
	matches := replaceRegex.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, nil
	}
	for _, match := range matches {
		if len(match) != 2 {
			return "", fmt.Errorf("failed to parse replace tag match %s", match[0])
		}
		replaceTag := match[0]
		replaceKey := strings.TrimLeft(strings.Trim(match[1], " "), ".")
		replaceType := strings.ToLower(replaceKey[:strings.Index(replaceKey, ".")])
		if replaceType != string(model.ReplaceTypeConfig) {
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