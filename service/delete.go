package service

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
	"log/slog"
)

type Deleter interface {
	Delete()
	Destroy() bool
}

type deleter struct {
	config               model.Config
	provider             model.CloudProvider
	resources            model.Resources
	deleteBucket         bool
	deleteServiceAccount bool
}

func NewDeleter(ctx context.Context, flags *common.Flags) Deleter {
	provider := GetCloudProvider(ctx, flags)
	resources := provider.GetResources()
	repo, err := resources.GetBucket().GetRepoMetadata()
	if err != nil {
		log.Fatalf("Failed to get repository metadata: %s", err)
	}
	if repo == nil && flags.Config == "" {
		return &deleter{
			config:    model.Config{},
			provider:  provider,
			resources: resources,
		}
	}
	config := getBaseConfig(resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	ValidateConfig(config, nil)
	return &deleter{
		config:               config,
		provider:             provider,
		resources:            resources,
		deleteBucket:         flags.Delete.DeleteBucket,
		deleteServiceAccount: flags.Delete.DeleteServiceAccount,
	}
}

func getBaseConfig(prefix, configFile string, bucket model.Bucket) model.Config {
	var config model.Config
	if configFile != "" {
		config = getLocalConfigFile(configFile)
	} else {
		config = getRemoteConfigFile(bucket)
	}
	return replaceConfigValues(nil, prefix, config)
}

func (d *deleter) Delete() {
	for i := len(d.config.Steps) - 1; i >= 0; i-- {
		step := d.config.Steps[i]
		projectName := fmt.Sprintf("%s-%s", d.resources.GetCloudPrefix(), step.Name)
		err := d.resources.GetPipeline().DeletePipeline(projectName)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete pipeline %s: %s", projectName, err)))
		}
		err = d.resources.GetBuilder().DeleteProject(projectName, step)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete project %s: %s", projectName, err)))
		}
	}
	d.provider.DeleteResources(d.deleteBucket, d.deleteServiceAccount)
}

func (d *deleter) Destroy() bool {
	failed := false
	for i := len(d.config.Steps) - 1; i >= 0; i-- {
		step := d.config.Steps[i]
		projectName := fmt.Sprintf("%s-%s", d.resources.GetCloudPrefix(), step.Name)
		err := d.resources.GetPipeline().StartDestroyExecution(projectName)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to start destroy execution for pipeline %s: %s", projectName, err)))
			failed = true
		}
	}
	return failed
}
