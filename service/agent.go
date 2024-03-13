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
	awsPrefix string
	resources AWSResources
}

func NewAgent(resources AWSResources) Agent {
	return &agent{
		name:      resources.AwsPrefix + "-agent",
		awsPrefix: resources.AwsPrefix,
		resources: resources,
	}
}

func (a *agent) CreatePipeline(version string) error {
	err := a.createCodeBuild(version)
	if err != nil {
		return err
	}
	err = a.resources.CodePipeline.CreateAgentPipeline(a.awsPrefix, a.name, a.name, a.resources.Bucket)
	if err != nil {
		return err
	}
	common.Logger.Println("Agent pipeline execution started")
	return nil
}

func (a *agent) createCodeBuild(version string) error {
	project, err := a.resources.CodeBuild.GetProject(a.name)
	if err != nil {
		return err
	}
	if project != nil {
		_, err = a.updateProjectImage(project, version)
		return err
	}
	return a.resources.CodeBuild.CreateAgentProject(a.name, a.awsPrefix, repoURL+":"+version)
}

func (a *agent) UpdateProjectImage(version string) error {
	project, err := a.resources.CodeBuild.GetProject(a.name)
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
	_, err = a.resources.CodePipeline.StartPipelineExecution(a.name)
	if err != nil {
		return fmt.Errorf("failed to start another execution: %w", err)
	}
	return fmt.Errorf("started another execution with updated image")
}

func (a *agent) updateProjectImage(project *types.Project, version string) (bool, error) {
	if version == "" {
		version = LatestAgentImage
	}
	if *project.Environment.Image == repoURL+":"+version {
		return false, nil
	}
	err := a.resources.CodeBuild.UpdateProject(*project.Name, repoURL+":"+version, nil)
	if err != nil {
		return false, err
	}
	common.Logger.Printf("Updated Agent CodeBuild project %s image to %s\n", *project.Name, repoURL+":"+version)
	return true, nil
}
