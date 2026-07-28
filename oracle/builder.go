package oracle

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"golang.org/x/sync/errgroup"
)

// Builder is the Oracle implementation of the agent's project/run-spec registry
// (the model.Builder role AWS fills with CodeBuild). OCI has no persistent job
// definition, so CreateProject/UpdateProject record the run spec (image, auth)
// in-memory only, mutex-guarded because steps run in parallel goroutines. The
// durable record is the eagerly created build pipelines (their parameters bake the
// image + non-secret env + secret OCIDs), so a fresh-process/agentless run triggers
// them by name. Builder owns the container environment and delegates launch/wait to
// DevOpsBuilder.
type Builder struct {
	ctx            context.Context
	secrets        secretResolver
	compartmentId  string
	region         string
	bucket         string
	s3Endpoint     string
	accessKey      string
	secretKey      string
	enableOpenTofu bool
	terraformCache bool
	cloudPrefix    string
	devopsBuild    *DevOpsBuilder
	campaignId     string
	pipelineIndex  int
	mu             sync.Mutex
	projects       map[string]*containerProject
}

// SetCampaignId / SetPipelineIndex store the campaign correlation that launch/trigger
// pass as per-run build-run arguments. Empty campaignId means no active campaign.
func (b *Builder) SetCampaignId(id string) {
	b.campaignId = id
}

func (b *Builder) SetPipelineIndex(index int) {
	b.pipelineIndex = index
}

// containerProject is the in-memory run spec. AuthSources carries only the
// non-secret git source + username; the password is a Vault secret resolved by name
// at launch (secretRefs).
type containerProject struct {
	Image       string
	StepType    model.StepType
	VpcConfig   *model.VpcConfig
	AuthSources map[string]model.SourceAuth
	AgentCmd    common.Command // set for agent projects
}

func NewBuilder(ctx context.Context, secrets secretResolver, region, compartmentId, bucket, s3Endpoint, accessKey, secretKey string, enableOpenTofu, terraformCache bool, cloudPrefix string) *Builder {
	return &Builder{
		ctx:            ctx,
		secrets:        secrets,
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
	b.storeProject(projectName, &containerProject{
		Image:       getImage(imageVersion, imageSource),
		StepType:    step.Type,
		VpcConfig:   vpcConfig,
		AuthSources: withoutSecrets(authSources),
	})
	return nil
}

// withoutSecrets strips git passwords from the auth sources; the password is a Vault
// secret resolved by name at launch, so it must never be cached in the run spec.
func withoutSecrets(authSources map[string]model.SourceAuth) map[string]model.SourceAuth {
	if authSources == nil {
		return nil
	}
	stripped := make(map[string]model.SourceAuth, len(authSources))
	for source, auth := range authSources {
		stripped[source] = model.SourceAuth{Username: auth.Username}
	}
	return stripped
}

func (b *Builder) UpdateProject(projectName, repoURL, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	return b.CreateProject(projectName, repoURL, stepName, step, imageVersion, imageSource, vpcConfig, authSources)
}

func (b *Builder) CreateAgentProject(projectName string, _ string, imageVersion string, cmd common.Command) error {
	b.storeProject(projectName, &containerProject{
		Image:    getImage(imageVersion, model.AgentImageOracle),
		AgentCmd: cmd,
	})
	return nil
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
	b.storeProject(projectName, &updated)
	return nil
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

// DeleteProject drops the in-memory spec. The durable record is the build
// pipelines themselves, removed separately by Pipeline.DeletePipeline.
func (b *Builder) DeleteProject(projectName string, _ model.Step) error {
	b.mu.Lock()
	delete(b.projects, projectName)
	b.mu.Unlock()
	return nil
}

func (b *Builder) storeProject(projectName string, project *containerProject) {
	b.mu.Lock()
	b.projects[projectName] = project
	b.mu.Unlock()
}

// getProject returns the in-memory spec, or nil if this process never registered it
// (a fresh destroy/agentless process triggers pipelines by name instead).
func (b *Builder) getProject(projectName string) (*containerProject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.projects[projectName], nil
}

// ensureStepPipelines eagerly creates (or reconciles) every build pipeline a step
// can run — plan, apply and their destroy variants — so any of them, destroy
// included, is triggerable from the OCI console with no agent. Each pipeline bakes
// the step's env + secret OCIDs + image as parameter defaults.
func (b *Builder) ensureStepPipelines(projectName string, step model.Step) error {
	project, err := b.getProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("no project registered for %s", projectName)
	}
	secretRefs, err := b.secretRefs(step, project)
	if err != nil {
		return err
	}
	specFile := specFileFor(projectName)
	log.Printf("Reconciling DevOps build pipelines for step %s\n", projectName)
	// Reconcile the commands concurrently — each blocks ~20s on OCI's async
	// provisioning work request, which would otherwise serialize into ~100s per step.
	var group errgroup.Group
	for _, command := range stepCommands(step.Type) {
		displayName := runName(projectName, command)
		params := b.nonSecretParams(projectName, command, step, project)
		group.Go(func() error {
			_, err := b.devopsBuild.ensurePipeline(displayName, specFile, project.Image, params, secretRefs, project.VpcConfig)
			return err
		})
	}
	return group.Wait()
}

// ensureAgentPipelines eagerly creates (or reconciles) the agent's own run and update
// build pipelines so the agent is triggerable from the console with no local agent.
// Both share one spec (agentSpecFile): COMMAND rides a pipeline parameter. The agent
// projects must already be registered (CreateAgentProject).
func (b *Builder) ensureAgentPipelines(agentPrefix string) error {
	specFile := agentSpecFile(b.cloudPrefix)
	log.Printf("Reconciling DevOps agent build pipelines for %s\n", agentPrefix)
	var group errgroup.Group
	for _, cmd := range []common.Command{common.RunCommand, common.UpdateCommand} {
		projectName := model.GetAgentProjectName(agentPrefix, cmd)
		project, err := b.getProject(projectName)
		if err != nil {
			return err
		}
		if project == nil {
			return fmt.Errorf("no agent project registered for %s", projectName)
		}
		params := b.nonSecretParams(projectName, "", model.Step{}, project)
		group.Go(func() error {
			_, err := b.devopsBuild.ensurePipeline(projectName, specFile, project.Image, params, nil, nil)
			return err
		})
	}
	return group.Wait()
}

// specFile returns the hosted-repo build-spec path for a project: the shared agent
// spec for the agent's own run/update projects, else the per-step spec.
func (b *Builder) specFile(projectName string, project *containerProject) string {
	if project.AgentCmd != "" {
		return agentSpecFile(b.cloudPrefix)
	}
	return specFileFor(projectName)
}

// stepCommands lists every action command a step can execute: the plan/apply pair
// plus their destroy counterparts.
func stepCommands(stepType model.StepType) []model.ActionCommand {
	plan, apply := model.GetCommands(stepType)
	planDestroy, applyDestroy := model.GetDestroyCommands(stepType)
	return []model.ActionCommand{plan, apply, planDestroy, applyDestroy}
}

// trigger starts a build run against an already-created pipeline by display name,
// relying on its baked-in parameter defaults. Used by the destroy flow, which runs in
// a fresh process that never registered the run spec.
func (b *Builder) trigger(displayName string) (string, error) {
	perRun := map[string]string{}
	if b.campaignId != "" {
		perRun["CAMPAIGN_ID"] = b.campaignId
		perRun["PIPELINE_INDEX"] = strconv.Itoa(b.pipelineIndex)
	}
	return b.devopsBuild.triggerBuildRun(displayName, perRun)
}

// launch runs the given command for a step as an OCI DevOps build run and returns its
// OCID. The container environment is split: non-secret values become build-pipeline
// parameters (nonSecretParams), secret values are injected from the Vault via the
// spec's vaultVariables (secretRefs); only the campaign correlation is per-run.
func (b *Builder) launch(projectName, prefixStep string, command model.ActionCommand, step model.Step) (string, error) {
	displayName := runName(projectName, command)
	project, err := b.getProject(projectName)
	if err != nil {
		return "", err
	}
	if project == nil {
		return "", fmt.Errorf("no project registered for %s", projectName)
	}
	params := b.nonSecretParams(prefixStep, command, step, project)
	secretRefs, err := b.secretRefs(step, project)
	if err != nil {
		return "", err
	}
	perRun := map[string]string{}
	if project.AgentCmd == "" && b.campaignId != "" {
		perRun["CAMPAIGN_ID"] = b.campaignId
		perRun["PIPELINE_INDEX"] = strconv.Itoa(b.pipelineIndex)
	}
	log.Printf("Executing build run %s\n", displayName)
	return b.devopsBuild.launchBuildRun(displayName, b.specFile(projectName, project), project.Image, params, secretRefs, perRun, project.VpcConfig)
}

// runName is the display name shared by a step+command's build pipeline and its build
// runs. It caps to maxNameLen here — the one place the name is formed — so the
// pipeline, the list-by-name lookups and the build-run name stay identical.
func runName(projectName string, command model.ActionCommand) string {
	name := projectName
	if command != "" {
		name = fmt.Sprintf("%s-%s", projectName, command)
	}
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	return name
}

// maxNameLen is the OCI DevOps display-name limit for build pipelines and runs.
const maxNameLen = 255

// waitForCompletion waits for a step's build run to finish and returns its
// process-style exit code (0 = SUCCEEDED, non-zero = failed).
func (b *Builder) waitForCompletion(buildRunId string) (int, error) {
	return b.devopsBuild.waitForBuildRun(buildRunId)
}

// nonSecretParams returns the non-secret container environment, baked as build-pipeline
// parameter defaults. The values mirror service.LocalPipeline.getEnv so the base image
// behaves identically locally and in a DevOps build run; secrets are handled by
// secretRefs. CAMPAIGN_ID/PIPELINE_INDEX carry placeholders overridden per run.
func (b *Builder) nonSecretParams(prefixStep string, command model.ActionCommand, step model.Step, project *containerProject) map[string]string {
	env := map[string]string{
		"COMMAND":                     string(command),
		"TF_VAR_prefix":               prefixStep,
		"INFRALIB_BUCKET":             b.bucket,
		model.OracleRegion:            b.region,
		common.OracleCompartmentIdEnv: b.compartmentId,
		"AWS_REGION":                  b.region,
		"AWS_ENDPOINT_URL_S3":         b.s3Endpoint,
		// The oci provider defaults `auth` to ApiKey, which finds no ~/.oci/config in
		// the container and fails "did not find a proper configuration for tenancy".
		// The only in-container credential is the build runner's resource principal
		// (its OCI_RESOURCE_PRINCIPAL_* vars are forwarded by the spec), so select it.
		"OCI_AUTH": "ResourcePrincipal",
	}
	if step.Name != "" {
		env["INFRALIB_STEP"] = step.Name
	}
	// AWS_ACCESS_KEY_ID is an identifier, not the secret half (that is a Vault secret,
	// see secretRefs), so it is a plain non-secret parameter.
	if b.accessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = b.accessKey
	}
	if project.AgentCmd != "" {
		// The relaunched agent selects the Oracle provider via the compartment id and
		// derives bucket names from the prefix; without these it falls through to AWS.
		env["COMMAND"] = string(project.AgentCmd)
		env[common.PrefixEnv] = b.cloudPrefix
		return env
	}
	env["CAMPAIGN_ID"] = model.CampaignSentinelNone
	env["PIPELINE_INDEX"] = "0"
	for source, auth := range project.AuthSources {
		hash := util.HashCode(source)
		env[fmt.Sprintf(model.GitSourceEnvFormat, hash)] = source
		env[fmt.Sprintf(model.GitUsernameEnvFormat, hash)] = auth.Username
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
			env["TF_TOOL"] = model.TofuTfTool
		}
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				name := strings.ToUpper(module.Name)
				env[fmt.Sprintf("GIT_AUTH_USERNAME_%s", name)] = module.HttpUsername
				env[fmt.Sprintf("GIT_AUTH_SOURCE_%s", name)] = module.Source
			}
		}
	}
	return env
}

// secretRefs maps each secret container env var this step needs to its Vault secret
// OCID. The spec's vaultVariables reference these OCIDs so the build runner fetches
// the value with its resource principal — it never touches the bucket or a build
// argument. Agent projects need none. Values already in the Vault (git source
// passwords, wrapper config) are resolved by name; in-memory values (the CSK secret
// half, client-module passwords) are upserted on demand.
func (b *Builder) secretRefs(step model.Step, project *containerProject) (map[string]string, error) {
	refs := map[string]string{}
	if project.AgentCmd != "" {
		return refs, nil
	}
	if b.secretKey != "" {
		ocid, err := b.secrets.ensureSecret(awsSecretAccessKeySecret, b.secretKey)
		if err != nil {
			return nil, err
		}
		refs["AWS_SECRET_ACCESS_KEY"] = ocid
	}
	wrapperOCID, err := b.secrets.secretOCID(model.WrapperConfigSecretName(b.cloudPrefix))
	if err != nil {
		return nil, err
	}
	if wrapperOCID != "" {
		refs[model.WrapperConfigEnv] = wrapperOCID
	}
	for source := range project.AuthSources {
		hash := util.HashCode(source)
		ocid, err := b.secrets.secretOCID(fmt.Sprintf(model.GitPasswordFormat, hash))
		if err != nil {
			return nil, err
		}
		if ocid != "" {
			refs[fmt.Sprintf(model.GitPasswordEnvFormat, hash)] = ocid
		}
	}
	if step.Type == model.StepTypeTerraform {
		for _, module := range step.Modules {
			if !util.IsClientModule(module) {
				continue
			}
			name := strings.ToUpper(module.Name)
			ocid, err := b.secrets.ensureSecret(fmt.Sprintf("git-%s-%s-password", step.Name, module.Name), module.HttpPassword)
			if err != nil {
				return nil, err
			}
			refs[fmt.Sprintf("GIT_AUTH_PASSWORD_%s", name)] = ocid
		}
	}
	return refs, nil
}

// awsSecretAccessKeySecret is the Vault secret name for the CSK secret half the
// terraform s3 backend consumes as AWS_SECRET_ACCESS_KEY (distinct from the full CSK
// JSON persisted under customerSecretKeyObject).
const awsSecretAccessKeySecret = "oracle-aws-secret-access-key"
