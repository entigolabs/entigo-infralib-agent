package service

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline"
	"github.com/aws/aws-sdk-go-v2/service/codepipeline/types"
)

type Pipeline interface {
	CreatePipeline(pipelineName string, roleArn string, bucket string, bucketArn string) error
}

type pipeline struct {
	codePipeline *codepipeline.Client
}

func NewPipeline(awsConfig aws.Config) Pipeline {
	return &pipeline{
		codePipeline: codepipeline.NewFromConfig(awsConfig),
	}
}

func (p *pipeline) CreatePipeline(pipelineName string, roleArn string, bucket string, bucketArn string) error {
	_, err := p.codePipeline.CreatePipeline(context.Background(), &codepipeline.CreatePipelineInput{
		Pipeline: &types.PipelineDeclaration{
			Name:    aws.String(pipelineName),
			RoleArn: aws.String(roleArn),
			ArtifactStore: &types.ArtifactStore{
				Location: aws.String(bucket),
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
					RunOrder:        NewInt32(1),
					Configuration: map[string]string{
						"S3Bucket":             bucketArn,
						"S3ObjectKey":          bucketArn,
						"PollForSourceChanges": "false",
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
					RunOrder:        NewInt32(2),
					Configuration: map[string]string{
						"ProjectName":   pipelineName,
						"PrimarySource": "source_output", // TODO Env variables
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
					RunOrder: NewInt32(3),
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
					RunOrder:       NewInt32(4),
					Configuration: map[string]string{
						"ProjectName":   pipelineName,
						"PrimarySource": "source_output", // TODO Env variables
					},
				},
				},
			},
			},
		},
	})
	return err
}

func NewInt32(x int32) *int32 {
	return &x
}
