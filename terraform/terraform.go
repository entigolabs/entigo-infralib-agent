package terraform

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
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
	GetTerraformProvider(step model.Step, moduleVersions map[string]model.ModuleVersion, sourceVersions map[model.SourceKey]string) ([]byte, map[model.SourceKey]model.Set[string], error)
	AddModule(prefix string, body *hclwrite.Body, step model.Step, module model.Module, moduleVersion model.ModuleVersion) error
}

type terraform struct {
	providerType  model.ProviderType
	configSources []model.ConfigSource
	sources       map[model.SourceKey]*model.Source
	provider      model.Provider
}

func NewTerraform(providerType model.ProviderType, configSources []model.ConfigSource, sources map[model.SourceKey]*model.Source, provider model.Provider) Terraform {
	return &terraform{
		providerType:  providerType,
		configSources: configSources,
		sources:       sources,
		provider:      provider,
	}
}

func (t *terraform) GetTerraformProvider(step model.Step, moduleVersions map[string]model.ModuleVersion, sourceVersions map[model.SourceKey]string) ([]byte, map[model.SourceKey]model.Set[string], error) {
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
	providers := make(map[model.SourceKey]model.Set[string])
	providers[baseSource] = model.NewSet(base)
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
	switch t.providerType {
	case model.AWS:
		backendBlock.SetLabels([]string{"s3"})
	case model.GCLOUD:
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
			moduleVersions[module.Name].Source)
		if err != nil {
			return nil, err
		}
		for name, attribute := range providerAttributes {
			providersAttributes[name] = attribute
		}
	}
	return providersAttributes, nil
}

func (t *terraform) findProviderFile(path string, fileName string, sourceVersions map[model.SourceKey]string) (*hclwrite.File, model.SourceKey, error) {
	var sourceKey model.SourceKey
	release := ""
	for _, configSource := range t.configSources {
		key := configSource.GetSourceKey()
		source := t.sources[key]
		if util.IsLocalSource(configSource.URL) && source.Storage.FileExists(filepath.Join(path, fileName), "") {
			sourceKey = key
			release = source.ForcedVersion
			break
		}
		sourceVersion := sourceVersions[key]
		if sourceVersion == "" {
			continue
		}
		if !source.Storage.FileExists(filepath.Join(path, fileName), sourceVersion) {
			continue
		}
		sourceKey = key
		release = sourceVersion
	}
	if sourceKey.IsEmpty() {
		return nil, model.SourceKey{}, model.NewNotFoundError(fileName)
	}
	file, err := t.getTerraformFile(sourceKey, path, fileName, release)
	if err != nil {
		return nil, model.SourceKey{}, err
	}
	return file, sourceKey, nil
}

func (t *terraform) getTerraformFile(sourceURL model.SourceKey, filePath, fileName, release string) (*hclwrite.File, error) {
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

func (t *terraform) getProviderAttributes(module model.Module, version string, source model.SourceKey) (map[string]*hclwrite.Attribute, error) {
	file, err := t.getTerraformFile(source, fmt.Sprintf(moduleTemplate, module.Source), versionsFile, version)
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

func (t *terraform) getProviderBlocks(providerName string, sourceVersions map[model.SourceKey]string) ([]*hclwrite.Block, model.SourceKey) {
	providerFile, providerSource, err := t.findProviderFile(providerPath, fmt.Sprintf("%s.tf", providerName), sourceVersions)
	if err != nil {
		var fileNotFoundError model.NotFoundError
		if errors.As(err, &fileNotFoundError) {
			slog.Debug(fmt.Sprintf("Provider file not found for %s\n", providerName))
			return []*hclwrite.Block{}, providerSource
		}
		return nil, providerSource
	}
	return providerFile.Body().Blocks(), providerSource
}

func (t *terraform) addProviderAttributes(baseBody *hclwrite.Body, providersBlock *hclwrite.Block, providersAttributes map[string]*hclwrite.Attribute, step model.Step, sourceVersions map[model.SourceKey]string) (map[string]model.SourceKey, error) {
	providerInputs := step.Provider.Inputs
	if providerInputs == nil {
		providerInputs = t.provider.Inputs
	}
	if providerInputs == nil {
		providerInputs = make(map[string]interface{})
	}
	providers := make(map[string]model.SourceKey)
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
			err := t.addProviderValues(name, providerBlock.Body(), step.Provider)
			if err != nil {
				return nil, err
			}
			addProviderInputs(providerInputs, providerBlock)
			baseBody.AppendBlock(providerBlock)
		}
	}
	if len(providerInputs) > 0 {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("WARNING! Unknown provider inputs: %v for step %s",
			providerInputs, step.Name)))
	}
	return providers, nil
}

func (t *terraform) addProviderValues(providerType string, body *hclwrite.Body, stepProvider model.Provider) error {
	switch providerType {
	case "aws":
		return t.addAwsProviderValues(body, stepProvider.Aws)
	case "kubernetes":
		return t.addKubernetesProviderValues(body, stepProvider.Kubernetes)
	}
	return nil
}

func (t *terraform) addAwsProviderValues(body *hclwrite.Body, awsProvider model.AwsProvider) error {
	ignoreTags := awsProvider.IgnoreTags
	if ignoreTags.IsEmpty() {
		ignoreTags = t.provider.Aws.IgnoreTags
	}
	err := addAwsProviderIgnoreTags(body, ignoreTags)
	if err != nil {
		return err
	}
	defaultTags := awsProvider.DefaultTags
	if defaultTags.IsEmpty() {
		defaultTags = t.provider.Aws.DefaultTags
	}
	return addAwsProviderDefaultTags(body, defaultTags)
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

func (t *terraform) addKubernetesProviderValues(body *hclwrite.Body, kubernetes model.KubernetesProvider) error {
	ignoreAnnotations := kubernetes.IgnoreAnnotations
	if len(ignoreAnnotations) == 0 {
		ignoreAnnotations = t.provider.Kubernetes.IgnoreAnnotations
	}
	err := addProviderBodyArray(body, "ignore_annotations", ignoreAnnotations)
	if err != nil {
		return err
	}
	ignoreLabels := kubernetes.IgnoreLabels
	if len(ignoreLabels) == 0 {
		ignoreLabels = t.provider.Kubernetes.IgnoreLabels
	}
	return addProviderBodyArray(body, "ignore_labels", ignoreLabels)
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
	} else if util.IsLocalSource(moduleVersion.Source.URL) {
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("%s/modules/%s", moduleVersion.Source.URL, module.Source)))
	} else {
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("git::%s.git//modules/%s?ref=%s", moduleVersion.Source.URL, module.Source,
				moduleVersion.Version)))
	}
	moduleBody.SetAttributeValue("prefix", cty.StringVal(fmt.Sprintf("%s-%s-%s", prefix, step.Name, module.Name)))
	addInputs(module.Inputs, moduleBody)
	return t.addOutputs(body, step.Type, module, moduleVersion.Version, moduleVersion.Source)
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

func (t *terraform) addOutputs(body *hclwrite.Body, stepType model.StepType, module model.Module, release string, source model.SourceKey) error {
	if util.IsClientModule(module) {
		return nil
	}
	moduleSource := module.Source
	if stepType == model.StepTypeArgoCD {
		moduleSource = fmt.Sprintf("k8s/%s", module.Source)
	}
	filePath := fmt.Sprintf("modules/%s", moduleSource)
	file, err := t.getTerraformFile(source, filePath, "outputs.tf", release)
	if err != nil {
		var fileError model.NotFoundError
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
		if sensitiveAttr, ok := block.Body().Attributes()["sensitive"]; ok {
			outputBody.Body().SetAttributeRaw("sensitive", sensitiveAttr.Expr().BuildTokens(nil))
		}
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
	return util.GetChangesFromMatches(pipelineName, message, matches)
}
