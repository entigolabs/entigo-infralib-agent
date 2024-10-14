package service

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type Agent interface {
	CreatePipeline(version string) error
	UpdateProjectImage(version string, cmd common.Command) error
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
	err := a.createCodeBuild(version, common.RunCommand)
	if err != nil {
		return err
	}
	err = a.createCodeBuild(version, common.UpdateCommand)
	if err != nil {
		return err
	}
	err = a.resources.GetPipeline().CreateAgentPipelines(a.cloudPrefix, a.name, a.resources.GetBucketName())
	if err != nil {
		return err
	}
	common.Logger.Println("Agent run pipeline execution started")
	return nil
}

func (a *agent) createCodeBuild(version string, cmd common.Command) error {
	projectName := fmt.Sprintf("%s-%s", a.name, cmd)
	project, err := a.resources.GetBuilder().GetProject(projectName)
	if err != nil {
		return err
	}
	if project != nil {
		_, err = a.updateProjectImage(project, version)
		return err
	}
	return a.resources.GetBuilder().CreateAgentProject(projectName, a.cloudPrefix, version, cmd)
}

func (a *agent) UpdateProjectImage(version string, cmd common.Command) error {
	projectName := fmt.Sprintf("%s-%s", a.name, cmd)
	project, err := a.resources.GetBuilder().GetProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return nil
	}
	updated, err := a.updateProjectImage(project, version)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}
	err = a.resources.GetPipeline().StartAgentExecution(projectName)
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
	err := a.resources.GetBuilder().UpdateAgentProject(project.Name, version, a.cloudPrefix)
	if err != nil {
		return false, err
	}
	common.Logger.Printf("Updated Agent CodeBuild project %s image version to %s\n", project.Name, version)
	return true, nil
}
