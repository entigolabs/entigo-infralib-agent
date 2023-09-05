package service

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
)

const repoURL = "public.ecr.aws/entigolabs/entigo-infralib-agent"
const agentSource = "agent-source.zip"

type Agent interface {
	CreatePipeline(version string) error
}

type agent struct {
	name      string
	resources awsResources
}

func NewAgent(resources awsResources) Agent {
	return &agent{
		name:      resources.awsPrefix + "-agent",
		resources: resources,
	}
}

func (a *agent) CreatePipeline(version string) error {
	err := a.createCodeBuild(version)
	if err != nil {
		return err
	}
	return a.resources.codePipeline.CreateAgentPipeline(a.name, a.name, a.resources.bucket)
}

func (a *agent) createCodeBuild(version string) error {
	project, err := a.resources.codeBuild.GetProject(a.name)
	if err != nil {
		return err
	}
	if project != nil {
		return a.updateProjectImage(project, version)
	}
	return a.resources.codeBuild.CreateAgentProject(a.name, a.resources.buildRoleArn, a.resources.logGroup, a.resources.logStream, a.resources.bucket, repoURL+":"+version)
}

func (a *agent) updateProjectImage(project *types.Project, version string) error {
	if *project.Environment.Image == repoURL+":"+version {
		return nil
	}
	err := a.resources.codeBuild.UpdateProjectImage(*project.Name, repoURL+":"+version)
	if err != nil {
		return err
	}
	_, err = a.resources.codePipeline.StartPipelineExecution(a.name)
	if err != nil {
		return fmt.Errorf("agent codeBuild project updated, failed to start another execution: %w", err)
	}
	return fmt.Errorf("agent codeBuild project updated, started another execution")
}
