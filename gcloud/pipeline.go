package gcloud

import (
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"google.golang.org/genproto/googleapis/api"
	"google.golang.org/protobuf/types/known/durationpb"
)

type pipeline struct {
	ctx            context.Context
	client         *run.JobsClient
	projectId      string
	location       string
	serviceAccount string
	bucket         string
}

func NewPipeline(ctx context.Context, projectId string, serviceAccount string, bucket string) (model.Pipeline, error) {
	client, err := run.NewJobsClient(ctx)
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
	job, err := p.getJob(pipelineName)
	if err != nil {
		return nil, err
	}
	if job != nil {
		return p.StartPipelineExecution(pipelineName)
	}
	bucket := p.bucket
	if customRepo != "" {
		bucket = customRepo
	}
	_, err = p.client.CreateJob(p.ctx, &runpb.CreateJobRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", p.projectId, p.location),
		Job: &runpb.Job{
			Template: &runpb.ExecutionTemplate{
				Template: &runpb.TaskTemplate{
					Containers: []*runpb.Container{{
						Name:  "terraform",
						Image: model.ProjectImageDocker,
						Env:   p.getEnvironmentVariables(model.PlanCommand, stepName, step.Workspace),
					}},
					Volumes: []*runpb.Volume{{
						Name: bucket,
						VolumeType: &runpb.Volume_Gcs{
							Gcs: &runpb.GCSVolumeSource{
								Bucket: bucket,
							},
						},
					}},
					Timeout:        &durationpb.Duration{Seconds: 86400},
					ServiceAccount: p.serviceAccount,
				},
			},
			LaunchStage: api.LaunchStage_BETA,
		},
		JobId: pipelineName,
	})
	if err != nil {
		return nil, err
	}
	return p.StartPipelineExecution(pipelineName)
}

func (p *pipeline) CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) error {
	job, err := p.getJob(pipelineName)
	if err != nil {
		return nil
	}
	if job != nil {
		// TODO Update env vars?
	}
	bucket := p.bucket
	if customRepo != "" {
		bucket = customRepo
	}
	_, err = p.client.CreateJob(p.ctx, &runpb.CreateJobRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", p.projectId, p.location),
		Job: &runpb.Job{
			Template: &runpb.ExecutionTemplate{
				Template: &runpb.TaskTemplate{
					Containers: []*runpb.Container{{
						Name:  "terraform",
						Image: model.ProjectImageDocker,
						Env:   p.getEnvironmentVariables(model.PlanDestroyCommand, stepName, step.Workspace),
					}},
					Volumes: []*runpb.Volume{{
						Name: bucket,
						VolumeType: &runpb.Volume_Gcs{
							Gcs: &runpb.GCSVolumeSource{
								Bucket: bucket,
							},
						},
					}},
					Timeout:        &durationpb.Duration{Seconds: 86400},
					ServiceAccount: p.serviceAccount,
				},
			},
			LaunchStage: api.LaunchStage_BETA,
		},
		JobId: pipelineName,
	})
	return err
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
	operation, err := p.client.RunJob(p.ctx, &runpb.RunJobRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/jobs/%s", p.projectId, p.location, pipelineName),
	})
	if err != nil {
		return nil, err
	}
	name := operation.Name()
	return &name, nil
}

func (p *pipeline) WaitPipelineExecution(pipelineName string, executionId *string, autoApprove bool, delay int, stepType model.StepType) error {
	operation, err := p.client.WaitOperation(p.ctx, &longrunningpb.WaitOperationRequest{
		Name: *executionId,
	})
	if err != nil {
		return err
	}
	if !operation.Done {
		return errors.New("operation is not done")
	}
	resultError := operation.GetError()
	if resultError == nil {
		return nil
	}
	return errors.New(resultError.Message)
}

func (p *pipeline) getJob(pipelineName string) (*runpb.Job, error) {
	job, err := p.client.GetJob(p.ctx, &runpb.GetJobRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/jobs/%s", p.projectId, p.location, pipelineName),
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (p *pipeline) getEnvironmentVariables(command model.ActionCommand, stepName string, workspace string) []*runpb.EnvVar {
	return []*runpb.EnvVar{
		{
			Name:   "GOOGLE_REGION",
			Values: &runpb.EnvVar_Value{Value: p.location},
		},
		{
			Name:   "GOOGLE_PROJECT",
			Values: &runpb.EnvVar_Value{Value: p.projectId},
		},
		{
			Name:   "COMMAND",
			Values: &runpb.EnvVar_Value{Value: string(command)},
		},
		{
			Name:   "TF_VAR_prefix",
			Values: &runpb.EnvVar_Value{Value: stepName},
		},
		{
			Name:   "WORKSPACE",
			Values: &runpb.EnvVar_Value{Value: workspace},
		},
	}
}
