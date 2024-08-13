package aws

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/google/uuid"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const pollingDelay = 30

const approveStageName = "Approve"
const approveActionName = "Approval"
const planName = "Plan"
const applyName = "Apply"
const sourceName = "Source"
const destroyName = "Destroy"
const applyDestroyName = "ApplyDestroy"

type Pipeline struct {
	codePipeline *codepipeline.Client
	roleArn      string
	cloudWatch   CloudWatch
	logGroup     string
	logStream    string
}

func NewPipeline(awsConfig aws.Config, roleArn string, cloudWatch CloudWatch, logGroup string, logStream string) *Pipeline {
	return &Pipeline{
		codePipeline: codepipeline.NewFromConfig(awsConfig),
		roleArn:      roleArn,
		cloudWatch:   cloudWatch,
		logGroup:     logGroup,
		logStream:    logStream,
	}
}

func (p *Pipeline) CreatePipeline(projectName string, stepName string, step model.Step, repo string) (*string, error) {
	execution, err := p.CreateApplyPipeline(projectName, projectName, stepName, step, repo)
	if err != nil {
		return nil, err
	}
	err = p.CreateDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step, repo)
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
	_, err := p.codePipeline.DeletePipeline(context.Background(), &codepipeline.DeletePipelineInput{
		Name: aws.String(projectName),
	})
	if err != nil {
		var notFoundError *types.PipelineNotFoundException
		if errors.As(err, &notFoundError) {
			return nil
		}
		return err
	}
	common.Logger.Printf("Deleted CodePipeline %s\n", projectName)
	return nil
}

func (p *Pipeline) CreateApplyPipeline(pipelineName string, projectName string, stepName string, step model.Step, bucket string) (*string, error) {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return nil, err
	}
	if pipe != nil {
		return p.startUpdatedPipeline(pipe, stepName, step)
	}
	var planCommand model.ActionCommand
	var applyCommand model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		planCommand = model.ArgoCDPlanCommand
		applyCommand = model.ArgoCDApplyCommand
	} else {
		planCommand = model.PlanCommand
		applyCommand = model.ApplyCommand
	}
	_, err = p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(planCommand, stepName, step),
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(applyCommand, stepName, step),
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
	common.Logger.Printf("Created CodePipeline %s\n", pipelineName)
	return p.getNewPipelineExecutionId(pipelineName)
}

func (p *Pipeline) CreateDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, bucket string) error {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipe != nil {
		return p.updatePipeline(pipe, stepName, step)
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
	_, err = p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(planCommand, stepName, step),
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
						"EnvironmentVariables": getTerraformEnvironmentVariables(applyCommand, stepName, step),
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
	common.Logger.Printf("Created destroy CodePipeline %s\n", pipelineName)
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

func (p *Pipeline) CreateAgentPipeline(prefix string, pipelineName string, projectName string, bucket string) error {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipe != nil {
		_, err = p.StartPipelineExecution(pipelineName, "", model.Step{}, "")
		return err
	}
	_, err = p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
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
	common.Logger.Printf("Created CodePipeline %s\n", pipelineName)
	return err
}

func (p *Pipeline) StartPipelineExecution(pipelineName string, stepName string, step model.Step, customRepo string) (*string, error) {
	common.Logger.Printf("Starting pipeline %s\n", pipelineName)
	execution, err := p.codePipeline.StartPipelineExecution(context.Background(), &codepipeline.StartPipelineExecutionInput{
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

func (p *Pipeline) startUpdatedPipeline(pipeline *types.PipelineDeclaration, stepName string, step model.Step) (*string, error) {
	err := p.updatePipeline(pipeline, stepName, step)
	if err != nil {
		return nil, err
	}
	return p.StartPipelineExecution(*pipeline.Name, stepName, step, "")
}

func (p *Pipeline) UpdatePipeline(pipelineName string, stepName string, step model.Step, _ string) error {
	pipe, err := p.getPipeline(pipelineName)
	if err != nil {
		return err
	}
	if pipe == nil {
		return fmt.Errorf("pipeline %s not found", pipelineName)
	}
	err = p.updatePipeline(pipe, stepName, step)
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
	return p.updatePipeline(pipe, stepName, step)
}

func (p *Pipeline) updatePipeline(pipeline *types.PipelineDeclaration, stepName string, step model.Step) error {
	changed := false
	for _, stage := range pipeline.Stages {
		if *stage.Name == sourceName || *stage.Name == approveStageName {
			continue
		}
		for _, action := range stage.Actions {
			envVars := getActionEnvironmentVariables(*action.Name, stepName, step)
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
	_, err := p.codePipeline.UpdatePipeline(context.Background(), &codepipeline.UpdatePipelineInput{
		Pipeline: pipeline,
	})
	if err == nil {
		common.Logger.Printf("Updated CodePipeline %s\n", *pipeline.Name)
	}
	return err
}

func getActionEnvironmentVariables(actionName string, stepName string, step model.Step) string {
	command := getCommand(actionName, step.Type)
	if step.Type == model.StepTypeTerraform || step.Type == model.StepTypeTerraformCustom {
		return getTerraformEnvironmentVariables(command, stepName, step)
	} else {
		return getEnvironmentVariables(command, stepName, step)
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

func (p *Pipeline) WaitPipelineExecution(pipelineName string, projectName string, executionId *string, autoApprove bool, stepType model.StepType) error {
	if executionId == nil {
		return fmt.Errorf("execution id is nil")
	}
	common.Logger.Printf("Waiting for pipeline %s to complete, polling delay %d s\n", pipelineName, pollingDelay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var pipeChanges *model.TerraformChanges
	var approved *bool
	for ctx.Err() == nil {
		time.Sleep(pollingDelay * time.Second)
		execution, err := p.codePipeline.GetPipelineExecution(context.Background(), &codepipeline.GetPipelineExecutionInput{
			PipelineName:        aws.String(pipelineName),
			PipelineExecutionId: executionId,
		})
		if err != nil {
			return err
		}
		if execution.PipelineExecution.Status != types.PipelineExecutionStatusInProgress {
			return getExecutionResult(execution.PipelineExecution.Status)
		}
		executionsList, err := p.codePipeline.ListActionExecutions(context.Background(), &codepipeline.ListActionExecutionsInput{
			PipelineName: aws.String(pipelineName),
			Filter:       &types.ActionExecutionFilter{PipelineExecutionId: executionId},
		})
		if err != nil {
			return err
		}
		pipeChanges, approved, err = p.processStateStages(pipelineName, executionsList.ActionExecutionDetails, stepType, pipeChanges, approved, autoApprove)
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (p *Pipeline) getNewPipelineExecutionId(pipelineName string) (*string, error) {
	time.Sleep(5 * time.Second) // Wait for the pipeline to start executing
	executions, err := p.codePipeline.ListPipelineExecutions(context.Background(), &codepipeline.ListPipelineExecutionsInput{
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

func (p *Pipeline) processStateStages(pipelineName string, actions []types.ActionExecutionDetail, stepType model.StepType, pipeChanges *model.TerraformChanges, approved *bool, autoApprove bool) (*model.TerraformChanges, *bool, error) {
	for _, action := range actions {
		if *action.StageName != approveStageName || *action.ActionName != approveActionName ||
			action.Status != types.ActionExecutionStatusInProgress {
			continue
		}
		if approved != nil && *approved {
			return pipeChanges, approved, nil
		}
		var err error
		pipeChanges, err = p.getChanges(pipelineName, pipeChanges, actions, stepType)
		if err != nil {
			return pipeChanges, approved, err
		}
		if pipeChanges != nil && pipeChanges.Destroyed == 0 && (pipeChanges.Changed == 0 || autoApprove) {
			approved, err = p.approveStage(pipelineName)
			if err != nil {
				return pipeChanges, approved, err
			}
		} else {
			common.Logger.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
		}
	}
	return pipeChanges, approved, nil
}

func (p *Pipeline) getChanges(pipelineName string, pipeChanges *model.TerraformChanges, actions []types.ActionExecutionDetail, stepType model.StepType) (*model.TerraformChanges, error) {
	if pipeChanges != nil {
		return pipeChanges, nil
	}
	switch stepType {
	case model.StepTypeTerraformCustom:
		fallthrough
	case model.StepTypeTerraform:
		return p.getTerraformChanges(pipelineName, actions)
	}
	return &model.TerraformChanges{}, nil
}

func (p *Pipeline) getTerraformChanges(pipelineName string, actions []types.ActionExecutionDetail) (*model.TerraformChanges, error) {
	codeBuildRunId, err := getCodeBuildRunId(actions)
	if err != nil {
		return nil, err
	}
	logs, err := p.cloudWatch.GetLogs(p.logGroup, fmt.Sprintf("%s/%s", p.logStream, codeBuildRunId), 50)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(terraform.PlanRegex)
	for _, log := range logs {
		matches := re.FindStringSubmatch(log)
		tfChanges := model.TerraformChanges{}
		if matches != nil {
			common.Logger.Printf("Pipeline %s: %s", pipelineName, log)
			changed := matches[2]
			destroyed := matches[3]
			if changed != "0" || destroyed != "0" {
				tfChanges.Changed, err = strconv.Atoi(changed)
				if err != nil {
					return nil, err
				}
				tfChanges.Destroyed, err = strconv.Atoi(destroyed)
				if err != nil {
					return nil, err
				}
				return &tfChanges, nil
			} else {
				return &tfChanges, nil
			}
		} else if strings.HasPrefix(log, "No changes. Your infrastructure matches the configuration.") ||
			strings.HasPrefix(log, "You can apply this plan to save these new output values") {
			common.Logger.Printf("Pipeline %s: %s", pipelineName, log)
			return &tfChanges, nil
		}
	}
	return nil, fmt.Errorf("couldn't find terraform plan output from logs for %s", pipelineName)
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
		common.Logger.Printf("No approval token found yet for %s, please wait or approve manually\n", pipelineName)
		return aws.Bool(false), nil
	}
	_, err := p.codePipeline.PutApprovalResult(context.Background(), &codepipeline.PutApprovalResultInput{
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
	common.Logger.Printf("Approved stage %s for %s\n", approveStageName, pipelineName)
	return aws.Bool(true), nil
}

func (p *Pipeline) disableStageTransition(pipelineName string, stage string) error {
	_, err := p.codePipeline.DisableStageTransition(context.Background(), &codepipeline.DisableStageTransitionInput{
		PipelineName:   aws.String(pipelineName),
		StageName:      aws.String(stage),
		Reason:         aws.String("Disable pipeline transition to prevent accidental destruction of infrastructure"),
		TransitionType: types.StageTransitionTypeInbound,
	})
	return err
}

func (p *Pipeline) stopLatestPipelineExecutions(pipelineName string, latestCount int32) error {
	executions, err := p.codePipeline.ListPipelineExecutions(context.Background(), &codepipeline.ListPipelineExecutionsInput{
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
		_, err = p.codePipeline.StopPipelineExecution(context.Background(), &codepipeline.StopPipelineExecutionInput{
			PipelineName:        aws.String(pipelineName),
			PipelineExecutionId: execution.PipelineExecutionId,
			Abandon:             true,
			Reason:              aws.String("Abandon pipeline execution to prevent accidental destruction of infrastructure"),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) getPipeline(pipelineName string) (*types.PipelineDeclaration, error) {
	pipelineOutput, err := p.codePipeline.GetPipeline(context.Background(), &codepipeline.GetPipelineInput{
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

func getTerraformEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step) string {
	envVars := getEnvironmentVariablesList(command, stepName, step)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_USERNAME_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.HttpUsername))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_PASSWORD_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.HttpPassword))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_SOURCE_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.Source))
		}
	}
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step) string {
	envVars := getEnvironmentVariablesList(command, stepName, step)
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariablesList(command model.ActionCommand, stepName string, step model.Step) []string {
	var envVars []string
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"COMMAND\",\"value\":\"%s\"}", command))
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"TF_VAR_prefix\",\"value\":\"%s\"}", stepName))
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"WORKSPACE\",\"value\":\"%s\"}", step.Workspace))
	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"KUBERNETES_CLUSTER_NAME\",\"value\":\"%s\"}", step.KubernetesClusterName))
		}
		if step.ArgocdNamespace == "" {
			envVars = append(envVars, fmt.Sprint("{\"name\":\"ARGOCD_NAMESPACE\",\"value\":\"argocd\"}"))
		} else {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"ARGOCD_NAMESPACE\",\"value\":\"%s\"}", step.ArgocdNamespace))
		}
	}
	return envVars
}

func (p *Pipeline) getApprovalToken(pipelineName string) *string {
	state, err := p.codePipeline.GetPipelineState(context.Background(), &codepipeline.GetPipelineStateInput{
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
