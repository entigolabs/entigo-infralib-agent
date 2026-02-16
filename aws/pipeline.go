package aws

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/google/uuid"
)

const (
	pollingDelay = 10 * time.Second
	waitTimeout  = 2 * time.Minute

	approveStageName  = "Approve"
	approveActionName = "Approval"
	planName          = "Plan"
	applyName         = "Apply"
	sourceName        = "Source"
	destroyName       = "Destroy"
	applyDestroyName  = "ApplyDestroy"

	linkFormat = "https://%s.console.aws.amazon.com/codesuite/codepipeline/pipelines/%s/view?region=%s"
)

type approvalStatus string

const (
	approvalStatusApproved approvalStatus = "Approved"
	approvalStatusStop     approvalStatus = "Stop"
	approvalStatusWaiting  approvalStatus = "Waiting"
	approvalStatusApprove  approvalStatus = "Approve"
)

type Pipeline struct {
	ctx            context.Context
	region         string
	codePipeline   *codepipeline.Client
	roleArn        string
	cloudWatch     CloudWatch
	logGroup       string
	logStream      string
	terraformCache bool
	manager        model.NotificationManager
}

func NewPipeline(ctx context.Context, awsConfig aws.Config, roleArn string, cloudWatch CloudWatch, logGroup string, logStream string, terraformCache bool, manager model.NotificationManager) *Pipeline {
	return &Pipeline{
		ctx:            ctx,
		region:         awsConfig.Region,
		codePipeline:   codepipeline.NewFromConfig(awsConfig),
		roleArn:        roleArn,
		cloudWatch:     cloudWatch,
		logGroup:       logGroup,
		logStream:      logStream,
		terraformCache: terraformCache,
		manager:        manager,
	}
}

func (p *Pipeline) CreatePipeline(projectName string, stepName string, step model.Step, bucket model.Bucket, authSources map[string]model.SourceAuth) (*string, error) {
	metadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return nil, err
	}
	execution, err := p.CreateApplyPipeline(projectName, projectName, stepName, step, metadata.Name, authSources)
	if err != nil {
		return nil, err
	}
	err = p.CreateDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step,
		metadata.Name, authSources)
	return execution, err
}

func (p *Pipeline) DeletePipeline(projectName string) error {
	err := p.deletePipeline(projectName)
	if err != nil {
		return err
	}
	return p.deletePipeline(fmt.Sprintf("%s-destroy", projectName))
}

func (p *Pipeline) deletePipeline(projectName string) error {
	_, err := p.codePipeline.DeletePipeline(p.ctx, &codepipeline.DeletePipelineInput{
		Name: aws.String(projectName),
	})
	if err != nil {
		var notFoundError *types.PipelineNotFoundException
		if errors.As(err, &notFoundError) {
			return nil
		}
		return err
	}
	log.Printf("Deleted CodePipeline %s\n", projectName)
	return nil
}

func (p *Pipeline) CreateApplyPipeline(pipelineName string, projectName string, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) (*string, error) {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return nil, err
	}
	if pipe != nil {
		return p.startUpdatedPipeline(pipe, stepName, step, bucket, authSources)
	}
	planCommand, applyCommand := model.GetCommands(step.Type)
	_, err = p.codePipeline.CreatePipeline(p.ctx, &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(bucket),
				Type:     types.ArtifactStoreTypeS3,
			},
			Stages: []types.StageDeclaration{{
				Name: aws.String(sourceName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(sourceName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategorySource,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("S3"),
						Version:  aws.String("1"),
					},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("source_output")}},
					RunOrder:        aws.Int32(1),
					Configuration: map[string]string{
						"S3Bucket":             bucket,
						"S3ObjectKey":          model.AgentSource,
						"PollForSourceChanges": "false",
					},
				},
				},
			}, {
				Name: aws.String(planName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(planName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryBuild,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeBuild"),
						Version:  aws.String("1"),
					},
					InputArtifacts:  []types.InputArtifact{{Name: aws.String("source_output")}},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("Plan")}},
					RunOrder:        aws.Int32(2),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": p.getEnvironmentVariablesByType(planCommand, stepName, step, bucket, authSources),
					},
				},
				},
			}, {
				Name: aws.String(approveStageName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(approveActionName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryApproval,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("Manual"),
						Version:  aws.String("1"),
					},
					RunOrder:         aws.Int32(3),
					TimeoutInMinutes: aws.Int32(60),
				}},
			}, {
				Name: aws.String(applyName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(applyName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryBuild,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeBuild"),
						Version:  aws.String("1"),
					},
					InputArtifacts: []types.InputArtifact{{Name: aws.String("source_output")}, {Name: aws.String("Plan")}},
					RunOrder:       aws.Int32(4),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": p.getEnvironmentVariablesByType(applyCommand, stepName, step, bucket, authSources),
					},
				},
				},
			},
			},
		},
		Tags: []types.Tag{{
			Key:   aws.String(model.ResourceTagKey),
			Value: aws.String(model.ResourceTagValue),
		}},
	})
	if err != nil {
		return nil, err
	}
	log.Printf("Created CodePipeline %s\n", pipelineName)
	return p.getNewPipelineExecutionId(pipelineName)
}

func (p *Pipeline) getNewPipelineExecutionId(pipelineName string) (*string, error) {
	time.Sleep(5 * time.Second) // Wait for the pipeline to start executing
	executions, err := p.codePipeline.ListPipelineExecutions(p.ctx, &codepipeline.ListPipelineExecutionsInput{
		PipelineName: aws.String(pipelineName),
	})
	if err != nil {
		return nil, err
	}
	summaries := executions.PipelineExecutionSummaries
	if len(summaries) == 0 {
		return nil, fmt.Errorf("couldn't find a pipeline execution id")
	}
	var oldestExecutionId *string
	var oldestStartTime *time.Time
	for _, execution := range summaries {
		if oldestStartTime == nil || execution.StartTime.Before(*oldestStartTime) {
			oldestExecutionId = execution.PipelineExecutionId
			oldestStartTime = execution.StartTime
		}
	}
	return oldestExecutionId, nil
}

func (p *Pipeline) CreateDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) error {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipe != nil {
		return p.updatePipeline(pipe, stepName, step, bucket, authSources)
	}
	var planCommand model.ActionCommand
	var applyCommand model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		planCommand = model.ArgoCDPlanDestroyCommand
		applyCommand = model.ArgoCDApplyDestroyCommand
	} else {
		planCommand = model.PlanDestroyCommand
		applyCommand = model.ApplyDestroyCommand
	}
	_, err = p.codePipeline.CreatePipeline(p.ctx, &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(bucket),
				Type:     types.ArtifactStoreTypeS3,
			}, Stages: []types.StageDeclaration{{
				Name: aws.String(sourceName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(sourceName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategorySource,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("S3"),
						Version:  aws.String("1"),
					},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("source_output")}},
					RunOrder:        aws.Int32(1),
					Configuration: map[string]string{
						"S3Bucket":             bucket,
						"S3ObjectKey":          model.AgentSource,
						"PollForSourceChanges": "false",
					},
				},
				},
			}, {
				Name: aws.String(destroyName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(destroyName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryBuild,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeBuild"),
						Version:  aws.String("1"),
					},
					InputArtifacts:  []types.InputArtifact{{Name: aws.String("source_output")}},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("Plan")}},
					RunOrder:        aws.Int32(2),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": p.getEnvironmentVariablesByType(planCommand, stepName, step, bucket, authSources),
					},
				},
				},
			}, {
				Name: aws.String(approveStageName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(approveActionName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryApproval,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("Manual"),
						Version:  aws.String("1"),
					},
					RunOrder:         aws.Int32(3),
					TimeoutInMinutes: aws.Int32(60),
				}},
			}, {
				Name: aws.String(applyDestroyName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(applyDestroyName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryBuild,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeBuild"),
						Version:  aws.String("1"),
					},
					InputArtifacts: []types.InputArtifact{{Name: aws.String("source_output")}, {Name: aws.String("Plan")}},
					RunOrder:       aws.Int32(4),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": p.getEnvironmentVariablesByType(applyCommand, stepName, step, bucket, authSources),
					},
				},
				},
			},
			},
		},
		Tags: []types.Tag{{
			Key:   aws.String(model.ResourceTagKey),
			Value: aws.String(model.ResourceTagValue),
		}},
	})
	if err != nil {
		return err
	}
	log.Printf("Created destroy CodePipeline %s\n", pipelineName)
	err = p.disableStageTransition(pipelineName, destroyName)
	if err != nil {
		return err
	}
	err = p.disableStageTransition(pipelineName, approveStageName)
	if err != nil {
		return err
	}
	err = p.disableStageTransition(pipelineName, applyDestroyName)
	if err != nil {
		return err
	}
	time.Sleep(5 * time.Second) // Wait for pipeline to start executing
	return p.stopLatestPipelineExecutions(pipelineName, 1)
}

func (p *Pipeline) CreateAgentPipelines(prefix string, projectName string, bucket string, run bool) error {
	updatePipeline := fmt.Sprintf("%s-%s", projectName, common.UpdateCommand)
	pipe, err := p.getPipeline(updatePipeline)
	if err != nil {
		return err
	}
	if pipe == nil {
		err = p.createAgentPipeline(prefix, updatePipeline, bucket)
		if err != nil {
			return err
		}
		time.Sleep(5 * time.Second) // Wait for pipeline to start executing
		err = p.stopLatestPipelineExecutions(updatePipeline, 1)
		if err != nil {
			return err
		}
	}

	runPipeline := fmt.Sprintf("%s-%s", projectName, common.RunCommand)
	pipe, err = p.getPipeline(runPipeline)
	if err != nil {
		return err
	}
	if pipe != nil {
		if !run {
			return nil
		}
		_, err = p.StartPipelineExecution(runPipeline, "", model.Step{}, "")
		return err
	}
	err = p.createAgentPipeline(prefix, runPipeline, bucket)
	if err != nil || run {
		return err
	}
	time.Sleep(5 * time.Second)
	return p.stopLatestPipelineExecutions(runPipeline, 1)
}

func (p *Pipeline) createAgentPipeline(prefix string, projectName string, bucket string) error {
	_, err := p.codePipeline.CreatePipeline(p.ctx, &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(projectName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(bucket),
				Type:     types.ArtifactStoreTypeS3,
			}, Stages: []types.StageDeclaration{{
				Name: aws.String(sourceName),
				Actions: []types.ActionDeclaration{{
					Name: aws.String(sourceName),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategorySource,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("S3"),
						Version:  aws.String("1"),
					},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("source_output")}},
					RunOrder:        aws.Int32(1),
					Configuration: map[string]string{
						"S3Bucket":             bucket,
						"S3ObjectKey":          model.AgentSource,
						"PollForSourceChanges": "false",
					},
				},
				},
			}, {
				Name: aws.String("AgentRun"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("AgentRun"),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryBuild,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeBuild"),
						Version:  aws.String("1"),
					},
					InputArtifacts: []types.InputArtifact{{Name: aws.String("source_output")}},
					RunOrder:       aws.Int32(2),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": fmt.Sprintf(`[{"name":"%s","value":"%s"}]`, common.AwsPrefixEnv, prefix),
					},
				},
				},
			},
			},
		},
		Tags: []types.Tag{{
			Key:   aws.String(model.ResourceTagKey),
			Value: aws.String(model.ResourceTagValue),
		}},
	})
	if err == nil {
		log.Printf("Created CodePipeline %s\n", projectName)
	}
	return err
}

func (p *Pipeline) StartPipelineExecution(pipelineName string, _ string, _ model.Step, _ string) (*string, error) {
	log.Printf("Starting pipeline %s\n", pipelineName)
	execution, err := p.codePipeline.StartPipelineExecution(p.ctx, &codepipeline.StartPipelineExecutionInput{
		Name:               aws.String(pipelineName),
		ClientRequestToken: aws.String(uuid.New().String()),
	})
	if err != nil {
		return nil, err
	}
	return execution.PipelineExecutionId, nil
}

func (p *Pipeline) StartAgentExecution(pipelineName string) error {
	_, err := p.StartPipelineExecution(pipelineName, "", model.Step{}, "")
	return err
}

func (p *Pipeline) startUpdatedPipeline(pipeline *types.PipelineDeclaration, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) (*string, error) {
	err := p.updatePipeline(pipeline, stepName, step, bucket, authSources)
	if err != nil {
		return nil, err
	}
	return p.StartPipelineExecution(*pipeline.Name, stepName, step, "")
}

func (p *Pipeline) UpdatePipeline(pipelineName string, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) error {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipe == nil {
		return fmt.Errorf("pipeline %s not found", pipelineName)
	}
	err = p.updatePipeline(pipe, stepName, step, bucket, authSources)
	if err != nil {
		return err
	}
	destroyPipeline := fmt.Sprintf("%s-destroy", pipelineName)
	pipe, err = p.getPipeline(destroyPipeline)
	if err != nil {
		return err
	}
	if pipe == nil {
		return nil
	}
	return p.updatePipeline(pipe, stepName, step, bucket, authSources)
}

func (p *Pipeline) updatePipeline(pipeline *types.PipelineDeclaration, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) error {
	changed := false
	for _, stage := range pipeline.Stages {
		if *stage.Name == sourceName || *stage.Name == approveStageName {
			continue
		}
		for _, action := range stage.Actions {
			envVars := p.getActionEnvironmentVariables(*action.Name, stepName, step, bucket, authSources)
			if action.Configuration == nil || action.Configuration["EnvironmentVariables"] == envVars {
				continue
			}
			action.Configuration["EnvironmentVariables"] = envVars
			changed = true
		}
	}

	if !changed {
		return nil
	}
	_, err := p.codePipeline.UpdatePipeline(p.ctx, &codepipeline.UpdatePipelineInput{
		Pipeline: pipeline,
	})
	if err == nil {
		log.Printf("Updated CodePipeline %s\n", *pipeline.Name)
	}
	return err
}

func (p *Pipeline) getActionEnvironmentVariables(actionName string, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) string {
	command := getCommand(actionName, step.Type)
	if step.Type == model.StepTypeTerraform {
		return p.getTerraformEnvironmentVariables(command, stepName, step, bucket, authSources)
	} else {
		return getEnvironmentVariables(command, stepName, step, bucket, authSources)
	}
}

func getCommand(actionName string, stepType model.StepType) model.ActionCommand {
	switch actionName {
	case planName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDPlanCommand
		} else {
			return model.PlanCommand
		}
	case applyName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDApplyCommand
		} else {
			return model.ApplyCommand
		}
	case destroyName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDPlanDestroyCommand
		} else {
			return model.PlanDestroyCommand
		}
	case applyDestroyName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDApplyDestroyCommand
		} else {
			return model.ApplyDestroyCommand
		}
	}
	return ""
}

func (p *Pipeline) WaitPipelineExecution(pipelineName string, _ string, executionId *string, autoApprove bool, step model.Step, approve model.ManualApprove) error {
	if executionId == nil {
		return fmt.Errorf("execution id is nil")
	}
	log.Printf("Waiting for pipeline %s to complete, polling delay %s\n", pipelineName, pollingDelay)
	err := p.waitPipelineExecutionStart(pipelineName, executionId)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(pollingDelay)
	defer ticker.Stop()
	var status approvalStatus
	for {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-ticker.C:
			execution, err := p.codePipeline.GetPipelineExecution(p.ctx, &codepipeline.GetPipelineExecutionInput{
				PipelineName:        aws.String(pipelineName),
				PipelineExecutionId: executionId,
			})
			if err != nil {
				return err
			}
			if execution.PipelineExecution.Status != types.PipelineExecutionStatusInProgress {
				return getExecutionResult(execution.PipelineExecution.Status)
			}
			if status == approvalStatusApproved {
				continue
			}
			executionsList, err := p.codePipeline.ListActionExecutions(p.ctx, &codepipeline.ListActionExecutionsInput{
				PipelineName: aws.String(pipelineName),
				Filter:       &types.ActionExecutionFilter{PipelineExecutionId: executionId},
			})
			if err != nil {
				return err
			}
			p.stopPreviousExecution(pipelineName, *executionId, executionsList.ActionExecutionDetails)
			status, err = p.processStateStages(pipelineName, *executionId, executionsList.ActionExecutionDetails, step, autoApprove, status, approve)
			if err != nil {
				return err
			}
			if status == approvalStatusStop {
				return nil
			}
		}
	}
}

func (p *Pipeline) waitPipelineExecutionStart(pipelineName string, executionId *string) error {
	ctx, cancel := context.WithTimeout(p.ctx, waitTimeout)
	ticker := time.NewTicker(pollingDelay)
	defer ticker.Stop()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("execution %s failed to start in %s", *executionId, waitTimeout)
			}
			return ctx.Err()
		case <-ticker.C:
			_, err := p.codePipeline.GetPipelineExecution(p.ctx, &codepipeline.GetPipelineExecutionInput{
				PipelineName:        aws.String(pipelineName),
				PipelineExecutionId: executionId,
			})
			if err == nil {
				return nil
			}
			var notFoundError *types.PipelineExecutionNotFoundException
			if errors.As(err, &notFoundError) {
				continue
			}
			return err
		}
	}
}

func getExecutionResult(status types.PipelineExecutionStatus) error {
	switch status {
	case types.PipelineExecutionStatusCancelled:
		return errors.New("pipeline execution cancelled")
	case types.PipelineExecutionStatusFailed:
		return errors.New("pipeline execution failed")
	case types.PipelineExecutionStatusStopped:
		return errors.New("pipeline execution stopped")
	case types.PipelineExecutionStatusStopping:
		return errors.New("pipeline execution stopping")
	case types.PipelineExecutionStatusSuperseded:
		return errors.New("pipeline execution superseded")
	case types.PipelineExecutionStatusSucceeded:
		return nil
	}
	return fmt.Errorf("unknown pipeline execution status %s", status)
}

func (p *Pipeline) stopPreviousExecution(pipelineName, executionId string, actions []types.ActionExecutionDetail) {
	if !preApproveStage(actions) {
		return
	}
	state, err := p.codePipeline.GetPipelineState(p.ctx, &codepipeline.GetPipelineStateInput{
		Name: aws.String(pipelineName),
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("Couldn't get pipeline state for %s: %s", pipelineName, err.Error()))
		return
	}
	for _, stage := range state.StageStates {
		if *stage.StageName != approveStageName {
			continue
		}
		if stage.InboundExecution == nil || *stage.InboundExecution.PipelineExecutionId != executionId ||
			stage.LatestExecution == nil || stage.LatestExecution.Status != types.StageExecutionStatusInProgress {
			return
		}
		previousId := *stage.LatestExecution.PipelineExecutionId
		log.Printf("Stopping previous pipeline %s execution\n", pipelineName)
		err = p.stopPipelineExecution(pipelineName, previousId, "New pipeline execution started")
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Couldn't stop previous pipeline %s execution %s, "+
				"please stop it manually, error: %s", pipelineName, previousId, err.Error())))
		}
		return
	}
}

func preApproveStage(actions []types.ActionExecutionDetail) bool {
	planned := false
	for _, action := range actions {
		if *action.StageName == approveStageName { // Execution has already transitioned to the approval stage
			return false
		}
		if *action.ActionName == planName && action.Status == types.ActionExecutionStatusSucceeded {
			planned = true
		}
	}
	return planned
}

func (p *Pipeline) processStateStages(pipelineName, executionId string, actions []types.ActionExecutionDetail, step model.Step, autoApprove bool, status approvalStatus, approve model.ManualApprove) (approvalStatus, error) {
	for _, action := range actions {
		if *action.StageName != approveStageName || *action.ActionName != approveActionName {
			continue
		}
		if action.Status == types.ActionExecutionStatusSucceeded {
			if status == approvalStatusWaiting && p.manager != nil {
				message := fmt.Sprintf("Pipeline %s was approved", pipelineName)
				params := map[string]string{"pipeline": pipelineName, "step": step.Name}
				if action.UpdatedBy != nil {
					message += fmt.Sprintf("\nApproved by %s", *action.UpdatedBy)
					params["approvedBy"] = *action.UpdatedBy
				}
				p.manager.Message(model.MessageTypeApprovals, message, params)
			}
			return approvalStatusApproved, nil
		}
		if action.Status != types.ActionExecutionStatusInProgress {
			return status, nil
		}
		switch status {
		case approvalStatusWaiting:
			log.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
			return status, nil
		case approvalStatusApprove:
			return p.approveStage(pipelineName)
		default:
			return p.processChanges(pipelineName, executionId, actions, step, autoApprove, approve)
		}
	}
	return status, nil
}

func (p *Pipeline) processChanges(pipelineName string, executionId string, actions []types.ActionExecutionDetail, step model.Step, autoApprove bool, approve model.ManualApprove) (approvalStatus, error) {
	pipeChanges, err := p.getChanges(pipelineName, actions, step.Type)
	if err != nil {
		return approvalStatusStop, err
	}
	if pipeChanges == nil {
		return approvalStatusStop, fmt.Errorf("couldn't get pipeline changes for %s", pipelineName)
	}
	if util.ShouldStopPipeline(*pipeChanges, step.Approve, approve) {
		return p.stopPipeline(pipelineName, executionId, step.Approve, approve)
	}
	if util.ShouldApprovePipeline(*pipeChanges, step.Approve, autoApprove, approve) {
		return p.approveStage(pipelineName)
	}
	log.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
	if p.manager != nil {
		p.manager.ManualApproval(pipelineName, *pipeChanges, p.getLink(pipelineName))
	}
	return approvalStatusWaiting, nil
}

func (p *Pipeline) getLink(pipelineName string) string {
	return fmt.Sprintf(linkFormat, p.region, pipelineName, p.region)
}

func (p *Pipeline) getChanges(pipelineName string, actions []types.ActionExecutionDetail, stepType model.StepType) (*model.PipelineChanges, error) {
	switch stepType {
	case model.StepTypeTerraform:
		return p.getPipelineChanges(pipelineName, actions, terraform.ParseLogChanges)
	case model.StepTypeArgoCD:
		return p.getPipelineChanges(pipelineName, actions, argocd.ParseLogChanges)
	}
	return &model.PipelineChanges{}, nil
}

func (p *Pipeline) getPipelineChanges(pipelineName string, actions []types.ActionExecutionDetail, logParser func(string, string) (*model.PipelineChanges, error)) (*model.PipelineChanges, error) {
	codeBuildRunId, err := getCodeBuildRunId(actions)
	if err != nil {
		return nil, err
	}
	logs, err := p.cloudWatch.GetLogs(p.logGroup, fmt.Sprintf("%s/%s", p.logStream, codeBuildRunId))
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

func (p *Pipeline) stopPipeline(pipelineName, executionId string, approve model.Approve, manualApprove model.ManualApprove) (approvalStatus, error) {
	log.Printf("Stopping pipeline %s\n", pipelineName)
	reason := "No changes detected"
	if approve == model.ApproveReject || manualApprove == model.ManualApproveReject {
		reason = "Rejected"
	}
	err := p.stopPipelineExecution(pipelineName, executionId, reason)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Couldn't stop pipeline %s, please stop manually: %s", pipelineName, err.Error())))
	}
	if approve == model.ApproveReject || manualApprove == model.ManualApproveReject {
		return approvalStatusStop, fmt.Errorf("stopped because step approve type is 'reject'")
	}
	return approvalStatusStop, nil
}

func getCodeBuildRunId(actions []types.ActionExecutionDetail) (string, error) {
	for _, action := range actions {
		if *action.ActionName != planName && *action.ActionName != destroyName {
			continue
		}
		if action.Output == nil || action.Output.ExecutionResult == nil || action.Output.ExecutionResult.ExternalExecutionId == nil {
			return "", fmt.Errorf("couldn't get plan action external execution id")
		}
		externalExecutionId := action.Output.ExecutionResult.ExternalExecutionId
		parts := strings.Split(*externalExecutionId, ":")
		if len(parts) != 2 {
			return "", fmt.Errorf("couldn't parse plan action external execution id from %s", *externalExecutionId)
		}
		return parts[1], nil
	}
	return "", fmt.Errorf("couldn't find a terraform plan action")
}

func (p *Pipeline) approveStage(pipelineName string) (approvalStatus, error) {
	token := p.getApprovalToken(pipelineName)
	if token == nil {
		log.Printf("No approval token found yet for %s, please wait or approve manually\n", pipelineName)
		return approvalStatusApprove, nil
	}
	_, err := p.codePipeline.PutApprovalResult(p.ctx, &codepipeline.PutApprovalResultInput{
		PipelineName: aws.String(pipelineName),
		StageName:    aws.String(approveStageName),
		ActionName:   aws.String(approveActionName),
		Token:        token,
		Result: &types.ApprovalResult{
			Status:  types.ApprovalStatusApproved,
			Summary: aws.String("Approved by entigo-infralib-agent"),
		},
	})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Couldn't approve pipeline %s, approve manually: %s",
			pipelineName, err.Error())))
		return approvalStatusApprove, nil
	}
	log.Printf("Approved stage %s for %s\n", approveStageName, pipelineName)
	return approvalStatusApproved, nil
}

func (p *Pipeline) disableStageTransition(pipelineName string, stage string) error {
	_, err := p.codePipeline.DisableStageTransition(p.ctx, &codepipeline.DisableStageTransitionInput{
		PipelineName:   aws.String(pipelineName),
		StageName:      aws.String(stage),
		Reason:         aws.String("Disable pipeline transition to prevent accidental destruction of infrastructure"),
		TransitionType: types.StageTransitionTypeInbound,
	})
	return err
}

func (p *Pipeline) stopLatestPipelineExecutions(pipelineName string, latestCount int32) error {
	executions, err := p.codePipeline.ListPipelineExecutions(p.ctx, &codepipeline.ListPipelineExecutionsInput{
		PipelineName: aws.String(pipelineName),
		MaxResults:   aws.Int32(latestCount),
	})
	if err != nil {
		return err
	}
	for _, execution := range executions.PipelineExecutionSummaries {
		if execution.Status != types.PipelineExecutionStatusInProgress {
			continue
		}
		err = p.stopPipelineExecution(pipelineName, *execution.PipelineExecutionId,
			"Abandoned pipeline execution")
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) stopPipelineExecution(pipelineName string, executionId string, reason string) error {
	_, err := p.codePipeline.StopPipelineExecution(p.ctx, &codepipeline.StopPipelineExecutionInput{
		PipelineName:        &pipelineName,
		PipelineExecutionId: &executionId,
		Abandon:             true,
		Reason:              &reason,
	})
	if err != nil {
		var awsError *types.PipelineExecutionNotStoppableException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	return nil
}

func (p *Pipeline) getPipeline(pipelineName string) (*types.PipelineDeclaration, error) {
	pipelineOutput, err := p.codePipeline.GetPipeline(p.ctx, &codepipeline.GetPipelineInput{
		Name: aws.String(pipelineName),
	})
	if err != nil {
		var awsError *types.PipelineNotFoundException
		if errors.As(err, &awsError) {
			return nil, nil
		}
		return nil, err
	}
	return pipelineOutput.Pipeline, nil
}

func (p *Pipeline) getEnvironmentVariablesByType(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) string {
	if step.Type == model.StepTypeTerraform {
		return p.getTerraformEnvironmentVariables(command, stepName, step, bucket, authSources)
	}
	return getEnvironmentVariables(command, stepName, step, bucket, authSources)
}

func (p *Pipeline) getTerraformEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) string {
	envVars := getEnvironmentVariablesList(command, stepName, step, bucket, authSources)
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"TERRAFORM_CACHE\",\"value\":\"%t\"}", p.terraformCache))
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_USERNAME_%s\",\"value\":\"%s\"}",
				strings.ToUpper(module.Name), module.HttpUsername))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_PASSWORD_%s\",\"value\":\"%s\"}",
				strings.ToUpper(module.Name), module.HttpPassword))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_SOURCE_%s\",\"value\":\"%s\"}",
				strings.ToUpper(module.Name), module.Source))
		}
	}
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) string {
	envVars := getEnvironmentVariablesList(command, stepName, step, bucket, authSources)
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariablesList(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) []string {
	var envVars []string
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"COMMAND\",\"value\":\"%s\"}", command))
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"TF_VAR_prefix\",\"value\":\"%s\"}", stepName))
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"INFRALIB_BUCKET\",\"value\":\"%s\"}", bucket))
	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"KUBERNETES_CLUSTER_NAME\",\"value\":\"%s\"}", step.KubernetesClusterName))
		}
		if step.ArgocdNamespace == "" {
			envVars = append(envVars, "{\"name\":\"ARGOCD_NAMESPACE\",\"value\":\"argocd\"}")
		} else {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"ARGOCD_NAMESPACE\",\"value\":\"%s\"}", step.ArgocdNamespace))
		}
	}
	for source := range authSources {
		hash := util.HashCode(source)
		envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_USERNAME_%s\",\"value\":\"%s\",\"type\":\"SECRETS_MANAGER\"}",
			hash, fmt.Sprintf(model.GitUsernameFormat, hash)))
		envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_PASSWORD_%s\",\"value\":\"%s\",\"type\":\"SECRETS_MANAGER\"}",
			hash, fmt.Sprintf(model.GitPasswordFormat, hash)))
		envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_SOURCE_%s\",\"value\":\"%s\",\"type\":\"SECRETS_MANAGER\"}",
			hash, fmt.Sprintf(model.GitSourceFormat, hash)))
	}
	return envVars
}

func (p *Pipeline) getApprovalToken(pipelineName string) *string {
	state, err := p.codePipeline.GetPipelineState(p.ctx, &codepipeline.GetPipelineStateInput{
		Name: aws.String(pipelineName),
	})
	if err != nil {
		return nil
	}
	for _, stage := range state.StageStates {
		if *stage.StageName != approveStageName {
			continue
		}
		for _, action := range stage.ActionStates {
			if *action.ActionName != approveActionName {
				continue
			}
			if action.LatestExecution == nil {
				return nil
			}
			return action.LatestExecution.Token
		}
		break
	}
	return nil
}

func (p *Pipeline) StartDestroyExecution(projectName string, step model.Step) error {
	pipelineName := fmt.Sprintf("%s-destroy", projectName)
	pipeline, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipeline == nil {
		return model.NewNotFoundError(fmt.Sprintf("pipeline %s", pipelineName))
	}
	err = p.enableAllStageTransitions(pipelineName)
	if err != nil {
		return err
	}
	executionId, err := p.StartPipelineExecution(pipelineName, "", step, "")
	if err != nil {
		return err
	}
	return p.WaitPipelineExecution(pipelineName, pipelineName, executionId, true, step, "")
}

func (p *Pipeline) enableAllStageTransitions(pipelineName string) error {
	state, err := p.codePipeline.GetPipelineState(p.ctx, &codepipeline.GetPipelineStateInput{
		Name: aws.String(pipelineName),
	})
	if err != nil {
		return fmt.Errorf("failed to get pipeline state: %w", err)
	}
	for _, stage := range state.StageStates {
		if stage.InboundTransitionState == nil || stage.InboundTransitionState.Enabled {
			continue
		}
		_, err = p.codePipeline.EnableStageTransition(p.ctx, &codepipeline.EnableStageTransitionInput{
			PipelineName:   aws.String(pipelineName),
			StageName:      stage.StageName,
			TransitionType: types.StageTransitionTypeInbound,
		})
		if err != nil {
			return fmt.Errorf("failed to enable inbound transition for stage %s: %v", *stage.StageName, err)
		}
	}
	log.Printf("Enabled all stage transitions for pipeline %s", pipelineName)
	return nil
}
