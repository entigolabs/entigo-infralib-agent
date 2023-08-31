package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"regexp"
	"strings"
	"time"
)

const approveStageName = "Approve"
const approveActionName = "Approval"
const planName = "Plan"

type Pipeline interface {
	CreateTerraformPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	StartPipelineExecution(pipelineName string) error
	WaitPipelineExecution(pipelineName string, autoApprove bool, delay int, stepType string) error
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

func (p *pipeline) CreateTerraformPipeline(pipelineName string, projectName string, stepName string, workspace string) error {
	if p.pipelineExists(pipelineName) {
		common.Logger.Printf("Pipeline %s already exists. Continuing...\n", projectName)
		return p.startPipelineExecutionIfNeeded(pipelineName)
	}
	common.Logger.Printf("Creating CodePipeline %s\n", pipelineName)
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
	return err
}

func (p *pipeline) CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error {
	common.Logger.Printf("Creating destroy CodePipeline %s\n", pipelineName)
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
		common.Logger.Printf("Pipeline %s already exists. Continuing...\n", projectName)
		return nil
	}
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

func (p *pipeline) CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, workspace string) error {
	if p.pipelineExists(pipelineName) {
		common.Logger.Printf("Pipeline %s already exists. Continuing...\n", projectName)
		return p.startPipelineExecutionIfNeeded(pipelineName)
	}
	common.Logger.Printf("Creating CodePipeline %s\n", pipelineName)
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
	return err
}

func (p *pipeline) CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error {
	common.Logger.Printf("Creating destroy CodePipeline %s\n", pipelineName)
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

func (p *pipeline) StartPipelineExecution(pipelineName string) error {
	common.Logger.Printf("Starting pipeline %s\n", pipelineName)
	_, err := p.codePipeline.StartPipelineExecution(context.Background(), &codepipeline.StartPipelineExecutionInput{
		Name: aws.String(pipelineName),
	})
	return err
}

func (p *pipeline) WaitPipelineExecution(pipelineName string, autoApprove bool, delay int, stepType string) error {
	common.Logger.Printf("Waiting for pipeline %s to complete, polling delay %d s\n", pipelineName, delay)
	time.Sleep(5 * time.Second) // Wait for the pipeline to start executing
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var noChanges *bool
	for ctx.Err() == nil {
		state, err := p.codePipeline.GetPipelineState(context.Background(), &codepipeline.GetPipelineStateInput{
			Name: aws.String(pipelineName),
		})
		if err != nil {
			return err
		}
		successes := 0
	stages:
		for _, stage := range state.StageStates {
			if stage.LatestExecution == nil {
				break
			}
			switch stage.LatestExecution.Status {
			case types.StageExecutionStatusInProgress:
				if *stage.StageName == approveStageName {
					if noChanges == nil {
						switch stepType {
						case "terraform":
							noChanges, err = p.hasTerraformNotChanged(state)
							if err != nil {
								return err
							}
						case "argocd-apps":
							noChanges = aws.Bool(false)
						}
					}
					if *noChanges || autoApprove {
						err = p.approveStage(pipelineName, stage)
						if err != nil {
							return err
						}
					} else {
						common.Logger.Printf("Waiting for approval of pipeline %s\n", pipelineName)
					}
				}
				break stages
			case types.StageExecutionStatusCancelled:
				return errors.New("pipeline execution cancelled")
			case types.StageExecutionStatusFailed:
				return errors.New("pipeline execution failed")
			case types.StageExecutionStatusStopped:
				return errors.New("pipeline execution stopped")
			case types.StageExecutionStatusStopping:
				continue
			case types.StageExecutionStatusSucceeded:
				successes++
			}
		}
		if successes == len(state.StageStates) {
			common.Logger.Printf("Pipeline %s completed successfully\n", pipelineName)
			return nil
		}
		time.Sleep(time.Duration(delay) * time.Second)
	}
	return ctx.Err()
}

func (p *pipeline) hasTerraformNotChanged(state *codepipeline.GetPipelineStateOutput) (*bool, error) {
	codeBuildRunId, err := getCodeBuildRunId(state)
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
		} else if strings.HasPrefix(log, "No changes. Your infrastructure matches the configuration.") {
			common.Logger.Print(log)
			return aws.Bool(true), nil
		}
	}
	return nil, fmt.Errorf("couldn't find terraform plan output from logs")
}

func getCodeBuildRunId(state *codepipeline.GetPipelineStateOutput) (string, error) {
	for _, stage := range state.StageStates {
		if *stage.StageName != planName || stage.ActionStates == nil {
			continue
		}
		for _, action := range stage.ActionStates {
			if *action.ActionName != planName || action.LatestExecution == nil {
				continue
			}
			externalExecutionId := action.LatestExecution.ExternalExecutionId
			if externalExecutionId == nil {
				return "", fmt.Errorf("couldn't get plan action external execution id")
			}
			parts := strings.Split(*externalExecutionId, ":")
			if len(parts) != 2 {
				return "", fmt.Errorf("couldn't parse plan action external execution id from %s", *externalExecutionId)
			}
			return parts[1], nil
		}
	}
	return "", fmt.Errorf("couldn't find a terraform plan action")
}

func (p *pipeline) approveStage(projectName string, stage types.StageState) error {
	token := getApprovalToken(stage)
	if token == nil {
		common.Logger.Printf("No approval token found, please wait or approve manually\n")
		return nil
	}
	_, err := p.codePipeline.PutApprovalResult(context.Background(), &codepipeline.PutApprovalResultInput{
		PipelineName: aws.String(projectName),
		StageName:    aws.String(approveStageName),
		ActionName:   aws.String(approveActionName),
		Token:        token,
		Result: &types.ApprovalResult{
			Status:  types.ApprovalStatusApproved,
			Summary: aws.String("Approved by entigo-infralib-agent"),
		},
	})
	if err == nil {
		common.Logger.Printf("Approved stage %s\n", approveStageName)
	}
	return err
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

func (p *pipeline) startPipelineExecutionIfNeeded(name string) error {
	state, err := p.codePipeline.GetPipelineState(context.Background(), &codepipeline.GetPipelineStateInput{
		Name: aws.String(name),
	})
	if err != nil {
		return err
	}
	if state == nil || state.StageStates == nil {
		return nil
	}
	for _, stage := range state.StageStates {
		if stage.LatestExecution == nil {
			continue
		}
		if stage.LatestExecution.Status == types.StageExecutionStatusInProgress {
			return nil
		}
	}
	return p.StartPipelineExecution(name)
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

func getApprovalToken(stage types.StageState) *string {
	if stage.ActionStates == nil {
		return nil
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
	return nil
}
