package service

import (
	"context"
	"fmt"
	"log"
	"log/slog"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

type Deleter interface {
	Delete() error
	Destroy() error
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

func NewDeleter(ctx context.Context, flags *common.Flags) (Deleter, error) {
	provider, err := GetCloudProvider(ctx, flags)
	if err != nil {
		return nil, err
	}
	resources, err := provider.GetResources()
	if err != nil {
		return nil, err
	}
	repo, err := resources.GetBucket().GetRepoMetadata()
	if err != nil {
		return nil, fmt.Errorf("failed to get repository metadata: %s", err)
	}
	if repo == nil && flags.Config == "" {
		return &deleter{
			config:    model.Config{},
			provider:  provider,
			resources: resources,
		}, nil
	}
	config, err := getBaseConfig(resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if err != nil {
		return nil, err
	}
	if err = ValidateConfig(config, nil); err != nil {
		return nil, fmt.Errorf("failed to validate config: %v", err)
	}
	steps, err := getRunnableSteps(config, flags.Steps)
	if err != nil {
		return nil, err
	}
	return &deleter{
		config:               config,
		steps:                steps,
		provider:             provider,
		resources:            resources,
		deleteBucket:         flags.Delete.DeleteBucket,
		deleteServiceAccount: flags.Delete.DeleteServiceAccount,
		localPipeline:        getLocalPipeline(resources, ProcessPipelineFlags(flags.Pipeline), flags.GCloud, nil),
	}, nil
}

func getBaseConfig(prefix, configFile string, bucket model.Bucket) (model.Config, error) {
	var config model.Config
	var err error
	if configFile != "" {
		config, err = getLocalConfigFile(configFile)
	} else {
		config, err = getRemoteConfigFile(bucket)
	}
	if err != nil {
		return config, err
	}
	return replaceConfigValues(nil, prefix, config)
}

func (d *deleter) Delete() error {
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
	d.deleteSourceSecrets()
	return d.provider.DeleteResources(d.deleteBucket, d.deleteServiceAccount)
}

func (d *deleter) deleteSourceSecrets() {
	for _, source := range d.config.Sources {
		if source.Username == "" {
			continue
		}
		hash := util.HashCode(source.URL)
		d.deleteSourceSecret(fmt.Sprintf(model.GitSourceFormat, hash))
		d.deleteSourceSecret(fmt.Sprintf(model.GitUsernameFormat, hash))
		d.deleteSourceSecret(fmt.Sprintf(model.GitPasswordFormat, hash))
	}
}

func (d *deleter) deleteSourceSecret(secret string) {
	err := d.resources.GetSSM().DeleteSecret(secret)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete secret %s: %s", secret, err)))
	}
}

func (d *deleter) Destroy() error {
	for i := len(d.steps) - 1; i >= 0; i-- {
		step := d.steps[i]
		projectName := fmt.Sprintf("%s-%s", d.resources.GetCloudPrefix(), step.Name)
		log.Printf("Starting destroy execution pipeline for step %s\n", step.Name)
		step.Approve = model.ApproveForce
		var err error
		if d.localPipeline != nil {
			err = d.localPipeline.startDestroyExecution(step, d.getSourceAuths())
		} else {
			err = d.resources.GetPipeline().StartDestroyExecution(projectName, step)
		}
		if err != nil {
			return fmt.Errorf("failed to run destroy pipeline %s: %s", projectName, err)
		}
		log.Printf("Successfully executed destroy pipeline for step %s\n", step.Name)
	}
	return nil
}

func (d *deleter) getSourceAuths() map[string]model.SourceAuth {
	authSources := make(map[string]model.SourceAuth)
	for _, source := range d.config.Sources {
		if source.Username == "" {
			continue
		}
		authSources[source.URL] = model.SourceAuth{
			Username: source.Username,
			Password: source.Password,
		}
	}
	return authSources
}
