package azure

import (
	"context"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"golang.org/x/sync/errgroup"
)

const (
	containerNameStep     = "infralib"
	projectService        = "Container Apps job"
	containerNameAgent    = "agent"
	jobTimeoutSeconds     = 28800
	executionPoll         = 10 * time.Second
	executionStartTimeout = 2 * time.Minute
)

// Builder keeps one Container Apps job per step command, so every command, apply and
// destroy included, can be started from the Portal. Pressing Run on a step's apply job
// is also how a manual approval is given.
type Builder struct {
	ctx            context.Context
	jobs           *armappcontainers.JobsClient
	executions     *armappcontainers.JobsExecutionsClient
	api            *armappcontainers.ContainerAppsAPIClient
	ssm            *SSM
	subscriptionId string
	tenantId       string
	resourceGroup  string
	location       string
	environment    *Environment
	identity       identity
	bucket         string
	cloudPrefix    string
	terraformCache bool
	enableOpenTofu bool
	updateCron     string
	campaignId     string
	pipelineIndex  int
	manager        model.NotificationManager
}

func NewBuilder(ctx context.Context, credential azcore.TokenCredential, ssm *SSM, subscriptionId, tenantId, resourceGroup, location string, environment *Environment, jobIdentity identity, bucket, cloudPrefix string, terraformCache, enableOpenTofu bool) (*Builder, error) {
	jobs, err := armappcontainers.NewJobsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	executions, err := armappcontainers.NewJobsExecutionsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	api, err := armappcontainers.NewContainerAppsAPIClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	return &Builder{
		ctx:            ctx,
		jobs:           jobs,
		executions:     executions,
		api:            api,
		ssm:            ssm,
		subscriptionId: subscriptionId,
		tenantId:       tenantId,
		resourceGroup:  resourceGroup,
		location:       location,
		environment:    environment,
		identity:       jobIdentity,
		bucket:         bucket,
		cloudPrefix:    cloudPrefix,
		terraformCache: terraformCache,
		enableOpenTofu: enableOpenTofu,
	}, nil
}

func (b *Builder) SetCampaignId(id string) {
	b.campaignId = id
}

func (b *Builder) SetPipelineIndex(index int) {
	b.pipelineIndex = index
}

func (b *Builder) SetUpdateCron(cron string) {
	b.updateCron = cron
}

func (b *Builder) SetNotificationManager(manager model.NotificationManager) {
	b.manager = manager
}

func getImage(imageVersion, imageSource string) string {
	if imageSource == "" {
		imageSource = model.ProjectImageAzure
	}
	return fmt.Sprintf("%s:%s", imageSource, imageVersion)
}

func (b *Builder) CreateProject(projectName, _, _ string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	environmentId, err := b.environment.ForSubnet(stepSubnet(vpcConfig))
	if err != nil {
		return err
	}
	image := getImage(imageVersion, imageSource)
	secrets, secretEnv, err := b.stepSecrets(step, authSources)
	if err != nil {
		return err
	}
	log.Printf("Reconciling container apps jobs for step %s\n", projectName)
	var group errgroup.Group
	for _, command := range model.GetStepCommands(step.Type) {
		container := &armappcontainers.Container{
			Name:      new(containerNameStep),
			Image:     &image,
			Env:       append(b.stepEnv(projectName, command, step, authSources), secretEnv...),
			Resources: &armappcontainers.ContainerResources{CPU: new(4.0), Memory: new("8Gi")},
		}
		group.Go(func() error {
			return b.ensureJob(jobName(projectName, command), environmentId, container, secrets, nil)
		})
	}
	return group.Wait()
}

func (b *Builder) UpdateProject(projectName, repoURL, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	return b.CreateProject(projectName, repoURL, stepName, step, imageVersion, imageSource, vpcConfig, authSources)
}

// DeleteProject also deletes the step's client module secrets, once no job references them.
func (b *Builder) DeleteProject(projectName string, step model.Step) error {
	var group errgroup.Group
	for _, command := range model.GetStepCommands(step.Type) {
		group.Go(func() error {
			return b.deleteJob(jobName(projectName, command))
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if step.Type != model.StepTypeTerraform {
		return nil
	}
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			if err := b.ssm.DeleteSecret(clientModuleSecretName(step.Name, module.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Builder) CreateAgentProject(projectName, _, imageVersion string, cmd common.Command) error {
	cron := b.agentSchedule(cmd)
	if err := b.ensureJob(jobName(projectName, ""), b.environment.Id(), b.agentContainer(imageVersion, cmd), nil, cron); err != nil {
		return err
	}
	if cron != nil && b.manager != nil {
		b.manager.Schedule(common.UpdateCommand, model.ScheduleAdded, *cron)
	}
	return nil
}

func (b *Builder) UpdateAgentProject(projectName, version, _ string) error {
	name := jobName(projectName, "")
	job, err := b.getJob(name)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found", name)
	}
	cmd := common.Command(strings.TrimPrefix(projectName, model.GetAgentPrefix(b.cloudPrefix)+"-"))
	return b.ensureJob(name, b.environment.Id(), b.agentContainer(version, cmd), nil, b.agentSchedule(cmd))
}

func (b *Builder) agentSchedule(cmd common.Command) *string {
	if cmd != common.UpdateCommand || b.updateCron == "" {
		return nil
	}
	return &b.updateCron
}

// reconcileSchedule moves the update job to the configured cron. The job is only
// created by bootstrap, so a missing job is left for it, and CreateAgentProject notifies.
func (b *Builder) reconcileSchedule() error {
	name := jobName(model.GetAgentProjectName(model.GetAgentPrefix(b.cloudPrefix), common.UpdateCommand), "")
	job, err := b.getJob(name)
	if err != nil {
		return err
	}
	if job == nil {
		if b.updateCron == "" {
			b.manager.ScheduleUnchanged(common.UpdateCommand, model.ScheduleRemoved, "")
		}
		return nil
	}
	current := scheduleCron(job)
	if current == b.updateCron {
		action := model.ScheduleAdded
		if current == "" {
			action = model.ScheduleRemoved
		}
		b.manager.ScheduleUnchanged(common.UpdateCommand, action, b.updateCron)
		return nil
	}
	container, err := jobContainer(name, job)
	if err != nil {
		return err
	}
	if err = b.ensureJob(name, *job.Properties.EnvironmentID, container, nil, b.agentSchedule(common.UpdateCommand)); err != nil {
		return err
	}
	switch {
	case b.updateCron == "":
		b.manager.Schedule(common.UpdateCommand, model.ScheduleRemoved, b.updateCron)
	case current == "":
		b.manager.Schedule(common.UpdateCommand, model.ScheduleAdded, b.updateCron)
	default:
		b.manager.Schedule(common.UpdateCommand, model.ScheduleModified, b.updateCron)
	}
	return nil
}

func (b *Builder) agentContainer(version string, cmd common.Command) *armappcontainers.Container {
	env := map[string]string{
		common.PrefixEnv:              b.cloudPrefix,
		common.AzureSubscriptionIdEnv: b.subscriptionId,
		model.AzureRegion:             b.location,
		common.AzureResourceGroupEnv:  b.resourceGroup,
		"AZURE_CLIENT_ID":             b.identity.ClientId,
		"AZURE_TOKEN_CREDENTIALS":     "ManagedIdentityCredential",
		"TERRAFORM_CACHE":             strconv.FormatBool(b.terraformCache),
	}
	return &armappcontainers.Container{
		Name:      new(containerNameAgent),
		Image:     new(fmt.Sprintf("%s:%s", model.AgentImageAzure, version)),
		Command:   []*string{new("ei-agent")},
		Args:      []*string{new(string(cmd))},
		Env:       toEnvVars(env),
		Resources: &armappcontainers.ContainerResources{CPU: new(1.0), Memory: new("2Gi")},
	}
}

func (b *Builder) GetProject(projectName string) (*model.Project, error) {
	name := jobName(projectName, "")
	job, err := b.getJob(name)
	if err != nil || job == nil {
		return nil, err
	}
	container, err := jobContainer(name, job)
	if err != nil {
		return nil, err
	}
	project := &model.Project{Name: projectName, Image: *container.Image}
	for _, env := range container.Env {
		if env.Name != nil && *env.Name == "TERRAFORM_CACHE" && env.Value != nil {
			project.TerraformCache = *env.Value
		}
	}
	return project, nil
}

// stepSubnet is the delegated subnet a VNet-attached step runs in.
func stepSubnet(vpcConfig *model.VpcConfig) string {
	if vpcConfig == nil || len(vpcConfig.Subnets) == 0 {
		return ""
	}
	return vpcConfig.Subnets[0]
}

// ensureJob recreates a job that moved to another environment, since a job's environment can't change.
func (b *Builder) ensureJob(name, environmentId string, container *armappcontainers.Container, secrets []*armappcontainers.Secret, cron *string) error {
	if err := checkJobName(name); err != nil {
		return err
	}
	existing, err := b.getJob(name)
	if err != nil {
		return err
	}
	if existing != nil && existing.Properties != nil && existing.Properties.EnvironmentID != nil &&
		!strings.EqualFold(*existing.Properties.EnvironmentID, environmentId) {
		log.Printf("Moving container apps job %s to environment %s\n", name, environmentId)
		if err = b.deleteJob(name); err != nil {
			return err
		}
	}
	poller, err := b.jobs.BeginCreateOrUpdate(b.ctx, b.resourceGroup, name, armappcontainers.Job{
		Location: &b.location,
		Tags:     resourceTags(),
		Identity: &armappcontainers.ManagedServiceIdentity{
			Type:                   new(armappcontainers.ManagedServiceIdentityTypeUserAssigned),
			UserAssignedIdentities: map[string]*armappcontainers.UserAssignedIdentity{b.identity.Id: {}},
		},
		Properties: &armappcontainers.JobProperties{
			EnvironmentID:       &environmentId,
			WorkloadProfileName: new(consumptionProfile),
			Configuration:       jobConfiguration(secrets, cron),
			Template: &armappcontainers.JobTemplate{
				Containers: []*armappcontainers.Container{container},
			},
		},
	}, nil)
	if err == nil {
		_, err = poller.PollUntilDone(b.ctx, nil)
	}
	if err != nil {
		return fmt.Errorf("failed to create container apps job %s: %w", name, err)
	}
	return nil
}

// jobConfiguration runs one replica without retries. Without a cron the job only runs when started.
func jobConfiguration(secrets []*armappcontainers.Secret, cron *string) *armappcontainers.JobConfiguration {
	configuration := &armappcontainers.JobConfiguration{
		ReplicaTimeout:    new(int32(jobTimeoutSeconds)),
		ReplicaRetryLimit: new(int32(0)),
		Secrets:           secrets,
	}
	if cron == nil {
		configuration.TriggerType = new(armappcontainers.TriggerTypeManual)
		configuration.ManualTriggerConfig = &armappcontainers.JobConfigurationManualTriggerConfig{
			Parallelism:            new(int32(1)),
			ReplicaCompletionCount: new(int32(1)),
		}
		return configuration
	}
	configuration.TriggerType = new(armappcontainers.TriggerTypeSchedule)
	configuration.ScheduleTriggerConfig = &armappcontainers.JobConfigurationScheduleTriggerConfig{
		CronExpression:         cron,
		Parallelism:            new(int32(1)),
		ReplicaCompletionCount: new(int32(1)),
	}
	return configuration
}

// scheduleCron is the cron of a scheduled job, empty for a manually started one.
func scheduleCron(job *armappcontainers.Job) string {
	if job.Properties == nil || job.Properties.Configuration == nil {
		return ""
	}
	configuration := job.Properties.Configuration
	if configuration.TriggerType == nil || *configuration.TriggerType != armappcontainers.TriggerTypeSchedule ||
		configuration.ScheduleTriggerConfig == nil || configuration.ScheduleTriggerConfig.CronExpression == nil {
		return ""
	}
	return *configuration.ScheduleTriggerConfig.CronExpression
}

func jobContainer(name string, job *armappcontainers.Job) (*armappcontainers.Container, error) {
	if job.Properties == nil || job.Properties.Template == nil || len(job.Properties.Template.Containers) == 0 {
		return nil, fmt.Errorf("container apps job %s has no container", name)
	}
	return job.Properties.Template.Containers[0], nil
}

func (b *Builder) getJob(name string) (*armappcontainers.Job, error) {
	response, err := b.jobs.Get(b.ctx, b.resourceGroup, name, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get container apps job %s: %w", name, err)
	}
	return &response.Job, nil
}

func (b *Builder) jobId(name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/jobs/%s",
		b.subscriptionId, b.resourceGroup, name)
}

func (b *Builder) jobLink(name string) string {
	return fmt.Sprintf("https://portal.azure.com/#@%s/resource%s/overview", b.tenantId, b.jobId(name))
}

// deleteJob checks existence first, since ARM answers deleting a missing resource with 204.
func (b *Builder) deleteJob(name string) error {
	job, err := b.getJob(name)
	if err != nil || job == nil {
		return err
	}
	poller, err := b.jobs.BeginDelete(b.ctx, b.resourceGroup, name, nil)
	if err == nil {
		_, err = poller.PollUntilDone(b.ctx, nil)
	}
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete container apps job %s: %w", name, err)
	}
	log.Printf("Deleted container apps job %s\n", name)
	return nil
}

// startJob starts an execution; the campaign correlation is a per-execution override,
// which replaces the whole container, so the job's own container is reused.
func (b *Builder) startJob(name string) (string, error) {
	job, err := b.getJob(name)
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", model.NewNotFoundError(fmt.Sprintf("container apps job %s", name))
	}
	var options *armappcontainers.JobsClientBeginStartOptions
	if overrides := b.campaignOverrides(); overrides != nil {
		container, err := jobContainer(name, job)
		if err != nil {
			return "", err
		}
		options = &armappcontainers.JobsClientBeginStartOptions{Template: &armappcontainers.JobExecutionTemplate{
			Containers: []*armappcontainers.JobExecutionContainer{{
				Name:      container.Name,
				Image:     container.Image,
				Command:   container.Command,
				Args:      container.Args,
				Resources: container.Resources,
				Env:       overrideEnv(container.Env, overrides),
			}},
		}}
	}
	log.Printf("Starting container apps job %s\n", name)
	poller, err := b.jobs.BeginStart(b.ctx, b.resourceGroup, name, options)
	if err != nil {
		return "", fmt.Errorf("failed to start container apps job %s: %w", name, err)
	}
	response, err := poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to start container apps job %s: %w", name, err)
	}
	if response.Name == nil {
		return "", fmt.Errorf("container apps job %s start returned no execution name", name)
	}
	return *response.Name, nil
}

func campaignEnv(campaignId string, pipelineIndex int) map[string]string {
	return map[string]string{"CAMPAIGN_ID": campaignId, "PIPELINE_INDEX": strconv.Itoa(pipelineIndex)}
}

func (b *Builder) campaignOverrides() map[string]string {
	if b.campaignId == "" {
		return nil
	}
	return campaignEnv(b.campaignId, b.pipelineIndex)
}

// setJobEnv changes env values in the job's own template, which a Portal execution uses.
func (b *Builder) setJobEnv(name string, overrides map[string]string) error {
	job, err := b.getJob(name)
	if err != nil {
		return err
	}
	if job == nil {
		return model.NewNotFoundError(fmt.Sprintf("container apps job %s", name))
	}
	container, err := jobContainer(name, job)
	if err != nil {
		return err
	}
	container.Env = overrideEnv(container.Env, overrides)
	poller, err := b.jobs.BeginUpdate(b.ctx, b.resourceGroup, name, armappcontainers.JobPatchProperties{
		Properties: &armappcontainers.JobPatchPropertiesProperties{Template: job.Properties.Template},
	}, nil)
	if err == nil {
		_, err = poller.PollUntilDone(b.ctx, nil)
	}
	if err != nil {
		return fmt.Errorf("failed to update env of container apps job %s: %w", name, err)
	}
	return nil
}

func overrideEnv(env []*armappcontainers.EnvironmentVar, overrides map[string]string) []*armappcontainers.EnvironmentVar {
	result := make([]*armappcontainers.EnvironmentVar, 0, len(env))
	for _, variable := range env {
		if variable.Name != nil {
			if value, ok := overrides[*variable.Name]; ok {
				result = append(result, &armappcontainers.EnvironmentVar{Name: variable.Name, Value: new(value)})
				continue
			}
		}
		result = append(result, variable)
	}
	return result
}

// waitForExecution blocks until the execution finishes and reports whether it succeeded.
func (b *Builder) waitForExecution(job, execution string) error {
	started := time.Now()
	for {
		response, err := b.api.JobExecution(b.ctx, b.resourceGroup, job, execution, nil)
		if err != nil && (!isNotFound(err) || time.Since(started) > executionStartTimeout) {
			return fmt.Errorf("failed to get execution %s of job %s: %w", execution, job, err)
		}
		if err == nil && response.Properties != nil && response.Properties.Status != nil {
			switch *response.Properties.Status {
			case armappcontainers.JobExecutionRunningStateSucceeded:
				return nil
			case armappcontainers.JobExecutionRunningStateFailed, armappcontainers.JobExecutionRunningStateStopped,
				armappcontainers.JobExecutionRunningStateDegraded:
				return fmt.Errorf("execution %s of job %s ended with status %s", execution, job, *response.Properties.Status)
			}
		}
		if err = util.Sleep(b.ctx, executionPoll); err != nil {
			return err
		}
	}
}

func (b *Builder) executionNames(job string) (map[string]bool, error) {
	names := map[string]bool{}
	pager := b.executions.NewListPager(b.resourceGroup, job, nil)
	for pager.More() {
		page, err := pager.NextPage(b.ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list executions of job %s: %w", job, err)
		}
		for _, execution := range page.Value {
			if execution.Name != nil {
				names[*execution.Name] = true
			}
		}
	}
	return names, nil
}

// newExecution returns an execution of the job that isn't among the known ones, if any.
func (b *Builder) newExecution(job string, known map[string]bool) (string, error) {
	names, err := b.executionNames(job)
	if err != nil {
		return "", err
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if !known[name] {
			return name, nil
		}
	}
	return "", nil
}

// stepEnv mirrors service.LocalPipeline.getEnv so the base image behaves the same locally
// and in a job. Terraform authenticates with the job's managed identity.
func (b *Builder) stepEnv(prefixStep string, command model.ActionCommand, step model.Step, authSources map[string]model.SourceAuth) []*armappcontainers.EnvironmentVar {
	env := map[string]string{
		"COMMAND":                     string(command),
		"TF_VAR_prefix":               prefixStep,
		"INFRALIB_BUCKET":             b.bucket,
		"INFRALIB_STEP":               step.Name,
		model.AzureRegion:             b.location,
		common.AzureSubscriptionIdEnv: b.subscriptionId,
		common.AzureResourceGroupEnv:  b.resourceGroup,
		"AZURE_CLIENT_ID":             b.identity.ClientId,
		"ARM_SUBSCRIPTION_ID":         b.subscriptionId,
		"ARM_TENANT_ID":               b.tenantId,
		"ARM_CLIENT_ID":               b.identity.ClientId,
		"ARM_USE_MSI":                 "true",
		"ARM_USE_AZUREAD":             "true",
	}
	maps.Copy(env, campaignEnv(model.CampaignSentinelNone, 0))
	for source, auth := range authSources {
		hash := util.HashCode(source)
		env[fmt.Sprintf(model.GitSourceEnvFormat, hash)] = source
		env[fmt.Sprintf(model.GitUsernameEnvFormat, hash)] = auth.Username
	}
	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			env["KUBERNETES_CLUSTER_NAME"] = step.KubernetesClusterName
		}
		env["ARGOCD_NAMESPACE"] = "argocd"
		if step.ArgocdNamespace != "" {
			env["ARGOCD_NAMESPACE"] = step.ArgocdNamespace
		}
	}
	if step.Type == model.StepTypeTerraform {
		env["TERRAFORM_CACHE"] = strconv.FormatBool(b.terraformCache)
		if b.enableOpenTofu {
			env["TF_TOOL"] = model.TofuTfTool
		}
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				name := strings.ToUpper(module.Name)
				env[fmt.Sprintf(model.GitUsernameEnvFormat, name)] = module.HttpUsername
				env[fmt.Sprintf(model.GitSourceEnvFormat, name)] = module.Source
			}
		}
	}
	return toEnvVars(env)
}

// stepSecrets references Key Vault secrets from the job, resolved with the job's managed
// identity, so no secret value is stored in the job definition. A reference to a missing
// secret fails the job creation, so only existing ones are referenced.
func (b *Builder) stepSecrets(step model.Step, authSources map[string]model.SourceAuth) ([]*armappcontainers.Secret, []*armappcontainers.EnvironmentVar, error) {
	refs := map[string]string{}
	wrapperExists, err := b.ssm.ParameterExists(model.WrapperConfigSecretName(b.cloudPrefix))
	if err != nil {
		return nil, nil, err
	}
	if wrapperExists {
		refs[model.WrapperConfigEnv] = model.WrapperConfigSecretName(b.cloudPrefix)
	}
	for source := range authSources {
		hash := util.HashCode(source)
		refs[fmt.Sprintf(model.GitPasswordEnvFormat, hash)] = fmt.Sprintf(model.GitPasswordFormat, hash)
	}
	if step.Type == model.StepTypeTerraform {
		for _, module := range step.Modules {
			if !util.IsClientModule(module) {
				continue
			}
			name := clientModuleSecretName(step.Name, module.Name)
			if err = b.ssm.ensureSecret(name, module.HttpPassword); err != nil {
				return nil, nil, err
			}
			refs[fmt.Sprintf(model.GitPasswordEnvFormat, strings.ToUpper(module.Name))] = name
		}
	}
	var secrets []*armappcontainers.Secret
	var env []*armappcontainers.EnvironmentVar
	for _, envName := range slices.Sorted(maps.Keys(refs)) {
		secret := appSecretName(envName)
		secrets = append(secrets, &armappcontainers.Secret{
			Name:        &secret,
			KeyVaultURL: new(b.ssm.secretURI(refs[envName])),
			Identity:    &b.identity.Id,
		})
		env = append(env, &armappcontainers.EnvironmentVar{Name: new(envName), SecretRef: &secret})
	}
	return secrets, env, nil
}

func toEnvVars(env map[string]string) []*armappcontainers.EnvironmentVar {
	vars := make([]*armappcontainers.EnvironmentVar, 0, len(env))
	for _, name := range slices.Sorted(maps.Keys(env)) {
		vars = append(vars, &armappcontainers.EnvironmentVar{Name: new(name), Value: new(env[name])})
	}
	return vars
}
