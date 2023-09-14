package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/google/uuid"
	"regexp"
	"strings"
	"time"
)

const approveStageName = "Approve"
const approveActionName = "Approval"
const planName = "Plan"

type Pipeline interface {
	CreateTerraformPipeline(pipelineName string, projectName string, stepName string, workspace string, customRepo string) (*string, error)
	CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string, customRepo string) error
	CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, workspace string) (*string, error)
	CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateAgentPipeline(pipelineName string, projectName string, bucket string) error
	StartPipelineExecution(pipelineName string) (*string, error)
	WaitPipelineExecution(pipelineName string, executionId *string, autoApprove bool, delay int, stepType model.StepType) error
}

type pipeline struct {
	codePipeline *codepipeline.Client
	repo         string
	branch       string
	roleArn      string
	bucket       string
	cloudWatch   CloudWatch
	logGroup     string
	logStream    string
}

func NewPipeline(awsConfig aws.Config, repo string, branch string, roleArn string, bucket string, cloudWatch CloudWatch, logGroup string, logStream string) Pipeline {
	return &pipeline{
		codePipeline: codepipeline.NewFromConfig(awsConfig),
		repo:         repo,
		branch:       branch,
		roleArn:      roleArn,
		bucket:       bucket,
		cloudWatch:   cloudWatch,
		logGroup:     logGroup,
		logStream:    logStream,
	}
}

func (p *pipeline) CreateTerraformPipeline(pipelineName string, projectName string, stepName string, workspace string, customRepo string) (*string, error) {
	if p.pipelineExists(pipelineName) {
		return p.StartPipelineExecution(pipelineName)
	}
	repo := p.repo
	if customRepo != "" {
		repo = customRepo
	}
	_, err := p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(p.bucket),
				Type:     types.ArtifactStoreTypeS3,
			}, Stages: []types.StageDeclaration{{
				Name: aws.String("Source"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Source"),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategorySource,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeCommit"),
						Version:  aws.String("1"),
					},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("source_output")}},
					RunOrder:        aws.Int32(1),
					Configuration: map[string]string{
						"RepositoryName":       repo,
						"BranchName":           p.branch,
						"PollForSourceChanges": "false",
						"OutputArtifactFormat": "CODEBUILD_CLONE_REF",
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
						"EnvironmentVariables": getEnvironmentVariables("plan", stepName, workspace),
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
				Name: aws.String("Apply"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Apply"),
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
						"EnvironmentVariables": getEnvironmentVariables("apply", stepName, workspace),
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

func (p *pipeline) CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string, customRepo string) error {
	repo := p.repo
	if customRepo != "" {
		repo = customRepo
	}
	_, err := p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(p.bucket),
				Type:     types.ArtifactStoreTypeS3,
			}, Stages: []types.StageDeclaration{{
				Name: aws.String("Source"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Source"),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategorySource,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeCommit"),
						Version:  aws.String("1"),
					},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("source_output")}},
					RunOrder:        aws.Int32(1),
					Configuration: map[string]string{
						"RepositoryName":       repo,
						"BranchName":           p.branch,
						"PollForSourceChanges": "false",
						"OutputArtifactFormat": "CODEBUILD_CLONE_REF",
					},
				},
				},
			}, {
				Name: aws.String("Destroy"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Destroy"),
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
						"EnvironmentVariables": getEnvironmentVariables("plan-destroy", stepName, workspace),
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
				Name: aws.String("ApplyDestroy"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("ApplyDestroy"),
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
						"EnvironmentVariables": getEnvironmentVariables("apply-destroy", stepName, workspace),
					},
				},
				},
			},
			},
		},
	})
	var awsError *types.PipelineNameInUseException
	if err != nil && errors.As(err, &awsError) {
		return nil
	}
	common.Logger.Printf("Created destroy CodePipeline %s\n", pipelineName)
	err = p.disableStageTransition(pipelineName, "Destroy")
	if err != nil {
		return err
	}
	err = p.disableStageTransition(pipelineName, "Approve")
	if err != nil {
		return err
	}
	err = p.disableStageTransition(pipelineName, "ApplyDestroy")
	if err != nil {
		return err
	}
	time.Sleep(5 * time.Second) // Wait for pipeline to start executing
	return p.stopLatestPipelineExecutions(pipelineName, 1)
}

func (p *pipeline) CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, workspace string) (*string, error) {
	if p.pipelineExists(pipelineName) {
		return p.StartPipelineExecution(pipelineName)
	}
	_, err := p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(p.bucket),
				Type:     types.ArtifactStoreTypeS3,
			}, Stages: []types.StageDeclaration{{
				Name: aws.String("Source"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Source"),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategorySource,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeCommit"),
						Version:  aws.String("1"),
					},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("source_output")}},
					RunOrder:        aws.Int32(1),
					Configuration: map[string]string{
						"RepositoryName":       p.repo,
						"BranchName":           p.branch,
						"PollForSourceChanges": "false",
						"OutputArtifactFormat": "CODEBUILD_CLONE_REF",
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
					RunOrder: aws.Int32(2),
				}},
			}, {
				Name: aws.String("ArgoCDApply"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("ArgoCDApply"),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryBuild,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeBuild"),
						Version:  aws.String("1"),
					},
					InputArtifacts: []types.InputArtifact{{Name: aws.String("source_output")}},
					RunOrder:       aws.Int32(3),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": getEnvironmentVariables("argocd-apply", stepName, workspace),
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

func (p *pipeline) CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error {
	_, err := p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(p.bucket),
				Type:     types.ArtifactStoreTypeS3,
			}, Stages: []types.StageDeclaration{{
				Name: aws.String("Source"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Source"),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategorySource,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeCommit"),
						Version:  aws.String("1"),
					},
					OutputArtifacts: []types.OutputArtifact{{Name: aws.String("source_output")}},
					RunOrder:        aws.Int32(1),
					Configuration: map[string]string{
						"RepositoryName":       p.repo,
						"BranchName":           p.branch,
						"PollForSourceChanges": "false",
						"OutputArtifactFormat": "CODEBUILD_CLONE_REF",
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
					RunOrder: aws.Int32(2),
				}},
			}, {
				Name: aws.String("ArgoCDDestroy"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("ArgoCDDestroy"),
					ActionTypeId: &types.ActionTypeId{
						Category: types.ActionCategoryBuild,
						Owner:    types.ActionOwnerAws,
						Provider: aws.String("CodeBuild"),
						Version:  aws.String("1"),
					},
					InputArtifacts: []types.InputArtifact{{Name: aws.String("source_output")}},
					RunOrder:       aws.Int32(3),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": getEnvironmentVariables("argocd-destroy", stepName, workspace),
					},
				},
				},
			},
			},
		},
	})
	var awsError *types.PipelineNameInUseException
	if err != nil && errors.As(err, &awsError) {
		common.Logger.Printf("Pipeline %s already exists. Continuing...\n", projectName)
		return nil
	}
	common.Logger.Printf("Created CodePipeline %s\n", pipelineName)
	err = p.disableStageTransition(pipelineName, "Approve")
	if err != nil {
		return err
	}
	err = p.disableStageTransition(pipelineName, "ArgoCDDestroy")
	if err != nil {
		return err
	}
	time.Sleep(5 * time.Second) // Wait for pipeline to start executing
	return p.stopLatestPipelineExecutions(pipelineName, 1)
}

func (p *pipeline) CreateAgentPipeline(pipelineName string, projectName string, bucket string) error {
	if p.pipelineExists(pipelineName) {
		_, err := p.StartPipelineExecution(pipelineName)
		return err
	}
	_, err := p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(p.roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(p.bucket),
				Type:     types.ArtifactStoreTypeS3,
			}, Stages: []types.StageDeclaration{{
				Name: aws.String("Source"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Source"),
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
						"S3ObjectKey":          agentSource,
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
						"ProjectName":   projectName,
						"PrimarySource": "source_output",
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

func (p *pipeline) StartPipelineExecution(pipelineName string) (*string, error) {
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

func (p *pipeline) WaitPipelineExecution(pipelineName string, executionId *string, autoApprove bool, delay int, stepType model.StepType) error {
	if executionId == nil {
		return fmt.Errorf("execution id is nil")
	}
	common.Logger.Printf("Waiting for pipeline %s to complete, polling delay %d s\n", pipelineName, delay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var noChanges *bool
	var approved *bool
	for ctx.Err() == nil {
		time.Sleep(time.Duration(delay) * time.Second)
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
		noChanges, approved, err = p.processStateStages(pipelineName, executionsList.ActionExecutionDetails, stepType, noChanges, approved, autoApprove)
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (p *pipeline) getNewPipelineExecutionId(pipelineName string) (*string, error) {
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

func (p *pipeline) processStateStages(pipelineName string, actions []types.ActionExecutionDetail, stepType model.StepType, noChanges *bool, approved *bool, autoApprove bool) (*bool, *bool, error) {
	for _, action := range actions {
		if *action.StageName != approveStageName || *action.ActionName != approveActionName ||
			action.Status != types.ActionExecutionStatusInProgress {
			continue
		}
		if approved != nil && *approved {
			return noChanges, approved, nil
		}
		var err error
		noChanges, err = p.hasNotChanged(noChanges, actions, stepType)
		if err != nil {
			return noChanges, approved, err
		}
		if *noChanges || autoApprove {
			approved, err = p.approveStage(pipelineName)
			if err != nil {
				return noChanges, approved, err
			}
		} else {
			common.Logger.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
		}
	}
	return noChanges, approved, nil
}

func (p *pipeline) hasNotChanged(noChanges *bool, actions []types.ActionExecutionDetail, stepType model.StepType) (*bool, error) {
	if noChanges != nil {
		return noChanges, nil
	}
	switch stepType {
	case model.StepTypeTerraform:
		return p.hasTerraformNotChanged(actions)
	}
	return aws.Bool(false), nil
}

func (p *pipeline) hasTerraformNotChanged(actions []types.ActionExecutionDetail) (*bool, error) {
	codeBuildRunId, err := getCodeBuildRunId(actions)
	if err != nil {
		return nil, err
	}
	logs, err := p.cloudWatch.GetLogs(p.logGroup, fmt.Sprintf("%s/%s", p.logStream, codeBuildRunId), 50)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`Plan: (\d+) to add, (\d+) to change, (\d+) to destroy`)
	for _, log := range logs {
		matches := re.FindStringSubmatch(log)
		if matches != nil {
			common.Logger.Print(log)
			changed := matches[2]
			destroyed := matches[3]
			if changed != "0" || destroyed != "0" {
				return aws.Bool(false), nil
			} else {
				return aws.Bool(true), nil
			}
		} else if strings.HasPrefix(log, "No changes. Your infrastructure matches the configuration.") ||
			strings.HasPrefix(log, "You can apply this plan to save these new output values") {
			common.Logger.Print(log)
			return aws.Bool(true), nil
		}
	}
	return nil, fmt.Errorf("couldn't find terraform plan output from logs")
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

func (p *pipeline) approveStage(pipelineName string) (*bool, error) {
	token := p.getApprovalToken(pipelineName)
	if token == nil {
		common.Logger.Printf("No approval token found, please wait or approve manually\n")
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
	common.Logger.Printf("Approved stage %s\n", approveStageName)
	return aws.Bool(true), nil
}

func (p *pipeline) disableStageTransition(pipelineName string, stage string) error {
	_, err := p.codePipeline.DisableStageTransition(context.Background(), &codepipeline.DisableStageTransitionInput{
		PipelineName:   aws.String(pipelineName),
		StageName:      aws.String(stage),
		Reason:         aws.String("Disable pipeline transition to prevent accidental destruction of infrastructure"),
		TransitionType: types.StageTransitionTypeInbound,
	})
	return err
}

func (p *pipeline) stopLatestPipelineExecutions(pipelineName string, latestCount int32) error {
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

func (p *pipeline) pipelineExists(pipelineName string) bool {
	_, err := p.codePipeline.GetPipeline(context.Background(), &codepipeline.GetPipelineInput{
		Name: aws.String(pipelineName),
	})
	if err != nil {
		var awsError *types.PipelineNotFoundException
		if errors.As(err, &awsError) {
			return false
		}
	}
	return true
}

func getEnvironmentVariables(command string, stepName string, workspace string) string {
	return "[{\"name\":\"COMMAND\",\"value\":\"" + command + "\"},{\"name\":\"TF_VAR_prefix\",\"value\":\"" + stepName + "\"},{\"name\":\"WORKSPACE\",\"value\":\"" + workspace + "\"}]"
}

func (p *pipeline) getApprovalToken(pipelineName string) *string {
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
