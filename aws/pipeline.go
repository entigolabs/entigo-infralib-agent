package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"slices"
	"strconv"
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
	pollingDelay         = 10 * time.Second
	waitTimeout          = 2 * time.Minute
	logsRetries          = 3
	logsPollingDelay     = 5 * time.Second
	autoExecutionTimeout = 30 * time.Second

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

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

type Pipeline struct {
	ctx            context.Context
	region         string
	codePipeline   *codepipeline.Client
	roleArn        string
	cloudWatch     CloudWatch
	logGroup       string
	logStream      string
	terraformCache bool
	enableOpenTofu bool
	cloudPrefix    string
	manager        model.NotificationManager
	campaignId     string
	pipelineIndex  string
}

func (p *Pipeline) SetCampaignId(id string) {
	p.campaignId = id
}

func (p *Pipeline) SetPipelineIndex(index int) {
	p.pipelineIndex = strconv.Itoa(index)
}

func NewPipeline(ctx context.Context, awsConfig aws.Config, roleArn string, cloudWatch CloudWatch, logGroup string, logStream string, terraformCache, enableOpenTofu bool, cloudPrefix string, manager model.NotificationManager) *Pipeline {
	return &Pipeline{
		ctx:            ctx,
		region:         awsConfig.Region,
		codePipeline:   codepipeline.NewFromConfig(awsConfig),
		roleArn:        roleArn,
		cloudWatch:     cloudWatch,
		logGroup:       logGroup,
		logStream:      logStream,
		terraformCache: terraformCache,
		cloudPrefix:    cloudPrefix,
		manager:        manager,
		enableOpenTofu: enableOpenTofu,
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
		if _, ok := errors.AsType[*types.PipelineNotFoundException](err); ok {
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
	planEnvars, err := p.getEnvironmentVariablesByType(planCommand, stepName, step, bucket, authSources)
	if err != nil {
		return nil, err
	}
	applyEnvVars, err := p.getEnvironmentVariablesByType(applyCommand, stepName, step, bucket, authSources)
	if err != nil {
		return nil, err
	}
	_, err = p.codePipeline.CreatePipeline(p.ctx, &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:         aws.String(pipelineName),
			RoleArn:      aws.String(p.roleArn),
			PipelineType: types.PipelineTypeV2,
			Variables: []types.PipelineVariableDeclaration{{
				Name:         aws.String("CampaignId"),
				DefaultValue: aws.String(model.CampaignSentinelNone),
			}, {
				Name:         aws.String("PipelineIndex"),
				DefaultValue: aws.String("0"),
			}},
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
						"EnvironmentVariables": planEnvars,
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
						"EnvironmentVariables": applyEnvVars,
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
	// The auto-start on create has no CampaignId — replace it with our own.
	if err := p.waitAndStopAutoExecution(pipelineName, autoExecutionTimeout); err != nil {
		return nil, err
	}
	return p.StartPipelineExecution(pipelineName, "", model.Step{}, "")
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
	planEnvVars, err := p.getEnvironmentVariablesByType(planCommand, stepName, step, bucket, authSources)
	if err != nil {
		return err
	}
	applyEnvVars, err := p.getEnvironmentVariablesByType(applyCommand, stepName, step, bucket, authSources)
	if err != nil {
		return err
	}
	_, err = p.codePipeline.CreatePipeline(p.ctx, &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:         aws.String(pipelineName),
			RoleArn:      aws.String(p.roleArn),
			PipelineType: types.PipelineTypeV2,
			Variables: []types.PipelineVariableDeclaration{{
				Name:         aws.String("CampaignId"),
				DefaultValue: aws.String(model.CampaignSentinelNone),
			}, {
				Name:         aws.String("PipelineIndex"),
				DefaultValue: aws.String("0"),
			}},
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
						"EnvironmentVariables": planEnvVars,
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
						"EnvironmentVariables": applyEnvVars,
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
	return p.waitAndStopAutoExecution(pipelineName, autoExecutionTimeout)
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
		err = p.waitAndStopAutoExecution(updatePipeline, autoExecutionTimeout)
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
	return p.waitAndStopAutoExecution(runPipeline, autoExecutionTimeout)
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
	input := &codepipeline.StartPipelineExecutionInput{
		Name:               aws.String(pipelineName),
		ClientRequestToken: aws.String(uuid.NewString()),
	}
	if p.campaignId != "" {
		input.Variables = append(input.Variables, types.PipelineVariable{
			Name:  aws.String("CampaignId"),
			Value: aws.String(p.campaignId),
		})
	} else {
		slog.Debug("starting pipeline without CampaignId — SetCampaignId was not called", "pipeline", pipelineName)
	}
	if p.pipelineIndex != "" {
		input.Variables = append(input.Variables, types.PipelineVariable{
			Name:  aws.String("PipelineIndex"),
			Value: aws.String(p.pipelineIndex),
		})
	}
	execution, err := p.codePipeline.StartPipelineExecution(p.ctx, input)
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

func hasPipelineVariable(vars []types.PipelineVariableDeclaration, name string) bool {
	for _, v := range vars {
		if v.Name != nil && *v.Name == name {
			return true
		}
	}
	return false
}

func (p *Pipeline) updatePipeline(pipeline *types.PipelineDeclaration, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) error {
	changed := false
	// V1 pipelines treat #{variables.CampaignId} as a literal string, which
	// would corrupt the wrapper's CAMPAIGN_ID env var — upgrade in place.
	if pipeline.PipelineType != types.PipelineTypeV2 {
		pipeline.PipelineType = types.PipelineTypeV2
		changed = true
	}
	if !hasPipelineVariable(pipeline.Variables, "CampaignId") {
		pipeline.Variables = append(pipeline.Variables, types.PipelineVariableDeclaration{
			Name:         aws.String("CampaignId"),
			DefaultValue: aws.String(model.CampaignSentinelNone),
		})
		changed = true
	}
	if !hasPipelineVariable(pipeline.Variables, "PipelineIndex") {
		pipeline.Variables = append(pipeline.Variables, types.PipelineVariableDeclaration{
			Name:         aws.String("PipelineIndex"),
			DefaultValue: aws.String("0"),
		})
		changed = true
	}
	for _, stage := range pipeline.Stages {
		if *stage.Name == sourceName || *stage.Name == approveStageName {
			continue
		}
		for _, action := range stage.Actions {
			envVars, err := p.getActionEnvironmentVariables(*action.Name, stepName, step, bucket, authSources)
			if err != nil {
				return err
			}
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

func (p *Pipeline) getActionEnvironmentVariables(actionName string, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) (string, error) {
	command := getCommand(actionName, step.Type)
	if step.Type == model.StepTypeTerraform {
		return p.getTerraformEnvironmentVariables(command, stepName, step, bucket, authSources)
	}
	return p.getEnvironmentVariables(command, stepName, step, bucket, authSources)
}

func getCommand(actionName string, stepType model.StepType) model.ActionCommand {
	switch actionName {
	case planName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDPlanCommand
		}
		return model.PlanCommand
	case applyName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDApplyCommand
		}
		return model.ApplyCommand
	case destroyName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDPlanDestroyCommand
		}
		return model.PlanDestroyCommand
	case applyDestroyName:
		if stepType == model.StepTypeArgoCD {
			return model.ArgoCDApplyDestroyCommand
		}
		return model.ApplyDestroyCommand
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
				var approvedBy string
				if action.UpdatedBy != nil {
					approvedBy = *action.UpdatedBy
				}
				p.manager.Approval(pipelineName, step.Name, approvedBy)
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
		p.manager.ManualApproval(pipelineName, step.Name, *pipeChanges, p.getLink(pipelineName))
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

	logStreamName := fmt.Sprintf("%s/%s", p.logStream, codeBuildRunId)
	for attempt := 1; attempt <= logsRetries; attempt++ {
		logs, err := p.cloudWatch.GetLogs(p.logGroup, logStreamName)
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
		if attempt < logsRetries {
			time.Sleep(logsPollingDelay)
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

// Bounded by timeout — if no execution appears we return without error so a
// subsequent StartPipelineExecution can still succeed.
func (p *Pipeline) waitAndStopAutoExecution(pipelineName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		executions, err := p.codePipeline.ListPipelineExecutions(p.ctx, &codepipeline.ListPipelineExecutionsInput{
			PipelineName: aws.String(pipelineName),
			MaxResults:   aws.Int32(1),
		})
		if err != nil {
			return err
		}
		for _, execution := range executions.PipelineExecutionSummaries {
			if execution.Status != types.PipelineExecutionStatusInProgress {
				continue
			}
			slog.Info("intercepting auto-execution", "pipeline", pipelineName, "execution", *execution.PipelineExecutionId)
			return p.stopPipelineExecution(pipelineName, *execution.PipelineExecutionId, "Superseded by agent-orchestrated execution")
		}
		if time.Now().After(deadline) {
			slog.Warn("no auto-execution appeared to intercept", "pipeline", pipelineName, "timeout", timeout)
			return nil
		}
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (p *Pipeline) stopPipelineExecution(pipelineName string, executionId string, reason string) error {
	_, err := p.codePipeline.StopPipelineExecution(p.ctx, &codepipeline.StopPipelineExecutionInput{
		PipelineName:        &pipelineName,
		PipelineExecutionId: &executionId,
		Abandon:             true,
		Reason:              &reason,
	})
	if err != nil {
		if _, ok := errors.AsType[*types.PipelineExecutionNotStoppableException](err); ok {
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

func (p *Pipeline) getEnvironmentVariablesByType(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) (string, error) {
	if step.Type == model.StepTypeTerraform {
		return p.getTerraformEnvironmentVariables(command, stepName, step, bucket, authSources)
	}
	return p.getEnvironmentVariables(command, stepName, step, bucket, authSources)
}

func (p *Pipeline) getTerraformEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) (string, error) {
	vars := p.buildEnvVars(command, stepName, step, bucket, authSources)
	vars = append(vars, envVar{Name: "TERRAFORM_CACHE", Value: fmt.Sprintf("%t", p.terraformCache)})
	if p.enableOpenTofu {
		vars = append(vars, envVar{Name: "TF_TOOL", Value: model.TofuTfTool})
	}
	for _, module := range step.Modules {
		if !util.IsClientModule(module) {
			continue
		}
		up := strings.ToUpper(module.Name)
		vars = append(vars,
			envVar{Name: "GIT_AUTH_USERNAME_" + up, Value: module.HttpUsername},
			envVar{Name: "GIT_AUTH_PASSWORD_" + up, Value: module.HttpPassword},
			envVar{Name: "GIT_AUTH_SOURCE_" + up, Value: module.Source},
		)
	}
	return marshalEnvVars(vars)
}

func (p *Pipeline) getEnvironmentVariables(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) (string, error) {
	return marshalEnvVars(p.buildEnvVars(command, stepName, step, bucket, authSources))
}

func marshalEnvVars(vars []envVar) (string, error) {
	b, err := json.Marshal(vars)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *Pipeline) buildEnvVars(command model.ActionCommand, stepName string, step model.Step, bucket string, authSources map[string]model.SourceAuth) []envVar {
	vars := []envVar{
		{Name: "COMMAND", Value: string(command)},
		{Name: "TF_VAR_prefix", Value: stepName},
		{Name: "INFRALIB_BUCKET", Value: bucket},
		{Name: "INFRALIB_STEP", Value: step.Name},
		{Name: "CAMPAIGN_ID", Value: "#{variables.CampaignId}"},
		{Name: "PIPELINE_INDEX", Value: "#{variables.PipelineIndex}"},
	}
	if p.campaignId != "" {
		vars = append(vars, envVar{Name: model.WrapperConfigEnv, Value: model.WrapperConfigSecretName(p.cloudPrefix),
			Type: "SECRETS_MANAGER"})
	}
	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			vars = append(vars, envVar{Name: "KUBERNETES_CLUSTER_NAME", Value: step.KubernetesClusterName})
		}
		ns := step.ArgocdNamespace
		if ns == "" {
			ns = "argocd"
		}
		vars = append(vars, envVar{Name: "ARGOCD_NAMESPACE", Value: ns})
	}
	keys := slices.Collect(maps.Keys(authSources))
	slices.Sort(keys)
	for _, source := range keys {
		hash := util.HashCode(source)
		vars = append(vars,
			envVar{Name: fmt.Sprintf(model.GitUsernameEnvFormat, hash), Value: fmt.Sprintf(model.GitUsernameFormat, hash), Type: "SECRETS_MANAGER"},
			envVar{Name: fmt.Sprintf(model.GitPasswordEnvFormat, hash), Value: fmt.Sprintf(model.GitPasswordFormat, hash), Type: "SECRETS_MANAGER"},
			envVar{Name: fmt.Sprintf(model.GitSourceEnvFormat, hash), Value: fmt.Sprintf(model.GitSourceFormat, hash), Type: "SECRETS_MANAGER"},
		)
	}
	return vars
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
