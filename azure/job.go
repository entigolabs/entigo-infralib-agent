package azure

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

type Builder struct {
	ctx                  context.Context
	credential           *azidentity.DefaultAzureCredential
	subscriptionId       string
	resourceGroup        string
	location             string
	cloudPrefix          string
	managedIdentity      string
	terraformCache       bool
	jobsClient           *armappcontainers.JobsClient
	managedEnvClient     *armappcontainers.ManagedEnvironmentsClient
	managedEnvironmentId string
}

func NewBuilder(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup, location, cloudPrefix, managedIdentity string, terraformCache bool) (*Builder, error) {
	jobsClient, err := armappcontainers.NewJobsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create jobs client: %w", err)
	}

	managedEnvClient, err := armappcontainers.NewManagedEnvironmentsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create managed environments client: %w", err)
	}

	b := &Builder{
		ctx:              ctx,
		credential:       credential,
		subscriptionId:   subscriptionId,
		resourceGroup:    resourceGroup,
		location:         location,
		cloudPrefix:      cloudPrefix,
		managedIdentity:  managedIdentity,
		terraformCache:   terraformCache,
		jobsClient:       jobsClient,
		managedEnvClient: managedEnvClient,
	}

	if managedIdentity != "" {
		err = b.ensureManagedEnvironment()
		if err != nil {
			return nil, err
		}
	}

	return b, nil
}

func (b *Builder) ensureManagedEnvironment() error {
	envName := b.getManagedEnvironmentName()
	resp, err := b.managedEnvClient.Get(b.ctx, b.resourceGroup, envName, nil)
	if err == nil {
		b.managedEnvironmentId = *resp.ID
		return nil
	}

	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) || respErr.StatusCode != 404 {
		return err
	}

	poller, err := b.managedEnvClient.BeginCreateOrUpdate(b.ctx, b.resourceGroup, envName,
		armappcontainers.ManagedEnvironment{
			Location: to.Ptr(b.location),
			Properties: &armappcontainers.ManagedEnvironmentProperties{
				ZoneRedundant: to.Ptr(false),
				WorkloadProfiles: []*armappcontainers.WorkloadProfile{
					{
						Name:                to.Ptr("Consumption"),
						WorkloadProfileType: to.Ptr("Consumption"),
					},
				},
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to begin creating managed environment: %w", err)
	}

	result, err := poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create managed environment: %w", err)
	}
	b.managedEnvironmentId = *result.ID
	log.Printf("Created Container Apps managed environment: %s\n", envName)
	return nil
}

func (b *Builder) getManagedEnvironmentName() string {
	name := fmt.Sprintf("%s-env", b.cloudPrefix)
	if len(name) > 32 {
		name = name[:32]
	}
	return strings.TrimSuffix(name, "-")
}

func (b *Builder) CreateProject(projectName, bucket, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	jobName := sanitizeJobName(projectName)
	job, err := b.getJob(jobName)
	if err != nil {
		return err
	}
	if job != nil {
		return b.UpdateProject(projectName, bucket, stepName, step, imageVersion, imageSource, vpcConfig, authSources)
	}

	image := getImage(imageVersion, imageSource)
	envVars := b.getEnvironmentVariables(projectName, stepName, step, bucket, model.PlanCommand, authSources)

	poller, err := b.jobsClient.BeginCreateOrUpdate(b.ctx, b.resourceGroup, jobName,
		armappcontainers.Job{
			Location: to.Ptr(b.location),
			Properties: &armappcontainers.JobProperties{
				EnvironmentID: to.Ptr(b.managedEnvironmentId),
				Configuration: &armappcontainers.JobConfiguration{
					TriggerType:       to.Ptr(armappcontainers.TriggerTypeManual),
					ReplicaTimeout:    to.Ptr(int32(86400)),
					ReplicaRetryLimit: to.Ptr(int32(0)),
					ManualTriggerConfig: &armappcontainers.JobConfigurationManualTriggerConfig{
						Parallelism:            to.Ptr(int32(1)),
						ReplicaCompletionCount: to.Ptr(int32(1)),
					},
				},
				Template: &armappcontainers.JobTemplate{
					Containers: []*armappcontainers.Container{{
						Name:  to.Ptr("infralib"),
						Image: to.Ptr(image),
						Env:   envVars,
						Resources: &armappcontainers.ContainerResources{
							CPU:    to.Ptr(float64(2)),
							Memory: to.Ptr("4Gi"),
						},
						Command: []*string{to.Ptr("/usr/bin/entrypoint.sh")},
					}},
				},
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to begin creating job: %w", err)
	}

	_, err = poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create job: %w", err)
	}
	log.Printf("Created Container Apps Job %s\n", jobName)
	return nil
}

func getImage(imageVersion, imageSource string) string {
	if imageSource == "" {
		imageSource = model.ProjectImageAzure
	}
	return fmt.Sprintf("%s:%s", imageSource, imageVersion)
}

func (b *Builder) CreateAgentProject(projectName, cloudPrefix, imageVersion string, cmd common.Command) error {
	jobName := sanitizeJobName(projectName)
	poller, err := b.jobsClient.BeginCreateOrUpdate(b.ctx, b.resourceGroup, jobName,
		armappcontainers.Job{
			Location: to.Ptr(b.location),
			Properties: &armappcontainers.JobProperties{
				EnvironmentID: to.Ptr(b.managedEnvironmentId),
				Configuration: &armappcontainers.JobConfiguration{
					TriggerType:       to.Ptr(armappcontainers.TriggerTypeManual),
					ReplicaTimeout:    to.Ptr(int32(86400)),
					ReplicaRetryLimit: to.Ptr(int32(0)),
					ManualTriggerConfig: &armappcontainers.JobConfigurationManualTriggerConfig{
						Parallelism:            to.Ptr(int32(1)),
						ReplicaCompletionCount: to.Ptr(int32(1)),
					},
				},
				Template: &armappcontainers.JobTemplate{
					Containers: []*armappcontainers.Container{{
						Name:  to.Ptr("agent"),
						Image: to.Ptr(fmt.Sprintf("%s:%s", model.AgentImageAzure, imageVersion)),
						Env:   b.getAgentEnvVars(cloudPrefix),
						Resources: &armappcontainers.ContainerResources{
							CPU:    to.Ptr(float64(1)),
							Memory: to.Ptr("2Gi"),
						},
						Command: []*string{to.Ptr("ei-agent"), to.Ptr(string(cmd))},
					}},
				},
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to begin creating agent job: %w", err)
	}

	_, err = poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create agent job: %w", err)
	}
	log.Printf("Created agent Container Apps Job %s\n", jobName)
	return nil
}

func (b *Builder) getAgentEnvVars(cloudPrefix string) []*armappcontainers.EnvironmentVar {
	return []*armappcontainers.EnvironmentVar{
		{Name: to.Ptr(common.AwsPrefixEnv), Value: to.Ptr(cloudPrefix)},
		{Name: to.Ptr(common.SubscriptionIdEnv), Value: to.Ptr(b.subscriptionId)},
		{Name: to.Ptr(common.ResourceGroupEnv), Value: to.Ptr(b.resourceGroup)},
		{Name: to.Ptr(common.LocationEnv), Value: to.Ptr(b.location)},
		{Name: to.Ptr("TERRAFORM_CACHE"), Value: to.Ptr(fmt.Sprintf("%t", b.terraformCache))},
		{Name: to.Ptr("AZURE_CONTAINER_APP_JOB"), Value: to.Ptr("true")},
	}
}

func (b *Builder) GetProject(projectName string) (*model.Project, error) {
	jobName := sanitizeJobName(projectName)
	job, err := b.getJob(jobName)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, nil
	}

	var image string
	var terraformCache string
	if job.Properties != nil && job.Properties.Template != nil && len(job.Properties.Template.Containers) > 0 {
		container := job.Properties.Template.Containers[0]
		if container.Image != nil {
			image = *container.Image
		}
		for _, env := range container.Env {
			if env.Name != nil && *env.Name == "TERRAFORM_CACHE" && env.Value != nil {
				terraformCache = *env.Value
				break
			}
		}
	}

	return &model.Project{
		Name:           projectName,
		Image:          image,
		TerraformCache: terraformCache,
	}, nil
}

func (b *Builder) UpdateAgentProject(projectName, version, cloudPrefix string) error {
	jobName := sanitizeJobName(projectName)
	job, err := b.getJob(jobName)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found", projectName)
	}

	if job.Properties != nil && job.Properties.Template != nil && len(job.Properties.Template.Containers) > 0 {
		job.Properties.Template.Containers[0].Image = to.Ptr(fmt.Sprintf("%s:%s", model.AgentImageAzure, version))
		job.Properties.Template.Containers[0].Env = b.getAgentEnvVars(cloudPrefix)
	}

	poller, err := b.jobsClient.BeginCreateOrUpdate(b.ctx, b.resourceGroup, jobName, *job, nil)
	if err != nil {
		return fmt.Errorf("failed to begin updating agent job: %w", err)
	}

	_, err = poller.PollUntilDone(b.ctx, nil)
	return err
}

func (b *Builder) UpdateProject(projectName, bucket, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	jobName := sanitizeJobName(projectName)
	job, err := b.getJob(jobName)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found", projectName)
	}

	image := getImage(imageVersion, imageSource)
	envVars := b.getEnvironmentVariables(projectName, stepName, step, bucket, model.PlanCommand, authSources)

	if job.Properties != nil && job.Properties.Template != nil && len(job.Properties.Template.Containers) > 0 {
		job.Properties.Template.Containers[0].Image = to.Ptr(image)
		job.Properties.Template.Containers[0].Env = envVars
	}

	poller, err := b.jobsClient.BeginCreateOrUpdate(b.ctx, b.resourceGroup, jobName, *job, nil)
	if err != nil {
		return fmt.Errorf("failed to begin updating job: %w", err)
	}

	_, err = poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}
	log.Printf("Updated Container Apps Job %s\n", jobName)
	return nil
}

func (b *Builder) DeleteProject(projectName string, _ model.Step) error {
	jobName := sanitizeJobName(projectName)
	poller, err := b.jobsClient.BeginDelete(b.ctx, b.resourceGroup, jobName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil
		}
		return err
	}

	_, err = poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil
		}
		return err
	}
	log.Printf("Deleted Container Apps Job %s\n", jobName)
	return nil
}

func (b *Builder) getJob(jobName string) (*armappcontainers.Job, error) {
	resp, err := b.jobsClient.Get(b.ctx, b.resourceGroup, jobName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, err
	}
	return &resp.Job, nil
}

func (b *Builder) executeJob(jobName string, wait bool) (string, error) {
	log.Printf("Executing job %s\n", jobName)
	jobName = sanitizeJobName(jobName)
	poller, err := b.jobsClient.BeginStart(b.ctx, b.resourceGroup, jobName, nil)
	if err != nil {
		return "", err
	}
	if !wait {
		return "", nil
	}
	result, err := poller.PollUntilDone(b.ctx, nil)
	if err != nil {
		return "", err
	}
	if result.ID != nil {
		return *result.ID, nil
	}
	return "", nil
}

func (b *Builder) getEnvironmentVariables(projectName, stepName string, step model.Step, bucket string, command model.ActionCommand, authSources map[string]model.SourceAuth) []*armappcontainers.EnvironmentVar {
	envVars := []*armappcontainers.EnvironmentVar{
		{Name: to.Ptr("PROJECT_NAME"), Value: to.Ptr(projectName)},
		{Name: to.Ptr("AZURE_SUBSCRIPTION_ID"), Value: to.Ptr(b.subscriptionId)},
		{Name: to.Ptr("AZURE_RESOURCE_GROUP"), Value: to.Ptr(b.resourceGroup)},
		{Name: to.Ptr("AZURE_LOCATION"), Value: to.Ptr(b.location)},
		{Name: to.Ptr("COMMAND"), Value: to.Ptr(string(command))},
		{Name: to.Ptr("TF_VAR_prefix"), Value: to.Ptr(stepName)},
		{Name: to.Ptr("INFRALIB_BUCKET"), Value: to.Ptr(bucket)},
		{Name: to.Ptr("AZURE_CONTAINER_APP_JOB"), Value: to.Ptr("true")},
	}

	if step.Type == model.StepTypeTerraform {
		envVars = append(envVars, &armappcontainers.EnvironmentVar{
			Name:  to.Ptr("TERRAFORM_CACHE"),
			Value: to.Ptr(fmt.Sprintf("%t", b.terraformCache)),
		})
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				envVars = append(envVars,
					&armappcontainers.EnvironmentVar{
						Name:  to.Ptr(fmt.Sprintf("GIT_AUTH_USERNAME_%s", strings.ToUpper(module.Name))),
						Value: to.Ptr(module.HttpUsername),
					},
					&armappcontainers.EnvironmentVar{
						Name:  to.Ptr(fmt.Sprintf("GIT_AUTH_PASSWORD_%s", strings.ToUpper(module.Name))),
						Value: to.Ptr(module.HttpPassword),
					},
					&armappcontainers.EnvironmentVar{
						Name:  to.Ptr(fmt.Sprintf("GIT_AUTH_SOURCE_%s", strings.ToUpper(module.Name))),
						Value: to.Ptr(module.Source),
					},
				)
			}
		}
	}

	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			envVars = append(envVars, &armappcontainers.EnvironmentVar{
				Name:  to.Ptr("KUBERNETES_CLUSTER_NAME"),
				Value: to.Ptr(step.KubernetesClusterName),
			})
		}
		namespace := "argocd"
		if step.ArgocdNamespace != "" {
			namespace = step.ArgocdNamespace
		}
		envVars = append(envVars, &armappcontainers.EnvironmentVar{
			Name:  to.Ptr("ARGOCD_NAMESPACE"),
			Value: to.Ptr(namespace),
		})
	}

	for source := range authSources {
		hash := util.HashCode(source)
		envVars = append(envVars,
			&armappcontainers.EnvironmentVar{
				Name:  to.Ptr(fmt.Sprintf(model.GitUsernameEnvFormat, hash)),
				Value: to.Ptr(fmt.Sprintf(model.GitUsernameFormat, hash)),
			},
			&armappcontainers.EnvironmentVar{
				Name:  to.Ptr(fmt.Sprintf(model.GitPasswordEnvFormat, hash)),
				Value: to.Ptr(fmt.Sprintf(model.GitPasswordFormat, hash)),
			},
			&armappcontainers.EnvironmentVar{
				Name:  to.Ptr(fmt.Sprintf(model.GitSourceEnvFormat, hash)),
				Value: to.Ptr(fmt.Sprintf(model.GitSourceFormat, hash)),
			},
		)
	}

	return envVars
}

func sanitizeJobName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, "_", "-")
	if len(name) > 32 {
		name = name[:32]
	}
	return strings.TrimSuffix(name, "-")
}
