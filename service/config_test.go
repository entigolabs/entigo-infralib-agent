package service

import (
	"github.com/brianvoe/gofakeit/v6"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/test"
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestMergeConfig(t *testing.T) {
	test.AddFakeConfigTypes()

	var baseConfig model.Config
	err := gofakeit.Struct(&baseConfig)
	if err != nil {
		t.Fatalf("Failed to generate fake base config: %v", err)
	}
	baseConfig.Steps = append(baseConfig.Steps, model.Step{
		Name: "remove-this-baseStep",
	})

	var patchConfig model.Config
	err = gofakeit.Struct(&patchConfig)
	if err != nil {
		t.Fatalf("Failed to generate fake patch config: %v", err)
	}
	patchConfig.Steps = append(patchConfig.Steps, model.Step{
		Name:   "remove-this-baseStep",
		Remove: true,
	})

	var baseStep model.Step
	err = gofakeit.Struct(&baseStep)
	if err != nil {
		t.Fatalf("Failed to generate fake baseStep: %v", err)
	}
	baseStep.Modules = append(baseStep.Modules, model.Module{
		Name:   "remove-this-module",
		Remove: true,
		Inputs: map[string]interface{}{},
	})
	baseConfig.Steps = append(baseConfig.Steps, baseStep)

	patchStep := copyStep(baseStep)
	assert.Equal(t, patchStep, baseStep, "Copying baseStep should not change the original baseStep")
	patchStep.KubernetesClusterName = gofakeit.Word()
	patchStep.Modules[0].Source = gofakeit.URL()
	patchStep.Modules[0].Inputs["key"] = gofakeit.Word()
	for key, _ := range patchStep.Modules[0].Inputs {
		patchStep.Modules[0].Inputs[key] = gofakeit.Word()
	}
	patchConfig.Steps = append(patchConfig.Steps, patchStep)

	resultConfig := MergeConfig(patchConfig, baseConfig)

	assert.Equal(t, patchConfig.Prefix, resultConfig.Prefix, "Merged config should have overwritten prefix from patch config")
	assert.Equal(t, resultConfig.Steps[0], baseConfig.Steps[0], "Merged config should contain base config baseStep")
	assert.Equal(t, resultConfig.Steps[2], patchConfig.Steps[0], "Merged config should contain patch config baseStep")
	for _, step := range resultConfig.Steps {
		assert.NotEqual(t, "remove-this-baseStep", step.Name, "Merged config should not contain steps with remove flag")
	}

	resultStep := resultConfig.Steps[1]
	assert.Equal(t, patchStep.Version, baseStep.Version, "Merged step should have kept version from base step")
	assert.Equal(t, patchStep.KubernetesClusterName, resultStep.KubernetesClusterName, "Merged step should have overwritten kubernetes cluster name from patch step")
	assert.Equal(t, patchStep.Modules[0].Source, resultStep.Modules[0].Source, "Merged step should have overwritten module source from patch step")
	for _, module := range resultStep.Modules {
		assert.NotEqual(t, "remove-this-module", module.Name, "Merged step should not contain modules with remove flag")
	}
}

func copyStep(step model.Step) model.Step {
	return model.Step{
		Name:                  step.Name,
		Type:                  step.Type,
		Before:                step.Before,
		Approve:               step.Approve,
		Remove:                step.Remove,
		Version:               step.Version,
		BaseImageVersion:      step.BaseImageVersion,
		VpcId:                 step.VpcId,
		VpcSubnetIds:          step.VpcSubnetIds,
		VpcSecurityGroupIds:   step.VpcSecurityGroupIds,
		KubernetesClusterName: step.KubernetesClusterName,
		ArgocdNamespace:       step.ArgocdNamespace,
		RepoUrl:               step.RepoUrl,
		Provider:              step.Provider,
		Modules:               copyModules(step.Modules),
	}
}

func copyModules(modules []model.Module) []model.Module {
	var copiedModules []model.Module
	for _, module := range modules {
		copiedModules = append(copiedModules, model.Module{
			Name:         module.Name,
			Source:       module.Source,
			HttpUsername: module.HttpUsername,
			HttpPassword: module.HttpPassword,
			Version:      module.Version,
			Remove:       module.Remove,
			Inputs:       copyInputMap(module.Inputs),
			InputsFile:   module.InputsFile,
			FileContent:  module.FileContent,
		})
	}
	return copiedModules
}

func copyInputMap(inputMap map[string]interface{}) map[string]interface{} {
	copiedInputMap := make(map[string]interface{})
	for key, value := range inputMap {
		copiedInputMap[key] = value
	}
	return copiedInputMap
}
