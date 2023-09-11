package service

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

const repoURL = "public.ecr.aws/entigolabs/entigo-infralib-agent"
const agentSource = "agent-source.zip"
const LatestAgentImage = "latest"

type Agent interface {
	CreatePipeline(version string) error
	UpdateProjectImage(version string) error
}

type agent struct {
	name      string
	resources AWSResources
}

func NewAgent(resources AWSResources) Agent {
	return &agent{
		name:      resources.AwsPrefix + "-agent",
		resources: resources,
	}
}

func (a *agent) CreatePipeline(version string) error {
	err := a.createCodeBuild(version)
	if err != nil {
		return err
	}
	err = a.resources.CodePipeline.CreateAgentPipeline(a.name, a.name, a.resources.Bucket)
	if err != nil {
		return err
	}
	common.Logger.Println("Approve the pipeline execution to continue")
	return nil
}

func (a *agent) createCodeBuild(version string) error {
	project, err := a.resources.CodeBuild.GetProject(a.name)
	if err != nil {
		return err
	}
	if project != nil {
		return a.updateProjectImage(project, version)
	}
	return a.resources.CodeBuild.CreateAgentProject(a.name, repoURL+":"+version)
}

func (a *agent) UpdateProjectImage(version string) error {
	project, err := a.resources.CodeBuild.GetProject(a.name)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("agent CodeBuild project not found")
	}
	err = a.updateProjectImage(project, version)
	if err != nil {
		return err
	}
	_, err = a.resources.CodePipeline.StartPipelineExecution(a.name)
	if err != nil {
		return fmt.Errorf("failed to start another execution: %w", err)
	}
	return fmt.Errorf("started another execution with updated image")
}

func (a *agent) updateProjectImage(project *types.Project, version string) error {
	if version == "" {
		version = LatestAgentImage
	}
	if *project.Environment.Image == repoURL+":"+version {
		return nil
	}
	err := a.resources.CodeBuild.UpdateProjectImage(*project.Name, repoURL+":"+version)
	if err != nil {
		return err
	}
	common.Logger.Printf("Updated Agent CodeBuild project %s image to %s\n", *project.Name, repoURL+":"+version)
	return nil
}
