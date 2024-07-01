package model

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

type CloudProvider interface {
	SetupResources(branch string) Resources
	SetupCustomCodeRepo(branch string) (CodeRepo, error)
	GetResources(branch string) Resources
	DeleteResources()
}

type Resources interface {
	GetProviderType() ProviderType
	GetCodeRepo() CodeRepo
	GetPipeline() Pipeline
	GetBuilder() Builder
	GetSSM() SSM
	GetCloudPrefix() string
	GetBucket() string
	GetBackendConfigVars(string) map[string]string
}

type CodeRepo interface {
	GetRepoMetadata() (*RepositoryMetadata, error)
	PutFile(file string, content []byte) error
	GetFile(file string) ([]byte, error)
	DeleteFile(file string) error
	CheckFolderExists(folder string) (bool, error)
	ListFolderFiles(folder string) ([]string, error)
	Delete() error
}

type Pipeline interface {
	CreatePipeline(projectName string, stepName string, step Step, repo string) (*string, error)
	CreateAgentPipeline(prefix string, pipelineName string, projectName string, bucket string) error
	UpdatePipeline(pipelineName string, stepName string, step Step, bucket string) error
	StartAgentExecution(pipelineName string) error
	StartPipelineExecution(pipelineName string, stepName string, step Step, customRepo string) (*string, error)
	WaitPipelineExecution(pipelineName string, projectName string, executionId *string, autoApprove bool, stepType StepType) error
	DeletePipeline(projectName string) error
	StartDestroyExecution(projectName string) error
}

type Builder interface {
	CreateProject(projectName string, repoURL string, stepName string, step Step, imageVersion string, vpcConfig *VpcConfig) error
	CreateAgentProject(projectName string, awsPrefix string, imageVersion string) error
	GetProject(projectName string) (*Project, error)
	UpdateAgentProject(projectName string, version string, cloudPrefix string) error
	UpdateProject(projectName, repoURL, stepName string, step Step, imageVersion string, vpcConfig *VpcConfig) error
	DeleteProject(projectName string, step Step) error
}

type SSM interface {
	GetParameter(name string) (*Parameter, error)
}

type CloudResources struct {
	ProviderType ProviderType
	CodeRepo     CodeRepo
	Pipeline     Pipeline
	CodeBuild    Builder
	SSM          SSM
	CloudPrefix  string
	Bucket       string
}

func (c CloudResources) GetProviderType() ProviderType {
	return c.ProviderType
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

func (c CloudResources) GetCloudPrefix() string {
	return c.CloudPrefix
}

func (c CloudResources) GetBucket() string {
	return c.Bucket
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

type Parameter struct {
	Value *string
	Type  string
}

type TerraformChanges struct {
	Changed   int
	Destroyed int
}
