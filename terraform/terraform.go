package terraform

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	providerPath   = "providers"
	moduleTemplate = "modules/%s"
	baseFile       = "base.tf"
	versionsFile   = "versions.tf"
	awsTagsRegex   = `(\w+)\s*=\s*"([^"]+)"`
)

var planRegex = regexp.MustCompile(`Plan: (\d+) to add, (\d+) to change, (\d+) to destroy`)

type Terraform interface {
	GetTerraformProvider(step model.Step, moduleVersions map[string]model.ModuleVersion, providerType model.ProviderType, sources map[string]*model.Source, moduleSources map[string]string) ([]byte, map[string]model.Set[string], error)
}

type terraform struct {
	github github.Github
}

func NewTerraform(github github.Github) Terraform {
	return &terraform{
		github: github,
	}
}

func (t *terraform) GetTerraformProvider(step model.Step, moduleVersions map[string]model.ModuleVersion, providerType model.ProviderType, sources map[string]*model.Source, moduleSources map[string]string) ([]byte, map[string]model.Set[string], error) {
	sourceVersions, err := getSourceVersions(step, moduleVersions, moduleSources)
	if err != nil {
		return nil, nil, err
	}
	if len(sourceVersions) == 0 {
		return make([]byte, 0), map[string]model.Set[string]{}, nil
	}
	file, baseSource, err := t.findTerraformFile(providerPath, baseFile, sources, sourceVersions)
	if err != nil {
		return nil, nil, err
	}
	modifyBackendType(file.Body(), providerType)
	providersAttributes, err := t.getProvidersAttributes(step, moduleVersions, moduleSources)
	if err != nil {
		return nil, nil, err
	}
	providersBlock, err := getRequiredProvidersBlock(file)
	if err != nil {
		return nil, nil, err
	}
	baseBody := file.Body()
	providers := make(map[string]model.Set[string])
	providers[baseSource] = model.ToSet([]string{baseFile})
	attrProviders, err := t.addProviderAttributes(baseBody, providersBlock, providersAttributes, step, sources, sourceVersions)
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

func getSourceVersions(step model.Step, moduleVersions map[string]model.ModuleVersion, moduleSources map[string]string) (map[string]*version.Version, error) {
	sourceVersions := make(map[string]*version.Version)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		source := moduleSources[module.Source]
		moduleVersion, err := version.NewVersion(moduleVersions[module.Name].Version)
		if err != nil {
			return nil, err
		}
		if sourceVersions[source] == nil {
			sourceVersions[source] = moduleVersion
		} else if moduleVersion.GreaterThan(sourceVersions[source]) {
			sourceVersions[source] = moduleVersion
		}
	}
	return sourceVersions, nil
}

func modifyBackendType(body *hclwrite.Body, providerType model.ProviderType) {
	terraformBlock := body.FirstMatchingBlock("terraform", []string{})
	if terraformBlock == nil {
		return
	}
	backendBlock := terraformBlock.Body().FirstMatchingBlock("backend", []string{"TYPE"})
	if backendBlock == nil {
		return
	}
	if providerType == model.AWS {
		backendBlock.SetLabels([]string{"s3"})
	} else if providerType == model.GCLOUD {
		backendBlock.SetLabels([]string{"gcs"})
	}
}

func (t *terraform) getProvidersAttributes(step model.Step, moduleVersions map[string]model.ModuleVersion, moduleSources map[string]string) (map[string]*hclwrite.Attribute, error) {
	providersAttributes := make(map[string]*hclwrite.Attribute)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		providerAttributes, err := t.getProviderAttributes(module, moduleVersions[module.Name].Version,
			moduleSources[module.Source])
		if err != nil {
			return nil, err
		}
		for name, attribute := range providerAttributes {
			providersAttributes[name] = attribute
		}
	}
	return providersAttributes, nil
}

func (t *terraform) findTerraformFile(filePath string, fileName string, sources map[string]*model.Source, sourceVersions map[string]*version.Version) (*hclwrite.File, string, error) {
	providerName := fmt.Sprintf("providers/%s", fileName)
	sourceURL := ""
	release := ""
	for _, source := range sources {
		if source.CurrentChecksums[providerName] == "" {
			continue
		}
		sourceURL = source.URL
		release = sourceVersions[sourceURL].Original()
	}
	if sourceURL == "" {
		return nil, "", model.NewFileNotFoundError(fileName)
	}
	return t.getTerraformFile(sourceURL, filePath, fileName, release)
}

func (t *terraform) getTerraformFile(sourceURL, filePath, fileName, release string) (*hclwrite.File, string, error) {
	rawFile, err := t.github.GetRawFileContent(sourceURL, fmt.Sprintf("%s/%s", filePath, fileName), release)
	if err != nil {
		return nil, "", err
	}
	file, err := UnmarshalTerraformFile(fileName, rawFile)
	if err != nil {
		return nil, "", err
	}
	return file, sourceURL, nil
}

func (t *terraform) getProviderAttributes(module model.Module, version, sourceURL string) (map[string]*hclwrite.Attribute, error) {
	file, _, err := t.getTerraformFile(sourceURL, fmt.Sprintf(moduleTemplate, module.Source), versionsFile, version)
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

func (t *terraform) getProviderBlocks(providerName string, sources map[string]*model.Source, sourceVersions map[string]*version.Version) ([]*hclwrite.Block, string) {
	providerFile, providerSource, err := t.findTerraformFile(providerPath, fmt.Sprintf("%s.tf", providerName), sources, sourceVersions)
	if err != nil {
		var fileNotFoundError model.FileNotFoundError
		if errors.As(err, &fileNotFoundError) {
			fmt.Printf("Provider file not found for %s\n", providerName)
			return []*hclwrite.Block{}, ""
		}
		return nil, ""
	}
	return providerFile.Body().Blocks(), providerSource
}

func (t *terraform) addProviderAttributes(baseBody *hclwrite.Body, providersBlock *hclwrite.Block, providersAttributes map[string]*hclwrite.Attribute, step model.Step, sources map[string]*model.Source, sourceVersions map[string]*version.Version) (map[string]string, error) {
	providerInputs := step.Provider.Inputs
	if providerInputs == nil {
		providerInputs = make(map[string]interface{})
	}
	providers := make(map[string]string)
	for name, attribute := range providersAttributes {
		providersBlock.Body().SetAttributeRaw(name, attribute.Expr().BuildTokens(nil))
		providerBlocks, providerSource := t.getProviderBlocks(name, sources, sourceVersions)
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

func AddInputs(inputs map[string]interface{}, moduleBody *hclwrite.Body) {
	if inputs == nil {
		return
	}
	for name, value := range inputs {
		if value == nil {
			continue
		}
		switch v := value.(type) {
		default:
			moduleBody.SetAttributeRaw(name, getTokens(v))
		case string:
			if strings.Contains(v, "\n") {
				v = strings.TrimRight(v, "\n")
				moduleBody.SetAttributeRaw(name, getTokens(v))
			} else {
				moduleBody.SetAttributeValue(name, cty.StringVal(v))
			}
		}
	}
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

func ReadTerraformFile(fileName string) (*hclwrite.File, error) {
	file, err := os.ReadFile(fileName)
	if err != nil {
		return nil, err
	}
	hclFile, diags := hclwrite.ParseConfig(file, fileName, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	return hclFile, nil
}

func UnmarshalTerraformFile(fileName string, fileContent []byte) (*hclwrite.File, error) {
	hclFile, diags := hclwrite.ParseConfig(fileContent, fileName, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	return hclFile, nil
}

func ParseLogChanges(pipelineName, log string) (*model.TerraformChanges, error) {
	tfChanges := model.TerraformChanges{}
	if strings.HasPrefix(log, "No changes. Your infrastructure matches the configuration.") {
		common.Logger.Printf("Pipeline %s: %s", pipelineName, log)
		tfChanges.NoChanges = true
		return &tfChanges, nil
	} else if strings.HasPrefix(log, "You can apply this plan to save these new output values") {
		common.Logger.Printf("Pipeline %s: %s", pipelineName, log)
		return &tfChanges, nil
	}

	matches := planRegex.FindStringSubmatch(log)
	if matches == nil {
		return nil, nil
	}
	common.Logger.Printf("Pipeline %s: %s", pipelineName, log)
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
