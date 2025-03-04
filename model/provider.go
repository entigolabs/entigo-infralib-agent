package model

import "github.com/entigolabs/entigo-infralib-agent/common"

const ProjectImage = "public.ecr.aws/entigolabs/entigo-infralib-base"
const ProjectImageDocker = "docker.io/entigolabs/entigo-infralib-base"
const AgentImage = "public.ecr.aws/entigolabs/entigo-infralib-agent"
const AgentImageDocker = "docker.io/entigolabs/entigo-infralib-agent"
const LatestImageVersion = "latest"
const AgentSource = "agent-source.zip"

type ProviderType string

const (
	AWS    ProviderType = "AWS"
	GCLOUD ProviderType = "GCLOUD"
)

const (
	ResourceTagKey   = "created-by"
	ResourceTagValue = "entigo-infralib-agent"
)

type CloudProvider interface {
	SetupResources() Resources
	GetResources() Resources
	DeleteResources(deleteBucket bool, deleteServiceAccount bool)
	CreateServiceAccount()
}

type ResourceProvider interface {
	GetSSM() SSM
}

type Resources interface {
	GetProviderType() ProviderType
	GetBucket() Bucket
	GetPipeline() Pipeline
	GetBuilder() Builder
	GetSSM() SSM
	GetCloudPrefix() string
	GetBucketName() string
	GetBackendConfigVars(string) map[string]string
	GetRegion() string
}

type Bucket interface {
	GetRepoMetadata() (*RepositoryMetadata, error)
	PutFile(file string, content []byte) error
	GetFile(file string) ([]byte, error)
	DeleteFile(file string) error
	DeleteFiles(files []string) error
	CheckFolderExists(folder string) (bool, error)
	ListFolderFiles(folder string) ([]string, error)
	ListFolderFilesWithExclude(folder string, excludeFolders Set[string]) ([]string, error)
	Delete() error
}

type Pipeline interface {
	CreatePipeline(projectName, stepName string, step Step, bucket Bucket) (*string, error)
	CreateAgentPipelines(prefix, projectName, bucket string) error
	UpdatePipeline(pipelineName, stepName string, step Step, bucket string) error
	StartAgentExecution(pipelineName string) error
	StartPipelineExecution(pipelineName, stepName string, step Step, customRepo string) (*string, error)
	WaitPipelineExecution(pipelineName, projectName string, executionId *string, autoApprove bool, step Step) error
	DeletePipeline(projectName string) error
	StartDestroyExecution(projectName string, step Step) error
}

type Builder interface {
	CreateProject(projectName, repoURL, stepName string, step Step, imageVersion, imageSource string, vpcConfig *VpcConfig) error
	CreateAgentProject(projectName string, awsPrefix string, imageVersion string, cmd common.Command) error
	GetProject(projectName string) (*Project, error)
	UpdateAgentProject(projectName, version, cloudPrefix string) error
	UpdateProject(projectName, repoURL, stepName string, step Step, imageVersion, imageSource string, vpcConfig *VpcConfig) error
	DeleteProject(projectName string, step Step) error
}

type SSM interface {
	GetParameter(name string) (*Parameter, error)
	ParameterExists(name string) (bool, error)
	PutParameter(name string, value string) error
	ListParameters() ([]string, error)
	DeleteParameter(name string) error
}

type Destination interface {
	UpdateFiles(branch, folder string, files map[string]File) error
}

type CloudResources struct {
	ProviderType ProviderType
	Bucket       Bucket
	Pipeline     Pipeline
	CodeBuild    Builder
	SSM          SSM
	CloudPrefix  string
	BucketName   string
	Region       string
}

func (c CloudResources) GetProviderType() ProviderType {
	return c.ProviderType
}

func (c CloudResources) GetBucket() Bucket {
	return c.Bucket
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

func (c CloudResources) GetCloudPrefix() string {
	return c.CloudPrefix
}

func (c CloudResources) GetBucketName() string {
	return c.BucketName
}

func (c CloudResources) GetRegion() string {
	return c.Region
}

type RepositoryMetadata struct {
	Name string
	URL  string
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

type ActionCommand string

const (
	PlanCommand               ActionCommand = "plan"
	ApplyCommand              ActionCommand = "apply"
	PlanDestroyCommand        ActionCommand = "plan-destroy"
	ApplyDestroyCommand       ActionCommand = "apply-destroy"
	ArgoCDPlanCommand         ActionCommand = "argocd-plan"
	ArgoCDApplyCommand        ActionCommand = "argocd-apply"
	ArgoCDPlanDestroyCommand  ActionCommand = "argocd-plan-destroy"
	ArgoCDApplyDestroyCommand ActionCommand = "argocd-apply-destroy"
)

func GetCommands(stepType StepType) (ActionCommand, ActionCommand) {
	switch stepType {
	case StepTypeArgoCD:
		return ArgoCDPlanCommand, ArgoCDApplyCommand
	default:
		return PlanCommand, ApplyCommand
	}
}

func GetDestroyCommands(stepType StepType) (ActionCommand, ActionCommand) {
	switch stepType {
	case StepTypeArgoCD:
		return ArgoCDPlanDestroyCommand, ArgoCDApplyDestroyCommand
	default:
		return PlanDestroyCommand, ApplyDestroyCommand
	}
}

type Parameter struct {
	Value *string
	Type  string
}

type PipelineChanges struct {
	Changed   int
	Destroyed int
	NoChanges bool
}
