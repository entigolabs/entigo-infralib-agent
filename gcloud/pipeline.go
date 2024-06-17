package gcloud

import (
	deploy "cloud.google.com/go/deploy/apiv1"
	"cloud.google.com/go/deploy/apiv1/deploypb"
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/google/uuid"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"os"
	"regexp"
	k8syaml "sigs.k8s.io/yaml"
	"strconv"
	"strings"
	"time"
)

type skaffold struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Metadata   metadata  `json:"metadata"`
	Deploy     runDeploy `json:"deploy"`
	Profiles   []profile `json:"profiles"`
}

type metadata struct {
	Name string `json:"name"`
}

type runDeploy struct {
	Cloudrun struct{} `json:"cloudrun"`
}

type profile struct {
	Name      string    `json:"name"`
	Manifests manifests `json:"manifests"`
}

type manifests struct {
	RawYaml []string `json:"rawYaml"`
}

type pipeline struct {
	ctx            context.Context
	client         *deploy.CloudDeployClient
	cloudPrefix    string
	projectId      string
	location       string
	serviceAccount string
	storage        *GStorage
	bucket         string
	builder        *Builder
	logging        *Logging
}

func NewPipeline(ctx context.Context, projectId string, location string, prefix string, serviceAccount string, storage *GStorage, bucket string, builder *Builder, logging *Logging) (model.Pipeline, error) {
	client, err := deploy.NewCloudDeployClient(ctx)
	if err != nil {
		return nil, err
	}
	err = createTargets(ctx, client, projectId, location, prefix, serviceAccount)
	if err != nil {
		return nil, err
	}
	return &pipeline{
		ctx:            ctx,
		client:         client,
		cloudPrefix:    prefix,
		projectId:      projectId,
		location:       location,
		serviceAccount: serviceAccount,
		storage:        storage,
		bucket:         bucket,
		builder:        builder,
		logging:        logging,
	}, nil
}

func createTargets(ctx context.Context, client *deploy.CloudDeployClient, projectId, location, prefix, serviceAccount string) error {
	collection := fmt.Sprintf("projects/%s/locations/%s", projectId, location)
	_, err := client.CreateTarget(ctx, &deploypb.CreateTargetRequest{
		Parent:   collection,
		TargetId: fmt.Sprintf("%s-plan", prefix),
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
				ServiceAccount:   serviceAccount,
			}},
		},
	})
	if err != nil {
		var apiError *apierror.APIError
		if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.AlreadyExists {
			return err
		}
	}
	_, err = client.CreateTarget(ctx, &deploypb.CreateTargetRequest{
		Parent:   collection,
		TargetId: fmt.Sprintf("%s-apply", prefix),
		Target: &deploypb.Target{
			RequireApproval: true,
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
				ServiceAccount:   serviceAccount,
			}},
		},
	})
	if err != nil {
		var apiError *apierror.APIError
		if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.AlreadyExists {
			return err
		}
	}
	return nil
}

// TODO Custom storage
func (p *pipeline) CreateTerraformPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) (*string, error) {
	folder := fmt.Sprintf("%s/%s/%s/%s", tempFolder, p.bucket, stepName, step.Workspace)
	err := p.createSkaffoldManifest(pipelineName, projectName, folder, model.PlanCommand, model.ApplyCommand)
	if err != nil {
		return nil, err
	}
	err = p.createSkaffoldManifest(fmt.Sprintf("%s-destroy", pipelineName), projectName, folder, model.PlanDestroyCommand, model.ApplyDestroyCommand)
	if err != nil {
		return nil, err
	}
	tarContent, err := util.TarGzWrite(folder)
	if err != nil {
		return nil, err
	}
	bucketFile := fmt.Sprintf("%s/%s.tar.gz", stepName, step.Workspace)
	err = p.storage.PutFile(bucketFile, tarContent)
	if err != nil {
		return nil, err
	}
	err = p.createDeliveryPipeline(pipelineName, model.PlanCommand, model.ApplyCommand)
	if err != nil {
		return nil, err
	}
	releaseId := fmt.Sprintf("%s-%s", pipelineName, uuid.New().String())
	release, err := p.client.CreateRelease(p.ctx, &deploypb.CreateReleaseRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s", p.projectId, p.location, pipelineName),
		ReleaseId: releaseId,
		Release: &deploypb.Release{
			SkaffoldConfigUri:  fmt.Sprintf("gs://%s/%s", p.bucket, bucketFile),
			SkaffoldConfigPath: fmt.Sprintf("%s/%s.yaml", step.Workspace, pipelineName),
		},
	})
	if err != nil {
		return nil, err
	}
	fmt.Printf("waiting for release %s to render\n", releaseId)
	err = p.waitForReleaseRender(release)
	if err != nil {
		return nil, err
	}
	rolloutId := fmt.Sprintf("%s-rollout-plan", pipelineName)
	rollout, err := p.client.CreateRollout(p.ctx, &deploypb.CreateRolloutRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s/releases/%s", p.projectId, p.location, pipelineName, releaseId),
		RolloutId: rolloutId,
		Rollout: &deploypb.Rollout{
			TargetId: fmt.Sprintf("%s-plan", p.cloudPrefix),
		},
	})
	if err != nil {
		return nil, err
	}
	fmt.Printf("waiting for rollout %s to finish\n", rolloutId)
	err = p.waitForRollout(rollout, pipelineName, step.Type, "", "", false)
	if err != nil {
		return nil, err
	}
	planJob := fmt.Sprintf("%s-%s", projectName, model.PlanCommand)
	executionName, err := p.builder.executeJob(planJob)
	if err != nil {
		return nil, err
	}
	rolloutId = fmt.Sprintf("%s-rollout-apply", pipelineName)
	rollout, err = p.client.CreateRollout(p.ctx, &deploypb.CreateRolloutRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s/releases/%s", p.projectId, p.location, pipelineName, releaseId),
		RolloutId: rolloutId,
		Rollout: &deploypb.Rollout{
			TargetId: fmt.Sprintf("%s-apply", p.cloudPrefix),
		},
	})
	if err != nil {
		return nil, err
	}
	fmt.Printf("waiting for rollout %s to finish\n", rolloutId)
	err = p.waitForRollout(rollout, pipelineName, step.Type, planJob, executionName, true)
	if err != nil {
		return nil, err
	}
	_, err = p.builder.executeJob(fmt.Sprintf("%s-%s", projectName, model.ApplyCommand))
	if err != nil {
		return nil, err
	}
	return &rolloutId, nil
}

func (p *pipeline) createSkaffoldManifest(name, projectName, folder string, firstCommand, secondCommand model.ActionCommand) error {
	skaffoldManifest := skaffold{
		APIVersion: "skaffold/v4beta7",
		Kind:       "Config",
		Metadata:   metadata{Name: name},
		Deploy:     runDeploy{},
		Profiles: []profile{
			{Name: string(firstCommand), Manifests: manifests{RawYaml: []string{fmt.Sprintf("%s-%s.yaml", projectName, firstCommand)}}},
			{Name: string(secondCommand), Manifests: manifests{RawYaml: []string{fmt.Sprintf("%s-%s.yaml", projectName, secondCommand)}}},
		},
	}
	bytes, err := k8syaml.Marshal(skaffoldManifest)
	if err != nil {
		return err
	}
	return os.WriteFile(fmt.Sprintf("%s/%s.yaml", folder, name), bytes, 0644)
}

func (p *pipeline) createDeliveryPipeline(pipelineName string, firstCommand, secondCommand model.ActionCommand) error {
	_, err := p.client.CreateDeliveryPipeline(p.ctx, &deploypb.CreateDeliveryPipelineRequest{
		Parent:             fmt.Sprintf("projects/%s/locations/%s", p.projectId, p.location),
		DeliveryPipelineId: pipelineName,
		DeliveryPipeline: &deploypb.DeliveryPipeline{
			Pipeline: &deploypb.DeliveryPipeline_SerialPipeline{
				SerialPipeline: &deploypb.SerialPipeline{
					Stages: []*deploypb.Stage{
						{
							TargetId: fmt.Sprintf("%s-plan", p.cloudPrefix),
							Profiles: []string{string(firstCommand)},
						},
						{
							TargetId: fmt.Sprintf("%s-apply", p.cloudPrefix),
							Profiles: []string{string(secondCommand)},
						},
					},
				},
			},
		},
	})
	if err != nil {
		var apiError *apierror.APIError
		if errors.As(err, &apiError) && apiError.GRPCStatus().Code() == codes.AlreadyExists {
			return nil
		} else {
			return err
		}
	}
	fmt.Printf("Created delivery pipeline %s\n", pipelineName)
	return nil
}

func (p *pipeline) waitForReleaseRender(releaseOp *deploy.CreateReleaseOperation) error {
	ctx, cancel := context.WithTimeout(p.ctx, 1*time.Minute)
	defer cancel()
	release, err := releaseOp.Wait(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for release to finish rendering: %w", ctx.Err())
		default:
			release, err = p.client.GetRelease(p.ctx, &deploypb.GetReleaseRequest{
				Name: release.GetName(),
			})
			if err != nil {
				return err
			}
			if release.GetRenderState() == deploypb.Release_RENDER_STATE_UNSPECIFIED || release.GetRenderState() == deploypb.Release_IN_PROGRESS {
				time.Sleep(5 * time.Second) // TODO Exponential sleep
				continue
			}
			if release.GetRenderState() != deploypb.Release_SUCCEEDED {
				return fmt.Errorf("release render failed: %s", release.GetRenderState())
			}
			return nil
		}
	}
}

func (p *pipeline) waitForRollout(rolloutOp *deploy.CreateRolloutOperation, pipelineName string, stepType model.StepType, jobName string, executionName string, autoApprove bool) error {
	ctx, cancel := context.WithTimeout(p.ctx, 2*time.Minute)
	defer cancel()
	rollout, err := rolloutOp.Wait(ctx)
	if err != nil {
		return err
	}
	approved := false
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for rollout to finish: %w", ctx.Err())
		default:
			rollout, err = p.client.GetRollout(p.ctx, &deploypb.GetRolloutRequest{
				Name: rollout.GetName(),
			})
			if err != nil {
				return err
			}
			if rollout.GetState() == deploypb.Rollout_STATE_UNSPECIFIED || rollout.GetState() == deploypb.Rollout_IN_PROGRESS {
				time.Sleep(5 * time.Second) // TODO Exponential sleep
				continue
			}
			if rollout.GetState() == deploypb.Rollout_PENDING_APPROVAL {
				if approved {
					time.Sleep(5 * time.Second)
					continue
				}
				if executionName == "" {
					common.Logger.Println("Execution name not found, please approve manually")
					time.Sleep(5 * time.Second)
					continue
				}
				pipeChanges, err := p.getChanges(pipelineName, stepType, jobName, executionName)
				if err != nil {
					return err
				}
				if pipeChanges != nil && pipeChanges.Destroyed == 0 && (pipeChanges.Changed == 0 || autoApprove) {
					_, err = p.client.ApproveRollout(p.ctx, &deploypb.ApproveRolloutRequest{
						Name:     rollout.GetName(),
						Approved: true,
					})
					if err != nil {
						common.Logger.Printf("Failed to approve rollout, please approve manually: %s", err)
					} else {
						common.Logger.Printf("Approved %s\n", pipelineName)
						approved = true
					}
				}
				time.Sleep(5 * time.Second)
				continue
			}
			if rollout.GetState() != deploypb.Rollout_SUCCEEDED {
				return fmt.Errorf("rollout failed: %s", rollout.GetState())
			}
			return nil
		}
	}
}

func (p *pipeline) getChanges(pipelineName string, stepType model.StepType, jobName string, executionName string) (*model.TerraformChanges, error) {
	switch stepType {
	case model.StepTypeTerraform:
		return p.getTerraformChanges(pipelineName, jobName, executionName)
	}
	return &model.TerraformChanges{}, nil
}

func (p *pipeline) getTerraformChanges(pipelineName string, jobName string, executionName string) (*model.TerraformChanges, error) {
	re := regexp.MustCompile(terraform.PlanRegex)
	lastSlash := strings.LastIndex(executionName, "/")
	logIterator := p.logging.GetJobExecutionLogs(jobName, executionName[lastSlash+1:], p.location)
	for {
		entry, err := logIterator.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		log := entry.GetTextPayload()
		matches := re.FindStringSubmatch(log)
		tfChanges := model.TerraformChanges{}
		if matches != nil {
			common.Logger.Printf("Pipeline %s: %s", pipelineName, log)
			changed := matches[2]
			destroyed := matches[3]
			if changed != "0" || destroyed != "0" {
				tfChanges.Changed, err = strconv.Atoi(changed)
				if err != nil {
					return nil, err
				}
				tfChanges.Destroyed, err = strconv.Atoi(destroyed)
				if err != nil {
					return nil, err
				}
				return &tfChanges, nil
			} else {
				return &tfChanges, nil
			}
		} else if strings.HasPrefix(log, "No changes. Your infrastructure matches the configuration.") ||
			strings.HasPrefix(log, "You can apply this plan to save these new output values") {
			common.Logger.Printf("Pipeline %s: %s", pipelineName, entry.GetTextPayload())
			return &tfChanges, nil
		}
	}
	return nil, fmt.Errorf("couldn't find terraform plan output from logs for %s", pipelineName)
}

func (p *pipeline) CreateTerraformDestroyPipeline(pipelineName string, projectName string, stepName string, step model.Step, customRepo string) error {
	return p.createDeliveryPipeline(fmt.Sprintf("%s-destroy", pipelineName), model.PlanDestroyCommand, model.ApplyDestroyCommand)
}

func (p *pipeline) CreateAgentPipeline(_ string, _ string, _ string, _ string) error {
	common.Logger.Println("GCloud uses Agent Job instead of pipeline")
	return nil
}

func (p *pipeline) UpdatePipeline(pipelineName string, stepName string, step model.Step) error {
	return errors.New("not implemented")
}

func (p *pipeline) StartPipelineExecution(pipelineName string) (*string, error) {
	// TODO Separate logic for agent?
	return nil, errors.New("not implemented")
}

func (p *pipeline) WaitPipelineExecution(pipelineName string, executionId *string, autoApprove bool, delay int, stepType model.StepType) error {
	return errors.New("not implemented")
}
