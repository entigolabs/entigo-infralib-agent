package azure

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const (
	pollingDelay = 10 * time.Second
	linkFormat   = "https://portal.azure.com/#@/resource/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/jobs/%s"
)

type Pipeline struct {
	ctx             context.Context
	credential      *azidentity.DefaultAzureCredential
	subscriptionId  string
	resourceGroup   string
	location        string
	cloudPrefix     string
	managedIdentity string
	storage         *BlobStorage
	builder         *Builder
	logging         *Logging
	manager         model.NotificationManager
	jobsClient      *armappcontainers.JobsClient
	executionClient *armappcontainers.JobsExecutionsClient
}

func NewPipeline(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup, location, cloudPrefix, managedIdentity string, storage *BlobStorage, builder *Builder, logging *Logging, manager model.NotificationManager) (*Pipeline, error) {
	jobsClient, err := armappcontainers.NewJobsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create jobs client: %w", err)
	}

	executionClient, err := armappcontainers.NewJobsExecutionsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create execution client: %w", err)
	}

	return &Pipeline{
		ctx:             ctx,
		credential:      credential,
		subscriptionId:  subscriptionId,
		resourceGroup:   resourceGroup,
		location:        location,
		cloudPrefix:     cloudPrefix,
		managedIdentity: managedIdentity,
		storage:         storage,
		builder:         builder,
		logging:         logging,
		manager:         manager,
		jobsClient:      jobsClient,
		executionClient: executionClient,
	}, nil
}

func (p *Pipeline) CreatePipeline(projectName, stepName string, step model.Step, bucket model.Bucket, authSources map[string]model.SourceAuth) (*string, error) {
	planCommand, applyCommand := model.GetCommands(step.Type)

	metadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return nil, err
	}

	err = p.createJob(projectName, stepName, step, metadata.Name, planCommand, authSources)
	if err != nil {
		return nil, fmt.Errorf("failed to create plan job: %w", err)
	}

	err = p.createJob(fmt.Sprintf("%s-apply", projectName), stepName, step, metadata.Name, applyCommand, authSources)
	if err != nil {
		return nil, fmt.Errorf("failed to create apply job: %w", err)
	}

	planDestroyCommand, applyDestroyCommand := model.GetDestroyCommands(step.Type)

	err = p.createJob(fmt.Sprintf("%s-plan-destroy", projectName), stepName, step, metadata.Name, planDestroyCommand, authSources)
	if err != nil {
		return nil, fmt.Errorf("failed to create plan-destroy job: %w", err)
	}

	err = p.createJob(fmt.Sprintf("%s-apply-destroy", projectName), stepName, step, metadata.Name, applyDestroyCommand, authSources)
	if err != nil {
		return nil, fmt.Errorf("failed to create apply-destroy job: %w", err)
	}

	return p.StartPipelineExecution(projectName, stepName, step, metadata.Name)
}

func (p *Pipeline) createJob(jobName, stepName string, step model.Step, bucket string, command model.ActionCommand, authSources map[string]model.SourceAuth) error {
	sanitizedName := sanitizeJobName(jobName)
	job, err := p.builder.getJob(sanitizedName)
	if err != nil {
		return err
	}
	if job != nil {
		return nil
	}

	image := getImage(step.BaseImageVersion, step.BaseImageSource)
	envVars := p.builder.getEnvironmentVariables(jobName, stepName, step, bucket, command, authSources)

	poller, err := p.jobsClient.BeginCreateOrUpdate(p.ctx, p.resourceGroup, sanitizedName,
		armappcontainers.Job{
			Location: to.Ptr(p.location),
			Properties: &armappcontainers.JobProperties{
				EnvironmentID: to.Ptr(p.builder.managedEnvironmentId),
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

	_, err = poller.PollUntilDone(p.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create job: %w", err)
	}
	log.Printf("Created Container Apps Job %s\n", sanitizedName)
	return nil
}

func (p *Pipeline) CreateAgentPipelines(prefix, projectName, bucket string, run bool) error {
	updateJob := fmt.Sprintf("%s-%s", projectName, common.UpdateCommand)
	job, err := p.builder.getJob(sanitizeJobName(updateJob))
	if err != nil {
		return err
	}
	if job == nil {
		err = p.builder.CreateAgentProject(updateJob, prefix, model.LatestImageVersion, common.UpdateCommand)
		if err != nil {
			return err
		}
	}

	runJob := fmt.Sprintf("%s-%s", projectName, common.RunCommand)
	job, err = p.builder.getJob(sanitizeJobName(runJob))
	if err != nil {
		return err
	}
	if job != nil {
		if !run {
			return nil
		}
		_, err = p.builder.executeJob(runJob, false)
		return err
	}

	err = p.builder.CreateAgentProject(runJob, prefix, model.LatestImageVersion, common.RunCommand)
	if err != nil || run {
		return err
	}
	return nil
}

func (p *Pipeline) UpdatePipeline(pipelineName, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) error {
	planCommand, applyCommand := model.GetCommands(step.Type)

	err := p.updateJob(pipelineName, stepName, step, bucket, planCommand, authSources)
	if err != nil {
		return err
	}

	err = p.updateJob(fmt.Sprintf("%s-apply", pipelineName), stepName, step, bucket, applyCommand, authSources)
	if err != nil {
		return err
	}

	planDestroyCommand, applyDestroyCommand := model.GetDestroyCommands(step.Type)

	err = p.updateJob(fmt.Sprintf("%s-plan-destroy", pipelineName), stepName, step, bucket, planDestroyCommand, authSources)
	if err != nil {
		return err
	}

	return p.updateJob(fmt.Sprintf("%s-apply-destroy", pipelineName), stepName, step, bucket, applyDestroyCommand, authSources)
}

func (p *Pipeline) updateJob(jobName, stepName string, step model.Step, bucket string, command model.ActionCommand, authSources map[string]model.SourceAuth) error {
	sanitizedName := sanitizeJobName(jobName)
	job, err := p.builder.getJob(sanitizedName)
	if err != nil {
		return err
	}
	if job == nil {
		return nil
	}

	image := getImage(step.BaseImageVersion, step.BaseImageSource)
	envVars := p.builder.getEnvironmentVariables(jobName, stepName, step, bucket, command, authSources)

	if job.Properties != nil && job.Properties.Template != nil && len(job.Properties.Template.Containers) > 0 {
		job.Properties.Template.Containers[0].Image = to.Ptr(image)
		job.Properties.Template.Containers[0].Env = envVars
	}

	poller, err := p.jobsClient.BeginCreateOrUpdate(p.ctx, p.resourceGroup, sanitizedName, *job, nil)
	if err != nil {
		return fmt.Errorf("failed to begin updating job: %w", err)
	}

	_, err = poller.PollUntilDone(p.ctx, nil)
	return err
}

func (p *Pipeline) StartAgentExecution(pipelineName string) error {
	_, err := p.builder.executeJob(pipelineName, false)
	return err
}

func (p *Pipeline) StartPipelineExecution(pipelineName, stepName string, step model.Step, bucket string) (*string, error) {
	log.Printf("Starting pipeline %s\n", pipelineName)
	jobName := sanitizeJobName(pipelineName)
	poller, err := p.jobsClient.BeginStart(p.ctx, p.resourceGroup, jobName, nil)
	if err != nil {
		return nil, err
	}

	result, err := poller.PollUntilDone(p.ctx, nil)
	if err != nil {
		return nil, err
	}

	var executionId string
	if result.ID != nil {
		executionId = *result.ID
	}
	return &executionId, nil
}

func (p *Pipeline) WaitPipelineExecution(pipelineName, projectName string, executionId *string, autoApprove bool, step model.Step, approve model.ManualApprove) error {
	if executionId == nil {
		return fmt.Errorf("execution id is nil")
	}

	log.Printf("Waiting for pipeline %s to complete, polling delay %s\n", pipelineName, pollingDelay)

	jobName := sanitizeJobName(pipelineName)
	err := p.waitForJobExecution(jobName, *executionId)
	if err != nil {
		return fmt.Errorf("plan job failed: %w", err)
	}

	pipeChanges, err := p.getChanges(pipelineName, step.Type, *executionId)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to get pipeline changes: %s", err)))
	}

	if pipeChanges != nil && util.ShouldStopPipeline(*pipeChanges, step.Approve, approve) {
		log.Printf("Stopping pipeline %s - no changes or rejected\n", pipelineName)
		if step.Approve == model.ApproveReject || approve == model.ManualApproveReject {
			return fmt.Errorf("stopped because step approve type is 'reject'")
		}
		return nil
	}

	if pipeChanges != nil && !util.ShouldApprovePipeline(*pipeChanges, step.Approve, autoApprove, approve) {
		log.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
		if p.manager != nil {
			p.manager.ManualApproval(pipelineName, *pipeChanges, p.getLink(pipelineName))
		}
		return p.waitForManualApproval(pipelineName, step, approve)
	}

	applyJobName := sanitizeJobName(fmt.Sprintf("%s-apply", projectName))
	applyPoller, err := p.jobsClient.BeginStart(p.ctx, p.resourceGroup, applyJobName, nil)
	if err != nil {
		return fmt.Errorf("failed to start apply job: %w", err)
	}

	applyResult, err := applyPoller.PollUntilDone(p.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start apply job: %w", err)
	}

	var applyExecutionId string
	if applyResult.ID != nil {
		applyExecutionId = *applyResult.ID
	}

	return p.waitForJobExecution(applyJobName, applyExecutionId)
}

func (p *Pipeline) waitForJobExecution(jobName, executionId string) error {
	ticker := time.NewTicker(pollingDelay)
	defer ticker.Stop()

	parts := strings.Split(executionId, "/")
	executionName := parts[len(parts)-1]

	for {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-ticker.C:
			status, err := p.getExecutionStatus(jobName, executionName)
			if err != nil {
				return err
			}
			if status == nil {
				continue
			}

			switch *status {
			case armappcontainers.JobExecutionRunningStateSucceeded:
				return nil
			case armappcontainers.JobExecutionRunningStateFailed:
				return fmt.Errorf("job execution failed")
			case armappcontainers.JobExecutionRunningStateRunning, armappcontainers.JobExecutionRunningStateProcessing:
				continue
			default:
				continue
			}
		}
	}
}

func (p *Pipeline) getExecutionStatus(jobName, executionName string) (*armappcontainers.JobExecutionRunningState, error) {
	pager := p.executionClient.NewListPager(p.resourceGroup, jobName, nil)
	for pager.More() {
		resp, err := pager.NextPage(p.ctx)
		if err != nil {
			return nil, err
		}
		for _, execution := range resp.Value {
			if execution.Name != nil && *execution.Name == executionName {
				if execution.Properties != nil && execution.Properties.Status != nil {
					return execution.Properties.Status, nil
				}
				return nil, nil
			}
		}
	}
	return nil, nil
}

func (p *Pipeline) waitForManualApproval(pipelineName string, step model.Step, approve model.ManualApprove) error {
	log.Println("Manual approval not yet implemented for Azure - auto-approving")
	return nil
}

func (p *Pipeline) getChanges(pipelineName string, stepType model.StepType, executionId string) (*model.PipelineChanges, error) {
	if p.logging == nil {
		return nil, nil
	}

	switch stepType {
	case model.StepTypeTerraform:
		return p.getPipelineChanges(pipelineName, executionId, terraform.ParseLogChanges)
	case model.StepTypeArgoCD:
		return p.getPipelineChanges(pipelineName, executionId, argocd.ParseLogChanges)
	}
	return &model.PipelineChanges{}, nil
}

func (p *Pipeline) getPipelineChanges(pipelineName, executionId string, logParser func(string, string) (*model.PipelineChanges, error)) (*model.PipelineChanges, error) {
	logs, err := p.logging.GetLogs(sanitizeJobName(pipelineName), executionId)
	if err != nil {
		return nil, err
	}

	for _, logRow := range logs {
		changes, err := logParser(pipelineName, logRow)
		if err != nil {
			return nil, err
		}
		if changes != nil {
			return changes, nil
		}
	}
	return nil, fmt.Errorf("couldn't find plan output from logs for %s", pipelineName)
}

func (p *Pipeline) getLink(pipelineName string) string {
	return fmt.Sprintf(linkFormat, p.subscriptionId, p.resourceGroup, sanitizeJobName(pipelineName))
}

func (p *Pipeline) DeletePipeline(projectName string) error {
	err := p.builder.DeleteProject(projectName, model.Step{})
	if err != nil {
		return err
	}

	err = p.builder.DeleteProject(fmt.Sprintf("%s-apply", projectName), model.Step{})
	if err != nil {
		return err
	}

	err = p.builder.DeleteProject(fmt.Sprintf("%s-plan-destroy", projectName), model.Step{})
	if err != nil {
		return err
	}

	return p.builder.DeleteProject(fmt.Sprintf("%s-apply-destroy", projectName), model.Step{})
}

func (p *Pipeline) StartDestroyExecution(projectName string, step model.Step) error {
	planDestroyJob := fmt.Sprintf("%s-plan-destroy", projectName)
	_, err := p.builder.executeJob(planDestroyJob, true)
	if err != nil {
		return err
	}

	applyDestroyJob := fmt.Sprintf("%s-apply-destroy", projectName)
	_, err = p.builder.executeJob(applyDestroyJob, true)
	return err
}
