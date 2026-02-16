package gcloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"time"

	deploy "cloud.google.com/go/deploy/apiv1"
	"cloud.google.com/go/deploy/apiv1/deploypb"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/google/uuid"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"gopkg.in/yaml.v3"
)

const (
	pollingDelay     = 10
	bucketFileFormat = "%s.tar.gz"
	linkFormat       = "https://console.cloud.google.com/deploy/delivery-pipelines/%s/%s?project=%s"
)

type skaffold struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Metadata   metadata  `yaml:"metadata"`
	Deploy     runDeploy `yaml:"deploy"`
	Profiles   []profile `yaml:"profiles"`
}

type metadata struct {
	Name string `yaml:"name"`
}

type runDeploy struct {
	Cloudrun struct{} `yaml:"cloudrun"`
}

type profile struct {
	Name      string    `yaml:"name"`
	Manifests manifests `yaml:"manifests"`
}

type manifests struct {
	RawYaml []string `yaml:"rawYaml"`
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
	manager        model.NotificationManager
}

func NewPipeline(ctx context.Context, options []option.ClientOption, projectId string, location string, prefix string, serviceAccount string, storage *GStorage, builder *Builder, logging *Logging, manager model.NotificationManager) (*Pipeline, error) {
	client, err := deploy.NewCloudDeployClient(ctx, options...)
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
		manager:        manager,
	}, nil
}

func createTargets(ctx context.Context, client *deploy.CloudDeployClient, projectId, location, prefix, serviceAccount string) error {
	collection := fmt.Sprintf("projects/%s/locations/%s", projectId, location)
	err := createTarget(ctx, client, collection, fmt.Sprintf("%s-plan", prefix), serviceAccount, false)
	if err != nil {
		return err
	}
	return createTarget(ctx, client, collection, fmt.Sprintf("%s-apply", prefix), serviceAccount, true)
}

func createTarget(ctx context.Context, client *deploy.CloudDeployClient, collection, name, serviceAccount string, requireApproval bool) error {
	target, err := client.GetTarget(ctx, &deploypb.GetTargetRequest{Name: fmt.Sprintf("%s/targets/%s", collection, name)})
	if err == nil {
		if target.GetRequireApproval() == requireApproval {
			return nil
		}
		updateTargetOp, err := client.UpdateTarget(ctx, &deploypb.UpdateTargetRequest{
			UpdateMask: &fieldmaskpb.FieldMask{
				Paths: []string{"require_approval"},
			},
			Target: &deploypb.Target{
				Name:            target.GetName(),
				RequireApproval: requireApproval,
			},
		})
		if err != nil {
			return err
		}
		_, err = updateTargetOp.Wait(ctx)
		return err
	}
	var apiError *apierror.APIError
	if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.NotFound {
		return err
	}
	createOp, err := client.CreateTarget(ctx, &deploypb.CreateTargetRequest{
		Parent:   collection,
		TargetId: name,
		Target: &deploypb.Target{
			DeploymentTarget: &deploypb.Target_Run{
				Run: &deploypb.CloudRunLocation{
					Location: collection,
				},
			},
			RequireApproval: requireApproval,
			ExecutionConfigs: []*deploypb.ExecutionConfig{{
				Usages: []deploypb.ExecutionConfig_ExecutionEnvironmentUsage{
					deploypb.ExecutionConfig_RENDER,
					deploypb.ExecutionConfig_DEPLOY,
				},
				ExecutionTimeout: &durationpb.Duration{Seconds: 86400},
				ServiceAccount:   serviceAccount,
			}},
			Labels: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		var apiError *apierror.APIError
		if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.AlreadyExists {
			return err
		}
	}
	_, err = createOp.Wait(ctx)
	return err
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
	log.Printf("Deleted target %s-plan\n", p.cloudPrefix)
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
	log.Printf("Deleted target %s-apply\n", p.cloudPrefix)
	return nil
}

func (p *Pipeline) CreatePipeline(projectName, stepName string, step model.Step, bucket model.Bucket, _ map[string]model.SourceAuth) (*string, error) {
	planCommand, applyCommand := model.GetCommands(step.Type)
	bucketMeta, err := bucket.GetRepoMetadata()
	if err != nil {
		return nil, err
	}
	folder := fmt.Sprintf("%s/%s/%s", tempFolder, bucketMeta.Name, stepName)
	err = p.createSkaffoldManifest(projectName, projectName, folder, planCommand, applyCommand)
	if err != nil {
		return nil, err
	}
	tarContent, err := util.TarGzWrite(folder)
	if err != nil {
		return nil, err
	}
	err = bucket.PutFile(fmt.Sprintf(bucketFileFormat, stepName), tarContent)
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
	log.Printf("Deleted delivery pipeline %s\n", projectName)
	return nil
}

func (p *Pipeline) createSkaffoldManifest(name, projectName, folder string, firstCommand, secondCommand model.ActionCommand) error {
	skaffoldManifest := skaffold{
		APIVersion: "skaffold/v4beta11",
		Kind:       "Steps",
		Metadata:   metadata{Name: name},
		Deploy:     runDeploy{},
		Profiles: []profile{
			{Name: string(firstCommand), Manifests: manifests{RawYaml: []string{fmt.Sprintf("%s-%s.yaml", projectName, firstCommand)}}},
			{Name: string(secondCommand), Manifests: manifests{RawYaml: []string{fmt.Sprintf("%s-%s.yaml", projectName, secondCommand)}}},
		},
	}
	bytes, err := yaml.Marshal(skaffoldManifest)
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
			Labels: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
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
	log.Printf("Created delivery pipeline %s\n", pipelineName)
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

func (p *Pipeline) getLink(pipelineName string) string {
	return fmt.Sprintf(linkFormat, p.location, pipelineName, p.projectId)
}

func (p *Pipeline) getChanges(pipelineName string, pipeChanges *model.PipelineChanges, stepType model.StepType, jobName string, executionName string) (*model.PipelineChanges, error) {
	if pipeChanges != nil {
		return pipeChanges, nil
	}
	switch stepType {
	case model.StepTypeTerraform:
		return p.getPipelineChanges(pipelineName, jobName, executionName, terraform.ParseLogChanges)
	case model.StepTypeArgoCD:
		return p.getPipelineChanges(pipelineName, jobName, executionName, argocd.ParseLogChanges)
	}
	return &model.PipelineChanges{}, nil
}

func (p *Pipeline) getPipelineChanges(pipelineName string, jobName string, executionName string, logParser func(string, string) (*model.PipelineChanges, error)) (*model.PipelineChanges, error) {
	lastSlash := strings.LastIndex(executionName, "/")
	logIterator := p.logging.GetJobExecutionLogs(jobName, executionName[lastSlash+1:], p.location)
	for {
		logRow, err := p.logging.GetLogRow(logIterator)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		changes, err := logParser(pipelineName, logRow)
		if err != nil {
			return nil, err
		}
		if changes != nil {
			return changes, nil
		}
	}
	return nil, fmt.Errorf("couldn't find plan output from logs for %s", pipelineName)
}

func (p *Pipeline) CreateAgentPipelines(_ string, pipelineName string, _ string, run bool) error {
	if !run {
		return nil
	}
	job := fmt.Sprintf("%s-%s", pipelineName, common.RunCommand)
	_, err := p.builder.executeJob(job, false)
	return err
}

func (p *Pipeline) UpdatePipeline(projectName string, stepName string, step model.Step, bucket string, _ map[string]model.SourceAuth) error {
	planCommand, applyCommand := model.GetCommands(step.Type)
	folder := fmt.Sprintf("%s/%s/%s", tempFolder, bucket, stepName)
	err := p.createSkaffoldManifest(projectName, projectName, folder, planCommand, applyCommand)
	if err != nil {
		return err
	}
	tarContent, err := util.TarGzWrite(folder)
	if err != nil {
		return err
	}
	err = p.storage.PutFile(fmt.Sprintf(bucketFileFormat, stepName), tarContent)
	if err != nil {
		return err
	}
	return nil
}

func (p *Pipeline) StartPipelineExecution(pipelineName string, stepName string, _ model.Step, bucket string) (*string, error) {
	log.Printf("Starting pipeline %s\n", pipelineName)
	prefix := pipelineName
	if len(prefix) > 26 { // Max length for id is 63, uuid v4 is 36 chars plus hyphen, 63 - 37 = 26
		prefix = prefix[:26]
	}
	releaseId := fmt.Sprintf("%s-%s", prefix, uuid.New().String())
	releaseOp, err := p.client.CreateRelease(p.ctx, &deploypb.CreateReleaseRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s", p.projectId, p.location, pipelineName),
		ReleaseId: releaseId,
		Release: &deploypb.Release{
			SkaffoldConfigUri:  fmt.Sprintf("gs://%s/%s", bucket, fmt.Sprintf(bucketFileFormat, stepName)),
			SkaffoldConfigPath: fmt.Sprintf("%s/%s.yaml", stepName, pipelineName),
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

func (p *Pipeline) WaitPipelineExecution(pipelineName string, projectName string, releaseId *string, autoApprove bool, step model.Step, approve model.ManualApprove) error {
	if releaseId == nil {
		return fmt.Errorf("release id is nil")
	}
	log.Printf("Waiting for pipeline %s to complete\n", pipelineName)
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
	slog.Debug(fmt.Sprintf("Waiting for pipeline %s rollout %s to finish\n", pipelineName, rolloutId))
	err = p.waitForPlanRollout(rollout)
	if err != nil {
		return err
	}
	planCommand, applyCommand := model.GetCommands(step.Type)
	planJob := fmt.Sprintf("%s-%s", projectName, planCommand)
	executionName, err := p.builder.executeJob(planJob, true)
	if err != nil {
		return err
	}
	pipeChanges, err := p.getChanges(pipelineName, nil, step.Type, planJob, executionName)
	if err != nil {
		return err
	}
	if pipeChanges != nil && util.ShouldStopPipeline(*pipeChanges, step.Approve, approve) {
		log.Printf("Stopping pipeline %s\n", pipelineName)
		_, err = p.client.AbandonRelease(p.ctx, &deploypb.AbandonReleaseRequest{
			Name: fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s/releases/%s", p.projectId,
				p.location, pipelineName, *releaseId),
		})
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Couldn't stop pipeline %s, please stop manually: %s", pipelineName, err.Error())))
		}
		if step.Approve == model.ApproveReject || approve == model.ManualApproveReject {
			return fmt.Errorf("stopped because step approve type is 'reject'")
		}
		return nil
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
	slog.Debug(fmt.Sprintf("Waiting for pipeline %s rollout %s to finish\n", pipelineName, rolloutId))
	err = p.waitForApplyRollout(rollout, pipelineName, step, executionName, autoApprove, pipeChanges, approve)
	if err != nil {
		return err
	}
	_, err = p.builder.executeJob(fmt.Sprintf("%s-%s", projectName, applyCommand), true)
	return err
}

func (p *Pipeline) StartDestroyExecution(projectName string, _ model.Step) error {
	_, err := p.builder.executeJob(fmt.Sprintf("%s-plan-destroy", projectName), true)
	if err != nil {
		return err
	}
	_, err = p.builder.executeJob(fmt.Sprintf("%s-apply-destroy", projectName), true)
	return err
}

func (p *Pipeline) waitForPlanRollout(rolloutOp *deploy.CreateRolloutOperation) error {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Minute)
	defer cancel()
	rollout, err := rolloutOp.Wait(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			_, _ = p.client.CancelRollout(ctx, &deploypb.CancelRolloutRequest{
				Name: rollout.GetName(),
			})
			return fmt.Errorf("timed out waiting for rollout to finish: %w", ctx.Err())
		default:
			rollout, err = p.client.GetRollout(ctx, &deploypb.GetRolloutRequest{
				Name: rollout.GetName(),
			})
			if err != nil {
				return err
			}
			switch rollout.GetState() {
			case deploypb.Rollout_PENDING_APPROVAL:
				return fmt.Errorf("plan rollout shouldn't have approval")
			case deploypb.Rollout_STATE_UNSPECIFIED, deploypb.Rollout_IN_PROGRESS:
				time.Sleep(pollingDelay * time.Second)
				continue
			case deploypb.Rollout_SUCCEEDED:
				return nil
			default:
				return fmt.Errorf("rollout failed: %s", rollout.GetState())
			}
		}
	}
}

func (p *Pipeline) waitForApplyRollout(rolloutOp *deploy.CreateRolloutOperation, pipelineName string, step model.Step, executionName string, autoApprove bool, pipeChanges *model.PipelineChanges, approve model.ManualApprove) error {
	ctx, cancel := context.WithTimeout(p.ctx, 1*time.Hour)
	defer cancel()
	rollout, err := rolloutOp.Wait(ctx)
	if err != nil {
		return err
	}
	approved := false
	notified := false
	for {
		select {
		case <-ctx.Done():
			_, _ = p.client.CancelRollout(ctx, &deploypb.CancelRolloutRequest{
				Name: rollout.GetName(),
			})
			return fmt.Errorf("timed out waiting for rollout to finish: %w", ctx.Err())
		default:
			rollout, err = p.client.GetRollout(p.ctx, &deploypb.GetRolloutRequest{
				Name: rollout.GetName(),
			})
			if err != nil {
				return err
			}
			if rollout.GetState() == deploypb.Rollout_PENDING_APPROVAL {
				if pipeChanges == nil {
					return fmt.Errorf("pipeline changes are nil, cannot approve rollout")
				}
				if approved {
					time.Sleep(pollingDelay * time.Second)
					continue
				}
				if executionName == "" {
					log.Println("Execution name not found, please approve manually")
				} else if util.ShouldApprovePipeline(*pipeChanges, step.Approve, autoApprove, approve) {
					_, err = p.client.ApproveRollout(p.ctx, &deploypb.ApproveRolloutRequest{
						Name:     rollout.GetName(),
						Approved: true,
					})
					if err != nil {
						log.Printf("Failed to approve rollout, please approve manually: %s", err)
					} else {
						log.Printf("Approved %s\n", pipelineName)
						approved = true
					}
				} else {
					log.Printf("Waiting for manual approval of pipeline %s\n", pipelineName)
					if !notified && p.manager != nil {
						p.manager.ManualApproval(pipelineName, *pipeChanges, p.getLink(pipelineName))
						notified = true
					}
				}
				time.Sleep(pollingDelay * time.Second)
				continue
			}
			if rollout.GetApprovalState() == deploypb.Rollout_APPROVED && notified {
				p.manager.Message(model.MessageTypeApprovals, fmt.Sprintf("Pipeline %s was approved", pipelineName),
					map[string]string{"pipeline": pipelineName, "step": step.Name})
				notified = false
			}
			if rollout.GetState() == deploypb.Rollout_STATE_UNSPECIFIED || rollout.GetState() == deploypb.Rollout_IN_PROGRESS {
				time.Sleep(pollingDelay * time.Second)
				continue
			}
			if rollout.GetState() != deploypb.Rollout_SUCCEEDED {
				return fmt.Errorf("rollout failed: %s", rollout.GetState())
			}
			return nil
		}
	}
}
