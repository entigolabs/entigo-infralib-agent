package service

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/hashicorp/go-version"
)

type Deleter interface {
	Delete()
}

type deleter struct {
	config                 model.Config
	provider               model.CloudProvider
	github                 github.Github
	resources              model.Resources
	baseConfigReleaseLimit *version.Version
}

func NewDeleter(flags *common.Flags) Deleter {
	provider := GetCloudProvider(context.Background(), flags)
	resources := provider.GetResources(flags.Branch)
	config := GetConfig(flags.Config, resources.GetCodeRepo())
	githubClient := github.NewGithub(config.Source)
	stableRelease := getLatestRelease(githubClient)
	return &deleter{
		config:                 config,
		provider:               provider,
		github:                 githubClient,
		baseConfigReleaseLimit: stableRelease,
	}
}

func (d *deleter) Delete() {
	if d.config.BaseConfig.Profile != "" {
		d.config = MergeBaseConfig(d.github, d.baseConfigReleaseLimit, d.config)
	}
	for i := len(d.config.Steps) - 1; i >= 0; i-- {
		step := d.config.Steps[i]
		stepName := fmt.Sprintf("%s-%s", d.config.Prefix, step.Name)
		projectName := fmt.Sprintf("%s-%s", stepName, step.Workspace)
		err := d.resources.GetPipeline().DeletePipeline(projectName)
		if err != nil {
			common.PrintWarning(fmt.Sprintf("Failed to delete pipeline %s: %s", projectName, err))
		}
		err = d.resources.GetBuilder().DeleteProject(projectName, step)
		if err != nil {
			common.PrintWarning(fmt.Sprintf("Failed to delete project %s: %s", projectName, err))
		}
	}
	d.provider.DeleteResources()
}