package terraform

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"log"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	providerPath   = "providers"
	moduleTemplate = "modules/%s"
	baseFile       = "base.tf"
	base           = "base"
	versionsFile   = "versions.tf"
	awsTagsRegex   = `(\w+)\s*=\s*"([^"]+)"`
	outputType     = "output"
)

var planRegex = regexp.MustCompile(`Plan: (\d+) to add, (\d+) to change, (\d+) to destroy`)

type Terraform interface {
	GetTerraformProvider(step model.Step, moduleVersions map[string]model.ModuleVersion, sourceVersions map[string]string) ([]byte, map[string]model.Set[string], error)
	AddModule(prefix string, body *hclwrite.Body, step model.Step, module model.Module, moduleVersion model.ModuleVersion) error
}

type terraform struct {
	providerType  model.ProviderType
	configSources []model.ConfigSource
	sources       map[string]*model.Source
}

func NewTerraform(providerType model.ProviderType, configSources []model.ConfigSource, sources map[string]*model.Source) Terraform {
	return &terraform{
		providerType:  providerType,
		configSources: configSources,
		sources:       sources,
	}
}

func (t *terraform) GetTerraformProvider(step model.Step, moduleVersions map[string]model.ModuleVersion, sourceVersions map[string]string) ([]byte, map[string]model.Set[string], error) {
	file, baseSource, err := t.findProviderFile(providerPath, baseFile, sourceVersions)
	if err != nil {
		return nil, nil, err
	}
	t.modifyBackendType(file.Body())
	providersAttributes, err := t.getProvidersAttributes(step, moduleVersions)
	if err != nil {
		return nil, nil, err
	}
	providersBlock, err := getRequiredProvidersBlock(file)
	if err != nil {
		return nil, nil, err
	}
	baseBody := file.Body()
	providers := make(map[string]model.Set[string])
	providers[baseSource] = model.ToSet([]string{base})
	attrProviders, err := t.addProviderAttributes(baseBody, providersBlock, providersAttributes, step, sourceVersions)
	if err != nil {
		return nil, nil, err
	}
	for provider, providerSource := range attrProviders {
		if providers[providerSource] == nil {
			providers[providerSource] = model.NewSet[string]()
		}
		providers[providerSource].Add(provider)
	}
	return hclwrite.Format(file.Bytes()), providers, nil
}

func (t *terraform) modifyBackendType(body *hclwrite.Body) {
	terraformBlock := body.FirstMatchingBlock("terraform", []string{})
	if terraformBlock == nil {
		return
	}
	backendBlock := terraformBlock.Body().FirstMatchingBlock("backend", []string{"TYPE"})
	if backendBlock == nil {
		return
	}
	if t.providerType == model.AWS {
		backendBlock.SetLabels([]string{"s3"})
	} else if t.providerType == model.GCLOUD {
		backendBlock.SetLabels([]string{"gcs"})
	}
}

func (t *terraform) getProvidersAttributes(step model.Step, moduleVersions map[string]model.ModuleVersion) (map[string]*hclwrite.Attribute, error) {
	providersAttributes := make(map[string]*hclwrite.Attribute)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		providerAttributes, err := t.getProviderAttributes(module, moduleVersions[module.Name].Version,
			moduleVersions[module.Name].SourceURL)
		if err != nil {
			return nil, err
		}
		for name, attribute := range providerAttributes {
			providersAttributes[name] = attribute
		}
	}
	return providersAttributes, nil
}

func (t *terraform) findProviderFile(path string, fileName string, sourceVersions map[string]string) (*hclwrite.File, string, error) {
	sourceURL := ""
	release := ""
	for _, configSource := range t.configSources {
		source := t.sources[configSource.URL]
		if util.IsLocalSource(configSource.URL) && source.Storage.FileExists(filepath.Join(path, fileName), "") {
			sourceURL = source.URL
			release = source.ForcedVersion
			break
		}
		sourceVersion := sourceVersions[source.URL]
		if sourceVersion == "" {
			continue
		}
		if !source.Storage.FileExists(filepath.Join(path, fileName), sourceVersion) {
			continue
		}
		sourceURL = source.URL
		release = sourceVersion
	}
	if sourceURL == "" {
		return nil, "", model.NewFileNotFoundError(fileName)
	}
	file, err := t.getTerraformFile(sourceURL, path, fileName, release)
	if err != nil {
		return nil, "", err
	}
	return file, sourceURL, nil
}

func (t *terraform) getTerraformFile(sourceURL, filePath, fileName, release string) (*hclwrite.File, error) {
	source := t.sources[sourceURL]
	if source == nil {
		return nil, fmt.Errorf("tf source %s not found", sourceURL)
	}
	rawFile, err := source.Storage.GetFile(fmt.Sprintf("%s/%s", filePath, fileName), release)
	if err != nil {
		return nil, err
	}
	file, err := UnmarshalTerraformFile(fileName, rawFile)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (t *terraform) getProviderAttributes(module model.Module, version, sourceURL string) (map[string]*hclwrite.Attribute, error) {
	file, err := t.getTerraformFile(sourceURL, fmt.Sprintf(moduleTemplate, module.Source), versionsFile, version)
	if err != nil {
		return nil, err
	}
	block, err := getRequiredProvidersBlock(file)
	if err != nil {
		return nil, err
	}
	return block.Body().Attributes(), nil
}

func getRequiredProvidersBlock(file *hclwrite.File) (*hclwrite.Block, error) {
	terraformBlock := file.Body().FirstMatchingBlock("terraform", []string{})
	if terraformBlock == nil {
		return nil, errors.New("terraform block not found")
	}
	providersBlock := terraformBlock.Body().FirstMatchingBlock("required_providers", []string{})
	if providersBlock == nil {
		return nil, errors.New("required_providers block not found")
	}
	return providersBlock, nil
}

func (t *terraform) getProviderBlocks(providerName string, sourceVersions map[string]string) ([]*hclwrite.Block, string) {
	providerFile, providerSource, err := t.findProviderFile(providerPath, fmt.Sprintf("%s.tf", providerName), sourceVersions)
	if err != nil {
		var fileNotFoundError model.FileNotFoundError
		if errors.As(err, &fileNotFoundError) {
			slog.Debug(fmt.Sprintf("Provider file not found for %s\n", providerName))
			return []*hclwrite.Block{}, ""
		}
		return nil, ""
	}
	return providerFile.Body().Blocks(), providerSource
}

func (t *terraform) addProviderAttributes(baseBody *hclwrite.Body, providersBlock *hclwrite.Block, providersAttributes map[string]*hclwrite.Attribute, step model.Step, sourceVersions map[string]string) (map[string]string, error) {
	providerInputs := step.Provider.Inputs
	if providerInputs == nil {
		providerInputs = make(map[string]interface{})
	}
	providers := make(map[string]string)
	keys := make([]string, 0, len(providersAttributes))
	for key := range providersAttributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, name := range keys {
		attribute := providersAttributes[name]
		providersBlock.Body().SetAttributeRaw(name, attribute.Expr().BuildTokens(nil))
		providerBlocks, providerSource := t.getProviderBlocks(name, sourceVersions)
		if len(providerBlocks) != 0 {
			providers[name] = providerSource
		}
		for _, providerBlock := range providerBlocks {
			err := addProviderValues(name, providerBlock.Body(), step.Provider)
			if err != nil {
				return nil, err
			}
			addProviderInputs(providerInputs, providerBlock)
			baseBody.AppendBlock(providerBlock)
		}
	}
	if len(providerInputs) > 0 {
		common.PrintWarning(fmt.Sprintf("WARNING! Unknown provider inputs: %v for step %s", providerInputs, step.Name))
	}
	return providers, nil
}

func addProviderValues(providerType string, body *hclwrite.Body, stepProvider model.Provider) error {
	if stepProvider.IsEmpty() {
		return nil
	}
	switch providerType {
	case "aws":
		return addAwsProviderValues(body, stepProvider.Aws)
	case "kubernetes":
		return addKubernetesProviderValues(body, stepProvider.Kubernetes)
	}
	return nil
}

func addAwsProviderValues(body *hclwrite.Body, awsProvider model.AwsProvider) error {
	if awsProvider.IsEmpty() {
		return nil
	}
	err := addAwsProviderIgnoreTags(body, awsProvider.IgnoreTags)
	if err != nil {
		return err
	}
	return addAwsProviderDefaultTags(body, awsProvider.DefaultTags)
}

func addAwsProviderIgnoreTags(body *hclwrite.Body, ignoreTags model.AwsIgnoreTags) error {
	if ignoreTags.IsEmpty() {
		return nil
	}
	ignoreTagsBlock := body.FirstMatchingBlock("ignore_tags", []string{})
	if ignoreTagsBlock == nil {
		ignoreTagsBlock = hclwrite.NewBlock("ignore_tags", []string{})
	} else {
		body.RemoveBlock(ignoreTagsBlock)
	}
	ignoreTagsBody := ignoreTagsBlock.Body()
	err := addProviderBodyArray(ignoreTagsBody, "key_prefixes", ignoreTags.KeyPrefixes)
	if err != nil {
		return err
	}
	err = addProviderBodyArray(ignoreTagsBody, "keys", ignoreTags.Keys)
	if err != nil {
		return err
	}
	body.AppendBlock(ignoreTagsBlock)
	return nil
}

func addAwsProviderDefaultTags(body *hclwrite.Body, defaultTags model.AwsDefaultTags) error {
	if defaultTags.IsEmpty() {
		return nil
	}
	defaultTagsBlock := body.FirstMatchingBlock("default_tags", []string{})
	if defaultTagsBlock == nil {
		defaultTagsBlock = hclwrite.NewBlock("default_tags", []string{})
	} else {
		body.RemoveBlock(defaultTagsBlock)
	}
	defaultTagsBody := defaultTagsBlock.Body()
	tagsAttr, found := defaultTagsBody.Attributes()["tags"]
	tags := make(map[string]string)
	if !found {
		tags = defaultTags.Tags
	} else {
		tokens := tagsAttr.Expr().BuildTokens(nil)
		re := regexp.MustCompile(awsTagsRegex)
		matches := re.FindAllStringSubmatch(string(tokens.Bytes()), -1)
		for _, match := range matches {
			tags[match[1]] = match[2]
		}
		for key, value := range defaultTags.Tags {
			tags[key] = value
		}
	}
	pairs, err := util.CreateKeyValuePairs(tags, "{\n", "}")
	if err != nil {
		return err
	}
	defaultTagsBody.SetAttributeRaw("tags", getBytesTokens(pairs))
	body.AppendBlock(defaultTagsBlock)
	return nil
}

func addKubernetesProviderValues(body *hclwrite.Body, kubernetes model.KubernetesProvider) error {
	err := addProviderBodyArray(body, "ignore_annotations", kubernetes.IgnoreAnnotations)
	if err != nil {
		return err
	}
	return addProviderBodyArray(body, "ignore_labels", kubernetes.IgnoreLabels)
}

func addProviderBodyArray(body *hclwrite.Body, attributeName string, values []string) error {
	if len(values) == 0 {
		return nil
	}
	attribute, found := body.Attributes()[attributeName]
	var tags []string
	if !found {
		tags = values
	} else {
		tokens := attribute.Expr().BuildTokens(nil)
		err := json.Unmarshal(tokens.Bytes(), &tags)
		if err != nil {
			return err
		}
		tags = append(tags, values...)
	}
	bytes, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	body.SetAttributeRaw(attributeName, getBytesTokens(bytes))
	return nil
}

func addProviderInputs(providerInputs map[string]interface{}, providerBlock *hclwrite.Block) {
	if providerBlock.Type() != "variable" {
		return
	}
	value, found := providerInputs[providerBlock.Labels()[0]]
	if found {
		if valueString, ok := value.(string); ok {
			if strings.Contains(valueString, "\n") {
				valueString = strings.TrimRight(valueString, "\n")
			}
			value = fmt.Sprintf("\"%s\"", valueString)
		}
		providerBlock.Body().SetAttributeRaw("default", getTokens(value))
		delete(providerInputs, providerBlock.Labels()[0])
	}
}

func (t *terraform) AddModule(prefix string, body *hclwrite.Body, step model.Step, module model.Module, moduleVersion model.ModuleVersion) error {
	newModule := body.AppendNewBlock("module", []string{module.Name})
	moduleBody := newModule.Body()
	if util.IsClientModule(module) {
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("%s?ref=%s", module.Source, moduleVersion.Version)))
	} else if util.IsLocalSource(moduleVersion.SourceURL) {
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("%s/modules/%s", moduleVersion.SourceURL, module.Source)))
	} else {
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("git::%s.git//modules/%s?ref=%s", moduleVersion.SourceURL, module.Source,
				moduleVersion.Version)))
	}
	moduleBody.SetAttributeValue("prefix", cty.StringVal(fmt.Sprintf("%s-%s-%s", prefix, step.Name, module.Name)))
	addInputs(module.Inputs, moduleBody)
	return t.addOutputs(body, step.Type, module, moduleVersion.SourceURL, moduleVersion.Version)
}

func addInputs(inputs map[string]interface{}, moduleBody *hclwrite.Body) {
	if inputs == nil {
		return
	}
	keys := make([]string, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, name := range keys {
		value := inputs[name]
		if value == nil {
			continue
		}
		switch v := value.(type) {
		default:
			moduleBody.SetAttributeRaw(name, getTokens(v))
		case string:
			if strings.HasPrefix(v, "module.") || strings.Contains(v, "\n") {
				v = strings.TrimRight(v, "\n")
				moduleBody.SetAttributeRaw(name, getTokens(v))
			} else {
				moduleBody.SetAttributeValue(name, cty.StringVal(v))
			}
		}
	}
}

func (t *terraform) addOutputs(body *hclwrite.Body, stepType model.StepType, module model.Module, sourceURL, release string) error {
	moduleSource := module.Source
	if stepType == model.StepTypeArgoCD {
		moduleSource = fmt.Sprintf("k8s/%s", module.Source)
	}
	filePath := fmt.Sprintf("modules/%s", moduleSource)
	file, err := t.getTerraformFile(sourceURL, filePath, "outputs.tf", release)
	if err != nil {
		var fileError model.FileNotFoundError
		if errors.As(err, &fileError) {
			return nil
		}
		return err
	}
	for _, block := range file.Body().Blocks() {
		if block.Type() != outputType {
			continue
		}
		outputBody := body.AppendNewBlock(outputType, []string{fmt.Sprintf("%s__%s", module.Name, block.Labels()[0])})
		value := fmt.Sprintf("module.%s.%s", module.Name, block.Labels()[0])
		outputBody.Body().SetAttributeRaw("value", getTokens(value))
	}
	return nil
}

func getTokens(value interface{}) hclwrite.Tokens {
	return getBytesTokens([]byte(fmt.Sprintf("%v", value)))
}

func getBytesTokens(bytes []byte) hclwrite.Tokens {
	return hclwrite.Tokens{
		{
			Type:  hclsyntax.TokenIdent,
			Bytes: bytes,
		},
	}
}

func UnmarshalTerraformFile(fileName string, fileContent []byte) (*hclwrite.File, error) {
	hclFile, diags := hclwrite.ParseConfig(fileContent, fileName, hcl.InitialPos)
	if diags != nil && diags.HasErrors() {
		return nil, diags
	}
	return hclFile, nil
}

func ParseLogChanges(pipelineName, message string) (*model.PipelineChanges, error) {
	tfChanges := model.PipelineChanges{}
	if strings.HasPrefix(message, "No changes. Your infrastructure matches the configuration.") {
		log.Printf("Pipeline %s: %s", pipelineName, message)
		tfChanges.NoChanges = true
		return &tfChanges, nil
	} else if strings.HasPrefix(message, "You can apply this plan to save these new output values") {
		log.Printf("Pipeline %s: %s", pipelineName, message)
		return &tfChanges, nil
	}

	matches := planRegex.FindStringSubmatch(message)
	if matches == nil {
		return nil, nil
	}
	log.Printf("Pipeline %s: %s", pipelineName, message)
	added := matches[1]
	changed := matches[2]
	destroyed := matches[3]
	if added == "0" && changed == "0" && destroyed == "0" {
		tfChanges.NoChanges = true
		return &tfChanges, nil
	}
	if changed == "0" && destroyed == "0" {
		return &tfChanges, nil
	}
	var err error
	tfChanges.Changed, err = strconv.Atoi(changed)
	if err != nil {
		return nil, err
	}
	tfChanges.Destroyed, err = strconv.Atoi(destroyed)
	if err != nil {
		return nil, err
	}
	return &tfChanges, nil
}
