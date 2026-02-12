package azure

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const (
	pollingDelay       = 10 * time.Second
	approvalTimeout    = 60 * time.Minute
	pipelineRepoName   = "entigo-infralib-pipelines"
	pipelineRepoFolder = "pipelines"
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
	devOps          *DevOpsClient
	repoID          string
}

func NewPipeline(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup, location, cloudPrefix, managedIdentity string, storage *BlobStorage, builder *Builder, logging *Logging, manager model.NotificationManager, devOps *DevOpsClient) (*Pipeline, error) {
	p := &Pipeline{
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
		devOps:          devOps,
	}

	// Initialize the pipeline repository if DevOps client is available
	if devOps != nil {
		repo, err := devOps.GetOrCreateRepository(pipelineRepoName)
		if err != nil {
			return nil, fmt.Errorf("failed to create pipeline repository: %w", err)
		}
		p.repoID = repo.ID
		log.Printf("Using Azure DevOps repository: %s\n", repo.Name)
	}

	return p, nil
}

func (p *Pipeline) CreatePipeline(projectName, stepName string, step model.Step, bucket model.Bucket, authSources map[string]model.SourceAuth) (*string, error) {
	if p.devOps == nil {
		return nil, fmt.Errorf("azure DevOps client not configured")
	}

	metadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return nil, err
	}

	// Create or update the apply pipeline
	_, err = p.createOrUpdatePipeline(projectName, stepName, step, metadata.Name, false, authSources)
	if err != nil {
		return nil, fmt.Errorf("failed to create apply pipeline: %w", err)
	}

	// Create or update the destroy pipeline
	_, err = p.createOrUpdatePipeline(fmt.Sprintf("%s-destroy", projectName), stepName, step, metadata.Name, true, authSources)
	if err != nil {
		return nil, fmt.Errorf("failed to create destroy pipeline: %w", err)
	}

	// Start the apply pipeline
	return p.StartPipelineExecution(projectName, stepName, step, metadata.Name)
}

func (p *Pipeline) createOrUpdatePipeline(pipelineName, stepName string, step model.Step, bucket string, isDestroy bool, authSources map[string]model.SourceAuth) (int, error) {
	// Generate YAML pipeline definition
	yamlContent := p.generatePipelineYAML(pipelineName, stepName, step, bucket, isDestroy, authSources)

	// Push YAML to repository
	yamlPath := fmt.Sprintf("/%s/%s.yml", pipelineRepoFolder, sanitizeJobName(pipelineName))
	err := p.devOps.PushFileToRepository(p.repoID, "main", yamlPath, yamlContent,
		fmt.Sprintf("Update pipeline definition for %s", pipelineName))
	if err != nil {
		return 0, fmt.Errorf("failed to push pipeline YAML: %w", err)
	}

	// Check if pipeline already exists
	existing, err := p.devOps.GetPipelineByName(sanitizeJobName(pipelineName))
	if err != nil {
		return 0, err
	}

	if existing != nil {
		log.Printf("Updated Azure Pipeline %s\n", pipelineName)
		return existing.ID, nil
	}

	// Create new pipeline
	pipeline, err := p.devOps.CreatePipeline(sanitizeJobName(pipelineName), p.repoID, yamlPath)
	if err != nil {
		return 0, fmt.Errorf("failed to create pipeline: %w", err)
	}

	log.Printf("Created Azure Pipeline %s\n", pipelineName)
	return pipeline.ID, nil
}

func (p *Pipeline) generatePipelineYAML(pipelineName, stepName string, step model.Step, bucket string, isDestroy bool, authSources map[string]model.SourceAuth) string {
	image := getImage(step.BaseImageVersion, step.BaseImageSource)

	var planCommand, applyCommand model.ActionCommand
	if isDestroy {
		planCommand, applyCommand = model.GetDestroyCommands(step.Type)
	} else {
		planCommand, applyCommand = model.GetCommands(step.Type)
	}

	envVars := p.buildEnvVarsYAML(stepName, step, bucket, authSources)

	planStageName := "Plan"
	applyStageName := "Apply"
	if isDestroy {
		planStageName = "PlanDestroy"
		applyStageName = "ApplyDestroy"
	}

	//	yaml := fmt.Sprintf(`# Auto-generated pipeline for %s
	//# Do not edit manually - managed by entigo-infralib-agent
	//
	//trigger: none
	//
	//pool:
	//  vmImage: 'ubuntu-latest'
	//
	//variables:
	//  infraImage: '%s'
	//
	//stages:
	//- stage: %s
	//  displayName: '%s'
	//  jobs:
	//  - job: %sJob
	//    displayName: 'Run %s'
	//    container:
	//      image: $(infraImage)
	//    steps:
	//    - script: /usr/bin/entrypoint.sh
	//      displayName: 'Execute %s'
	//      env:
	//        COMMAND: '%s'
	//        TF_VAR_prefix: '%s'
	//        INFRALIB_BUCKET: '%s'
	//        AZURE_SUBSCRIPTION_ID: '%s'
	//        AZURE_RESOURCE_GROUP: '%s'
	//        AZURE_LOCATION: '%s'
	//%s
	//
	//- stage: Approve
	//  displayName: 'Manual Approval'
	//  dependsOn: %s
	//  jobs:
	//  - job: waitForValidation
	//    displayName: 'Wait for approval'
	//    pool: server
	//    steps:
	//    - task: ManualValidation@1
	//      timeoutInMinutes: 60
	//      inputs:
	//        instructions: 'Review the %s plan output and approve to proceed with %s'
	//        onTimeout: 'reject'
	//
	//- stage: %s
	//  displayName: '%s'
	//  dependsOn: Approve
	//  jobs:
	//  - job: %sJob
	//    displayName: 'Run %s'
	//    container:
	//      image: $(infraImage)
	//    steps:
	//    - script: /usr/bin/entrypoint.sh
	//      displayName: 'Execute %s'
	//      env:
	//        COMMAND: '%s'
	//        TF_VAR_prefix: '%s'
	//        INFRALIB_BUCKET: '%s'
	//        AZURE_SUBSCRIPTION_ID: '%s'
	//        AZURE_RESOURCE_GROUP: '%s'
	//        AZURE_LOCATION: '%s'
	//%s
	//`,
	//		pipelineName,
	//		image,
	//		planStageName, planStageName,
	//		planStageName, string(planCommand),
	//		string(planCommand),
	//		planCommand, stepName, bucket, p.subscriptionId, p.resourceGroup, p.location,
	//		envVars,
	//		planStageName,
	//		pipelineName, string(applyCommand),
	//		applyStageName, applyStageName,
	//		applyStageName, string(applyCommand),
	//		string(applyCommand),
	//		applyCommand, stepName, bucket, p.subscriptionId, p.resourceGroup, p.location,
	//		envVars,
	//	)
	yaml := fmt.Sprintf(`# Auto-generated pipeline for %s
# Do not edit manually - managed by entigo-infralib-agent

trigger: none

pool:
  vmImage: 'ubuntu-latest'

variables:
  infraImage: '%s'

stages:
- stage: Deployment
  displayName: 'Infrastructure Deployment'
  jobs:
  - job: %[3]sJob
    displayName: 'Run %[4]s'
    container:
      image: $(infraImage)
    steps:
    - script: /usr/bin/entrypoint.sh
      displayName: 'Execute %[4]s'
      env:
        COMMAND: '%[5]s'
        TF_VAR_prefix: '%[6]s'
        INFRALIB_BUCKET: '%[7]s'
        AZURE_SUBSCRIPTION_ID: '%[8]s'
        AZURE_RESOURCE_GROUP: '%[9]s'
        AZURE_LOCATION: '%[10]s'
%[11]s

  - job: waitForValidation
    dependsOn: %[3]sJob
    displayName: 'Wait for approval'
    pool: server
    steps:
    - task: ManualValidation@1
      timeoutInMinutes: 60
      inputs:
        instructions: 'Review the %[3]s plan output and approve to proceed with %[13]s'
        onTimeout: 'reject'

  - job: %[12]sJob
    dependsOn: waitForValidation
    displayName: 'Run %[13]s'
    container:
      image: $(infraImage)
    steps:
    - script: /usr/bin/entrypoint.sh
      displayName: 'Execute %[13]s'
      env:
        COMMAND: '%[14]s'
        TF_VAR_prefix: '%[6]s'
        INFRALIB_BUCKET: '%[7]s'
        AZURE_SUBSCRIPTION_ID: '%[8]s'
        AZURE_RESOURCE_GROUP: '%[9]s'
        AZURE_LOCATION: '%[10]s'
%[11]s
`,
		pipelineName,         // 1
		image,                // 2
		planStageName,        // 3
		string(planCommand),  // 4
		planCommand,          // 5
		stepName,             // 6
		bucket,               // 7
		p.subscriptionId,     // 8
		p.resourceGroup,      // 9
		p.location,           // 10
		envVars,              // 11
		applyStageName,       // 12
		string(applyCommand), // 13
		applyCommand,         // 14
	)
	return yaml
}

func (p *Pipeline) buildEnvVarsYAML(stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) string {
	var envVars []string

	if step.Type == model.StepTypeTerraform {
		envVars = append(envVars, fmt.Sprintf("        TERRAFORM_CACHE: '%t'", p.builder.terraformCache))
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				envVars = append(envVars,
					fmt.Sprintf("        GIT_AUTH_USERNAME_%s: '%s'", strings.ToUpper(module.Name), module.HttpUsername),
					fmt.Sprintf("        GIT_AUTH_PASSWORD_%s: '%s'", strings.ToUpper(module.Name), module.HttpPassword),
					fmt.Sprintf("        GIT_AUTH_SOURCE_%s: '%s'", strings.ToUpper(module.Name), module.Source),
				)
			}
		}
	}

	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			envVars = append(envVars, fmt.Sprintf("        KUBERNETES_CLUSTER_NAME: '%s'", step.KubernetesClusterName))
		}
		namespace := "argocd"
		if step.ArgocdNamespace != "" {
			namespace = step.ArgocdNamespace
		}
		envVars = append(envVars, fmt.Sprintf("        ARGOCD_NAMESPACE: '%s'", namespace))
	}

	for source := range authSources {
		hash := util.HashCode(source)
		envVars = append(envVars,
			fmt.Sprintf("        %s: '%s'", fmt.Sprintf(model.GitUsernameEnvFormat, hash), fmt.Sprintf(model.GitUsernameFormat, hash)),
			fmt.Sprintf("        %s: '%s'", fmt.Sprintf(model.GitPasswordEnvFormat, hash), fmt.Sprintf(model.GitPasswordFormat, hash)),
			fmt.Sprintf("        %s: '%s'", fmt.Sprintf(model.GitSourceEnvFormat, hash), fmt.Sprintf(model.GitSourceFormat, hash)),
		)
	}

	return strings.Join(envVars, "\n")
}

func (p *Pipeline) CreateAgentPipelines(prefix, projectName, bucket string, run bool) error {
	if p.devOps == nil {
		// Fall back to Container Apps Jobs for agent pipelines
		return p.createAgentPipelinesWithJobs(prefix, projectName, bucket, run)
	}

	updatePipeline := fmt.Sprintf("%s-%s", projectName, common.UpdateCommand)
	existing, err := p.devOps.GetPipelineByName(sanitizeJobName(updatePipeline))
	if err != nil {
		return err
	}
	if existing == nil {
		err = p.createAgentPipeline(prefix, updatePipeline)
		if err != nil {
			return err
		}
	}

	runPipeline := fmt.Sprintf("%s-%s", projectName, common.RunCommand)
	existing, err = p.devOps.GetPipelineByName(sanitizeJobName(runPipeline))
	if err != nil {
		return err
	}
	if existing != nil {
		if !run {
			return nil
		}
		_, err = p.devOps.StartPipelineRun(existing.ID, nil)
		return err
	}

	err = p.createAgentPipeline(prefix, runPipeline)
	if err != nil || !run {
		return err
	}

	// Start the run pipeline
	newPipeline, err := p.devOps.GetPipelineByName(sanitizeJobName(runPipeline))
	if err != nil || newPipeline == nil {
		return err
	}
	_, err = p.devOps.StartPipelineRun(newPipeline.ID, nil)
	return err
}

func (p *Pipeline) createAgentPipeline(prefix, pipelineName string) error {
	yaml := p.generateAgentPipelineYAML(prefix, pipelineName)

	yamlPath := fmt.Sprintf("/%s/%s.yml", pipelineRepoFolder, sanitizeJobName(pipelineName))
	err := p.devOps.PushFileToRepository(p.repoID, "main", yamlPath, yaml,
		fmt.Sprintf("Create agent pipeline %s", pipelineName))
	if err != nil {
		return fmt.Errorf("failed to push agent pipeline YAML: %w", err)
	}

	_, err = p.devOps.CreatePipeline(sanitizeJobName(pipelineName), p.repoID, yamlPath)
	if err != nil {
		return fmt.Errorf("failed to create agent pipeline: %w", err)
	}

	log.Printf("Created agent Azure Pipeline %s\n", pipelineName)
	return nil
}

func (p *Pipeline) generateAgentPipelineYAML(prefix, pipelineName string) string {
	cmd := common.RunCommand
	if strings.HasSuffix(pipelineName, string(common.UpdateCommand)) {
		cmd = common.UpdateCommand
	}

	return fmt.Sprintf(`# Auto-generated agent pipeline for %s
# Do not edit manually - managed by entigo-infralib-agent

trigger: none

pool:
  vmImage: 'ubuntu-latest'

variables:
  agentImage: '%s:%s'

stages:
- stage: AgentRun
  displayName: 'Agent %s'
  jobs:
  - job: AgentJob
    displayName: 'Run Agent'
    container:
      image: $(agentImage)
    steps:
    - script: ei-agent %s
      displayName: 'Execute Agent'
      env:
        %s: '%s'
        %s: '%s'
        %s: '%s'
        %s: '%s'
        TERRAFORM_CACHE: '%t'
`,
		pipelineName,
		model.AgentImageAzure, model.LatestImageVersion,
		cmd,
		cmd,
		common.AwsPrefixEnv, prefix,
		common.SubscriptionIdEnv, p.subscriptionId,
		common.ResourceGroupEnv, p.resourceGroup,
		common.LocationEnv, p.location,
		p.builder.terraformCache,
	)
}

func (p *Pipeline) createAgentPipelinesWithJobs(prefix, projectName, bucket string, run bool) error {
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
	if p.devOps == nil {
		return nil
	}

	// Update apply pipeline
	_, err := p.createOrUpdatePipeline(pipelineName, stepName, step, bucket, false, authSources)
	if err != nil {
		return err
	}

	// Update destroy pipeline
	_, err = p.createOrUpdatePipeline(fmt.Sprintf("%s-destroy", pipelineName), stepName, step, bucket, true, authSources)
	return err
}

func (p *Pipeline) StartAgentExecution(pipelineName string) error {
	if p.devOps == nil {
		_, err := p.builder.executeJob(pipelineName, false)
		return err
	}

	pipeline, err := p.devOps.GetPipelineByName(sanitizeJobName(pipelineName))
	if err != nil || pipeline == nil {
		return fmt.Errorf("pipeline %s not found", pipelineName)
	}

	_, err = p.devOps.StartPipelineRun(pipeline.ID, nil)
	return err
}

func (p *Pipeline) StartPipelineExecution(pipelineName, stepName string, step model.Step, bucket string) (*string, error) {
	if p.devOps == nil {
		return nil, fmt.Errorf("azure DevOps client not configured")
	}

	log.Printf("Starting pipeline %s\n", pipelineName)

	pipeline, err := p.devOps.GetPipelineByName(sanitizeJobName(pipelineName))
	if err != nil {
		return nil, err
	}
	if pipeline == nil {
		return nil, fmt.Errorf("pipeline %s not found", pipelineName)
	}

	run, err := p.devOps.StartPipelineRun(pipeline.ID, nil)
	if err != nil {
		return nil, err
	}

	executionId := fmt.Sprintf("%d:%d", pipeline.ID, run.ID)
	return &executionId, nil
}

func (p *Pipeline) WaitPipelineExecution(pipelineName, projectName string, executionId *string, autoApprove bool, step model.Step, approve model.ManualApprove) error {
	if executionId == nil {
		return fmt.Errorf("execution id is nil")
	}

	if p.devOps == nil {
		return fmt.Errorf("azure DevOps client not configured")
	}

	log.Printf("Waiting for pipeline %s to complete, polling delay %s\n", pipelineName, pollingDelay)

	// Parse execution ID (format: pipelineID:runID)
	var pipelineID, runID int
	_, err := fmt.Sscanf(*executionId, "%d:%d", &pipelineID, &runID)
	if err != nil {
		return fmt.Errorf("invalid execution id format: %w", err)
	}

	ticker := time.NewTicker(pollingDelay)
	defer ticker.Stop()

	approvalHandled := false
	var pipeChanges *model.PipelineChanges

	for {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-ticker.C:
			run, err := p.devOps.GetPipelineRun(pipelineID, runID)
			if err != nil {
				return err
			}

			switch run.State {
			case "completed":
				if run.Result == "succeeded" {
					return nil
				}
				return fmt.Errorf("pipeline execution %s: %s", run.State, run.Result)
			case "canceling", "canceled":
				return fmt.Errorf("pipeline execution was canceled")
			}

			// Check for pending approvals
			if !approvalHandled {
				approvals, err := p.devOps.GetPendingApprovals(runID)
				if err != nil {
					slog.Debug(fmt.Sprintf("Failed to get approvals: %s", err))
					continue
				}

				if len(approvals) > 0 {
					// Get pipeline changes from logs
					if pipeChanges == nil {
						pipeChanges, err = p.getChanges(pipelineName, step.Type, runID)
						if err != nil {
							slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to get pipeline changes: %s", err)))
						}
					}

					approvalHandled, err = p.handleApproval(pipelineName, approvals[0], pipeChanges, step, autoApprove, approve)
					if err != nil {
						return err
					}
				}
			}
		}
	}
}

func (p *Pipeline) handleApproval(pipelineName string, approval Approval, pipeChanges *model.PipelineChanges, step model.Step, autoApprove bool, approve model.ManualApprove) (bool, error) {
	if pipeChanges != nil && util.ShouldStopPipeline(*pipeChanges, step.Approve, approve) {
		log.Printf("Rejecting pipeline %s - no changes or rejected\n", pipelineName)
		err := p.devOps.UpdateApproval(approval.ID, "rejected", "No changes detected or rejected by policy")
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to reject approval: %s", err)))
		}
		if step.Approve == model.ApproveReject || approve == model.ManualApproveReject {
			return true, fmt.Errorf("stopped because step approve type is 'reject'")
		}
		return true, nil
	}

	if pipeChanges != nil && util.ShouldApprovePipeline(*pipeChanges, step.Approve, autoApprove, approve) {
		log.Printf("Auto-approving pipeline %s\n", pipelineName)
		err := p.devOps.UpdateApproval(approval.ID, "approved", "Auto-approved by entigo-infralib-agent - no destructive changes")
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to auto-approve: %s", err)))
			return false, nil
		}
		return true, nil
	}

	// Wait for manual approval
	log.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
	if p.manager != nil && pipeChanges != nil {
		p.manager.ManualApproval(pipelineName, *pipeChanges, p.getLink(pipelineName))
	}

	return p.waitForManualApproval(pipelineName, approval.ID)
}

func (p *Pipeline) waitForManualApproval(pipelineName, approvalID string) (bool, error) {
	ticker := time.NewTicker(pollingDelay)
	defer ticker.Stop()

	timeout := time.After(approvalTimeout)

	for {
		select {
		case <-p.ctx.Done():
			return false, p.ctx.Err()
		case <-timeout:
			return false, fmt.Errorf("approval timed out after %s", approvalTimeout)
		case <-ticker.C:
			approvals, err := p.devOps.GetPendingApprovals(0)
			if err != nil {
				continue
			}

			// Check if our approval is still pending
			stillPending := false
			for _, a := range approvals {
				if a.ID == approvalID {
					stillPending = true
					break
				}
			}

			if !stillPending {
				// Approval was handled (either approved or rejected externally)
				log.Printf("Manual approval received for pipeline %s\n", pipelineName)
				if p.manager != nil {
					p.manager.Message(model.MessageTypeApprovals, fmt.Sprintf("Pipeline %s was approved", pipelineName))
				}
				return true, nil
			}
		}
	}
}

func (p *Pipeline) getChanges(pipelineName string, stepType model.StepType, runID int) (*model.PipelineChanges, error) {
	logs, err := p.devOps.GetBuildLogs(runID)
	if err != nil {
		return nil, err
	}

	var logParser func(string, string) (*model.PipelineChanges, error)
	switch stepType {
	case model.StepTypeTerraform:
		logParser = terraform.ParseLogChanges
	case model.StepTypeArgoCD:
		logParser = argocd.ParseLogChanges
	default:
		return &model.PipelineChanges{}, nil
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
	if p.devOps != nil {
		return p.devOps.GetPipelineLink(pipelineName)
	}
	// Fallback to Azure Portal link for Container Apps Jobs
	return fmt.Sprintf("https://portal.azure.com/#@/resource/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/jobs/%s",
		p.subscriptionId, p.resourceGroup, sanitizeJobName(pipelineName))
}

func (p *Pipeline) DeletePipeline(projectName string) error {
	// Delete Container Apps Jobs (kept for backwards compatibility and agent jobs)
	err := p.builder.DeleteProject(projectName, model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete job %s: %s", projectName, err)))
	}

	err = p.builder.DeleteProject(fmt.Sprintf("%s-apply", projectName), model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete apply job %s: %s", projectName, err)))
	}

	err = p.builder.DeleteProject(fmt.Sprintf("%s-plan-destroy", projectName), model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete plan-destroy job %s: %s", projectName, err)))
	}

	err = p.builder.DeleteProject(fmt.Sprintf("%s-apply-destroy", projectName), model.Step{})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete apply-destroy job %s: %s", projectName, err)))
	}

	// Note: Azure Pipelines don't have a simple delete API - they need to be deleted via the UI
	// or through additional API calls. For now, we just delete the jobs.
	log.Printf("Deleted pipeline resources for %s\n", projectName)
	return nil
}

func (p *Pipeline) StartDestroyExecution(projectName string, step model.Step) error {
	if p.devOps == nil {
		// Fall back to Container Apps Jobs
		planDestroyJob := fmt.Sprintf("%s-plan-destroy", projectName)
		_, err := p.builder.executeJob(planDestroyJob, true)
		if err != nil {
			return err
		}

		applyDestroyJob := fmt.Sprintf("%s-apply-destroy", projectName)
		_, err = p.builder.executeJob(applyDestroyJob, true)
		return err
	}

	destroyPipelineName := fmt.Sprintf("%s-destroy", projectName)
	executionId, err := p.StartPipelineExecution(destroyPipelineName, "", step, "")
	if err != nil {
		return err
	}

	return p.WaitPipelineExecution(destroyPipelineName, projectName, executionId, true, step, "")
}
