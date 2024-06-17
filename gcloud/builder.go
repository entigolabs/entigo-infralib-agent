package gcloud

import (
	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/googleapis/gax-go/v2/apierror"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/genproto/googleapis/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"io/fs"
	"os"
	k8syaml "sigs.k8s.io/yaml"
	"strings"
)

const tempFolder = "/tmp"

type Builder struct {
	ctx            context.Context
	client         *run.JobsClient
	projectId      string
	location       string
	serviceAccount string
}

func NewBuilder(ctx context.Context, projectId string, location string, serviceAccount string) (*Builder, error) {
	client, err := run.NewJobsClient(ctx)
	if err != nil {
		return nil, err
	}
	return &Builder{
		ctx:            ctx,
		client:         client,
		projectId:      projectId,
		location:       location,
		serviceAccount: serviceAccount,
	}, nil
}

// TODO ArgoCD has different commands!
// TODO Custom Repo storage?
func (b *Builder) CreateProject(projectName string, bucket string, stepName string, workspace string, imageVersion string, vpcConfig *model.VpcConfig) error {
	image := fmt.Sprintf("%s:%s", model.ProjectImageDocker, imageVersion)
	return b.createJobManifests(projectName, bucket, stepName, workspace, image, vpcConfig)
}

func (b *Builder) createJobManifests(projectName string, bucket string, stepName string, workspace string, image string, vpcConfig *model.VpcConfig) error {
	templateMeta, err := getVPCMeta(vpcConfig)
	if err != nil {
		return err
	}
	err = b.createJobManifest(projectName, model.PlanCommand, bucket, stepName, workspace, image, templateMeta)
	if err != nil {
		return fmt.Errorf("failed to create plan job manifest: %v", err)
	}
	err = b.createJobManifest(projectName, model.ApplyCommand, bucket, stepName, workspace, image, templateMeta)
	if err != nil {
		return fmt.Errorf("failed to create apply job manifest: %v", err)
	}
	err = b.createJobManifest(projectName, model.PlanDestroyCommand, bucket, stepName, workspace, image, templateMeta)
	if err != nil {
		return fmt.Errorf("failed to create plan-destroy job manifest: %v", err)
	}
	err = b.createJobManifest(projectName, model.ApplyDestroyCommand, bucket, stepName, workspace, image, templateMeta)
	if err != nil {
		return fmt.Errorf("failed to create destroy job manifest: %v", err)
	}
	return nil
}

func (b *Builder) createJobManifest(projectName string, command model.ActionCommand, bucket string, stepName string, workspace string, image string, templateMeta *runv1.ObjectMeta) error {
	job := b.GetJobManifest(projectName, command, bucket, stepName, workspace, image, templateMeta)
	bytes, err := k8syaml.Marshal(job)
	if err != nil {
		return err
	}
	err = os.MkdirAll(fmt.Sprintf("%s/%s/%s/%s", tempFolder, bucket, stepName, workspace), 0755)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.WriteFile(fmt.Sprintf("%s/%s/%s/%s/%s-%s.yaml", tempFolder, bucket, stepName, workspace, projectName, command),
		bytes, 0644)
}

func (b *Builder) GetJobManifest(projectName string, command model.ActionCommand, bucket string, stepName string, workspace string, image string, templateMeta *runv1.ObjectMeta) runv1.Job {
	return runv1.Job{
		ApiVersion: "run.googleapis.com/v1",
		Kind:       "Job",
		Metadata: &runv1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", projectName, command),
			Annotations: map[string]string{
				"run.googleapis.com/launch-stage": "BETA",
			},
		},
		Spec: &runv1.JobSpec{
			Template: &runv1.ExecutionTemplateSpec{
				Metadata: templateMeta,
				Spec: &runv1.ExecutionSpec{
					Template: &runv1.TaskTemplateSpec{
						Spec: &runv1.TaskSpec{
							TimeoutSeconds:     86400,
							ServiceAccountName: b.serviceAccount,
							MaxRetries:         1, // TODO 0 retries is cut away by the yaml marshal
							Containers: []*runv1.Container{{
								Name:  "infralib",
								Image: image,
								Env:   b.getEnvironmentVariables(projectName, stepName, workspace, "/bucket", command),
								VolumeMounts: []*runv1.VolumeMount{{
									Name:      bucket,
									MountPath: "/bucket",
								}, {
									Name:      "project",
									MountPath: "/project",
								}},
							}},
							Volumes: []*runv1.Volume{{
								Name: bucket,
								Csi: &runv1.CSIVolumeSource{
									Driver: "gcsfuse.run.googleapis.com",
									VolumeAttributes: map[string]string{
										"bucketName": bucket,
									},
								},
							}, {
								Name: "project",
								EmptyDir: &runv1.EmptyDirVolumeSource{
									SizeLimit: "1Gi",
								},
							}},
						},
					},
				},
			},
		},
	}
}

func (b *Builder) CreateAgentProject(projectName string, awsPrefix string, imageVersion string) error {
	jobOp, err := b.client.CreateJob(b.ctx, &runpb.CreateJobRequest{
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
						}, {
							Name:   "PROJECT_ID",
							Values: &runpb.EnvVar_Value{Value: b.projectId},
						}},
						VolumeMounts: []*runpb.VolumeMount{{
							Name:      "tmp",
							MountPath: "/tmp",
						}},
					}},
					Volumes: []*runpb.Volume{{
						Name:       "tmp",
						VolumeType: &runpb.Volume_EmptyDir{EmptyDir: &runpb.EmptyDirVolumeSource{SizeLimit: "100Mi"}},
					}},
					Retries:        &runpb.TaskTemplate_MaxRetries{MaxRetries: 0},
					Timeout:        &durationpb.Duration{Seconds: 86400},
					ServiceAccount: b.serviceAccount,
				},
			},
			LaunchStage: api.LaunchStage_BETA,
		},
		JobId: projectName,
	})
	if err != nil {
		return fmt.Errorf("failed to create agent job: %v", err)
	}
	_, err = jobOp.Wait(b.ctx)
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
		Name:  projectName,
		Image: job.Template.Template.Containers[0].Image,
	}, nil
}

func (b *Builder) UpdateAgentProject(projectName string, version string) error {
	job, err := b.getJob(projectName)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found", projectName)
	}
	image := fmt.Sprintf("%s:%s", model.AgentImageDocker, version)

	if image == "" || job.Template.Template.Containers[0].Image == image {
		return nil
	}

	job.Template.Template.Containers[0].Image = image
	_, err = b.client.UpdateJob(b.ctx, &runpb.UpdateJobRequest{Job: job})
	return err
}

func (b *Builder) UpdateProject(projectName, bucket, stepName, workspace, image string, vpcConfig *model.VpcConfig) error {
	job, err := b.getJob(fmt.Sprintf("%s-%s", projectName, model.PlanCommand))
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("project %s not found", projectName)
	}

	vpc := getGCloudVpcAccess(vpcConfig)
	vpcChanged := hasVPCConfigChanged(vpc, job.Template.Template.VpcAccess)
	imageChanged := image != "" && job.Template.Template.Containers[0].Image != image

	if !imageChanged && !vpcChanged {
		return nil
	}
	return b.createJobManifests(projectName, bucket, stepName, workspace, image, vpcConfig)
}

func (b *Builder) executeJob(projectName string, wait bool) (string, error) {
	fmt.Printf("Executing job %s\n", projectName)
	job, err := b.getJob(projectName)
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", fmt.Errorf("job %s not found", projectName)
	}
	jobOp, err := b.client.RunJob(b.ctx, &runpb.RunJobRequest{Name: job.Name})
	if err != nil {
		return "", err
	}
	if !wait {
		return "", err
	}
	execution, err := jobOp.Wait(b.ctx)
	if err != nil {
		return "", err
	}
	return execution.Name, err
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

func hasVPCConfigChanged(a, b *runpb.VpcAccess) bool {
	if a == nil || b == nil {
		return a != b
	}
	if a.Egress != b.Egress {
		return true
	}
	if len(a.NetworkInterfaces) != len(b.NetworkInterfaces) {
		return true
	}
	for i, ni := range a.NetworkInterfaces {
		if ni.Network != b.NetworkInterfaces[i].Network || ni.Subnetwork != b.NetworkInterfaces[i].Subnetwork {
			return true
		}
	}
	return false
}

func getVPCMeta(vpcConfig *model.VpcConfig) (*runv1.ObjectMeta, error) {
	vpcAccess := getGCloudVpcAccess(vpcConfig)
	if vpcAccess == nil {
		return nil, nil
	}
	interfaces := make([]map[string]string, len(vpcAccess.NetworkInterfaces))
	for i, ni := range vpcAccess.NetworkInterfaces {
		interfaces[i] = map[string]string{
			"network":    ni.Network,
			"subnetwork": ni.Subnetwork,
		}
	}
	interfacesJson, err := json.Marshal(interfaces)
	if err != nil {
		return nil, err
	}
	return &runv1.ObjectMeta{
		Annotations: map[string]string{
			"run.googleapis.com/vpc-access-egress":  strings.Replace(strings.ToLower(vpcAccess.Egress.String()), "_", "-", -1),
			"run.googleapis.com/network-interfaces": string(interfacesJson),
		},
	}, nil
}

func getGCloudVpcAccess(vpcConfig *model.VpcConfig) *runpb.VpcAccess {
	if vpcConfig == nil {
		return &runpb.VpcAccess{
			NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{
				Network:    "runner-main-biz",
				Subnetwork: "runner-main-biz-private-0",
			}},
			Egress: runpb.VpcAccess_PRIVATE_RANGES_ONLY,
		}
	}
	return nil // TODO
}

func (b *Builder) getEnvironmentVariables(projectName string, stepName string, workspace string, dir string, command model.ActionCommand) []*runv1.EnvVar {
	return []*runv1.EnvVar{
		{Name: "PROJECT_NAME", Value: projectName},
		{Name: "CODEBUILD_SRC_DIR", Value: dir},
		{Name: "GOOGLE_REGION", Value: b.location},
		{Name: "GOOGLE_PROJECT", Value: b.projectId},
		{Name: "COMMAND", Value: string(command)},
		{Name: "TF_VAR_prefix", Value: stepName},
		{Name: "WORKSPACE", Value: workspace},
	}
}
