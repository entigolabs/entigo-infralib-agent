package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"gopkg.in/yaml.v3"
)

type Builder interface {
	CreateProject(projectName string, repoURL string, stepName string, workspace string, vpcConfig *types.VpcConfig) error
	CreateAgentProject(projectName string, image string) error
	GetProject(projectName string) (*types.Project, error)
	UpdateProjectImage(projectName string, image string) error
	UpdateProjectVpc(projectName string, vpcConfig *types.VpcConfig) error
}

type BuildSpec struct {
	Version   string
	Phases    Phases
	Artifacts Artifacts
}

type Phases struct {
	Install Install
	Build   Build
}

type Install struct {
	Commands []string
}

type Build struct {
	Commands []string
}

type Artifacts struct {
	Files []string
}

type builder struct {
	codeBuild    *codebuild.Client
	region       *string
	buildRoleArn string
	logGroup     string
	logStream    string
	bucketArn    string
	buildSpec    *string
}

func NewBuilder(awsConfig aws.Config, buildRoleArn string, logGroup string, logStream string, bucketArn string) Builder {
	return &builder{
		codeBuild:    codebuild.NewFromConfig(awsConfig),
		region:       &awsConfig.Region,
		buildRoleArn: buildRoleArn,
		logGroup:     logGroup,
		logStream:    logStream,
		bucketArn:    bucketArn,
		buildSpec:    buildSpec(),
	}
}

func (b *builder) CreateProject(projectName string, repoURL string, stepName string, workspace string, vpcConfig *types.VpcConfig) error {
	_, err := b.codeBuild.CreateProject(context.Background(), &codebuild.CreateProjectInput{
		Name:             aws.String(projectName),
		TimeoutInMinutes: aws.Int32(480),
		ServiceRole:      aws.String(b.buildRoleArn),
		Artifacts:        &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts},
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Small,
			Image:                    aws.String("public.ecr.aws/entigolabs/entigo-infralib-base:latest"),
			Type:                     types.EnvironmentTypeLinuxContainer,
			ImagePullCredentialsType: types.ImagePullCredentialsTypeCodebuild,
			EnvironmentVariables:     b.getEnvironmentVariables(projectName, stepName, workspace),
		},
		LogsConfig: &types.LogsConfig{
			CloudWatchLogs: &types.CloudWatchLogsConfig{
				Status:     types.LogsConfigStatusTypeEnabled,
				GroupName:  aws.String(b.logGroup),
				StreamName: aws.String(b.logStream),
			},
			S3Logs: &types.S3LogsConfig{
				Status:   types.LogsConfigStatusTypeEnabled,
				Location: aws.String(fmt.Sprintf("%s/build-log-%s", b.bucketArn, projectName)),
			},
		},
		Source: &types.ProjectSource{
			Type:          types.SourceTypeCodecommit,
			GitCloneDepth: aws.Int32(0), // full clone
			Buildspec:     b.buildSpec,
			Location:      &repoURL,
		},
		VpcConfig: vpcConfig,
	})
	var awsError *types.ResourceAlreadyExistsException
	if err != nil && errors.As(err, &awsError) {
		if vpcConfig != nil {
			err = b.UpdateProjectVpc(projectName, vpcConfig)
			if err != nil {
				return err
			}
		}
		return nil
	}
	common.Logger.Printf("Created CodeBuild project %s\n", projectName)
	return err
}

func (b *builder) getEnvironmentVariables(projectName string, stepName string, workspace string) []types.EnvironmentVariable {
	return []types.EnvironmentVariable{{
		Name:  aws.String("PROJECT_NAME"),
		Value: aws.String(projectName),
	}, {
		Name:  aws.String("AWS_REGION"),
		Value: b.region,
	}, {
		Name:  aws.String("COMMAND"),
		Value: aws.String("plan"),
	}, {
		Name:  aws.String("TF_VAR_prefix"),
		Value: aws.String(stepName),
	}, {
		Name:  aws.String("WORKSPACE"),
		Value: aws.String(workspace),
	}}
}

func (b *builder) CreateAgentProject(projectName string, image string) error {
	common.Logger.Printf("Creating CodeBuild project %s\n", projectName)
	_, err := b.codeBuild.CreateProject(context.Background(), &codebuild.CreateProjectInput{
		Name:             aws.String(projectName),
		TimeoutInMinutes: aws.Int32(480),
		ServiceRole:      aws.String(b.buildRoleArn),
		Artifacts:        &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts},
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Small,
			Image:                    aws.String(image),
			Type:                     types.EnvironmentTypeLinuxContainer,
			ImagePullCredentialsType: types.ImagePullCredentialsTypeCodebuild,
		},
		LogsConfig: &types.LogsConfig{
			CloudWatchLogs: &types.CloudWatchLogsConfig{
				Status:     types.LogsConfigStatusTypeEnabled,
				GroupName:  aws.String(b.logGroup),
				StreamName: aws.String(b.logStream),
			},
			S3Logs: &types.S3LogsConfig{
				Status:   types.LogsConfigStatusTypeEnabled,
				Location: aws.String(fmt.Sprintf("%s/build-log-%s", b.bucketArn, projectName)),
			},
		},
		Source: &types.ProjectSource{
			Type:      types.SourceTypeNoSource,
			Buildspec: agentBuildSpec(),
		},
	})
	return err
}

func (b *builder) GetProject(projectName string) (*types.Project, error) {
	projects, err := b.codeBuild.BatchGetProjects(context.Background(), &codebuild.BatchGetProjectsInput{
		Names: []string{projectName},
	})
	if err != nil {
		return nil, err
	}
	if len(projects.Projects) != 1 {
		return nil, nil
	}
	return &projects.Projects[0], nil
}

func (b *builder) UpdateProjectImage(projectName string, image string) error {
	common.Logger.Printf("Updating CodeBuild project %s image to %s\n", projectName, image)
	_, err := b.codeBuild.UpdateProject(context.Background(), &codebuild.UpdateProjectInput{
		Name: aws.String(projectName),
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Small,
			Image:                    aws.String(image),
			Type:                     types.EnvironmentTypeLinuxContainer,
			ImagePullCredentialsType: types.ImagePullCredentialsTypeCodebuild,
		},
	})
	return err
}

func (b *builder) UpdateProjectVpc(projectName string, vpcConfig *types.VpcConfig) error {
	if vpcConfig == nil || vpcConfig.VpcId == nil {
		return nil
	}
	project, err := b.GetProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("project %s not found", projectName)
	}
	if project.VpcConfig != nil && project.VpcConfig.VpcId != nil && *project.VpcConfig.VpcId == *vpcConfig.VpcId {
		return nil
	}
	_, err = b.codeBuild.UpdateProject(context.Background(), &codebuild.UpdateProjectInput{
		Name:      aws.String(projectName),
		VpcConfig: vpcConfig,
	})
	if err != nil {
		common.Logger.Printf("updated CodeBuild project %s VPC to %s\n", projectName, *vpcConfig.VpcId)
	}
	return err
}

func buildSpec() *string {
	spec := BuildSpec{
		Version: "0.2",
		Phases: Phases{
			Install: Install{
				Commands: []string{"env", "find *"},
			},
			Build: Build{
				Commands: []string{"/usr/bin/entrypoint.sh"},
			},
		},
		Artifacts: Artifacts{
			Files: []string{"tf.tar.gz"},
		},
	}
	return buildSpecYaml(spec)
}

func agentBuildSpec() *string {
	spec := BuildSpec{
		Version: "0.2",
		Phases: Phases{
			Build: Build{
				Commands: []string{"cd /etc/ei-agent && /usr/bin/ei-agent run"},
			},
		},
		Artifacts: Artifacts{
			Files: []string{"**/*"},
		},
	}
	return buildSpecYaml(spec)
}

func buildSpecYaml(spec BuildSpec) *string {
	buildSpec, err := yaml.Marshal(spec)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal buildspec: %s", err)
	}
	specString := string(buildSpec)
	return &specString
}
