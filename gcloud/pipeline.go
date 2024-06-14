package gcloud

import (
	deploy "cloud.google.com/go/deploy/apiv1"
	"cloud.google.com/go/deploy/apiv1/deploypb"
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
	"os"
	k8syaml "sigs.k8s.io/yaml"
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
}

func NewPipeline(ctx context.Context, projectId string, location string, prefix string, serviceAccount string, storage *GStorage, bucket string) (model.Pipeline, error) {
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
	}, nil
}

func createTargets(ctx context.Context, client *deploy.CloudDeployClient, projectId, location, prefix, serviceAccount string) error {
	// TODO Does using shared targets cause queueing issues?
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
	skaffoldManifest := skaffold{
		APIVersion: "skaffold/v4beta7",
		Kind:       "Config",
		Metadata:   metadata{Name: pipelineName},
		Deploy:     runDeploy{},
		Profiles: []profile{
			{Name: "plan", Manifests: manifests{RawYaml: []string{fmt.Sprintf("%s-%s.yaml", projectName, model.PlanCommand)}}},
			{Name: "apply", Manifests: manifests{RawYaml: []string{fmt.Sprintf("%s-%s.yaml", projectName, model.ApplyCommand)}}},
		},
	}
	bytes, err := k8syaml.Marshal(skaffoldManifest)
	if err != nil {
		return nil, err
	}
	folder := fmt.Sprintf("%s/%s/%s/%s", tempFolder, p.bucket, stepName, step.Workspace)
	err = os.WriteFile(fmt.Sprintf("%s/%s.yaml", folder, pipelineName), bytes, 0644)
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
	_, err = p.client.CreateDeliveryPipeline(p.ctx, &deploypb.CreateDeliveryPipelineRequest{
		Parent:             fmt.Sprintf("projects/%s/locations/%s", p.projectId, p.location),
		DeliveryPipelineId: pipelineName,
		DeliveryPipeline: &deploypb.DeliveryPipeline{
			Pipeline: &deploypb.DeliveryPipeline_SerialPipeline{
				SerialPipeline: &deploypb.SerialPipeline{
					Stages: []*deploypb.Stage{
						{
							TargetId: fmt.Sprintf("%s-plan", p.cloudPrefix),
							Profiles: []string{"plan"},
						},
						{
							TargetId: fmt.Sprintf("%s-apply", p.cloudPrefix),
							Profiles: []string{"apply"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		var apiError *apierror.APIError
		if !errors.As(err, &apiError) || apiError.GRPCStatus().Code() != codes.AlreadyExists {
			return nil, err
		}
	}
	releaseId := fmt.Sprintf("%s-release-9", pipelineName)
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
	// TODO Wait and check until release is not in progress anymore
	for {
		wait, err := release.Poll(p.ctx)
		if err != nil {
			return nil, err
		}
		if wait.GetRenderState() == deploypb.Release_RENDER_STATE_UNSPECIFIED || wait.GetRenderState() == deploypb.Release_IN_PROGRESS {
			continue
		}
		if wait.GetRenderState() != deploypb.Release_SUCCEEDED {
			return nil, fmt.Errorf("release render failed: %s", wait.GetRenderState())
		}
		break
	}
	rolloutId := fmt.Sprintf("%s-rollout", pipelineName)
	_, err = p.client.CreateRollout(p.ctx, &deploypb.CreateRolloutRequest{
		Parent:    fmt.Sprintf("projects/%s/locations/%s/deliveryPipelines/%s/releases/%s", p.projectId, p.location, pipelineName, releaseId),
		RolloutId: rolloutId,
		Rollout: &deploypb.Rollout{
			TargetId: fmt.Sprintf("%s-plan", p.cloudPrefix),
		},
	})
	if err != nil {
		return nil, err
	}
	return &rolloutId, nil
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
