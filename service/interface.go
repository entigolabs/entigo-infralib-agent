package service

import (
	"github.com/aws/aws-sdk-go-v2/service/codecommit/types"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type CloudProvider interface {
	SetupResources(branch string) Resources
	SetupCustomCodeCommit(branch string) (CodeRepo, error)
}

type Resources interface {
	GetCodeRepo() CodeRepo
	GetPipeline() Pipeline
	GetBuilder() Builder
	GetSSM() SSM
	GetIAM() IAM
	GetCloudPrefix() string
	GetBucket() string
	GetAccountId() string
	GetDynamoDBTable() string
}

type CodeRepo interface {
	CreateRepository() (bool, error)
	GetLatestCommitId() (*string, error)
	GetRepoMetadata() (*types.RepositoryMetadata, error)
	PutFile(file string, content []byte) error
	GetFile(file string) ([]byte, error)
	DeleteFile(file string) error
	CheckFolderExists(folder string) (bool, error)
	ListFolderFiles(folder string) ([]string, error)
}

type Pipeline interface {
	CreateTerraformPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) (*string, error)
	CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) error
	CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, step model.Step) (*string, error)
	CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step) error
	CreateAgentPipeline(prefix string, pipelineName string, projectName string, bucket string) error
	UpdatePipeline(pipelineName string, stepName string, step model.Step) error
	StartPipelineExecution(pipelineName string) (*string, error)
	WaitPipelineExecution(pipelineName string, executionId *string, autoApprove bool, delay int, stepType model.StepType) error
}

type Builder interface {
	CreateProject(projectName string, repoURL string, stepName string, workspace string, imageVersion string, vpcConfig *VpcConfig) error
	CreateAgentProject(projectName string, awsPrefix string, image string) error
	GetProject(projectName string) (*Project, error)
	UpdateProject(projectName string, image string, vpcConfig *VpcConfig) error
}

type SSM interface {
	GetParameter(name string) (*ssmTypes.Parameter, error)
}

type IAM interface {
	AttachRolePolicy(policyArn string, roleName string) error
	CreatePolicy(policyName string, statement []PolicyStatement) *Policy
	CreateRole(roleName string, statement []PolicyStatement) *Role
	GetRole(roleName string) *Role
}

type CloudResources struct {
	CodeRepo      CodeRepo
	Pipeline      Pipeline
	CodeBuild     Builder
	SSM           SSM
	IAM           IAM
	CloudPrefix   string
	Bucket        string
	DynamoDBTable string
	AccountId     string
}

func (c CloudResources) GetCodeRepo() CodeRepo {
	return c.CodeRepo
}

func (c CloudResources) GetPipeline() Pipeline {
	return c.Pipeline
}

func (c CloudResources) GetBuilder() Builder {
	return c.CodeBuild
}

func (c CloudResources) GetSSM() SSM {
	return c.SSM
}

func (c CloudResources) GetIAM() IAM {
	return c.IAM
}

func (c CloudResources) GetCloudPrefix() string {
	return c.CloudPrefix
}

func (c CloudResources) GetBucket() string {
	return c.Bucket
}

func (c CloudResources) GetAccountId() string {
	return c.AccountId
}

func (c CloudResources) GetDynamoDBTable() string {
	return c.DynamoDBTable
}

type VpcConfig struct {
	SecurityGroupIds []string
	Subnets          []string
	VpcId            *string
}

type Project struct {
	Name  string
	Image string
}

type Policy struct {
	Arn string
}

type Role struct {
	Arn      string
	RoleName string
}
