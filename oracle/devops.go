package oracle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/devops"
)

const approvalStageName = "manual-approval"

var ErrApprovalRejected = errors.New("approval was rejected")

// Gate is the manual-approval gate for cloud pipeline executions. Each step gets
// its own DevOps deployment pipeline with a single Manual Approval stage — OCI
// allows only one running deployment per pipeline, so parallel steps can't share
// one. Plan/apply run as build runs (DevOpsBuilder); only the approval is a
// deployment.
type Gate struct {
	ctx         context.Context
	client      devops.DevopsClient
	cloudPrefix string
	projectId   string
	mu          sync.Mutex
	pipelines   map[string]approvalPipeline // step pipeline name → deployment pipeline
}

type approvalPipeline struct {
	id      string
	stageId string
}

// Approval is a deployment paused at its pipeline's approval stage.
type Approval struct {
	DeploymentId string
	stageId      string
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
		pipelines:   map[string]approvalPipeline{},
	}, nil
}

// UseProject points the gate at the DevOps project (owned by DevOpsBuilder) that
// hosts its approval pipelines. Must be called before Ensure.
func (g *Gate) UseProject(projectId string) { g.projectId = projectId }

func (g *Gate) Ensure() error {
	if g.projectId == "" {
		return fmt.Errorf("approval gate has no DevOps project; call UseProject first")
	}
	return nil
}

// RequestApproval creates a deployment that pauses at the step pipeline's approval
// stage. The summary becomes the deployment display name so the approver sees what
// they are deciding on in the console list.
func (g *Gate) RequestApproval(pipelineName, summary string) (Approval, error) {
	pipeline, err := g.ensureStepPipeline(pipelineName)
	if err != nil {
		return Approval{}, err
	}
	if len(summary) > 100 { // deployment display name limit
		summary = summary[:100]
	}
	response, err := g.client.CreateDeployment(g.ctx, devops.CreateDeploymentRequest{
		CreateDeploymentDetails: devops.CreateDeployPipelineDeploymentDetails{
			DeployPipelineId: &pipeline.id,
			DisplayName:      &summary,
			FreeformTags:     map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return Approval{}, fmt.Errorf("failed to create approval deployment: %w", err)
	}
	return Approval{DeploymentId: *response.GetId(), stageId: pipeline.stageId}, nil
}

func (g *Gate) Decide(approval Approval, approve bool) error {
	action := devops.ApproveDeploymentDetailsActionApprove
	reason := "Approved through Entigo portal"
	if !approve {
		action = devops.ApproveDeploymentDetailsActionReject
		reason = "Rejected through Entigo portal"
	}
	_, err := g.client.ApproveDeployment(g.ctx, devops.ApproveDeploymentRequest{
		DeploymentId:  &approval.DeploymentId,
		DeployStageId: &approval.stageId,
		Action:        action,
		Reason:        &reason,
	})
	if err != nil {
		return fmt.Errorf("failed to decide approval deployment %s: %w", approval.DeploymentId, err)
	}
	return nil
}

// ensureStepPipeline gets or creates the step's approval pipeline; the id cache is
// mutex-guarded because steps run in parallel goroutines.
func (g *Gate) ensureStepPipeline(pipelineName string) (approvalPipeline, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if pipeline, ok := g.pipelines[pipelineName]; ok {
		return pipeline, nil
	}
	if g.projectId == "" {
		return approvalPipeline{}, fmt.Errorf("approval gate is not initialized")
	}
	pipelineId, err := g.ensurePipeline(g.projectId, pipelineName)
	if err != nil {
		return approvalPipeline{}, err
	}
	stageId, err := g.ensureApprovalStage(pipelineId)
	if err != nil {
		return approvalPipeline{}, err
	}
	pipeline := approvalPipeline{id: pipelineId, stageId: stageId}
	g.pipelines[pipelineName] = pipeline
	return pipeline, nil
}

// WaitForApproval blocks until the deployment resolves. Approval → nil;
// rejection (ErrApprovalRejected) or cancellation → error, which fails the step
// and halts dependents.
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
			if rejected(response.Deployment) {
				return fmt.Errorf("approval deployment %s: %w", deploymentId, ErrApprovalRejected)
			}
			return fmt.Errorf("approval deployment %s was canceled or failed", deploymentId)
		}
		if err := util.Sleep(g.ctx, pollInterval); err != nil {
			return err
		}
	}
}

func rejected(deployment devops.Deployment) bool {
	progress := deployment.GetDeploymentExecutionProgress()
	if progress == nil {
		return false
	}
	for _, stage := range progress.DeployStageExecutionProgress {
		approval, ok := stage.(devops.ManualApprovalDeployStageExecutionProgress)
		if !ok {
			continue
		}
		for _, action := range approval.ApprovalActions {
			if action.Action == devops.ApprovalActionActionReject {
				return true
			}
		}
	}
	return false
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
		ProjectId:   &projectId,
		DisplayName: &displayName,
		Description: &description,
		FreeformTags: map[string]string{
			model.ResourceTagKey: model.ResourceTagValue,
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create deploy pipeline %s: %w", displayName, err)
	}
	return *created.Id, nil
}

func (g *Gate) ensureApprovalStage(pipelineId string) (string, error) {
	displayName := approvalStageName
	list, err := g.client.ListDeployStages(g.ctx, devops.ListDeployStagesRequest{
		DeployPipelineId: &pipelineId,
		DisplayName:      &displayName,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list deploy stages: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].GetId(), nil
	}
	approvals := 1
	created, err := g.client.CreateDeployStage(g.ctx, devops.CreateDeployStageRequest{
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
		return "", fmt.Errorf("failed to create manual approval stage: %w", err)
	}
	return *created.GetId(), nil
}
