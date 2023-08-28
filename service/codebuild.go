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
	CreateProject(projectName string, roleArn string, logGroup string, streamName string, bucketArn string, repoURL string, stepName string, workspace string, vpcConfig *types.VpcConfig) error
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
	region    *string
}

func NewBuilder(awsConfig aws.Config) Builder {
	return &builder{
		codeBuild: codebuild.NewFromConfig(awsConfig),
		region:    &awsConfig.Region,
	}
}

func (b *builder) CreateProject(projectName string, roleArn string, logGroup string, streamName string, bucketArn string, repoURL string, stepName string, workspace string, vpcConfig *types.VpcConfig) error {
	common.Logger.Printf("Creating CodeBuild project %s\n", projectName)
	buildSpec, err := yaml.Marshal(createBuildSpec())
	if err != nil {
		return err
	}
	_, err = b.codeBuild.CreateProject(context.Background(), &codebuild.CreateProjectInput{
		Name:             aws.String(projectName),
		TimeoutInMinutes: aws.Int32(480),
		ServiceRole:      aws.String(roleArn),
		Artifacts:        &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts},
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Small,
			Image:                    aws.String("public.ecr.aws/a3z4f8w3/entigo-infralib-base:latest"),
			Type:                     types.EnvironmentTypeLinuxContainer,
			ImagePullCredentialsType: types.ImagePullCredentialsTypeCodebuild,
			EnvironmentVariables: []types.EnvironmentVariable{{
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
			GitCloneDepth: aws.Int32(1),
			Buildspec:     aws.String(string(buildSpec)),
			Location:      &repoURL,
		},
		VpcConfig: vpcConfig,
	})
	var awsError *types.ResourceAlreadyExistsException
	if err != nil && errors.As(err, &awsError) {
		common.Logger.Printf("Project %s already exists. Continuing...\n", projectName)
		return nil
	}
	return err
}

func createBuildSpec() BuildSpec {
	return BuildSpec{
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
}
