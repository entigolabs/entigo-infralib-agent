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
	Destroy()
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
	repo, err := resources.GetCodeRepo().GetRepoMetadata()
	if err != nil {
		common.Logger.Fatalf("Failed to get repository metadata: %s", err)
	}
	if repo == nil && flags.Config == "" {
		return &deleter{
			config:    model.Config{},
			provider:  provider,
			resources: resources,
		}
	}
	config := getConfig(flags.Config, resources.GetCodeRepo())
	if config.Version == "" {
		config.Version = StableVersion
	}
	githubClient := github.NewGithub(config.Source)
	stableRelease := getLatestRelease(githubClient)
	if config.BaseConfig.Profile != "" && config.Source != "" {
		config = MergeBaseConfig(githubClient, stableRelease, config)
	}
	return &deleter{
		config:                 config,
		provider:               provider,
		resources:              resources,
		github:                 githubClient,
		baseConfigReleaseLimit: stableRelease,
	}
}

func getConfig(configFile string, codeCommit model.CodeRepo) model.Config {
	if configFile != "" {
		return GetLocalConfig(configFile)
	}
	return GetRemoteConfig(codeCommit)
}

func (d *deleter) Delete() {
	for i := len(d.config.Steps) - 1; i >= 0; i-- {
		step := d.config.Steps[i]
		projectName := fmt.Sprintf("%s-%s-%s", d.config.Prefix, step.Name, step.Workspace)
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

func (d *deleter) Destroy() {
	for i := len(d.config.Steps) - 1; i >= 0; i-- {
		step := d.config.Steps[i]
		projectName := fmt.Sprintf("%s-%s-%s", d.config.Prefix, step.Name, step.Workspace)
		err := d.resources.GetPipeline().StartDestroyExecution(projectName)
		if err != nil {
			common.PrintWarning(fmt.Sprintf("Failed to start destroy execution for pipeline %s: %s", projectName, err))
		}
	}
}
