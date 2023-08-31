package terraform

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"os"
	"strings"
)

func GetTerraformProvider(step model.Step) ([]byte, error) {
	file, err := ReadTerraformFile("base.tf")
	if err != nil {
		return nil, err
	}
	body := file.Body()
	err = injectEKS(body, step)
	if err != nil {
		return nil, err
	}
	return hclwrite.Format(file.Bytes()), nil
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

func injectEKS(body *hclwrite.Body, step model.Step) error {
	hasEKSModule := false
	for _, module := range step.Modules {
		if module.Name == "eks" {
			hasEKSModule = true
			break
		}
	}
	if !hasEKSModule {
		return nil
	}
	file, err := ReadTerraformFile("eks.tf")
	if err != nil {
		return err
	}
	for _, block := range file.Body().Blocks() {
		if block == nil {
			continue
		}
		body.AppendBlock(block)
	}
	body.AppendNewline()
	terraformBlock := body.FirstMatchingBlock("terraform", []string{})
	if terraformBlock == nil {
		return fmt.Errorf("terraform block not found")
	}
	providersBlock := terraformBlock.Body().FirstMatchingBlock("required_providers", []string{})
	if providersBlock == nil {
		providersBlock = terraformBlock.Body().AppendNewBlock("required_providers", []string{})
	}
	kubernetesProvider := map[string]string{
		"source":  "hashicorp/kubernetes",
		"version": "~>2.0",
	}
	providerBytes, err := util.CreateKeyValuePairs(kubernetesProvider, "{\n", "}")
	if err != nil {
		return err
	}
	providersBlock.Body().SetAttributeRaw("kubernetes", getBytesTokens(providerBytes))
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
