package service

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

const repoURL = "public.ecr.aws/entigolabs/entigo-infralib-agent"
const LatestAgentImage = "latest"

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
	return a.resources.GetBuilder().CreateAgentProject(a.name, a.cloudPrefix, repoURL+":"+version)
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
	_, err = a.resources.GetPipeline().StartPipelineExecution(a.name)
	if err != nil {
		return fmt.Errorf("failed to start another execution: %w", err)
	}
	return fmt.Errorf("started another execution with updated image")
}

func (a *agent) updateProjectImage(project *model.Project, version string) (bool, error) {
	if version == "" {
		version = LatestAgentImage
	}
	if project.Image == repoURL+":"+version {
		return false, nil
	}
	err := a.resources.GetBuilder().UpdateProject(project.Name, repoURL+":"+version, nil)
	if err != nil {
		return false, err
	}
	common.Logger.Printf("Updated Agent CodeBuild project %s image to %s\n", project.Name, repoURL+":"+version)
	return true, nil
}
