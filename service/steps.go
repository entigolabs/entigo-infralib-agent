package service

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"gopkg.in/yaml.v3"
	"strings"
	"time"
)

type Steps interface {
	CreateStepsFiles()
	CreateStepsPipelines()
}

type steps struct {
	config     model.Config
	awsConfig  aws.Config
	awsPrefix  string
	accountId  string
	codeCommit CodeCommit
	branch     string
}

func NewSteps(awsConfig aws.Config, accountId string, flags *common.Flags) Steps {
	codeCommit := setupCodeCommit(awsConfig, accountId, flags.AWSPrefix, flags.Branch)
	return &steps{
		config:     getConfig(flags.Config, codeCommit),
		awsConfig:  awsConfig,
		awsPrefix:  flags.AWSPrefix,
		accountId:  accountId,
		codeCommit: codeCommit,
		branch:     flags.Branch,
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
	var config model.Config
	err := yaml.Unmarshal(bytes, &config)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal config: %s", err)
	}
	return config
}

func (s *steps) CreateStepsFiles() {
	releaseTag, err := GetLatestReleaseTag(s.config.Source)
	if err != nil {
		common.Logger.Fatalf("Failed to get latest release: %s", err)
	}

	for _, step := range s.config.Steps {
		switch step.Type {
		case "terraform":
			s.createTerraformFiles(step, releaseTag)
		case "argocd-apps":
			s.createArgoCDFiles(step)
		}
	}
}

func (s *steps) CreateStepsPipelines() {
	repoMetadata := s.codeCommit.GetRepoMetadata()

	s3 := NewS3(s.awsConfig)
	bucket := fmt.Sprintf("%s-%s", s.awsPrefix, s.accountId)
	s3Arn, err := s3.CreateBucket(bucket)
	if err != nil {
		common.Logger.Fatalf("Failed to create S3 bucket: %s", err)
	}

	dynamoDBTable, err := CreateDynamoDBTable(s.awsConfig, fmt.Sprintf("%s-%s", s.awsPrefix, s.accountId))
	if err != nil {
		common.Logger.Fatalf("Failed to create DynamoDB table: %s", err)
	}

	cloudwatch := NewCloudWatch(s.awsConfig)
	logGroup := fmt.Sprintf("log-%s", s.awsPrefix)
	logGroupArn, err := cloudwatch.CreateLogGroup(logGroup)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log group: %s", err)
	}
	logStream := fmt.Sprintf("log-%s", s.awsPrefix)
	err = cloudwatch.CreateLogStream(logGroup, logStream)
	if err != nil {
		common.Logger.Fatalf("Failed to create CloudWatch log stream: %s", err)
	}

	iam := NewIAM(s.awsConfig)

	buildRoleName := fmt.Sprintf("%s-build", s.awsPrefix)
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

	pipelineRoleName := fmt.Sprintf("%s-pipeline", s.awsPrefix)
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
	time.Sleep(5 * time.Second) // Wait for roles to be available

	ssm := NewSSM(s.awsConfig)

	codeBuild := NewBuilder(s.awsConfig)
	codePipeline := NewPipeline(s.awsConfig, *repoMetadata.RepositoryName, s.branch, *pipelineRole.Arn, bucket)

	for _, step := range s.config.Steps {
		stepName := fmt.Sprintf("%s-%s", s.config.Prefix, step.Name)
		projectName := fmt.Sprintf("%s-%s", stepName, step.Workspace)

		var vpcConfig *types.VpcConfig = nil
		if step.VpcPrefix != "" {
			vpcConfig = getVpcConfig(ssm, step.VpcPrefix, step.Workspace)
		}
		err = codeBuild.CreateProject(projectName, *buildRole.Arn, logGroup, logStream, s3Arn, *repoMetadata.CloneUrlHttp, vpcConfig)
		if err != nil {
			common.Logger.Fatalf("Failed to create CodeBuild project: %s", err)
		}

		switch step.Type {
		case "terraform":
			s.createBackendConf(bucket, *dynamoDBTable.TableName, stepName) // TODO This should be together with other codecommit files
			err = codePipeline.CreateTerraformPipeline(projectName, projectName, stepName, step.Workspace)
			if err != nil {
				common.Logger.Fatalf("Failed to create CodePipeline: %s", err)
			}
			err = codePipeline.CreateTerraformDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace)
			if err != nil {
				common.Logger.Fatalf("Failed to create destroy CodePipeline: %s", err)
			}
			err = codePipeline.WaitPipelineExecution(projectName, 30)
			if err != nil {
				common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
			}
		case "argocd-apps":
			err = codePipeline.CreateArgoCDPipeline(projectName, projectName, stepName, step.Workspace)
			if err != nil {
				common.Logger.Fatalf("Failed to create CodePipeline: %s", err)
			}
			err = codePipeline.CreateArgoCDDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace)
			if err != nil {
				common.Logger.Fatalf("Failed to create destroy CodePipeline: %s", err)
			}
			err = codePipeline.WaitPipelineExecution(projectName, 30)
			if err != nil {
				common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
			}
		}
	}
}

func getVpcConfig(ssm SSM, vpcPrefix string, workspace string) *types.VpcConfig {
	common.Logger.Printf("Getting VPC config for %s-%s\n", vpcPrefix, workspace)
	vpcId, err := ssm.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/vpc_id", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get VPC ID: %s", err)
	}
	subnetIds, err := ssm.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/private_subnets", vpcPrefix, workspace))
	if err != nil {
		common.Logger.Fatalf("Failed to get subnet IDs: %s", err)
	}
	securityGroupIds, err := ssm.GetParameter(fmt.Sprintf("/entigo-infralib/%s-%s/vpc/pipeline_security_group", vpcPrefix, workspace))
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
	s.codeCommit.PutFile(fmt.Sprintf("%s-%s/%s/provider.tf", s.config.Prefix, step.Name, step.Workspace), provider)
	main, err := terraform.GetTerraformMain(step, s.config, releaseTag)
	if err != nil {
		common.Logger.Fatalf("Failed to create terraform main: %s", err)
	}
	s.codeCommit.PutFile(fmt.Sprintf("%s-%s/%s/main.tf", s.config.Prefix, step.Name, step.Workspace), main)
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
		s.codeCommit.PutFile(fmt.Sprintf("%s-%s/%s/%s-values.yaml", s.config.Prefix, step.Name, step.Workspace, module.Name),
			yamlBytes)
	}
}

func (s *steps) createBackendConf(bucket string, dynamoDBTable string, path string) {
	bytes, err := util.CreateKeyValuePairs(map[string]string{
		"bucket":         bucket,
		"key":            fmt.Sprintf("%s/terraform.tfstate", path),
		"dynamodb_table": dynamoDBTable,
		"encrypt":        "true",
	}, "", "")
	if err != nil {
		common.Logger.Fatalf("Failed to convert backend config values: %s", err)
	}
	s.codeCommit.PutFile(fmt.Sprintf("%s/backend.conf", path), bytes)
}
