package service

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"gopkg.in/yaml.v3"
)

func CreateStepFiles(config model.Config, codeCommit CodeCommit) {
	releaseTag, err := GetLatestReleaseTag(config.Source)
	if err != nil {
		common.Logger.Fatalf("Failed to get latest release: %s", err)
	}

	for _, step := range config.Steps {
		switch step.Type {
		case "terraform":
			createTerraformFiles(step, config, codeCommit, releaseTag)
			break
		case "argocd-apps":
			createArgoCDFiles(step, config, codeCommit)
			break
		}
	}
}

func createTerraformFiles(step model.Steps, config model.Config, codeCommit CodeCommit, releaseTag string) {
	provider, err := terraform.GetTerraformProvider(step)
	if err != nil {
		common.Logger.Fatalf("Failed to create terraform provider: %s", err)
	}
	codeCommit.PutFile(fmt.Sprintf("%s-%s/provider.tf", config.Prefix, step.Name), provider)
	main, err := terraform.GetTerraformMain(step, config.Source, releaseTag)
	if err != nil {
		common.Logger.Fatalf("Failed to create terraform main: %s", err)
	}
	codeCommit.PutFile(fmt.Sprintf("%s-%s/main.tf", config.Prefix, step.Name), main)
}

func createArgoCDFiles(step model.Steps, config model.Config, codeCommit CodeCommit) {
	for _, module := range step.Modules {
		inputs := module.Inputs
		if len(inputs) == 0 {
			continue
		}
		yamlBytes, err := yaml.Marshal(inputs)
		if err != nil {
			common.Logger.Fatalf("Failed to marshal helm values: %s", err)
		}
		codeCommit.PutFile(fmt.Sprintf("%s-%s/%s-values.yaml", config.Prefix, step.Name, module.Name),
			yamlBytes)
	}
}

func CreateBackendConf(bucket string, dynamoDBTable string, codeCommit CodeCommit) {
	bytes, err := util.CreateKeyValuePairs(map[string]string{
		"bucket":         bucket,
		"key":            "terraform.tfstate",
		"dynamodb_table": dynamoDBTable,
		"encrypt":        "true",
	}, "", "")
	if err != nil {
		common.Logger.Fatalf("Failed to convert backend config values: %s", err)
	}
	codeCommit.PutFile("backend.conf", bytes)
}
