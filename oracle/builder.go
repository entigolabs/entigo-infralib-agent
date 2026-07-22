package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

// Builder is the Oracle implementation of the agent's project/run-spec registry
// (the model.Builder role AWS fills with CodeBuild). OCI has no persistent job
// definition, so CreateProject/UpdateProject record the run spec (image,
// networking, auth) that the Pipeline replays when it launches an execution.
// Specs are persisted to the config bucket so destroy/delete and agent flows work
// from a fresh process, and cached in-memory behind a mutex because steps run in
// parallel goroutines. Execution itself runs through OCI DevOps build pipelines
// (DevOpsBuilder); Builder owns the run spec and the container environment and
// delegates launch/wait to that backend.
type Builder struct {
	ctx               context.Context
	configStore       objectStore
	compartmentId     string
	region            string
	bucket            string
	s3Endpoint        string
	accessKey         string
	secretKey         string
	enableOpenTofu    bool
	terraformCache    bool
	cloudPrefix       string
	devopsBuild       *DevOpsBuilder
	campaignId        string
	pipelineIndex     int
	mu                sync.Mutex
	projects          map[string]*containerProject
	wrapperConfigOnce sync.Once
	wrapperConfig     string
}

// SetCampaignId / SetPipelineIndex store the portal campaign correlation that
// containerEnv bakes into each step's per-run env file (CAMPAIGN_ID /
// PIPELINE_INDEX), where the wrapper reads them like every other provider. Empty
// campaignId means no active campaign — the wrapper then runs transparently.
func (b *Builder) SetCampaignId(id string) {
	b.campaignId = id
}

func (b *Builder) SetPipelineIndex(index int) {
	b.pipelineIndex = index
}

// containerProject is persisted as JSON in the config bucket (projectSpecFormat).
// AuthSources carries git credentials; the config bucket already holds the SSM
// secrets/ tree and the customer secret key, so the trust boundary is unchanged.
type containerProject struct {
	Image       string                      `json:"image"`
	StepType    model.StepType              `json:"stepType,omitempty"`
	VpcConfig   *model.VpcConfig            `json:"vpcConfig,omitempty"`
	AuthSources map[string]model.SourceAuth `json:"authSources,omitempty"`
	AgentCmd    common.Command              `json:"agentCmd,omitempty"` // set for agent projects
}

const projectSpecFormat = "projects/%s"

func NewBuilder(ctx context.Context, configStore objectStore, region, compartmentId, bucket, s3Endpoint, accessKey, secretKey string, enableOpenTofu, terraformCache bool, cloudPrefix string) *Builder {
	return &Builder{
		ctx:            ctx,
		configStore:    configStore,
		compartmentId:  compartmentId,
		region:         region,
		bucket:         bucket,
		s3Endpoint:     s3Endpoint,
		accessKey:      accessKey,
		secretKey:      secretKey,
		enableOpenTofu: enableOpenTofu,
		terraformCache: terraformCache,
		cloudPrefix:    cloudPrefix,
		projects:       map[string]*containerProject{},
	}
}

func getImage(imageVersion, imageSource string) string {
	if imageSource == "" {
		imageSource = model.ProjectImageOracle
	}
	return fmt.Sprintf("%s:%s", imageSource, imageVersion)
}

func (b *Builder) CreateProject(projectName, _, _ string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	return b.putProject(projectName, &containerProject{
		Image:       getImage(imageVersion, imageSource),
		StepType:    step.Type,
		VpcConfig:   vpcConfig,
		AuthSources: authSources,
	})
}

func (b *Builder) UpdateProject(projectName, repoURL, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	return b.CreateProject(projectName, repoURL, stepName, step, imageVersion, imageSource, vpcConfig, authSources)
}

func (b *Builder) CreateAgentProject(projectName string, _ string, imageVersion string, cmd common.Command) error {
	return b.putProject(projectName, &containerProject{
		Image:    getImage(imageVersion, model.AgentImageOracle),
		AgentCmd: cmd,
	})
}

func (b *Builder) UpdateAgentProject(projectName, version, _ string) error {
	project, err := b.getProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return b.CreateAgentProject(projectName, b.cloudPrefix, version, common.RunCommand)
	}
	updated := *project
	updated.Image = getImage(version, model.AgentImageOracle)
	return b.putProject(projectName, &updated)
}

func (b *Builder) GetProject(projectName string) (*model.Project, error) {
	project, err := b.getProject(projectName)
	if err != nil || project == nil {
		return nil, err
	}
	return &model.Project{
		Name:           projectName,
		Image:          project.Image,
		TerraformCache: strconv.FormatBool(b.terraformCache),
	}, nil
}

func (b *Builder) DeleteProject(projectName string, _ model.Step) error {
	b.mu.Lock()
	delete(b.projects, projectName)
	b.mu.Unlock()
	return b.configStore.DeleteFile(fmt.Sprintf(projectSpecFormat, projectName))
}

func (b *Builder) putProject(projectName string, project *containerProject) error {
	data, err := json.Marshal(project)
	if err != nil {
		return err
	}
	if err = b.configStore.PutFile(fmt.Sprintf(projectSpecFormat, projectName), data); err != nil {
		return fmt.Errorf("failed to persist project spec %s: %w", projectName, err)
	}
	b.mu.Lock()
	b.projects[projectName] = project
	b.mu.Unlock()
	return nil
}

// wrapperConfigValue returns the portal wrapper config yaml that
// upsertWrapperConfig stored in the config bucket, or "" when portal
// notifications aren't configured. Read once per process: it is written before
// any pipeline runs and stable afterwards.
func (b *Builder) wrapperConfigValue() string {
	b.wrapperConfigOnce.Do(func() {
		content, err := b.configStore.GetFile(secretKey(model.WrapperConfigSecretName(b.cloudPrefix)))
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to read wrapper config secret: %s", err)))
			return
		}
		b.wrapperConfig = string(content)
	})
	return b.wrapperConfig
}

// getProject returns the cached spec, falling back to the persisted copy so
// destroy/delete and agent flows work in processes that never called CreateProject.
func (b *Builder) getProject(projectName string) (*containerProject, error) {
	b.mu.Lock()
	project, ok := b.projects[projectName]
	b.mu.Unlock()
	if ok {
		return project, nil
	}
	content, err := b.configStore.GetFile(fmt.Sprintf(projectSpecFormat, projectName))
	if err != nil || content == nil {
		return nil, err
	}
	loaded := &containerProject{}
	if err = json.Unmarshal(content, loaded); err != nil {
		return nil, fmt.Errorf("failed to parse project spec %s: %w", projectName, err)
	}
	b.mu.Lock()
	b.projects[projectName] = loaded
	b.mu.Unlock()
	return loaded, nil
}

// launch runs the given command for a step as an OCI DevOps build run and returns
// its OCID (consumed by waitForCompletion).
func (b *Builder) launch(projectName, prefixStep string, command model.ActionCommand, step model.Step) (string, error) {
	displayName := runName(projectName, command)
	project, err := b.getProject(projectName)
	if err != nil {
		return "", err
	}
	if project == nil {
		return "", fmt.Errorf("no project registered for %s", projectName)
	}
	env := b.containerEnv(prefixStep, command, step, project)
	log.Printf("Executing build run %s\n", displayName)
	return b.devopsBuild.launchBuildRun(displayName, project.Image, env)
}

// runName is the display name shared by a step+command's build pipeline and its
// build runs. Agent projects already carry the command suffix in their name and
// launch with an empty command.
func runName(projectName string, command model.ActionCommand) string {
	if command == "" {
		return projectName
	}
	return fmt.Sprintf("%s-%s", projectName, command)
}

// waitForCompletion waits for a step's build run to finish and returns its
// process-style exit code (0 = SUCCEEDED, non-zero = failed).
func (b *Builder) waitForCompletion(buildRunId string) (int, error) {
	return b.devopsBuild.waitForBuildRun(buildRunId)
}

// containerEnv mirrors service.LocalPipeline.getEnv so the base image behaves
// identically whether a step runs locally or as a docker container inside a
// DevOps build run.
func (b *Builder) containerEnv(prefixStep string, command model.ActionCommand, step model.Step, project *containerProject) map[string]string {
	env := map[string]string{
		"COMMAND":                     string(command),
		"TF_VAR_prefix":               prefixStep,
		"INFRALIB_BUCKET":             b.bucket,
		model.OracleRegion:            b.region,
		common.OracleCompartmentIdEnv: b.compartmentId,
		"AWS_REGION":                  b.region,
		"AWS_ENDPOINT_URL_S3":         b.s3Endpoint,
		// The OpenTofu/Terraform oci provider defaults its `auth` argument to
		// ApiKey (env MultiEnvDefaultFunc over TF_VAR_auth/OCI_AUTH), which in the
		// step container finds no ~/.oci/config and fails with "did not find a
		// proper configuration for tenancy". In-container the only credential is
		// the build runner's resource principal (its OCI_RESOURCE_PRINCIPAL_* vars
		// are forwarded into the container by the build spec), so select it
		// explicitly. Region comes from OCI_REGION, set above via
		// model.OracleRegion — without it RP auth pins the provider to the tenancy
		// home region.
		"OCI_AUTH": "ResourcePrincipal",
	}
	if step.Name != "" {
		env["INFRALIB_STEP"] = step.Name
	}
	if project.AgentCmd == "" {
		if config := b.wrapperConfigValue(); config != "" {
			env[model.WrapperConfigEnv] = config
		}
		// Campaign correlation rides the per-run env file like every other value
		// (the build definition stays static because the whole env is delivered
		// out-of-band via the PAR'd file, not baked into the pipeline). Empty
		// campaignId → env vars omitted → wrapper runs transparently.
		if b.campaignId != "" {
			env["CAMPAIGN_ID"] = b.campaignId
			env["PIPELINE_INDEX"] = strconv.Itoa(b.pipelineIndex)
		}
	}
	if project.AgentCmd != "" {
		// The relaunched agent selects the Oracle provider via the compartment id
		// and derives every bucket name from the prefix; without these it would
		// fall through to the AWS provider.
		env["COMMAND"] = string(project.AgentCmd)
		env[common.PrefixEnv] = b.cloudPrefix
	}
	if b.accessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = b.accessKey
		env["AWS_SECRET_ACCESS_KEY"] = b.secretKey
	}
	for source, auth := range project.AuthSources {
		hash := util.HashCode(source)
		env[fmt.Sprintf(model.GitSourceEnvFormat, hash)] = source
		env[fmt.Sprintf(model.GitUsernameEnvFormat, hash)] = auth.Username
		env[fmt.Sprintf(model.GitPasswordEnvFormat, hash)] = auth.Password
	}
	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			env["KUBERNETES_CLUSTER_NAME"] = step.KubernetesClusterName
		}
		if step.ArgocdNamespace == "" {
			env["ARGOCD_NAMESPACE"] = "argocd"
		} else {
			env["ARGOCD_NAMESPACE"] = step.ArgocdNamespace
		}
	}
	if step.Type == model.StepTypeTerraform {
		env["TERRAFORM_CACHE"] = fmt.Sprintf("%t", b.terraformCache)
		if b.enableOpenTofu {
			env["TF_TOOL"] = string(model.TofuTfTool)
		}
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				name := strings.ToUpper(module.Name)
				env[fmt.Sprintf("GIT_AUTH_USERNAME_%s", name)] = module.HttpUsername
				env[fmt.Sprintf("GIT_AUTH_PASSWORD_%s", name)] = module.HttpPassword
				env[fmt.Sprintf("GIT_AUTH_SOURCE_%s", name)] = module.Source
			}
		}
	}
	return env
}
