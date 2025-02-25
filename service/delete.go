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
	Destroy()
}

type deleter struct {
	config               model.Config
	steps                []model.Step
	provider             model.CloudProvider
	resources            model.Resources
	deleteBucket         bool
	deleteServiceAccount bool
	localPipeline        *LocalPipeline
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
		steps:                getRunnableSteps(config, flags.Steps),
		provider:             provider,
		resources:            resources,
		deleteBucket:         flags.Delete.DeleteBucket,
		deleteServiceAccount: flags.Delete.DeleteServiceAccount,
		localPipeline:        getLocalPipeline(resources, flags),
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

func (d *deleter) Destroy() {
	for i := len(d.steps) - 1; i >= 0; i-- {
		step := d.steps[i]
		projectName := fmt.Sprintf("%s-%s", d.resources.GetCloudPrefix(), step.Name)
		log.Printf("Starting destroy execution pipeline for step %s\n", step.Name)
		step.Approve = model.ApproveForce
		var err error
		if d.localPipeline != nil {
			err = d.localPipeline.startDestroyExecution(step)
		} else {
			err = d.resources.GetPipeline().StartDestroyExecution(projectName, step)
		}
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to run destroy pipeline %s: %s", projectName, err)))
		}
		log.Printf("Successfully executed destroy pipeline for step %s\n", step.Name)
	}
}
