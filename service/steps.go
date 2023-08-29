package service

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	dynamoDBTypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"gopkg.in/yaml.v3"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const stateFile = "state.yaml"

type Steps interface {
	ProcessSteps()
}

type steps struct {
	config    model.Config
	resources awsResources
	version   string
}

type awsResources struct {
	codeCommit    CodeCommit
	codePipeline  Pipeline
	codeBuild     Builder
	ssm           SSM
	bucket        string
	s3Arn         string
	dynamoDBTable *dynamoDBTypes.TableDescription
	logGroup      string
	logStream     string
	buildRoleArn  string
}

func NewSteps(awsConfig aws.Config, accountId string, flags *common.Flags) Steps {
	resources := setupResources(awsConfig, accountId, flags.AWSPrefix, flags.Branch)
	return &steps{
		config:    getConfig(flags.Config, resources.codeCommit),
		resources: resources,
		version:   flags.InitVersion,
	}
}

func setupResources(awsConfig aws.Config, accountId string, prefix string, branch string) awsResources {
	codeCommit := setupCodeCommit(awsConfig, accountId, prefix, branch)
	repoMetadata := codeCommit.GetRepoMetadata()

	s3 := NewS3(awsConfig)
	bucket := fmt.Sprintf("%s-%s", prefix, accountId)
	s3Arn, err := s3.CreateBucket(bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create S3 bucket: %s", err)
	}

	dynamoDBTable, err := CreateDynamoDBTable(awsConfig, fmt.Sprintf("%s-%s", prefix, accountId))
	if err != nil {
		common.Logger.Fatalf("Failed to create DynamoDB table: %s", err)
	}

	cloudwatch := NewCloudWatch(awsConfig)
	logGroup := fmt.Sprintf("log-%s", prefix)
	logGroupArn, err := cloudwatch.CreateLogGroup(logGroup)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log group: %s", err)
	}
	logStream := fmt.Sprintf("log-%s", prefix)
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}

	iam := NewIAM(awsConfig)

	buildRoleName := fmt.Sprintf("%s-build", prefix)
	buildRole := iam.CreateRole(buildRoleName, []PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codebuild.amazonaws.com"},
	}})
	if buildRole != nil {
		err = iam.AttachRolePolicy("arn:aws:iam::aws:policy/AdministratorAccess", *buildRole.RoleName)
		if err != nil {
			common.Logger.Fatalf("Failed to attach admin policy to role %s: %s", *buildRole.RoleName, err)
		}
		buildPolicy := iam.CreatePolicy(buildRoleName,
			CodeBuildPolicy(logGroupArn, s3Arn, *repoMetadata.Arn, *dynamoDBTable.TableArn))
		err = iam.AttachRolePolicy(*buildPolicy.Arn, *buildRole.RoleName)
		if err != nil {
			common.Logger.Fatalf("Failed to attach build policy to role %s: %s", *buildRole.RoleName, err)
		}
	} else {
		buildRole = iam.GetRole(buildRoleName)
	}

	pipelineRoleName := fmt.Sprintf("%s-pipeline", prefix)
	pipelineRole := iam.CreateRole(pipelineRoleName, []PolicyStatement{{
		Effect:    "Allow",
		Action:    []string{"sts:AssumeRole"},
		Principal: map[string]string{"Service": "codepipeline.amazonaws.com"},
	}})
	if pipelineRole != nil {
		pipelinePolicy := iam.CreatePolicy(pipelineRoleName, CodePipelinePolicy(s3Arn, *repoMetadata.Arn))
		err = iam.AttachRolePolicy(*pipelinePolicy.Arn, *pipelineRole.RoleName)
		if err != nil {
			common.Logger.Fatalf("Failed to attach pipeline policy to role %s: %s", *pipelineRole.RoleName, err)
		}
	} else {
		pipelineRole = iam.GetRole(pipelineRoleName)
	}
	common.Logger.Println("Waiting for roles to be available...")
	time.Sleep(10 * time.Second)

	ssm := NewSSM(awsConfig)

	codeBuild := NewBuilder(awsConfig)
	codePipeline := NewPipeline(awsConfig, *repoMetadata.RepositoryName, branch, *pipelineRole.Arn, bucket, cloudwatch, logGroup, logStream)

	return awsResources{
		codeCommit:    codeCommit,
		codePipeline:  codePipeline,
		codeBuild:     codeBuild,
		ssm:           ssm,
		bucket:        bucket,
		s3Arn:         s3Arn,
		dynamoDBTable: dynamoDBTable,
		logGroup:      logGroup,
		logStream:     logStream,
		buildRoleArn:  *buildRole.Arn,
	}
}

func setupCodeCommit(awsConfig aws.Config, accountID string, prefix string, branch string) CodeCommit {
	repoName := fmt.Sprintf("%s-%s", prefix, accountID)
	codeCommit := NewCodeCommit(awsConfig, repoName, branch)
	err := codeCommit.CreateRepository()
	if err != nil {
		common.Logger.Fatalf("Failed to create CodeCommit repository: %s", err)
	}
	codeCommit.PutFile("README.md", []byte("# Entigo infralib repository\nThis is the README file."))
	return codeCommit
}

func getConfig(configFile string, codeCommit CodeCommit) model.Config {
	if configFile != "" {
		config := GetConfig(configFile)
		bytes, err := yaml.Marshal(config)
		if err != nil {
			common.Logger.Fatalf("Failed to marshal config: %s", err)
		}
		codeCommit.PutFile("config.yaml", bytes)
		return config
	}
	bytes := codeCommit.GetFile("config.yaml")
	if bytes == nil {
		common.Logger.Fatalf("Config file not found")
	}
	var config model.Config
	err := yaml.Unmarshal(bytes, &config)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal config: %s", err)
	}
	return config
}

func (s *steps) ProcessSteps() {
	state := s.getLatestState()
	releases := s.getReleases(state)

	firstRun := true
	for _, release := range releases {
		common.Logger.Printf("Applying infralib release %s\n", release.Tag)
		if firstRun {
			s.createStepsFiles(release.Tag)
			s.createExecuteStepsPipelines(release.Tag, state)
			firstRun = false
		} else {
			s.updateStepsFiles(release.Tag)
			s.executeStepsPipelines(release.Tag, state)
		}
		state = s.putStateFile(release, state)
		common.Logger.Printf("Release %s applied successfully\n", release.Tag)
	}
}

func (s *steps) createStepsFiles(releaseTag string) {
	for _, step := range s.config.Steps {
		switch step.Type {
		case "terraform":
			s.createTerraformFiles(step, releaseTag)
		case "argocd-apps":
			s.createArgoCDFiles(step)
		}
	}
}

func (s *steps) updateStepsFiles(releaseTag string) {
	for _, step := range s.config.Steps {
		switch step.Type {
		case "terraform":
			s.createTerraformMain(step, releaseTag)
		}
	}
}

func (s *steps) createExecuteStepsPipelines(releaseTag string, state *model.State) {
	repoMetadata := s.resources.codeCommit.GetRepoMetadata()

	for _, step := range s.config.Steps {
		stepName := fmt.Sprintf("%s-%s", s.config.Prefix, step.Name)
		projectName := fmt.Sprintf("%s-%s", stepName, step.Workspace)

		vpcConfig := s.getVpcConfig(step.VpcPrefix, step.Workspace)
		err := s.resources.codeBuild.CreateProject(projectName, s.resources.buildRoleArn, s.resources.logGroup,
			s.resources.logStream, s.resources.s3Arn, *repoMetadata.CloneUrlHttp, stepName, step.Workspace, vpcConfig)
		if err != nil {
			common.Logger.Fatalf("Failed to create CodeBuild project: %s", err)
		}
		autoApprove := getAutoApprove(releaseTag, state, step.Approve)

		switch step.Type {
		case "terraform":
			s.createExecuteTerraformPipelines(projectName, stepName, step, autoApprove)
		case "argocd-apps":
			s.createExecuteArgoCDPipelines(projectName, stepName, step, autoApprove)
		}
	}
}

func getAutoApprove(releaseTag string, state *model.State, approve model.Approve) bool {
	if state == nil || approve == model.ApproveNever {
		return true
	}
	if approve == "" || approve == model.ApproveAlways {
		return false
	}
	releaseMajor, releaseMinor := getMajorMinorVersions(releaseTag)
	stateMajor, stateMinor := getMajorMinorVersions(state.Version)
	if approve == model.ApproveMajor {
		return releaseMajor > stateMajor
	}
	if approve == model.ApproveMinor {
		return releaseMajor > stateMajor || releaseMinor > stateMinor
	}
	return false
}

func getMajorMinorVersions(version string) (int, int) {
	re := regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+$`)
	matches := re.FindStringSubmatch(version)
	if len(matches) == 3 {
		major, err := strconv.Atoi(matches[1])
		if err != nil {
			common.Logger.Fatalf("Failed to convert major version %s to int: %s", version, err)
		}
		minor, err := strconv.Atoi(matches[1])
		if err != nil {
			common.Logger.Fatalf("Failed to convert minor version %s to int: %s", version, err)
		}
		return major, minor
	}
	common.Logger.Fatalf("Failed to get major and minor versions from tag %s", version)
	return 0, 0
}

func (s *steps) createExecuteTerraformPipelines(projectName string, stepName string, step model.Steps, autoApprove bool) {
	err := s.resources.codePipeline.CreateTerraformPipeline(projectName, projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodePipeline: %s", err)
	}
	err = s.resources.codePipeline.CreateTerraformDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create destroy CodePipeline: %s", err)
	}
	err = s.resources.codePipeline.WaitPipelineExecution(projectName, autoApprove, 30)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
	}
}

func (s *steps) createExecuteArgoCDPipelines(projectName string, stepName string, step model.Steps, autoApprove bool) {
	err := s.resources.codePipeline.CreateArgoCDPipeline(projectName, projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodePipeline: %s", err)
	}
	err = s.resources.codePipeline.CreateArgoCDDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create destroy CodePipeline: %s", err)
	}
	err = s.resources.codePipeline.WaitPipelineExecution(projectName, autoApprove, 30)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
	}
}

func (s *steps) executeStepsPipelines(releaseTag string, state *model.State) {
	for _, step := range s.config.Steps {
		projectName := fmt.Sprintf("%s-%s-%s", s.config.Prefix, step.Name, step.Workspace)
		err := s.resources.codePipeline.StartPipelineExecution(projectName)
		if err != nil {
			common.Logger.Fatalf("Failed to start pipeline execution: %s", err)
		}
		autoApprove := getAutoApprove(releaseTag, state, step.Approve)
		err = s.resources.codePipeline.WaitPipelineExecution(projectName, autoApprove, 30)
		if err != nil {
			common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
		}
	}
}

func (s *steps) getLatestState() *model.State {
	file := s.resources.codeCommit.GetFile(stateFile)
	if file == nil {
		return nil
	}
	var state model.State
	err := yaml.Unmarshal(file, &state)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal state file: %s", err)
	}
	return &state
}

func (s *steps) getReleases(state *model.State) []Release {
	githubClient := NewGithub(s.config.Source)
	if state != nil {
		return getReleasesByState(*state, githubClient)
	} else if s.version != "" {
		return getRelease(githubClient.GetReleaseByTag(s.version))
	} else {
		return getRelease(githubClient.GetLatestReleaseTag())
	}

}

func getReleasesByState(state model.State, githubClient Github) []Release {
	releases, err := githubClient.GetNewerReleases(state.VersionPublishedAt)
	if err != nil {
		common.Logger.Fatalf("Failed to get newer releases: %s", err)
	}
	if len(releases) > 0 {
		return releases
	}
	return getRelease(githubClient.GetLatestReleaseTag())
}

func getRelease(release *Release, err error) []Release {
	if err != nil {
		common.Logger.Fatalf("Failed to get release: %s", err)
	}
	return []Release{*release}
}

func (s *steps) getVpcConfig(vpcPrefix string, workspace string) *types.VpcConfig {
	if vpcPrefix == "" {
		return nil
	}
	common.Logger.Printf("Getting VPC config for %s-%s\n", vpcPrefix, workspace)
	vpcId, err := s.resources.ssm.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/vpc_id", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get VPC ID: %s", err)
	}
	subnetIds, err := s.resources.ssm.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/private_subnets", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get subnet IDs: %s", err)
	}
	securityGroupIds, err := s.resources.ssm.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/pipeline_security_group", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get security group IDs: %s", err)
	}
	return &types.VpcConfig{
		SecurityGroupIds: strings.Split(securityGroupIds, ","),
		Subnets:          strings.Split(subnetIds, ","),
		VpcId:            aws.String(vpcId),
	}
}

func (s *steps) createTerraformFiles(step model.Steps, releaseTag string) {
	provider, err := terraform.GetTerraformProvider(step)
	if err != nil {
		common.Logger.Fatalf("Failed to create terraform provider: %s", err)
	}
	s.resources.codeCommit.PutFile(fmt.Sprintf("%s-%s/%s/provider.tf", s.config.Prefix, step.Name, step.Workspace), provider)
	s.createTerraformMain(step, releaseTag)
	s.createBackendConf(fmt.Sprintf("%s-%s", s.config.Prefix, step.Name))
}

func (s *steps) createArgoCDFiles(step model.Steps) {
	for _, module := range step.Modules {
		inputs := module.Inputs
		if len(inputs) == 0 {
			continue
		}
		yamlBytes, err := yaml.Marshal(inputs)
		if err != nil {
			common.Logger.Fatalf("Failed to marshal helm values: %s", err)
		}
		s.resources.codeCommit.PutFile(fmt.Sprintf("%s-%s/%s/%s-values.yaml", s.config.Prefix, step.Name, step.Workspace, module.Name),
			yamlBytes)
	}
}

func (s *steps) createBackendConf(path string) {
	bytes, err := util.CreateKeyValuePairs(map[string]string{
		"bucket":         s.resources.bucket,
		"key":            fmt.Sprintf("%s/terraform.tfstate", path),
		"dynamodb_table": *s.resources.dynamoDBTable.TableName,
		"encrypt":        "true",
	}, "", "")
	if err != nil {
		common.Logger.Fatalf("Failed to convert backend config values: %s", err)
	}
	s.resources.codeCommit.PutFile(fmt.Sprintf("%s/backend.conf", path), bytes)
}

func (s *steps) putStateFile(release Release, state *model.State) *model.State {
	if state == nil {
		state = &model.State{}
	}
	state.Version = release.Tag
	state.VersionPublishedAt = release.PublishedAt
	state.VersionAppliedAt = time.Now()
	bytes, err := yaml.Marshal(state)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal state: %s", err)
	}
	s.resources.codeCommit.PutFile(stateFile, bytes)
	return state
}

func (s *steps) createTerraformMain(step model.Steps, releaseTag string) {
	main, err := terraform.GetTerraformMain(step, s.config, releaseTag)
	if err != nil {
		common.Logger.Fatalf("Failed to create terraform main: %s", err)
	}
	s.resources.codeCommit.PutFile(fmt.Sprintf("%s-%s/%s/main.tf", s.config.Prefix, step.Name, step.Workspace), main)
}
