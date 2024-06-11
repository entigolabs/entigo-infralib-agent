package gcloud

import (
	deploy "cloud.google.com/go/deploy/apiv1"
	"cloud.google.com/go/deploy/apiv1/deploypb"
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

type pipeline struct {
	ctx            context.Context
	client         *deploy.CloudDeployClient
	projectId      string
	location       string
	serviceAccount string
	bucket         string
}

func NewPipeline(ctx context.Context, projectId string, serviceAccount string, bucket string) (model.Pipeline, error) {
	client, err := deploy.NewCloudDeployClient(ctx)
	if err != nil {
		return nil, err
	}
	return &pipeline{
		ctx:            ctx,
		client:         client,
		projectId:      projectId,
		location:       "europe-north1", // TODO make this configurable
		serviceAccount: serviceAccount,
		bucket:         bucket,
	}, nil
}

func (p *pipeline) CreateTerraformPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) (*string, error) {
	collection := fmt.Sprintf("projects/%s/locations/%s", p.projectId, p.location)
	_, err := p.client.CreateTarget(p.ctx, &deploypb.CreateTargetRequest{
		Parent:   collection,
		TargetId: fmt.Sprintf("%s-plan", pipelineName),
		Target: &deploypb.Target{
			DeploymentTarget: &deploypb.Target_Run{
				Run: &deploypb.CloudRunLocation{
					Location: collection,
				},
			},
			ExecutionConfigs: []*deploypb.ExecutionConfig{{
				Usages: []deploypb.ExecutionConfig_ExecutionEnvironmentUsage{
					deploypb.ExecutionConfig_RENDER,
					deploypb.ExecutionConfig_DEPLOY,
				},
				ExecutionTimeout: &durationpb.Duration{Seconds: 86400},
				ServiceAccount:   p.serviceAccount,
			}},
		},
	})
	if err != nil {
		var apiError *apierror.APIError
		if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.AlreadyExists {
			return nil, err
		}
	}
	_, err = p.client.CreateTarget(p.ctx, &deploypb.CreateTargetRequest{
		Parent:   collection,
		TargetId: fmt.Sprintf("%s-apply", pipelineName),
		Target: &deploypb.Target{
			DeploymentTarget: &deploypb.Target_Run{
				Run: &deploypb.CloudRunLocation{
					Location: collection,
				},
			},
			ExecutionConfigs: []*deploypb.ExecutionConfig{{
				Usages: []deploypb.ExecutionConfig_ExecutionEnvironmentUsage{
					deploypb.ExecutionConfig_RENDER,
					deploypb.ExecutionConfig_DEPLOY,
				},
				ExecutionTimeout: &durationpb.Duration{Seconds: 86400},
				ServiceAccount:   p.serviceAccount,
			}},
			RequireApproval: true,
		},
	})
	if err != nil {
		var apiError *apierror.APIError
		if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.AlreadyExists {
			return nil, err
		}
	}
	common.Logger.Printf("%s/targets/%s-plan\n", collection, pipelineName)
	deliveryPipeline, err := p.client.CreateDeliveryPipeline(p.ctx, &deploypb.CreateDeliveryPipelineRequest{
		Parent:             collection,
		DeliveryPipelineId: pipelineName,
		DeliveryPipeline: &deploypb.DeliveryPipeline{
			Pipeline: &deploypb.DeliveryPipeline_SerialPipeline{
				SerialPipeline: &deploypb.SerialPipeline{
					Stages: []*deploypb.Stage{
						{
							TargetId: fmt.Sprintf("%s-plan", pipelineName),
							Profiles: []string{"plan"},
						},
						{
							TargetId: fmt.Sprintf("%s-apply", pipelineName),
							Profiles: []string{"apply"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	name := deliveryPipeline.Name()
	return &name, nil
}

func (p *pipeline) CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) error {
	return errors.New("not implemented")
}

func (p *pipeline) CreateArgoCDPipeline(pipelineName string, projectName string, stepName string, step model.Step) (*string, error) {
	return nil, errors.New("not implemented")
}

func (p *pipeline) CreateArgoCDDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step) error {
	return errors.New("not implemented")
}

func (p *pipeline) CreateAgentPipeline(prefix string, pipelineName string, projectName string, bucket string) error {
	return errors.New("not implemented")
}

func (p *pipeline) UpdatePipeline(pipelineName string, stepName string, step model.Step) error {
	return errors.New("not implemented")
}

func (p *pipeline) StartPipelineExecution(pipelineName string) (*string, error) {
	return nil, errors.New("not implemented")
}

func (p *pipeline) WaitPipelineExecution(pipelineName string, executionId *string, autoApprove bool, delay int, stepType model.StepType) error {
	return errors.New("not implemented")
}
