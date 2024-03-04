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
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/google/uuid"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const approveStageName = "Approve"
const approveActionName = "Approval"
const planName = "Plan"

type Pipeline interface {
	CreateTerraformPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) (*string, error)
	CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) error
	CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, workspace string) (*string, error)
	CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateAgentPipeline(prefix string, pipelineName string, projectName string, bucket string) error
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

func (p *pipeline) CreateTerraformPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) (*string, error) {
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
						"EnvironmentVariables": getTerraformEnvironmentVariables("plan", stepName, step),
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
						"EnvironmentVariables": getTerraformEnvironmentVariables("apply", stepName, step),
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

func (p *pipeline) CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) error {
	if p.pipelineExists(pipelineName) {
		return nil
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
						"EnvironmentVariables": getTerraformEnvironmentVariables("plan-destroy", stepName, step),
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
						"EnvironmentVariables": getTerraformEnvironmentVariables("apply-destroy", stepName, step),
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

func (p *pipeline) CreateAgentPipeline(prefix string, pipelineName string, projectName string, bucket string) error {
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
	var pipeChanges *changes
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
		pipeChanges, approved, err = p.processStateStages(pipelineName, executionsList.ActionExecutionDetails, stepType, pipeChanges, approved, autoApprove)
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

func (p *pipeline) processStateStages(pipelineName string, actions []types.ActionExecutionDetail, stepType model.StepType, pipeChanges *changes, approved *bool, autoApprove bool) (*changes, *bool, error) {
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
		if pipeChanges != nil && pipeChanges.destroyed == 0 && (pipeChanges.changed == 0 || autoApprove) {
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

func (p *pipeline) getChanges(pipelineName string, pipeChanges *changes, actions []types.ActionExecutionDetail, stepType model.StepType) (*changes, error) {
	if pipeChanges != nil {
		return pipeChanges, nil
	}
	switch stepType {
	case model.StepTypeTerraform:
		return p.getTerraformChanges(pipelineName, actions)
	}
	return &changes{}, nil
}

func (p *pipeline) getTerraformChanges(pipelineName string, actions []types.ActionExecutionDetail) (*changes, error) {
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
		tfChanges := changes{}
		if matches != nil {
			common.Logger.Printf("Pipeline %s: %s", pipelineName, log)
			changed := matches[2]
			destroyed := matches[3]
			if changed != "0" || destroyed != "0" {
				tfChanges.changed, err = strconv.Atoi(changed)
				if err != nil {
					return nil, err
				}
				tfChanges.destroyed, err = strconv.Atoi(destroyed)
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

type changes struct {
	changed   int
	destroyed int
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

func getTerraformEnvironmentVariables(command string, stepName string, step model.Step) string {
	envVars := getEnvironmentVariablesList(command, stepName, step.Workspace)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_USERNAME_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.HttpUsername))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_PASSWORD_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.HttpPassword))
			envVars = append(envVars, fmt.Sprintf("{\"name\":\"GIT_AUTH_SOURCE_%s\",\"value\":\"%s\"}", strings.ToUpper(module.Name), module.Source))
		}
	}
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariables(command string, stepName string, workspace string) string {
	envVars := getEnvironmentVariablesList(command, stepName, workspace)
	return "[" + strings.Join(envVars, ",") + "]"
}

func getEnvironmentVariablesList(command string, stepName string, workspace string) []string {
	var envVars []string
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"COMMAND\",\"value\":\"%s\"}", command))
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"TF_VAR_prefix\",\"value\":\"%s\"}", stepName))
	envVars = append(envVars, fmt.Sprintf("{\"name\":\"WORKSPACE\",\"value\":\"%s\"}", workspace))
	return envVars
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
