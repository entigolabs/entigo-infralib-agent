package terraform

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"os"
	"regexp"
	"strings"
)

const PlanRegex = `Plan: (\d+) to add, (\d+) to change, (\d+) to destroy`
const providerPath = "providers"
const moduleTemplate = "modules/%s"
const baseFile = "base.tf"
const versionsFile = "versions.tf"
const awsTagsRegex = `(\w+)\s*=\s*"([^"]+)"`

type Terraform interface {
	GetTerraformProvider(step model.Step, moduleVersions map[string]string, providerType model.ProviderType) ([]byte, []string, error)
	GetEmptyTerraformProvider(version string, providerType model.ProviderType) ([]byte, error)
}

type terraform struct {
	github github.Github
}

func NewTerraform(github github.Github) Terraform {
	return &terraform{
		github: github,
	}
}

func (t *terraform) GetTerraformProvider(step model.Step, moduleVersions map[string]string, providerType model.ProviderType) ([]byte, []string, error) {
	if len(moduleVersions) == 0 {
		return make([]byte, 0), []string{}, nil
	}
	versions := util.MapValues(moduleVersions)
	providersVersion, err := util.GetNewestVersion(versions)
	if err != nil {
		return nil, nil, err
	}
	file, err := t.getTerraformFile(providerPath, baseFile, providersVersion)
	if err != nil {
		return nil, nil, err
	}
	modifyBackendType(file.Body(), providerType)
	providersAttributes := make(map[string]*hclwrite.Attribute)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		providerAttributes, err := t.getProviderAttributes(module, moduleVersions[module.Name])
		if err != nil {
			return nil, nil, err
		}
		for name, attribute := range providerAttributes {
			providersAttributes[name] = attribute
		}
	}
	providersBlock, err := getRequiredProvidersBlock(file)
	if err != nil {
		return nil, nil, err
	}
	baseBody := file.Body()
	providers, err := t.addProviderAttributes(baseBody, providersBlock, providersAttributes, providersVersion, step)
	if err != nil {
		return nil, nil, err
	}
	return hclwrite.Format(file.Bytes()), providers, nil
}

func (t *terraform) GetEmptyTerraformProvider(version string, providerType model.ProviderType) ([]byte, error) {
	file, err := t.getTerraformFile(providerPath, baseFile, version)
	if err != nil {
		return nil, err
	}
	modifyBackendType(file.Body(), providerType)
	return hclwrite.Format(file.Bytes()), nil
}

func modifyBackendType(body *hclwrite.Body, providerType model.ProviderType) {
	if providerType != model.GCLOUD {
		return
	}
	terraformBlock := body.FirstMatchingBlock("terraform", []string{})
	if terraformBlock == nil {
		return
	}
	backendBlock := terraformBlock.Body().FirstMatchingBlock("backend", []string{"s3"})
	if backendBlock == nil {
		return
	}
	backendBlock.SetLabels([]string{"gcs"})
}

func (t *terraform) getTerraformFile(filePath string, fileName string, release string) (*hclwrite.File, error) {
	baseFile, err := t.github.GetRawFileContent(fmt.Sprintf("%s/%s", filePath, fileName), release)
	if err != nil {
		return nil, err
	}
	file, err := UnmarshalTerraformFile(fileName, baseFile)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (t *terraform) getProviderAttributes(module model.Module, version string) (map[string]*hclwrite.Attribute, error) {
	file, err := t.getTerraformFile(fmt.Sprintf(moduleTemplate, module.Source), versionsFile, version)
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

func (t *terraform) getProviderBlocks(providerName string, version string) []*hclwrite.Block {
	providerFile, err := t.getTerraformFile(providerPath, fmt.Sprintf("%s.tf", providerName), version)
	if err != nil {
		var fileNotFoundError github.FileNotFoundError
		if errors.As(err, &fileNotFoundError) {
			fmt.Printf("Provider file not found for %s\n", providerName)
			return []*hclwrite.Block{}
		}
		return nil
	}
	return providerFile.Body().Blocks()
}

func (t *terraform) addProviderAttributes(baseBody *hclwrite.Body, providersBlock *hclwrite.Block, providersAttributes map[string]*hclwrite.Attribute, providersVersion string, step model.Step) ([]string, error) {
	providerInputs := step.Provider.Inputs
	if providerInputs == nil {
		providerInputs = make(map[string]interface{})
	}
	providers := make([]string, 0)
	for name, attribute := range providersAttributes {
		providersBlock.Body().SetAttributeRaw(name, attribute.Expr().BuildTokens(nil))
		providerBlocks := t.getProviderBlocks(name, providersVersion)
		if len(providerBlocks) != 0 {
			providers = append(providers, name)
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

func GetMockTerraformMain() []byte {
	file := hclwrite.NewEmptyFile()
	output := file.Body().AppendNewBlock("output", []string{"hello_world"})
	output.Body().SetAttributeValue("value", cty.StringVal("Hello, World!"))
	return hclwrite.Format(file.Bytes())
}
