package service

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type Agent interface {
	CreatePipeline(version string) error
	UpdateProjectImage(version string) error
}

type agent struct {
	name        string
	cloudPrefix string
	resources   model.Resources
}

func NewAgent(resources model.Resources) Agent {
	return &agent{
		name:        resources.GetCloudPrefix() + "-agent",
		cloudPrefix: resources.GetCloudPrefix(),
		resources:   resources,
	}
}

func (a *agent) CreatePipeline(version string) error {
	err := a.createCodeBuild(version)
	if err != nil {
		return err
	}
	err = a.resources.GetPipeline().CreateAgentPipeline(a.cloudPrefix, a.name, a.name, a.resources.GetBucket())
	if err != nil {
		return err
	}
	common.Logger.Println("Agent pipeline execution started")
	return nil
}

func (a *agent) createCodeBuild(version string) error {
	project, err := a.resources.GetBuilder().GetProject(a.name)
	if err != nil {
		return err
	}
	if project != nil {
		_, err = a.updateProjectImage(project, version)
		return err
	}
	return a.resources.GetBuilder().CreateAgentProject(a.name, a.cloudPrefix, version)
}

func (a *agent) UpdateProjectImage(version string) error {
	project, err := a.resources.GetBuilder().GetProject(a.name)
	if err != nil {
		return err
	}
	if project == nil {
		common.Logger.Printf("Agent CodeBuild project not found\n")
		return nil
	}
	updated, err := a.updateProjectImage(project, version)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}
	err = a.resources.GetPipeline().StartAgentExecution(a.name)
	if err != nil {
		return fmt.Errorf("failed to start another execution: %w", err)
	}
	return fmt.Errorf("started another execution with updated image")
}

func (a *agent) updateProjectImage(project *model.Project, version string) (bool, error) {
	if version == "" {
		version = model.LatestImageVersion
	}
	var image string
	if a.resources.GetProviderType() == model.AWS {
		image = model.AgentImage
	} else {
		image = model.AgentImageDocker
	}
	if project.Image == image+":"+version {
		return false, nil
	}
	err := a.resources.GetBuilder().UpdateAgentProject(project.Name, version)
	if err != nil {
		return false, err
	}
	common.Logger.Printf("Updated Agent CodeBuild project %s image version to %s\n", project.Name, version)
	return true, nil
}
