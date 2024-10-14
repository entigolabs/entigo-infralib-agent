package service

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type Deleter interface {
	Delete()
	Destroy()
}

type deleter struct {
	config       model.Config
	provider     model.CloudProvider
	resources    model.Resources
	deleteBucket bool
}

func NewDeleter(ctx context.Context, flags *common.Flags) Deleter {
	provider := GetCloudProvider(ctx, flags)
	resources := provider.GetResources()
	repo, err := resources.GetBucket().GetRepoMetadata()
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
	config := getConfig(flags.Config, resources.GetBucket())
	return &deleter{
		config:       config,
		provider:     provider,
		resources:    resources,
		deleteBucket: flags.Delete.DeleteBucket,
	}
}

func getConfig(configFile string, codeCommit model.Bucket) model.Config {
	if configFile != "" {
		return GetLocalConfig(configFile)
	}
	return GetRemoteConfig(codeCommit)
}

func (d *deleter) Delete() {
	for i := len(d.config.Steps) - 1; i >= 0; i-- {
		step := d.config.Steps[i]
		projectName := fmt.Sprintf("%s-%s", d.config.Prefix, step.Name)
		err := d.resources.GetPipeline().DeletePipeline(projectName)
		if err != nil {
			common.PrintWarning(fmt.Sprintf("Failed to delete pipeline %s: %s", projectName, err))
		}
		err = d.resources.GetBuilder().DeleteProject(projectName, step)
		if err != nil {
			common.PrintWarning(fmt.Sprintf("Failed to delete project %s: %s", projectName, err))
		}
	}
	d.provider.DeleteResources(d.deleteBucket)
}

func (d *deleter) Destroy() {
	for i := len(d.config.Steps) - 1; i >= 0; i-- {
		step := d.config.Steps[i]
		projectName := fmt.Sprintf("%s-%s", d.config.Prefix, step.Name)
		err := d.resources.GetPipeline().StartDestroyExecution(projectName)
		if err != nil {
			common.PrintWarning(fmt.Sprintf("Failed to start destroy execution for pipeline %s: %s", projectName, err))
		}
	}
}
