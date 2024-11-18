package aws

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"gopkg.in/yaml.v3"
	"log"
)

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
	ctx          context.Context
	codeBuild    *codebuild.Client
	region       *string
	buildRoleArn string
	logGroup     string
	logStream    string
	bucketArn    string
	buildSpec    *string
}

func NewBuilder(ctx context.Context, awsConfig aws.Config, buildRoleArn string, logGroup string, logStream string, bucketArn string) model.Builder {
	return &builder{
		ctx:          ctx,
		codeBuild:    codebuild.NewFromConfig(awsConfig),
		region:       &awsConfig.Region,
		buildRoleArn: buildRoleArn,
		logGroup:     logGroup,
		logStream:    logStream,
		bucketArn:    bucketArn,
		buildSpec:    buildSpec(),
	}
}

func (b *builder) CreateProject(projectName string, bucket string, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig) error {
	if imageSource == "" {
		imageSource = model.ProjectImage
	}
	image := fmt.Sprintf("%s:%s", imageSource, imageVersion)
	_, err := b.codeBuild.CreateProject(b.ctx, &codebuild.CreateProjectInput{
		Name:             aws.String(projectName),
		TimeoutInMinutes: aws.Int32(480),
		ServiceRole:      aws.String(b.buildRoleArn),
		Artifacts:        &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts},
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Medium,
			Image:                    aws.String(image),
			Type:                     types.EnvironmentTypeLinuxContainer,
			ImagePullCredentialsType: types.ImagePullCredentialsTypeCodebuild,
			EnvironmentVariables:     b.getEnvironmentVariables(projectName, stepName, bucket),
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
			Buildspec: b.buildSpec,
		},
		VpcConfig: getAwsVpcConfig(vpcConfig),
	})
	var awsError *types.ResourceAlreadyExistsException
	if err != nil && errors.As(err, &awsError) {
		return b.UpdateProject(projectName, "", "", step, imageVersion, imageSource, vpcConfig)
	}
	log.Printf("Created CodeBuild project %s\n", projectName)
	return err
}

func (b *builder) getEnvironmentVariables(projectName, stepName, bucket string) []types.EnvironmentVariable {
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
		Name:  aws.String("INFRALIB_BUCKET"),
		Value: aws.String(bucket),
	}}
}

func (b *builder) CreateAgentProject(projectName string, awsPrefix string, imageVersion string, cmd common.Command) error {
	_, err := b.codeBuild.CreateProject(b.ctx, &codebuild.CreateProjectInput{
		Name:             aws.String(projectName),
		TimeoutInMinutes: aws.Int32(480),
		ServiceRole:      aws.String(b.buildRoleArn),
		Artifacts:        &types.ProjectArtifacts{Type: types.ArtifactsTypeNoArtifacts},
		Environment: &types.ProjectEnvironment{
			ComputeType:              types.ComputeTypeBuildGeneral1Small,
			Image:                    aws.String(model.AgentImage + ":" + imageVersion),
			Type:                     types.EnvironmentTypeLinuxContainer,
			ImagePullCredentialsType: types.ImagePullCredentialsTypeCodebuild,
			EnvironmentVariables:     getAgentEnvVars(awsPrefix),
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
			Buildspec: agentBuildSpec(cmd),
		},
	})
	if err == nil {
		log.Printf("Created CodeBuild project %s\n", projectName)
	}
	return err
}

func getAgentEnvVars(awsPrefix string) []types.EnvironmentVariable {
	return []types.EnvironmentVariable{
		{
			Name:  aws.String(common.AwsPrefixEnv),
			Value: aws.String(awsPrefix),
		},
	}
}

func (b *builder) GetProject(projectName string) (*model.Project, error) {
	project, err := b.getProject(projectName)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, nil
	}
	return &model.Project{
		Name:  *project.Name,
		Image: *project.Environment.Image,
	}, nil
}

func (b *builder) getProject(projectName string) (*types.Project, error) {
	projects, err := b.codeBuild.BatchGetProjects(b.ctx, &codebuild.BatchGetProjectsInput{
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

func (b *builder) UpdateAgentProject(projectName string, version string, awsPrefix string) error {
	project, err := b.getProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("project %s not found", projectName)
	}
	image := fmt.Sprintf("%s:%s", model.AgentImage, version)

	if *project.Environment.Image == image {
		return nil
	}

	project.Environment.Image = aws.String(image)
	project.Environment.EnvironmentVariables = getAgentEnvVars(awsPrefix)
	_, err = b.codeBuild.UpdateProject(b.ctx, &codebuild.UpdateProjectInput{
		Name:        aws.String(projectName),
		Environment: project.Environment,
	})
	return err
}

func (b *builder) UpdateProject(projectName, _, _ string, _ model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig) error {
	project, err := b.getProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("project %s not found", projectName)
	}

	awsVpcConfig := getAwsVpcConfig(vpcConfig)
	vpcChanged := awsVpcConfig != nil && (project.VpcConfig == nil ||
		(project.VpcConfig.VpcId == nil || *project.VpcConfig.VpcId != *awsVpcConfig.VpcId) ||
		!util.EqualLists(project.VpcConfig.Subnets, awsVpcConfig.Subnets) ||
		!util.EqualLists(project.VpcConfig.SecurityGroupIds, awsVpcConfig.SecurityGroupIds))

	if imageSource == "" {
		imageSource = model.ProjectImage
	}
	image := fmt.Sprintf("%s:%s", imageSource, imageVersion)
	imageChanged := image != "" && project.Environment != nil && project.Environment.Image != nil &&
		*project.Environment.Image != image

	if !vpcChanged && !imageChanged {
		return nil
	}

	if !vpcChanged {
		awsVpcConfig = project.VpcConfig
	}
	if imageChanged {
		project.Environment.Image = aws.String(image)
	}

	_, err = b.codeBuild.UpdateProject(b.ctx, &codebuild.UpdateProjectInput{
		Name:        aws.String(projectName),
		VpcConfig:   awsVpcConfig,
		Environment: project.Environment,
	})

	if err != nil {
		return fmt.Errorf("failed to update CodeBuild project %s: %w", projectName, err)
	}

	if awsVpcConfig != nil && awsVpcConfig.VpcId != nil {
		log.Printf("updated CodeBuild project %s image to %s and vpc to %s\n", projectName, image,
			*awsVpcConfig.VpcId)
	} else if imageChanged {
		log.Printf("updated CodeBuild project %s image to %s\n", projectName, image)
	}
	return nil
}

func (b *builder) DeleteProject(projectName string, _ model.Step) error {
	_, err := b.codeBuild.DeleteProject(b.ctx, &codebuild.DeleteProjectInput{
		Name: aws.String(projectName),
	})
	if err != nil {
		var awsError *types.ResourceNotFoundException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	log.Printf("Deleted CodeBuild project %s\n", projectName)
	return nil
}

func getAwsVpcConfig(vpcConfig *model.VpcConfig) *types.VpcConfig {
	if vpcConfig == nil {
		return nil
	}
	return &types.VpcConfig{
		SecurityGroupIds: vpcConfig.SecurityGroupIds,
		Subnets:          vpcConfig.Subnets,
		VpcId:            vpcConfig.VpcId,
	}
}

func buildSpec() *string {
	spec := BuildSpec{
		Version: "0.2",
		Phases: Phases{
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

func agentBuildSpec(cmd common.Command) *string {
	spec := BuildSpec{
		Version: "0.2",
		Phases: Phases{
			Build: Build{
				Commands: []string{fmt.Sprintf("cd /etc/ei-agent && /usr/bin/ei-agent %s", cmd)},
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
		log.Fatalf("Failed to marshal buildspec: %s", err)
	}
	specString := string(buildSpec)
	return &specString
}
