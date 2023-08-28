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

func GetTerraformProvider(step model.Steps) ([]byte, error) {
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

func GetTerraformMain(step model.Steps, config model.Config, releaseTag string) ([]byte, error) {
	file := hclwrite.NewEmptyFile()
	body := file.Body()
	newLocals := body.AppendNewBlock("locals", []string{})
	newLocals.Body().SetAttributeValue("current_version", cty.StringVal(releaseTag))
	for _, module := range step.Modules {
		newModule := body.AppendNewBlock("module", []string{module.Name})
		moduleBody := newModule.Body()
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("git::%s.git//modules/%s?ref=%s", config.Source, module.Source, releaseTag)))
		moduleBody.SetAttributeValue("prefix", cty.StringVal(fmt.Sprintf("%s-%s-%s", config.Prefix, step.Name, module.Name)))
		addInputs(module.Inputs, moduleBody)
	}
	return file.Bytes(), nil
}

func addInputs(inputs map[string]interface{}, moduleBody *hclwrite.Body) {
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

func injectEKS(body *hclwrite.Body, step model.Steps) error {
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
