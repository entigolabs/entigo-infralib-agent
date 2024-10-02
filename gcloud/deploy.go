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

const bucketFileFormat = "%s/%s.tar.gz"

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

type Pipeline struct {
	ctx            context.Context
	client         *deploy.CloudDeployClient
	cloudPrefix    string
	projectId      string
	location       string
	serviceAccount string
	storage        *GStorage
	builder        *Builder
	logging        *Logging
}

func NewPipeline(ctx context.Context, projectId string, location string, prefix string, serviceAccount string, storage *GStorage, builder *Builder, logging *Logging) (*Pipeline, error) {
	client, err := deploy.NewCloudDeployClient(ctx)
	if err != nil {
		return nil, err
	}
	err = createTargets(ctx, client, projectId, location, prefix, serviceAccount)
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		ctx:            ctx,
		client:         client,
		cloudPrefix:    prefix,
		projectId:      projectId,
		location:       location,
		serviceAccount: serviceAccount,
		storage:        storage,
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

func (p *Pipeline) deleteTargets() error {
	targetOp, err := p.client.DeleteTarget(p.ctx, &deploypb.DeleteTargetRequest{
		Name:         fmt.Sprintf("projects/%s/locations/%s/targets/%s-plan", p.projectId, p.location, p.cloudPrefix),
		AllowMissing: true,
	})
	if err != nil {
		return err
	}
	err = targetOp.Wait(p.ctx)
	if err != nil {
		return err
	}
	common.Logger.Printf("Deleted target %s-plan\n", p.cloudPrefix)
	targetOp, err = p.client.DeleteTarget(p.ctx, &deploypb.DeleteTargetRequest{
		Name:         fmt.Sprintf("projects/%s/locations/%s/targets/%s-apply", p.projectId, p.location, p.cloudPrefix),
		AllowMissing: true,
	})
	if err != nil {
		return err
	}
	err = targetOp.Wait(p.ctx)
	if err != nil {
		return err
	}
	common.Logger.Printf("Deleted target %s-apply\n", p.cloudPrefix)
	return nil
}

func (p *Pipeline) CreatePipeline(projectName, stepName string, step model.Step, bucket model.Bucket) (*string, error) {
	var planCommand model.ActionCommand
	var applyCommand model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		planCommand = model.ArgoCDPlanCommand
		applyCommand = model.ArgoCDApplyCommand
	} else {
		planCommand = model.PlanCommand
		applyCommand = model.ApplyCommand
	}
	bucketMeta, err := bucket.GetRepoMetadata()
	if err != nil {
		return nil, err
	}
	folder := fmt.Sprintf("%s/%s/%s/%s", tempFolder, bucketMeta.Name, stepName, step.Workspace)
	err = p.createSkaffoldManifest(projectName, projectName, folder, planCommand, applyCommand)
	if err != nil {
		return nil, err
	}
	tarContent, err := util.TarGzWrite(folder)
	if err != nil {
		return nil, err
	}
	err = bucket.PutFile(fmt.Sprintf(bucketFileFormat, stepName, step.Workspace), tarContent)
	if err != nil {
		return nil, err
	}
	err = p.createDeliveryPipeline(projectName, planCommand, applyCommand)
	if err != nil {
		return nil, err
	}
	return p.StartPipelineExecution(projectName, stepName, step, bucketMeta.Name)
}

func (p *Pipeline) DeletePipeline(projectName string) error {
	name := fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s", p.projectId, p.location,
		projectName)
	_, err := p.client.GetDeliveryPipeline(p.ctx, &deploypb.GetDeliveryPipelineRequest{
		Name: name,
	})
	if err != nil {
		var apiError *apierror.APIError
		if errors.As(err, &apiError) && apiError.GRPCStatus().Code() == codes.NotFound {
			return nil
		}
		return err
	}
	pipelineOp, err := p.client.DeleteDeliveryPipeline(p.ctx, &deploypb.DeleteDeliveryPipelineRequest{
		Name:         name,
		AllowMissing: true,
		Force:        true,
	})
	if err != nil {
		return err
	}
	err = pipelineOp.Wait(p.ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Deleted delivery pipeline %s\n", projectName)
	return nil
}

func (p *Pipeline) createSkaffoldManifest(name, projectName, folder string, firstCommand, secondCommand model.ActionCommand) error {
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

func (p *Pipeline) createDeliveryPipeline(pipelineName string, firstCommand, secondCommand model.ActionCommand) error {
	pipelineOp, err := p.client.CreateDeliveryPipeline(p.ctx, &deploypb.CreateDeliveryPipelineRequest{
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
		}
		return err
	}
	_, err = pipelineOp.Wait(p.ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Created delivery pipeline %s\n", pipelineName)
	return nil
}

func (p *Pipeline) waitForReleaseRender(pipelineName string, releaseId string) error {
	ctx, cancel := context.WithTimeout(p.ctx, 1*time.Minute)
	defer cancel()
	delay := 1
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for release to finish rendering: %w", ctx.Err())
		default:
			release, err := p.client.GetRelease(p.ctx, &deploypb.GetReleaseRequest{
				Name: fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s/releases/%s", p.projectId,
					p.location, pipelineName, releaseId),
			})
			if err != nil {
				return err
			}
			if release.GetRenderState() == deploypb.Release_RENDER_STATE_UNSPECIFIED || release.GetRenderState() == deploypb.Release_IN_PROGRESS {
				time.Sleep(time.Duration(delay) * time.Second)
				delay = util.MinInt(delay*2, 5)
				continue
			}
			if release.GetRenderState() != deploypb.Release_SUCCEEDED {
				return fmt.Errorf("release render failed: %s", release.GetRenderState())
			}
			return nil
		}
	}
}

func (p *Pipeline) waitForRollout(rolloutOp *deploy.CreateRolloutOperation, pipelineName string, stepType model.StepType, jobName string, executionName string, autoApprove bool) error {
	ctx, cancel := context.WithTimeout(p.ctx, 4*time.Hour)
	defer cancel()
	rollout, err := rolloutOp.Wait(ctx)
	if err != nil {
		return err
	}
	approved := false
	delay := 1
	var pipeChanges *model.TerraformChanges
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
				time.Sleep(time.Duration(delay) * time.Second)
				delay = util.MinInt(delay*2, 30)
				continue
			}
			if rollout.GetState() == deploypb.Rollout_PENDING_APPROVAL {
				if approved {
					time.Sleep(time.Duration(delay) * time.Second)
					delay = util.MinInt(delay*2, 30)
					continue
				}
				if executionName == "" {
					common.Logger.Println("Execution name not found, please approve manually")
					time.Sleep(time.Duration(delay) * time.Second)
					delay = util.MinInt(delay*2, 30)
					continue
				}
				pipeChanges, err = p.getChanges(pipelineName, pipeChanges, stepType, jobName, executionName)
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
				} else {
					common.Logger.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
				}
				time.Sleep(time.Duration(delay) * time.Second)
				delay = util.MinInt(delay*2, 30)
				continue
			}
			if rollout.GetState() != deploypb.Rollout_SUCCEEDED {
				return fmt.Errorf("rollout failed: %s", rollout.GetState())
			}
			return nil
		}
	}
}

func (p *Pipeline) getChanges(pipelineName string, pipeChanges *model.TerraformChanges, stepType model.StepType, jobName string, executionName string) (*model.TerraformChanges, error) {
	if pipeChanges != nil {
		return pipeChanges, nil
	}
	switch stepType {
	case model.StepTypeTerraformCustom:
		fallthrough
	case model.StepTypeTerraform:
		return p.getTerraformChanges(pipelineName, jobName, executionName)
	}
	return &model.TerraformChanges{}, nil
}

func (p *Pipeline) getTerraformChanges(pipelineName string, jobName string, executionName string) (*model.TerraformChanges, error) {
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

func (p *Pipeline) CreateAgentPipeline(_ string, pipelineName string, _ string, _ string) error {
	_, err := p.builder.executeJob(pipelineName, false)
	return err
}

func (p *Pipeline) UpdatePipeline(projectName string, stepName string, step model.Step, bucket string) error {
	var planCommand model.ActionCommand
	var applyCommand model.ActionCommand
	if step.Type == model.StepTypeArgoCD {
		planCommand = model.ArgoCDPlanCommand
		applyCommand = model.ArgoCDApplyCommand
	} else {
		planCommand = model.PlanCommand
		applyCommand = model.ApplyCommand
	}
	folder := fmt.Sprintf("%s/%s/%s/%s", tempFolder, bucket, stepName, step.Workspace)
	err := p.createSkaffoldManifest(projectName, projectName, folder, planCommand, applyCommand)
	if err != nil {
		return err
	}
	tarContent, err := util.TarGzWrite(folder)
	if err != nil {
		return err
	}
	err = p.storage.PutFile(fmt.Sprintf(bucketFileFormat, stepName, step.Workspace), tarContent)
	if err != nil {
		return err
	}
	return nil
}

func (p *Pipeline) StartPipelineExecution(pipelineName string, stepName string, step model.Step, bucket string) (*string, error) {
	common.Logger.Printf("Starting pipeline %s\n", pipelineName)
	prefix := pipelineName
	if len(prefix) > 26 { // Max length for id is 63, uuid v4 is 36 chars plus hyphen, 63 - 37 = 26
		prefix = prefix[:26]
	}
	releaseId := fmt.Sprintf("%s-%s", prefix, uuid.New().String())
	releaseOp, err := p.client.CreateRelease(p.ctx, &deploypb.CreateReleaseRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s", p.projectId, p.location, pipelineName),
		ReleaseId: releaseId,
		Release: &deploypb.Release{
			SkaffoldConfigUri:  fmt.Sprintf("gs://%s/%s", bucket, fmt.Sprintf(bucketFileFormat, stepName, step.Workspace)),
			SkaffoldConfigPath: fmt.Sprintf("%s/%s.yaml", step.Workspace, pipelineName),
		},
	})
	if err != nil {
		return nil, err
	}
	_, err = releaseOp.Wait(p.ctx) // Wait for release creation, otherwise wait loop will fail
	return &releaseId, err
}

func (p *Pipeline) StartAgentExecution(pipelineName string) error {
	_, err := p.builder.executeJob(pipelineName, false)
	return err
}

func (p *Pipeline) WaitPipelineExecution(pipelineName string, projectName string, releaseId *string, autoApprove bool, stepType model.StepType) error {
	if releaseId == nil {
		return fmt.Errorf("release id is nil")
	}
	common.Logger.Printf("Waiting for pipeline %s to complete\n", pipelineName)
	err := p.waitForReleaseRender(pipelineName, *releaseId)
	if err != nil {
		return err
	}
	rolloutId := fmt.Sprintf("%s-rollout-plan", pipelineName)
	rollout, err := p.client.CreateRollout(p.ctx, &deploypb.CreateRolloutRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s/releases/%s", p.projectId, p.location, pipelineName, *releaseId),
		RolloutId: rolloutId,
		Rollout: &deploypb.Rollout{
			TargetId: fmt.Sprintf("%s-plan", p.cloudPrefix),
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Waiting for pipeline %s rollout %s to finish\n", pipelineName, rolloutId)
	err = p.waitForRollout(rollout, pipelineName, stepType, "", "", autoApprove)
	if err != nil {
		return err
	}
	var planCommand model.ActionCommand
	var applyCommand model.ActionCommand
	if stepType == model.StepTypeArgoCD {
		planCommand = model.ArgoCDPlanCommand
		applyCommand = model.ArgoCDApplyCommand
	} else {
		planCommand = model.PlanCommand
		applyCommand = model.ApplyCommand
	}
	planJob := fmt.Sprintf("%s-%s", projectName, planCommand)
	executionName, err := p.builder.executeJob(planJob, true)
	if err != nil {
		return err
	}
	rolloutId = fmt.Sprintf("%s-rollout-apply", pipelineName)
	rollout, err = p.client.CreateRollout(p.ctx, &deploypb.CreateRolloutRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s/releases/%s", p.projectId, p.location, pipelineName, *releaseId),
		RolloutId: rolloutId,
		Rollout: &deploypb.Rollout{
			TargetId: fmt.Sprintf("%s-apply", p.cloudPrefix),
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Waiting for pipeline %s rollout %s to finish\n", pipelineName, rolloutId)
	err = p.waitForRollout(rollout, pipelineName, stepType, planJob, executionName, autoApprove)
	if err != nil {
		return err
	}
	_, err = p.builder.executeJob(fmt.Sprintf("%s-%s", projectName, applyCommand), true)
	return err
}

func (p *Pipeline) StartDestroyExecution(projectName string) error {
	common.Logger.Printf("Starting destroy execution for pipeline %s\n", projectName)
	_, err := p.builder.executeJob(fmt.Sprintf("%s-plan-destroy", projectName), true)
	if err != nil {
		return err
	}
	_, err = p.builder.executeJob(fmt.Sprintf("%s-apply-destroy", projectName), true)
	if err != nil {
		return err
	}
	common.Logger.Printf("Successfully executed destroy pipeline %s\n", projectName)
	return nil
}
