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
// definition, so CreateProject/UpdateProject record the run spec (image,
// networking, auth) that the Pipeline replays when it launches an execution.
// The spec is cached in-memory only (behind a mutex, because steps run in
// parallel goroutines) and NEVER written to the bucket: the eagerly created,
// self-describing build pipelines (their parameters bake the image + non-secret
// env + secret OCIDs) are the durable record, so a fresh-process/agentless run
// (console-triggerable destroy) triggers them by name without any persisted spec.
// Execution itself runs through OCI DevOps build pipelines (DevOpsBuilder);
// Builder owns the run spec and the container environment and delegates
// launch/wait to that backend.
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

// SetCampaignId / SetPipelineIndex store the portal campaign correlation that
// launch/trigger pass as per-run build-run arguments (CAMPAIGN_ID /
// PIPELINE_INDEX), where the wrapper reads them like every other provider. Empty
// campaignId means no active campaign — the wrapper then runs transparently.
func (b *Builder) SetCampaignId(id string) {
	b.campaignId = id
}

func (b *Builder) SetPipelineIndex(index int) {
	b.pipelineIndex = index
}

// containerProject is the in-memory run spec (never serialised to the bucket).
// AuthSources carries only the NON-secret git source + username; the password is
// a Vault secret resolved by name at launch (secretRefs).
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

// withoutSecrets strips git passwords from the auth sources before they are
// persisted; the password is a Vault secret (written by upsertSourceCredentials)
// resolved by name at launch, so it must never land in the bucket.
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

// getProject returns the in-memory spec, or nil if this process never registered
// it. A fresh destroy/agentless process has no spec — it triggers the eagerly
// created pipelines by name instead (see Pipeline.StartDestroyExecution), so the
// spec is only ever needed in the same process that created the project.
func (b *Builder) getProject(projectName string) (*containerProject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.projects[projectName], nil
}

// ensureStepPipelines eagerly creates (or reconciles) every build pipeline a step
// can run — plan, apply and their destroy variants — so a gitops engineer can
// trigger any of them, destroy included, from the OCI console with no agent. Each
// pipeline bakes the step's full non-secret env + secret OCIDs + image as
// parameter defaults and carries its own spec in the repo, so a later
// fresh-process/agentless run just triggers it by name (see Pipeline.trigger).
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
	// A step's plan/apply/destroy pipelines are independent (distinct display names →
	// distinct keyLocks) and share one spec (pushMu + pushedSpecs dedupe the single git
	// push), so reconcile them concurrently — each blocks ~20s on OCI's async provisioning
	// work requests, which otherwise serialize into ~100s per step.
	var group errgroup.Group
	for _, command := range stepCommands(step.Type) {
		displayName := runName(projectName, command)
		params := b.nonSecretParams(projectName, command, step, project)
		group.Go(func() error {
			_, err := b.devopsBuild.ensurePipeline(displayName, specFile, project.Image, params, secretRefs)
			return err
		})
	}
	return group.Wait()
}

// stepCommands lists every action command a step of the given type can execute:
// the plan/apply pair plus their destroy counterparts. Eager pipeline creation
// walks this so the destroy pipelines exist before any destroy is requested.
func stepCommands(stepType model.StepType) []model.ActionCommand {
	plan, apply := model.GetCommands(stepType)
	planDestroy, applyDestroy := model.GetDestroyCommands(stepType)
	return []model.ActionCommand{plan, apply, planDestroy, applyDestroy}
}

// trigger starts a build run against an already-created (step,command) pipeline by
// display name, relying on its baked-in parameter defaults (image + env + secret
// OCIDs). Used by the destroy flow, which runs in a fresh process that never
// registered the run spec. A never-created step surfaces as model.NotFoundError so
// the caller can skip it.
func (b *Builder) trigger(displayName string) (string, error) {
	perRun := map[string]string{}
	if b.campaignId != "" {
		perRun["CAMPAIGN_ID"] = b.campaignId
		perRun["PIPELINE_INDEX"] = strconv.Itoa(b.pipelineIndex)
	}
	return b.devopsBuild.triggerBuildRun(displayName, perRun)
}

// launch runs the given command for a step as an OCI DevOps build run and returns
// its OCID (consumed by waitForCompletion). The container environment is split:
// non-secret values become build-pipeline parameters (see nonSecretParams) and
// secret values are injected from the Vault via the build spec's vaultVariables
// (see secretRefs); only the portal campaign correlation is supplied per run.
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
	return b.devopsBuild.launchBuildRun(displayName, specFileFor(projectName), project.Image, params, secretRefs, perRun)
}

// runName is the display name shared by a step+command's build pipeline and its
// build runs. Agent projects already carry the command suffix in their name and
// launch with an empty command. It caps to maxNameLen here — the one place the
// name is formed — so the pipeline, the list-by-name lookups and the build-run
// name are always the identical string (a per-call cap would desync them).
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

// maxNameLen is the OCI DevOps display-name limit shared by build pipelines and
// build runs.
const maxNameLen = 255

// waitForCompletion waits for a step's build run to finish and returns its
// process-style exit code (0 = SUCCEEDED, non-zero = failed).
func (b *Builder) waitForCompletion(buildRunId string) (int, error) {
	return b.devopsBuild.waitForBuildRun(buildRunId)
}

// nonSecretParams returns the non-secret container environment. These become
// build-pipeline parameters (defaults baked at pipeline creation); the values
// mirror service.LocalPipeline.getEnv so the base image behaves identically
// locally and in a DevOps build run. Secret values are handled by secretRefs.
// CAMPAIGN_ID/PIPELINE_INDEX carry placeholder defaults and are overridden per
// run (the wrapper treats "none" as transparent, model.CampaignSentinelNone).
func (b *Builder) nonSecretParams(prefixStep string, command model.ActionCommand, step model.Step, project *containerProject) map[string]string {
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
	// AWS_ACCESS_KEY_ID is an identifier, not the secret half (that is a Vault
	// secret, see secretRefs), so it is a plain non-secret parameter.
	if b.accessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = b.accessKey
	}
	if project.AgentCmd != "" {
		// The relaunched agent selects the Oracle provider via the compartment id
		// and derives every bucket name from the prefix; without these it would
		// fall through to the AWS provider.
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

// secretRefs maps each secret container env var this step needs to the OCID of
// its Vault secret. The build spec's vaultVariables reference these OCIDs (via
// per-run parameters) so the build runner fetches the secret with its resource
// principal — the value never touches the bucket or a build argument. A step only
// gets the secrets it actually uses. Agent projects need none (they authenticate
// as a resource principal). Values already in the Vault (git source passwords,
// wrapper config) are resolved by name; values the Builder holds in memory (the
// CSK secret half, client-module passwords) are upserted on demand.
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

// awsSecretAccessKeySecret is the Vault secret name for the CSK secret half that
// the terraform s3 backend consumes as AWS_SECRET_ACCESS_KEY (distinct from the
// full CSK JSON persisted under customerSecretKeyObject).
const awsSecretAccessKeySecret = "oracle-aws-secret-access-key"
