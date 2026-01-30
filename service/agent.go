package service

import (
	"fmt"
	"log"
	"strconv"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type Agent interface {
	CreatePipeline(version string, start bool) error
	UpdateProjectImage(version string, cmd common.Command, local bool) error
}

type agent struct {
	name           string
	cloudPrefix    string
	resources      model.Resources
	terraformCache bool
}

func NewAgent(resources model.Resources, terraformCache bool) Agent {
	return &agent{
		name:           model.GetAgentPrefix(resources.GetCloudPrefix()),
		cloudPrefix:    resources.GetCloudPrefix(),
		resources:      resources,
		terraformCache: terraformCache,
	}
}

func (a *agent) CreatePipeline(version string, start bool) error {
	err := a.createCodeBuild(version, common.RunCommand)
	if err != nil {
		return err
	}
	err = a.createCodeBuild(version, common.UpdateCommand)
	if err != nil {
		return err
	}
	err = a.resources.GetPipeline().CreateAgentPipelines(a.cloudPrefix, a.name, a.resources.GetBucketName(), start)
	if err != nil {
		return err
	}
	if start {
		log.Println("Agent run pipeline execution started")
	} else {
		log.Println("Agent run pipeline execution not started")
	}
	return nil
}

func (a *agent) createCodeBuild(version string, cmd common.Command) error {
	projectName := model.GetAgentProjectName(a.name, cmd)
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

func (a *agent) UpdateProjectImage(version string, cmd common.Command, local bool) error {
	projectName := model.GetAgentProjectName(a.name, cmd)
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
	if local || !updated {
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
	switch a.resources.GetProviderType() {
	case model.AWS:
		image = model.AgentImage
	case model.GCLOUD:
		image = model.AgentImageGCloud
	case model.AZURE:
		image = model.AgentImageAzure
	}
	tfCache := strconv.FormatBool(a.terraformCache)
	if project.Image == image+":"+version && tfCache == project.TerraformCache {
		return false, nil
	}
	err := a.resources.GetBuilder().UpdateAgentProject(project.Name, version, a.cloudPrefix)
	if err != nil {
		return false, err
	}
	log.Printf("Updated Agent CodeBuild project %s image version to %s\n", project.Name, version)
	return true, nil
}
