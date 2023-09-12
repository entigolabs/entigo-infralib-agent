package terraform

import (
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"os"
	"strings"
)

const providerPath = "providers"
const moduleTemplate = "modules/%s"
const baseFile = "base.tf"
const versionsFile = "versions.tf"

type Terraform interface {
	GetTerraformProvider(step model.Step, moduleVersions map[string]string) ([]byte, error)
}

type terraform struct {
	github github.Github
}

func NewTerraform(github github.Github) Terraform {
	return &terraform{
		github: github,
	}
}

func (t *terraform) GetTerraformProvider(step model.Step, moduleVersions map[string]string) ([]byte, error) {
	versions := util.MapValues(moduleVersions)
	providersVersion, err := util.GetNewestVersion(versions)
	if err != nil {
		return nil, err
	}
	file, err := t.getTerraformFile(providerPath, baseFile, providersVersion)
	if err != nil {
		return nil, err
	}
	providersAttributes := make(map[string]*hclwrite.Attribute)
	for _, module := range step.Modules {
		providerAttributes, err := t.getProviderAttributes(module, moduleVersions[module.Name])
		if err != nil {
			return nil, err
		}
		for name, attribute := range providerAttributes {
			providersAttributes[name] = attribute
		}
	}
	providersBlock, err := getRequiredProvidersBlock(file)
	if err != nil {
		return nil, err
	}
	baseBody := file.Body()
	for name, attribute := range providersAttributes {
		providersBlock.Body().SetAttributeRaw(name, attribute.Expr().BuildTokens(nil))
		providerBlocks := t.getProviderBlocks(name, providersVersion)
		for _, providerBlock := range providerBlocks {
			baseBody.AppendBlock(providerBlock)
		}
	}
	return hclwrite.Format(file.Bytes()), nil
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

func AddInputs(inputs map[string]interface{}, moduleBody *hclwrite.Body, branch string) {
	if inputs == nil {
		return
	}
	for name, value := range inputs {
		if name == "branch" {
			moduleBody.SetAttributeValue(name, cty.StringVal(branch))
			continue
		}
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

func UnmarshalTerraformFile(fileName string, fileContent string) (*hclwrite.File, error) {
	hclFile, diags := hclwrite.ParseConfig([]byte(fileContent), fileName, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	return hclFile, nil
}
