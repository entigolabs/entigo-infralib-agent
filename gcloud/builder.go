package gcloud

import (
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/genproto/googleapis/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

type Builder struct {
	ctx            context.Context
	client         *run.JobsClient
	projectId      string
	location       string
	serviceAccount string
	bucket         string
}

func NewBuilder(ctx context.Context, projectId string, serviceAccount string, bucket string) (*Builder, error) {
	client, err := run.NewJobsClient(ctx)
	if err != nil {
		return nil, err
	}
	return &Builder{
		ctx:            ctx,
		client:         client,
		projectId:      projectId,
		location:       "europe-north1", // TODO make this configurable
		serviceAccount: serviceAccount,
		bucket:         bucket,
	}, nil
}

func (b *Builder) CreateProject(projectName string, repoURL string, stepName string, workspace string, imageVersion string, vpcConfig *model.VpcConfig) error {
	job, err := b.getJob(projectName)
	if err != nil {
		return err
	}
	image := fmt.Sprintf("%s:%s", model.ProjectImageDocker, imageVersion)
	if job != nil {
		return b.UpdateProject(projectName, image, vpcConfig)
	}
	_, err = b.client.CreateJob(b.ctx, &runpb.CreateJobRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", b.projectId, b.location),
		Job: &runpb.Job{
			Template: &runpb.ExecutionTemplate{
				Template: &runpb.TaskTemplate{
					Containers: []*runpb.Container{{
						Name:  "terraform",
						Image: image,
						Env:   b.getEnvironmentVariables(projectName, stepName, workspace, "/bucket"),
						VolumeMounts: []*runpb.VolumeMount{{
							Name:      b.bucket,
							MountPath: "/bucket",
						}, {
							Name:      "project",
							MountPath: "/project",
						}},
					}},
					Volumes: []*runpb.Volume{{
						Name: b.bucket,
						VolumeType: &runpb.Volume_Gcs{
							Gcs: &runpb.GCSVolumeSource{Bucket: b.bucket},
						},
					}, {
						Name: "project",
						VolumeType: &runpb.Volume_EmptyDir{
							EmptyDir: &runpb.EmptyDirVolumeSource{SizeLimit: "1Gi"},
						},
					}},
					Timeout:        &durationpb.Duration{Seconds: 86400},
					ServiceAccount: b.serviceAccount,
					VpcAccess:      getGCloudVpcAccess(vpcConfig),
					Retries:        &runpb.TaskTemplate_MaxRetries{MaxRetries: 0},
				},
			},
			LaunchStage: api.LaunchStage_BETA,
		},
		JobId: projectName,
	})
	return err
}

func (b *Builder) CreateAgentProject(projectName string, awsPrefix string, imageVersion string) error {
	_, err := b.client.CreateJob(b.ctx, &runpb.CreateJobRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", b.projectId, b.location),
		Job: &runpb.Job{
			Template: &runpb.ExecutionTemplate{
				Template: &runpb.TaskTemplate{
					Containers: []*runpb.Container{{
						Name:  "agent",
						Image: model.AgentImageDocker + ":" + imageVersion,
						Env: []*runpb.EnvVar{{
							Name:   common.AwsPrefixEnv,
							Values: &runpb.EnvVar_Value{Value: awsPrefix},
						}},
					}},
					Timeout:        &durationpb.Duration{Seconds: 86400},
					ServiceAccount: b.serviceAccount,
				},
			},
			LaunchStage: api.LaunchStage_BETA,
		},
		JobId: projectName,
	})
	return err
}

func (b *Builder) GetProject(projectName string) (*model.Project, error) {
	job, err := b.getJob(projectName)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, nil
	}
	return &model.Project{
		Name:  job.Name,
		Image: job.Template.Template.Containers[0].Image,
	}, nil
}

func (b *Builder) UpdateProject(projectName string, image string, vpcConfig *model.VpcConfig) error {
	job, err := b.getJob(projectName)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("project %s not found", projectName)
	}

	_ = getGCloudVpcAccess(vpcConfig) // TODO Check if vpc hasn't changed
	imageChanged := job.Template.Template.Containers[0].Image != image

	if !imageChanged {
		return nil
	}

	job.Template.Template.Containers[0].Image = image
	_, err = b.client.UpdateJob(b.ctx, &runpb.UpdateJobRequest{Job: job})
	return err
}

func (b *Builder) getJob(projectName string) (*runpb.Job, error) {
	job, err := b.client.GetJob(b.ctx, &runpb.GetJobRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/jobs/%s", b.projectId, b.location, projectName),
	})
	if err != nil {
		var apiError *apierror.APIError
		if errors.As(err, &apiError) && (apiError.HTTPCode() == 404 || apiError.GRPCStatus().Code() == codes.NotFound) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

func getGCloudVpcAccess(vpcConfig *model.VpcConfig) *runpb.VpcAccess {
	if vpcConfig == nil {
		return &runpb.VpcAccess{
			NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{
				Network:    "taivopikkmets-rd-203-biz",
				Subnetwork: "taivopikkmets-rd-203-biz",
			}},
			Egress: runpb.VpcAccess_PRIVATE_RANGES_ONLY,
		}
	}
	return nil // TODO
}

func (b *Builder) getEnvironmentVariables(projectName string, stepName string, workspace string, dir string) []*runpb.EnvVar {
	return []*runpb.EnvVar{
		{
			Name:   "PROJECT_NAME",
			Values: &runpb.EnvVar_Value{Value: projectName},
		},
		{
			Name:   "CODEBUILD_SRC_DIR",
			Values: &runpb.EnvVar_Value{Value: dir},
		},
		{
			Name:   "GOOGLE_REGION",
			Values: &runpb.EnvVar_Value{Value: b.location},
		},
		{
			Name:   "GOOGLE_PROJECT",
			Values: &runpb.EnvVar_Value{Value: b.projectId},
		},
		{
			Name:   "COMMAND",
			Values: &runpb.EnvVar_Value{Value: string(model.PlanCommand)},
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
