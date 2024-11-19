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
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/googleapis/gax-go/v2/apierror"
	runv1 "google.golang.org/api/run/v1"
	"google.golang.org/genproto/googleapis/api"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"io/fs"
	"log"
	"os"
	"strings"
)

const tempFolder = "/tmp"

type Builder struct {
	ctx            context.Context
	client         *run.JobsClient
	projectId      string
	location       string
	zone           string
	serviceAccount string
}

func NewBuilder(ctx context.Context, projectId, location, zone, serviceAccount string) (*Builder, error) {
	client, err := run.NewJobsClient(ctx)
	if err != nil {
		return nil, err
	}
	return &Builder{
		ctx:            ctx,
		client:         client,
		projectId:      projectId,
		location:       location,
		zone:           zone,
		serviceAccount: serviceAccount,
	}, nil
}

func (b *Builder) CreateProject(projectName string, bucket string, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig) error {
	if imageSource == "" {
		imageSource = model.ProjectImageDocker
	}
	image := fmt.Sprintf("%s:%s", imageSource, imageVersion)
	err := b.createJobManifests(projectName, bucket, stepName, step, image, vpcConfig)
	if err != nil {
		return err
	}
	return b.createDestroyJobs(projectName, bucket, stepName, step, image, vpcConfig)
}

func (b *Builder) createJobManifests(projectName string, bucket string, stepName string, step model.Step, image string, vpcConfig *model.VpcConfig) error {
	templateMeta, err := getVPCMeta(vpcConfig)
	if err != nil {
		return err
	}
	var commands []model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		commands = []model.ActionCommand{model.ArgoCDPlanCommand, model.ArgoCDApplyCommand}
	} else {
		commands = []model.ActionCommand{model.PlanCommand, model.ApplyCommand}
	}
	err = os.MkdirAll(fmt.Sprintf("%s/%s/%s", tempFolder, bucket, stepName), 0755)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	for _, command := range commands {
		err = b.createJobManifest(projectName, command, bucket, stepName, step, image, templateMeta)
		if err != nil {
			return fmt.Errorf("failed to create %s job manifest: %v", model.PlanCommand, err)
		}
	}
	return nil
}

func (b *Builder) createJobManifest(projectName string, command model.ActionCommand, bucket string, stepName string, step model.Step, image string, templateMeta *runv1.ObjectMeta) error {
	job := b.GetJobManifest(projectName, command, bucket, stepName, step, image, templateMeta)
	bytes, err := util.MarshalYamlWithJsonTags(job)
	if err != nil {
		return err
	}
	return os.WriteFile(fmt.Sprintf("%s/%s/%s/%s-%s.yaml", tempFolder, bucket, stepName, projectName, command),
		bytes, 0644)
}

func (b *Builder) GetJobManifest(projectName string, command model.ActionCommand, bucket string, stepName string, step model.Step, image string, templateMeta *runv1.ObjectMeta) runv1.Job {
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
							MaxRetries:         0,
							ForceSendFields:    []string{"MaxRetries"},
							Containers: []*runv1.Container{{
								Name:  "infralib",
								Image: image,
								Env:   b.getEnvironmentVariables(projectName, stepName, step, bucket, command),
								VolumeMounts: []*runv1.VolumeMount{{
									Name:      "project",
									MountPath: "/project",
								}},
								Resources: &runv1.ResourceRequirements{
									Limits: map[string]string{
										"memory": "5Gi",
										"cpu":    "6",
									},
								},
							}},
							Volumes: []*runv1.Volume{{
								Name: "project",
								EmptyDir: &runv1.EmptyDirVolumeSource{
									SizeLimit: "4Gi",
								},
							}},
						},
					},
				},
			},
		},
	}
}

func (b *Builder) createDestroyJobs(name string, bucket string, stepName string, step model.Step, image string, vpcConfig *model.VpcConfig) error {
	var planCommand model.ActionCommand
	var applyCommand model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		planCommand = model.ArgoCDPlanDestroyCommand
		applyCommand = model.ArgoCDApplyDestroyCommand
	} else {
		planCommand = model.PlanDestroyCommand
		applyCommand = model.ApplyDestroyCommand
	}
	err := b.createJob(fmt.Sprintf("%s-plan-destroy", name), bucket, stepName, step, image, vpcConfig, planCommand)
	if err != nil {
		return err
	}
	return b.createJob(fmt.Sprintf("%s-apply-destroy", name), bucket, stepName, step, image, vpcConfig, applyCommand)
}

func (b *Builder) createJob(projectName string, bucket string, stepName string, step model.Step, image string, vpcConfig *model.VpcConfig, command model.ActionCommand) error {
	job, err := b.getJob(projectName)
	if err != nil {
		return err
	}
	if job != nil {
		return b.updateJob(projectName, stepName, step, bucket, image, vpcConfig, command)
	}
	_, err = b.client.CreateJob(b.ctx, &runpb.CreateJobRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", b.projectId, b.location),
		JobId:  projectName,
		Job: &runpb.Job{
			LaunchStage: api.LaunchStage_BETA,
			Template: &runpb.ExecutionTemplate{
				Template: &runpb.TaskTemplate{
					Retries:        &runpb.TaskTemplate_MaxRetries{MaxRetries: 0},
					Timeout:        &durationpb.Duration{Seconds: 86400},
					ServiceAccount: b.serviceAccount,
					VpcAccess:      getGCloudVpcAccess(vpcConfig),
					Containers: []*runpb.Container{{
						Name:  "infralib",
						Image: image,
						Env:   b.getJobEnvironmentVariables(projectName, stepName, step, bucket, command),
						VolumeMounts: []*runpb.VolumeMount{{
							Name:      "project",
							MountPath: "/project",
						}},
						Resources: &runpb.ResourceRequirements{
							Limits: map[string]string{
								"memory": "5Gi",
								"cpu":    "6",
							},
						},
					}},
					Volumes: []*runpb.Volume{{
						Name: "project",
						VolumeType: &runpb.Volume_EmptyDir{
							EmptyDir: &runpb.EmptyDirVolumeSource{SizeLimit: "4Gi"},
						},
					}},
				},
			},
		},
	})
	return err
}

func (b *Builder) CreateAgentProject(projectName string, awsPrefix string, imageVersion string, cmd common.Command) error {
	jobOp, err := b.client.CreateJob(b.ctx, &runpb.CreateJobRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", b.projectId, b.location),
		Job: &runpb.Job{
			Template: &runpb.ExecutionTemplate{
				Template: &runpb.TaskTemplate{
					Containers: []*runpb.Container{{
						Name:  "agent",
						Image: model.AgentImageDocker + ":" + imageVersion,
						Args:  []string{"ei-agent", string(cmd)},
						Env:   b.getAgentEnvVars(awsPrefix),
						VolumeMounts: []*runpb.VolumeMount{{
							Name:      "tmp",
							MountPath: "/tmp",
						}},
						Resources: &runpb.ResourceRequirements{
							Limits: map[string]string{
								"memory": "1Gi",
							},
						},
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

func (b *Builder) getAgentEnvVars(awsPrefix string) []*runpb.EnvVar {
	return []*runpb.EnvVar{{
		Name:   common.AwsPrefixEnv,
		Values: &runpb.EnvVar_Value{Value: awsPrefix},
	}, {
		Name:   common.GCloudProjectIdEnv,
		Values: &runpb.EnvVar_Value{Value: b.projectId},
	}, {
		Name:   common.GCloudLocationEnv,
		Values: &runpb.EnvVar_Value{Value: b.location},
	}, {
		Name:   common.GCloudZoneEnv,
		Values: &runpb.EnvVar_Value{Value: b.zone},
	}}
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

func (b *Builder) UpdateAgentProject(projectName string, version string, cloudPrefix string) error {
	job, err := b.getJob(projectName)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found", projectName)
	}
	image := fmt.Sprintf("%s:%s", model.AgentImageDocker, version)

	if job.Template.Template.Containers[0].Image == image {
		return nil
	}

	job.Template.Template.Containers[0].Image = image
	job.Template.Template.Containers[0].Env = b.getAgentEnvVars(cloudPrefix)
	_, err = b.client.UpdateJob(b.ctx, &runpb.UpdateJobRequest{Job: job})
	return err
}

func (b *Builder) UpdateProject(projectName, bucket, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig) error {
	if imageSource == "" {
		imageSource = model.ProjectImageDocker
	}
	image := fmt.Sprintf("%s:%s", imageSource, imageVersion)
	err := b.createJobManifests(projectName, bucket, stepName, step, image, vpcConfig)
	if err != nil {
		return err
	}
	return b.updateDestroyJobs(projectName, bucket, stepName, step, image, vpcConfig)
}

func (b *Builder) DeleteProject(projectName string, step model.Step) error {
	var commands []model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		commands = []model.ActionCommand{model.ArgoCDPlanCommand, model.ArgoCDApplyCommand}
	} else {
		commands = []model.ActionCommand{model.PlanCommand, model.ApplyCommand}
	}
	for _, command := range commands {
		err := b.deleteJob(fmt.Sprintf("%s-%s", projectName, command))
		if err != nil {
			return err
		}
	}
	err := b.deleteJob(fmt.Sprintf("%s-plan-destroy", projectName))
	if err != nil {
		return err
	}
	return b.deleteJob(fmt.Sprintf("%s-apply-destroy", projectName))
}

func (b *Builder) updateDestroyJobs(projectName string, bucket string, stepName string, step model.Step, image string, vpcConfig *model.VpcConfig) error {
	var planCommand model.ActionCommand
	var applyCommand model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		planCommand = model.ArgoCDPlanDestroyCommand
		applyCommand = model.ArgoCDApplyDestroyCommand
	} else {
		planCommand = model.PlanDestroyCommand
		applyCommand = model.ApplyDestroyCommand
	}
	err := b.updateJob(fmt.Sprintf("%s-plan-destroy", projectName), stepName, step, bucket, image, vpcConfig, planCommand)
	if err != nil {
		return err
	}
	return b.updateJob(fmt.Sprintf("%s-apply-destroy", projectName), stepName, step, bucket, image, vpcConfig, applyCommand)
}

func (b *Builder) updateJob(projectName string, stepName string, step model.Step, bucket, image string, vpcConfig *model.VpcConfig, command model.ActionCommand) error {
	job, err := b.getJob(projectName)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found", projectName)
	}
	job.Template.Template.Containers[0].Image = image
	job.Template.Template.Containers[0].Env = b.getJobEnvironmentVariables(projectName, stepName, step, bucket, command)
	job.Template.Template.VpcAccess = getGCloudVpcAccess(vpcConfig)
	_, err = b.client.UpdateJob(b.ctx, &runpb.UpdateJobRequest{Job: job})
	return err
}

func (b *Builder) executeJob(projectName string, wait bool) (string, error) {
	log.Printf("Executing job %s\n", projectName)
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
	if vpcConfig == nil || vpcConfig.VpcId == nil {
		return nil
	}
	var subnet string
	if len(vpcConfig.Subnets) > 0 {
		subnet = vpcConfig.Subnets[0]
	}
	return &runpb.VpcAccess{
		Egress: runpb.VpcAccess_PRIVATE_RANGES_ONLY,
		NetworkInterfaces: []*runpb.VpcAccess_NetworkInterface{{
			Network:    *vpcConfig.VpcId,
			Subnetwork: subnet,
		}},
	}
}

func (b *Builder) getEnvironmentVariables(projectName string, stepName string, step model.Step, bucket string, command model.ActionCommand) []*runv1.EnvVar {
	rawEnvVars := b.getRawEnvironmentVariables(projectName, stepName, step, bucket, command)
	var envVars []*runv1.EnvVar
	for key, value := range rawEnvVars {
		envVars = append(envVars, &runv1.EnvVar{Name: key, Value: value})
	}
	return envVars
}

func (b *Builder) getJobEnvironmentVariables(projectName, stepName string, step model.Step, bucket string, command model.ActionCommand) []*runpb.EnvVar {
	rawEnvVars := b.getRawEnvironmentVariables(projectName, stepName, step, bucket, command)
	var envVars []*runpb.EnvVar
	for key, value := range rawEnvVars {
		envVars = append(envVars, &runpb.EnvVar{Name: key, Values: &runpb.EnvVar_Value{Value: value}})
	}
	return envVars
}

func (b *Builder) getRawEnvironmentVariables(projectName, stepName string, step model.Step, bucket string, command model.ActionCommand) map[string]string {
	envVars := map[string]string{
		"PROJECT_NAME":    projectName,
		"GOOGLE_REGION":   b.location,
		"GOOGLE_PROJECT":  b.projectId,
		"GOOGLE_ZONE":     b.zone,
		"COMMAND":         string(command),
		"TF_VAR_prefix":   stepName,
		"INFRALIB_BUCKET": bucket,
	}
	if step.Type == model.StepTypeTerraform {
		envVars = addTerraformEnvironmentVariables(envVars, step)
	}
	if step.Type == model.StepTypeArgoCD {
		envVars = addArgoCDEnvironmentVariables(envVars, step)
	}
	return envVars
}

func (b *Builder) deleteJob(name string) error {
	jobOp, err := b.client.DeleteJob(b.ctx, &runpb.DeleteJobRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/jobs/%s", b.projectId, b.location, name),
	})
	if err != nil {
		var apiError *apierror.APIError
		if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.NotFound {
			return err
		}
		return nil
	}
	_, err = jobOp.Wait(b.ctx)
	if err == nil {
		log.Printf("Deleted job %s\n", name)
	}
	return err
}

func addTerraformEnvironmentVariables(envVars map[string]string, step model.Step) map[string]string {
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			envVars[fmt.Sprintf("GIT_AUTH_USERNAME_%s", strings.ToUpper(module.Name))] = module.HttpUsername
			envVars[fmt.Sprintf("GIT_AUTH_PASSWORD_%s", strings.ToUpper(module.Name))] = module.HttpPassword
			envVars[fmt.Sprintf("GIT_AUTH_SOURCE_%s", strings.ToUpper(module.Name))] = module.Source
		}
	}
	return envVars
}

func addArgoCDEnvironmentVariables(envVars map[string]string, step model.Step) map[string]string {
	if step.KubernetesClusterName != "" {
		envVars["KUBERNETES_CLUSTER_NAME"] = step.KubernetesClusterName
	}
	if step.ArgocdNamespace == "" {
		envVars["ARGOCD_NAMESPACE"] = "argocd"
	} else {
		envVars["ARGOCD_NAMESPACE"] = step.ArgocdNamespace
	}
	return envVars
}
