package service

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"gopkg.in/yaml.v3"
)

type Builder interface {
	CreateProject(projectName string, roleArn string, logGroup string, streamName string, bucketArn string, repoURL string) error
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
	codeBuild *codebuild.Client
}

func NewBuilder(awsConfig aws.Config) Builder {
	return &builder{
		codeBuild: codebuild.NewFromConfig(awsConfig),
	}
}

func (b *builder) CreateProject(projectName string, roleArn string, logGroup string, streamName string, bucketArn string, repoURL string) error {
	// TODO Buildspec, either dynamic or copy from file or S3, same for terraform.sh, env variables
	common.Logger.Printf("Creating CodeBuild project %s\n", projectName)
	buildSpec, err := yaml.Marshal(createBuildSpec())
	if err != nil {
		return err
	}
	_, err = b.codeBuild.CreateProject(context.Background(), &codebuild.CreateProjectInput{
		Name:             aws.String(projectName),
		TimeoutInMinutes: NewInt32(5),
		ServiceRole:      aws.String(roleArn),
		Artifacts:        &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts},
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Small,
			Image:                    aws.String("aws/codebuild/amazonlinux2-x86_64-standard:5.0"),
			Type:                     types.EnvironmentTypeLinuxContainer,
			ImagePullCredentialsType: types.ImagePullCredentialsTypeCodebuild,
			EnvironmentVariables: []types.EnvironmentVariable{{
				Name:  aws.String("PROJECT_NAME"),
				Value: aws.String(projectName),
			}},
		},
		LogsConfig: &types.LogsConfig{
			CloudWatchLogs: &types.CloudWatchLogsConfig{
				Status:     types.LogsConfigStatusTypeEnabled,
				GroupName:  aws.String(logGroup),
				StreamName: aws.String(streamName),
			},
			S3Logs: &types.S3LogsConfig{
				Status:   types.LogsConfigStatusTypeEnabled,
				Location: aws.String(fmt.Sprintf("%s/build-log-%s", bucketArn, projectName)),
			},
		},
		Source: &types.ProjectSource{
			Type:          types.SourceTypeCodecommit,
			GitCloneDepth: NewInt32(1),
			Buildspec:     aws.String(string(buildSpec)),
			Location:      &repoURL,
		},
	})
	return err
}

func createBuildSpec() BuildSpec {
	return BuildSpec{
		Version: "0.2",
		Phases: Phases{
			Install: Install{
				Commands: []string{"find .", "env"},
			},
			Build: Build{
				Commands: []string{
					"echo Build started on `date`",
					"chmod +x terraform.sh",
					"./terraform.sh",
					"echo Build ended on `date`",
				},
			},
		},
		Artifacts: Artifacts{
			Files: []string{"tf.tar.gz"},
		},
	}
}
