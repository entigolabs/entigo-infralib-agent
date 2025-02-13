package aws

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/google/uuid"
	"log"
	"log/slog"
	"strings"
	"time"
)

const pollingDelay = 10

const approveStageName = "Approve"
const approveActionName = "Approval"
const planName = "Plan"
const applyName = "Apply"
const sourceName = "Source"
const destroyName = "Destroy"
const applyDestroyName = "ApplyDestroy"

type Pipeline struct {
	ctx          context.Context
	codePipeline *codepipeline.Client
	roleArn      string
	cloudWatch   CloudWatch
	logGroup     string
	logStream    string
}

func NewPipeline(ctx context.Context, awsConfig aws.Config, roleArn string, cloudWatch CloudWatch, logGroup string, logStream string) *Pipeline {
	return &Pipeline{
		ctx:          ctx,
		codePipeline: codepipeline.NewFromConfig(awsConfig),
		roleArn:      roleArn,
		cloudWatch:   cloudWatch,
		logGroup:     logGroup,
		logStream:    logStream,
	}
}

func (p *Pipeline) CreatePipeline(projectName string, stepName string, step model.Step, bucket model.Bucket) (*string, error) {
	metadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return nil, err
	}
	execution, err := p.CreateApplyPipeline(projectName, projectName, stepName, step, metadata.Name)
	if err != nil {
		return nil, err
	}
	err = p.CreateDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step, metadata.Name)
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

func (p *Pipeline) CreateApplyPipeline(pipelineName string, projectName string, stepName string, step model.Step, bucket string) (*string, error) {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return nil, err
	}
	if pipe != nil {
		return p.startUpdatedPipeline(pipe, stepName, step, bucket)
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(planCommand, stepName, step, bucket),
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
					RunOrder: aws.Int32(3),
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(applyCommand, stepName, step, bucket),
					},
				},
				},
			},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	log.Printf("Created CodePipeline %s\n", pipelineName)
	return p.getNewPipelineExecutionId(pipelineName)
}

func (p *Pipeline) CreateDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, bucket string) error {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipe != nil {
		return p.updatePipeline(pipe, stepName, step, bucket)
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(planCommand, stepName, step, bucket),
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
					RunOrder: aws.Int32(3),
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(applyCommand, stepName, step, bucket),
					},
				},
				},
			},
			},
		},
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

func (p *Pipeline) CreateAgentPipelines(prefix string, projectName string, bucket string) error {
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
		_, err = p.StartPipelineExecution(runPipeline, "", model.Step{}, "")
		return err
	}
	return p.createAgentPipeline(prefix, runPipeline, bucket)
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

func (p *Pipeline) startUpdatedPipeline(pipeline *types.PipelineDeclaration, stepName string, step model.Step, bucket string) (*string, error) {
	err := p.updatePipeline(pipeline, stepName, step, bucket)
	if err != nil {
		return nil, err
	}
	return p.StartPipelineExecution(*pipeline.Name, stepName, step, "")
}

func (p *Pipeline) UpdatePipeline(pipelineName string, stepName string, step model.Step, bucket string) error {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipe == nil {
		return fmt.Errorf("pipeline %s not found", pipelineName)
	}
	err = p.updatePipeline(pipe, stepName, step, bucket)
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
	return p.updatePipeline(pipe, stepName, step, bucket)
}

func (p *Pipeline) updatePipeline(pipeline *types.PipelineDeclaration, stepName string, step model.Step, bucket string) error {
	changed := false
	for _, stage := range pipeline.Stages {
		if *stage.Name == sourceName || *stage.Name == approveStageName {
			continue
		}
		for _, action := range stage.Actions {
			envVars := getActionEnvironmentVariables(*action.Name, stepName, step, bucket)
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

func getActionEnvironmentVariables(actionName string, stepName string, step model.Step, bucket string) string {
	command := getCommand(actionName, step.Type)
	if step.Type == model.StepTypeTerraform {
		return getTerraformEnvironmentVariables(command, stepName, step, bucket)
	} else {
		return getEnvironmentVariables(command, stepName, step, bucket)
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

func (p *Pipeline) WaitPipelineExecution(pipelineName string, projectName string, executionId *string, autoApprove bool, step model.Step) error {
	if executionId == nil {
		return fmt.Errorf("execution id is nil")
	}
	log.Printf("Waiting for pipeline %s to complete, polling delay %d s\n", pipelineName, pollingDelay)
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	var pipeChanges *model.PipelineChanges
	var approved *bool
	for ctx.Err() == nil {
		if pipeChanges != nil && (step.Approve == model.ApproveReject || pipeChanges.NoChanges) {
			log.Printf("Stopping pipeline %s\n", pipelineName)
			reason := "No changes detected"
			if step.Approve == model.ApproveReject {
				reason = "Rejected"
			}
			err := p.stopPipelineExecution(pipelineName, *executionId, reason)
			if err != nil {
				common.PrintWarning(fmt.Sprintf("Couldn't stop pipeline %s, please stop manually: %s", pipelineName, err.Error()))
			}
			if step.Approve == model.ApproveReject {
				return fmt.Errorf("stopped because step approve type is 'reject'")
			}
			return nil
		}
		time.Sleep(pollingDelay * time.Second)
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
		executionsList, err := p.codePipeline.ListActionExecutions(p.ctx, &codepipeline.ListActionExecutionsInput{
			PipelineName: aws.String(pipelineName),
			Filter:       &types.ActionExecutionFilter{PipelineExecutionId: executionId},
		})
		if err != nil {
			return err
		}
		p.stopPreviousExecution(pipelineName, *executionId, executionsList.ActionExecutionDetails)
		pipeChanges, approved, err = p.processStateStages(pipelineName, executionsList.ActionExecutionDetails, step, pipeChanges, approved, autoApprove)
		if err != nil {
			return err
		}
	}
	return ctx.Err()
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
			slog.Warn(fmt.Sprintf("Couldn't stop previous pipeline %s execution %s, please stop it manually, error: %s", pipelineName, previousId, err.Error()))
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

func (p *Pipeline) processStateStages(pipelineName string, actions []types.ActionExecutionDetail, step model.Step, pipeChanges *model.PipelineChanges, approved *bool, autoApprove bool) (*model.PipelineChanges, *bool, error) {
	for _, action := range actions {
		if *action.StageName != approveStageName || *action.ActionName != approveActionName ||
			action.Status != types.ActionExecutionStatusInProgress {
			continue
		}
		if approved != nil && *approved {
			return pipeChanges, approved, nil
		}
		var err error
		pipeChanges, err = p.getChanges(pipelineName, pipeChanges, actions, step.Type)
		if err != nil {
			return pipeChanges, approved, err
		}
		if pipeChanges != nil && (step.Approve == model.ApproveReject || pipeChanges.NoChanges) {
			return pipeChanges, aws.Bool(true), nil
		}
		if pipeChanges != nil && (step.Approve == model.ApproveForce || (pipeChanges.Destroyed == 0 && (pipeChanges.Changed == 0 || autoApprove))) {
			approved, err = p.approveStage(pipelineName)
			if err != nil {
				return pipeChanges, approved, err
			}
		} else {
			log.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
		}
	}
	return pipeChanges, approved, nil
}

func (p *Pipeline) getChanges(pipelineName string, pipeChanges *model.PipelineChanges, actions []types.ActionExecutionDetail, stepType model.StepType) (*model.PipelineChanges, error) {
	if pipeChanges != nil {
		return pipeChanges, nil
	}
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

func getCodeBuildRunId(actions []types.ActionExecutionDetail) (string, error) {
	for _, action := range actions {
		if *action.ActionName != planName {
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

func (p *Pipeline) approveStage(pipelineName string) (*bool, error) {
	token := p.getApprovalToken(pipelineName)
	if token == nil {
		log.Printf("No approval token found yet for %s, please wait or approve manually\n", pipelineName)
		return aws.Bool(false), nil
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
		return nil, err
	}
	log.Printf("Approved stage %s for %s\n", approveStageName, pipelineName)
	return aws.Bool(true), nil
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

func getTerraformEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step, bucket string) string {
	envVars := getEnvironmentVariablesList(command, stepName, step, bucket)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_USERNAME_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.HttpUsername))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_PASSWORD_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.HttpPassword))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_SOURCE_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.Source))
		}
	}
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step, bucket string) string {
	envVars := getEnvironmentVariablesList(command, stepName, step, bucket)
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariablesList(command model.ActionCommand, stepName string, step model.Step, bucket string) []string {
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

func (p *Pipeline) StartDestroyExecution(_ string) error {
	common.PrintWarning("Executing destroy pipelines not implemented for AWS")
	return nil
}
