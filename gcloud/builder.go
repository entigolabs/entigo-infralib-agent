package gcloud

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"strings"
)

// TODO Is Builder needed for GCloud?
type Builder struct {
	image string
}

func NewBuilder() *Builder {
	return &Builder{}
}

func (b *Builder) CreateProject(projectName string, repoURL string, stepName string, workspace string, imageVersion string, vpcConfig *model.VpcConfig) error {
	b.image = fmt.Sprintf("%s:%s", repoURL, imageVersion)
	return nil
}

func (b *Builder) CreateAgentProject(projectName string, awsPrefix string, image string) error {
	return nil
}

func (b *Builder) GetProject(projectName string) (*model.Project, error) {
	image := b.image
	if strings.Contains(projectName, "agent") {
		image = model.AgentImage + ":" + model.LatestImageVersion
	}
	return &model.Project{
		Name:  projectName,
		Image: image,
	}, nil
}

func (b *Builder) UpdateProject(projectName string, image string, vpcConfig *model.VpcConfig) error {
	b.image = image
	return nil
}
