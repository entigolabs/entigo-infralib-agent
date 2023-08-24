package service

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"time"
)

type Pipeline interface {
	CreateTerraformPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, workspace string) error
	WaitPipelineExecution(pipelineName string, delay int) error
}

type pipeline struct {
	codePipeline *codepipeline.Client
	repo         string
	branch       string
	roleArn      string
	bucket       string
}

func NewPipeline(awsConfig aws.Config, repo string, branch string, roleArn string, bucket string) Pipeline {
	return &pipeline{
		codePipeline: codepipeline.NewFromConfig(awsConfig),
		repo:         repo,
		branch:       branch,
		roleArn:      roleArn,
		bucket:       bucket,
	}
}

func (p *pipeline) CreateTerraformPipeline(pipelineName string, projectName string, stepName string, workspace string) error {
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
				Name: aws.String("Plan"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Plan"),
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
						"EnvironmentVariables": "[{\"name\":\"COMMAND\",\"value\":\"plan\"},{\"name\":\"TF_VAR_prefix\",\"value\":\"" + stepName + "\"},{\"name\":\"WORKSPACE\",\"value\":\"" + workspace + "\"}]",
					},
				},
				},
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
					RunOrder:       aws.Int32(3),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": "[{\"name\":\"COMMAND\",\"value\":\"apply\"},{\"name\":\"TF_VAR_prefix\",\"value\":\"" + stepName + "\"},{\"name\":\"WORKSPACE\",\"value\":\"" + workspace + "\"}]",
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
						"EnvironmentVariables": "[{\"name\":\"COMMAND\",\"value\":\"plan-destroy\"},{\"name\":\"TF_VAR_prefix\",\"value\":\"" + stepName + "\"},{\"name\":\"WORKSPACE\",\"value\":\"" + workspace + "\"}]",
					},
				},
				},
			}, {
				Name: aws.String("Approve"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Approval"),
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
						"EnvironmentVariables": "[{\"name\":\"COMMAND\",\"value\":\"apply-destroy\"},{\"name\":\"TF_VAR_prefix\",\"value\":\"" + stepName + "\"},{\"name\":\"WORKSPACE\",\"value\":\"" + workspace + "\"}]",
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
	return err
}

func (p *pipeline) CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, workspace string) error {
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
					RunOrder:       aws.Int32(2),
					Configuration: map[string]string{
						"ProjectName":          projectName,
						"PrimarySource":        "source_output",
						"EnvironmentVariables": "[{\"name\":\"COMMAND\",\"value\":\"argocd-apply\"},{\"name\":\"TF_VAR_prefix\",\"value\":\"" + stepName + "\"},{\"name\":\"WORKSPACE\",\"value\":\"" + workspace + "\"}]",
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
				Name: aws.String("Approve"),
				Actions: []types.ActionDeclaration{{
					Name: aws.String("Approval"),
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
						"EnvironmentVariables": "[{\"name\":\"COMMAND\",\"value\":\"argocd-destroy\"},{\"name\":\"TF_VAR_prefix\",\"value\":\"" + stepName + "\"},{\"name\":\"WORKSPACE\",\"value\":\"" + workspace + "\"}]",
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
	return err
}

func (p *pipeline) WaitPipelineExecution(pipelineName string, delay int) error {
	common.Logger.Printf("Waiting for pipeline %s to complete, polling delay %d s\n", pipelineName, delay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for ctx.Err() == nil {
		state, err := p.codePipeline.GetPipelineState(context.Background(), &codepipeline.GetPipelineStateInput{
			Name: aws.String(pipelineName),
		})
		if err != nil {
			return err
		}
		if state != nil && state.StageStates != nil {
			successes := 0
			for _, stage := range state.StageStates {
				if stage.LatestExecution == nil {
					break
				}
				switch stage.LatestExecution.Status {
				case types.StageExecutionStatusInProgress:
					continue
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
		}
		time.Sleep(time.Duration(delay) * time.Second)
	}
	return ctx.Err()
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
