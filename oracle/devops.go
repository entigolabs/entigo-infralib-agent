package oracle

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/devops"
	"github.com/oracle/oci-go-sdk/v65/ons"
)

const approvalStageName = "manual-approval"

// Gate is the durable manual-approval gate for cloud pipeline executions. Each
// step gets its own DevOps deployment pipeline containing a single Manual
// Approval stage — OCI allows only one running deployment per pipeline, and a
// pending approval can stay open for hours, so parallel steps must not share a
// pipeline. When a step's plan needs human sign-off, the agent creates a
// deployment on the step's pipeline and blocks until an IAM-authorized user
// approves or rejects it in the OCI Console. Plan/apply themselves run as
// Container Instances (DevOps shell stages cannot run custom images), so only
// the approval decision lives in DevOps.
type Gate struct {
	ctx           context.Context
	client        devops.DevopsClient
	onsClient     ons.NotificationControlPlaneClient
	compartmentId string
	cloudPrefix   string
	projectId     string
	mu            sync.Mutex
	pipelines     map[string]string // step pipeline name → pipeline OCID
}

func NewGate(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, cloudPrefix string) (*Gate, error) {
	client, err := devops.NewDevopsClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	onsClient, err := ons.NewNotificationControlPlaneClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
		onsClient.SetRegion(region)
	}
	return &Gate{
		ctx:           ctx,
		client:        client,
		onsClient:     onsClient,
		compartmentId: compartmentId,
		cloudPrefix:   cloudPrefix,
		pipelines:     map[string]string{},
	}, nil
}

// Ensure gets or creates the shared topic → project chain. Per-step pipelines
// are created lazily by RequestApproval.
func (g *Gate) Ensure() error {
	topicId, err := g.ensureTopic(fmt.Sprintf("%s-approvals", g.cloudPrefix))
	if err != nil {
		return err
	}
	projectId, err := g.ensureProject(fmt.Sprintf("%s-approval", g.cloudPrefix), topicId)
	if err != nil {
		return err
	}
	g.projectId = projectId
	return nil
}

// RequestApproval creates a deployment that pauses at the approval stage of the
// step's own pipeline. The summary (step name + plan changes) becomes the
// deployment display name so the approver sees what they are deciding on in the
// console list.
func (g *Gate) RequestApproval(pipelineName, summary string) (string, error) {
	pipelineId, err := g.ensureStepPipeline(pipelineName)
	if err != nil {
		return "", err
	}
	if len(summary) > 100 { // deployment display name limit
		summary = summary[:100]
	}
	response, err := g.client.CreateDeployment(g.ctx, devops.CreateDeploymentRequest{
		CreateDeploymentDetails: devops.CreateDeployPipelineDeploymentDetails{
			DeployPipelineId: &pipelineId,
			DisplayName:      &summary,
			FreeformTags:     map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create approval deployment: %w", err)
	}
	return *response.GetId(), nil
}

// ensureStepPipeline gets or creates the step's approval pipeline. Steps run in
// parallel goroutines, so the id cache is mutex-guarded; the whole get-or-create
// is serialized, which is fine for a per-step one-time setup call.
func (g *Gate) ensureStepPipeline(pipelineName string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if pipelineId, ok := g.pipelines[pipelineName]; ok {
		return pipelineId, nil
	}
	if g.projectId == "" {
		return "", fmt.Errorf("approval gate is not initialized")
	}
	pipelineId, err := g.ensurePipeline(g.projectId, pipelineName)
	if err != nil {
		return "", err
	}
	if err = g.ensureApprovalStage(pipelineId); err != nil {
		return "", err
	}
	g.pipelines[pipelineName] = pipelineId
	return pipelineId, nil
}

// WaitForApproval blocks until the deployment resolves. Approval → nil;
// rejection or cancellation → error, which fails the step and halts dependents.
func (g *Gate) WaitForApproval(deploymentId string) error {
	log.Printf("Waiting for manual approval of deployment %s in the OCI Console (DevOps project %s-approval)\n",
		deploymentId, g.cloudPrefix)
	for {
		response, err := g.client.GetDeployment(g.ctx, devops.GetDeploymentRequest{DeploymentId: &deploymentId})
		if err != nil {
			return fmt.Errorf("failed to get approval deployment %s: %w", deploymentId, err)
		}
		switch response.GetLifecycleState() {
		case devops.DeploymentLifecycleStateSucceeded:
			return nil
		case devops.DeploymentLifecycleStateFailed, devops.DeploymentLifecycleStateCanceled:
			return fmt.Errorf("approval deployment %s was rejected or canceled", deploymentId)
		}
		select {
		case <-g.ctx.Done():
			return g.ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (g *Gate) ensureTopic(name string) (string, error) {
	list, err := g.onsClient.ListTopics(g.ctx, ons.ListTopicsRequest{
		CompartmentId: &g.compartmentId,
		Name:          &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list notification topics: %w", err)
	}
	for _, topic := range list.Items {
		if topic.LifecycleState == ons.NotificationTopicSummaryLifecycleStateActive {
			return *topic.TopicId, nil
		}
	}
	description := "Entigo infralib approval notifications"
	created, err := g.onsClient.CreateTopic(g.ctx, ons.CreateTopicRequest{
		CreateTopicDetails: ons.CreateTopicDetails{
			Name:          &name,
			CompartmentId: &g.compartmentId,
			Description:   &description,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create notification topic %s: %w", name, err)
	}
	return *created.TopicId, nil
}

func (g *Gate) ensureProject(name, topicId string) (string, error) {
	list, err := g.client.ListProjects(g.ctx, devops.ListProjectsRequest{
		CompartmentId: &g.compartmentId,
		Name:          &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list devops projects: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, nil
	}
	description := "Entigo infralib manual approval gate"
	created, err := g.client.CreateProject(g.ctx, devops.CreateProjectRequest{
		CreateProjectDetails: devops.CreateProjectDetails{
			Name:               &name,
			CompartmentId:      &g.compartmentId,
			Description:        &description,
			NotificationConfig: &devops.NotificationConfig{TopicId: &topicId},
			FreeformTags:       map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create devops project %s: %w", name, err)
	}
	return *created.Id, nil
}

func (g *Gate) ensurePipeline(projectId, displayName string) (string, error) {
	list, err := g.client.ListDeployPipelines(g.ctx, devops.ListDeployPipelinesRequest{
		ProjectId:   &projectId,
		DisplayName: &displayName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list deploy pipelines: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, nil
	}
	description := "Pauses at a manual approval stage; approving applies the step's planned changes"
	created, err := g.client.CreateDeployPipeline(g.ctx, devops.CreateDeployPipelineRequest{
		CreateDeployPipelineDetails: devops.CreateDeployPipelineDetails{
			ProjectId:   &projectId,
			DisplayName: &displayName,
			Description: &description,
			FreeformTags: map[string]string{
				model.ResourceTagKey: model.ResourceTagValue,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create deploy pipeline %s: %w", displayName, err)
	}
	return *created.Id, nil
}

func (g *Gate) ensureApprovalStage(pipelineId string) error {
	displayName := approvalStageName
	list, err := g.client.ListDeployStages(g.ctx, devops.ListDeployStagesRequest{
		DeployPipelineId: &pipelineId,
		DisplayName:      &displayName,
	})
	if err != nil {
		return fmt.Errorf("failed to list deploy stages: %w", err)
	}
	if len(list.Items) > 0 {
		return nil
	}
	approvals := 1
	_, err = g.client.CreateDeployStage(g.ctx, devops.CreateDeployStageRequest{
		CreateDeployStageDetails: devops.CreateManualApprovalDeployStageDetails{
			DeployPipelineId: &pipelineId,
			DisplayName:      &displayName,
			// The first stage's predecessor is the pipeline itself.
			DeployStagePredecessorCollection: &devops.DeployStagePredecessorCollection{
				Items: []devops.DeployStagePredecessor{{Id: &pipelineId}},
			},
			ApprovalPolicy: devops.CountBasedApprovalPolicy{NumberOfApprovalsRequired: &approvals},
			FreeformTags:   map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create manual approval stage: %w", err)
	}
	return nil
}
