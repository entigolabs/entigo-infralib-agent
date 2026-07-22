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
)

const approvalStageName = "manual-approval"

// Gate is the durable manual-approval gate for cloud pipeline executions. Each
// step gets its own DevOps deployment pipeline containing a single Manual
// Approval stage — OCI allows only one running deployment per pipeline, and a
// pending approval can stay open for hours, so parallel steps must not share a
// pipeline. When a step's plan needs human sign-off, the agent creates a
// deployment on the step's pipeline and blocks until an IAM-authorized user
// approves or rejects it in the OCI Console. Plan/apply themselves run as DevOps
// build runs (see DevOpsBuilder); only the approval decision is a deployment.
type Gate struct {
	ctx         context.Context
	client      devops.DevopsClient
	cloudPrefix string
	projectId   string
	mu          sync.Mutex
	pipelines   map[string]string // step pipeline name → pipeline OCID
}

func NewGate(ctx context.Context, provider ocicommon.ConfigurationProvider, region, cloudPrefix string) (*Gate, error) {
	client, err := devops.NewDevopsClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
	}
	return &Gate{
		ctx:         ctx,
		client:      client,
		cloudPrefix: cloudPrefix,
		pipelines:   map[string]string{},
	}, nil
}

// UseProject points the gate at the DevOps project that hosts its approval
// pipelines. The project (shared <prefix>-infralib), its notification topic and
// the per-step approval pipelines are owned by DevOpsBuilder; the gate only
// creates deployments in them, so this must be called before Ensure.
func (g *Gate) UseProject(projectId string) { g.projectId = projectId }

// Ensure verifies the gate has been pointed at a DevOps project via UseProject.
// Per-step approval pipelines are created lazily by RequestApproval.
func (g *Gate) Ensure() error {
	if g.projectId == "" {
		return fmt.Errorf("approval gate has no DevOps project; call UseProject first")
	}
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
	log.Printf("Waiting for manual approval of deployment %s in the OCI Console (DevOps project %s-infralib)\n",
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
