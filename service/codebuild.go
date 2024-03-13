package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"gopkg.in/yaml.v3"
)

const ProjectImage = "public.ecr.aws/entigolabs/entigo-infralib-base"

type Builder interface {
	CreateProject(projectName string, repoURL string, stepName string, workspace string, imageVersion string, vpcConfig *types.VpcConfig) error
	CreateAgentProject(projectName string, awsPrefix string, image string) error
	GetProject(projectName string) (*types.Project, error)
	UpdateProject(projectName string, image string, vpcConfig *types.VpcConfig) error
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

func (b *builder) CreateProject(projectName string, repoURL string, stepName string, workspace string, imageVersion string, vpcConfig *types.VpcConfig) error {
	_, err := b.codeBuild.CreateProject(context.Background(), &codebuild.CreateProjectInput{
		Name:             aws.String(projectName),
		TimeoutInMinutes: aws.Int32(480),
		ServiceRole:      aws.String(b.buildRoleArn),
		Artifacts:        &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts},
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Small,
			Image:                    aws.String(fmt.Sprintf("%s:%s", ProjectImage, imageVersion)),
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
		return b.UpdateProject(projectName, fmt.Sprintf("%s:%s", ProjectImage, imageVersion), vpcConfig)
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

func (b *builder) CreateAgentProject(projectName string, awsPrefix string, image string) error {
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
			EnvironmentVariables: []types.EnvironmentVariable{
				{
					Name:  aws.String(common.AwsPrefixEnv),
					Value: aws.String(awsPrefix),
				},
			},
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

func (b *builder) UpdateProject(projectName string, image string, vpcConfig *types.VpcConfig) error {
	project, err := b.GetProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("project %s not found", projectName)
	}

	vpcChanged := vpcConfig != nil && (project.VpcConfig == nil ||
		(project.VpcConfig.VpcId == nil || *project.VpcConfig.VpcId != *vpcConfig.VpcId) ||
		!util.EqualLists(project.VpcConfig.Subnets, vpcConfig.Subnets) ||
		!util.EqualLists(project.VpcConfig.SecurityGroupIds, vpcConfig.SecurityGroupIds))

	imageChanged := image != "" && project.Environment != nil && project.Environment.Image != nil &&
		*project.Environment.Image != image

	if !vpcChanged && !imageChanged {
		return nil
	}

	if !vpcChanged {
		vpcConfig = project.VpcConfig
	}
	if imageChanged {
		project.Environment.Image = aws.String(image)
	}

	_, err = b.codeBuild.UpdateProject(context.Background(), &codebuild.UpdateProjectInput{
		Name:        aws.String(projectName),
		VpcConfig:   vpcConfig,
		Environment: project.Environment,
	})

	if err != nil {
		return fmt.Errorf("failed to update CodeBuild project %s: %w", projectName, err)
	}

	if vpcConfig != nil && vpcConfig.VpcId != nil {
		common.Logger.Printf("updated CodeBuild project %s image to %s and vpc to %s\n", projectName, image,
			*vpcConfig.VpcId)
	} else if imageChanged {
		common.Logger.Printf("updated CodeBuild project %s image to %s\n", projectName, image)
	}
	return nil
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
