package azure

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/generator"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const (
	approvalPending = "pending"
	approvalReject  = "reject"
	approvalTimeout = 60 * time.Minute
)

// Pipeline orchestrates a step's plan → approve → apply over job executions. A manual
// approval is given by starting the step's apply job in the Portal; setting the apply
// job's approval tag to reject rejects it. The apply job's execution tag names the plan
// execution that owns it, so a newer plan supersedes an older one waiting for approval.
type Pipeline struct {
	ctx     context.Context
	builder *Builder
	tags    *armresources.TagsClient
	bucket  model.Bucket
	manager model.NotificationManager
}

func NewPipeline(ctx context.Context, credential azcore.TokenCredential, subscriptionId string, builder *Builder, bucket model.Bucket, manager model.NotificationManager) (*Pipeline, error) {
	tags, err := armresources.NewTagsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, err
	}
	return &Pipeline{ctx: ctx, builder: builder, tags: tags, bucket: bucket, manager: manager}, nil
}

func (p *Pipeline) SetCampaignId(campaignId string) {
	p.builder.SetCampaignId(campaignId)
}

func (p *Pipeline) SetPipelineIndex(index int) {
	p.builder.SetPipelineIndex(index)
}

func (p *Pipeline) CreatePipeline(projectName, stepName string, step model.Step, _ model.Bucket, _ map[string]model.SourceAuth) (*string, error) {
	return p.StartPipelineExecution(projectName, stepName, step, "")
}

func (p *Pipeline) UpdatePipeline(_, _ string, _ model.Step, _ string, _ map[string]model.SourceAuth) error {
	return nil
}

func (p *Pipeline) StartPipelineExecution(pipelineName, _ string, step model.Step, _ string) (*string, error) {
	planCommand, _ := model.GetCommands(step.Type)
	execution, err := p.builder.startJob(jobName(pipelineName, planCommand))
	if err != nil {
		return nil, err
	}
	return &execution, nil
}

func (p *Pipeline) WaitPipelineExecution(pipelineName, projectName string, executionId *string, autoApprove bool, step model.Step, approve model.ManualApprove) error {
	if executionId == nil {
		return fmt.Errorf("no execution id for pipeline %s", pipelineName)
	}
	planCommand, applyCommand := model.GetCommands(step.Type)
	log.Printf("Waiting for %s of %s to complete\n", planCommand, pipelineName)
	if err := p.builder.waitForExecution(jobName(projectName, planCommand), *executionId); err != nil {
		return fmt.Errorf("plan failed for %s: %w", pipelineName, err)
	}
	applyJob := jobName(projectName, applyCommand)
	scope := p.builder.jobId(applyJob)
	if err := p.setTags(scope, map[string]string{executionTagKey: *executionId}); err != nil {
		return err
	}
	defer p.releaseTags(scope, *executionId)
	changes, err := p.planChanges(pipelineName, step)
	if err != nil {
		return err
	}
	if util.ShouldStopPipeline(*changes, step.Approve, approve) {
		if step.Approve == model.ApproveReject || approve == model.ManualApproveReject {
			return fmt.Errorf("stopped because step approve type is 'reject'")
		}
		log.Printf("No changes detected for %s, skipping apply\n", pipelineName)
		return nil
	}
	var execution string
	if util.ShouldApprovePipeline(*changes, step.Approve, autoApprove, approve) {
		execution, err = p.builder.startJob(applyJob)
	} else {
		execution, err = p.waitForManualApproval(pipelineName, applyJob, *executionId, step, changes)
	}
	if err != nil {
		return err
	}
	if err = p.builder.waitForExecution(applyJob, execution); err != nil {
		return fmt.Errorf("apply failed for %s: %w", pipelineName, err)
	}
	return nil
}

// waitForManualApproval returns the apply execution an approver started in the Portal.
// A Portal execution can't carry overrides, so the campaign is set on the job itself.
func (p *Pipeline) waitForManualApproval(pipelineName, applyJob, planExecution string, step model.Step, changes *model.PipelineChanges) (string, error) {
	scope := p.builder.jobId(applyJob)
	known, err := p.builder.executionNames(applyJob)
	if err != nil {
		return "", err
	}
	if overrides := p.builder.campaignOverrides(); overrides != nil {
		if err = p.builder.setJobEnv(applyJob, overrides); err != nil {
			return "", err
		}
		defer p.resetCampaignEnv(scope, applyJob, planExecution)
	}
	if err = p.setTags(scope, map[string]string{approvalTagKey: approvalPending}); err != nil {
		return "", err
	}
	link := p.builder.jobLink(applyJob)
	if p.manager != nil {
		p.manager.ManualApproval(pipelineName, step.Name, *changes, link)
	}
	log.Printf("Waiting for manual approval of %s: run job %s in the Azure Portal to apply, or set its tag %s=%s to reject. %s\n",
		pipelineName, applyJob, approvalTagKey, approvalReject, link)
	deadline := time.Now().Add(approvalTimeout)
	for {
		tags, err := p.getTags(scope)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to read approval tags of %s: %s", applyJob, err)))
		} else {
			if owner := tags[executionTagKey]; owner != "" && owner != planExecution {
				return "", fmt.Errorf("manual approval for %s was superseded by a newer execution", pipelineName)
			}
			if tags[approvalTagKey] == approvalReject {
				return "", fmt.Errorf("manual approval for %s was rejected", pipelineName)
			}
		}
		execution, err := p.builder.newExecution(applyJob, known)
		if err != nil {
			return "", err
		}
		if execution != "" {
			log.Printf("Approved %s, apply execution %s started\n", pipelineName, execution)
			if p.manager != nil {
				p.manager.Approval(pipelineName, step.Name, "")
			}
			return execution, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("manual approval for %s timed out after %s", pipelineName, approvalTimeout)
		}
		if err = sleep(p.ctx, executionPoll); err != nil {
			return "", err
		}
	}
}

// resetCampaignEnv leaves the campaign on the job when a newer execution already owns it.
func (p *Pipeline) resetCampaignEnv(scope, applyJob, planExecution string) {
	tags, err := p.getTags(scope)
	if err != nil || tags[executionTagKey] != planExecution {
		return
	}
	err = p.builder.setJobEnv(applyJob, map[string]string{"CAMPAIGN_ID": model.CampaignSentinelNone, "PIPELINE_INDEX": "0"})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to reset campaign of %s: %s", applyJob, err)))
	}
}

func (p *Pipeline) setTags(scope string, values map[string]string) error {
	tags := map[string]*string{}
	for key, value := range values {
		tags[key] = new(value)
	}
	_, err := p.tags.UpdateAtScope(p.ctx, scope, armresources.TagsPatchResource{
		Operation:  new(armresources.TagsPatchOperationMerge),
		Properties: &armresources.Tags{Tags: tags},
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to set approval tags on %s: %w", scope, err)
	}
	return nil
}

// releaseTags removes the approval tags unless a newer execution owns the job.
func (p *Pipeline) releaseTags(scope, planExecution string) {
	tags, err := p.getTags(scope)
	if err != nil || tags[executionTagKey] != planExecution {
		return
	}
	remove := map[string]*string{}
	for _, key := range []string{executionTagKey, approvalTagKey} {
		if value, ok := tags[key]; ok {
			remove[key] = new(value)
		}
	}
	if len(remove) == 0 {
		return
	}
	_, err = p.tags.UpdateAtScope(p.ctx, scope, armresources.TagsPatchResource{
		Operation:  new(armresources.TagsPatchOperationDelete),
		Properties: &armresources.Tags{Tags: remove},
	}, nil)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to clear approval tags on %s: %s", scope, err)))
	}
}

func (p *Pipeline) getTags(scope string) (map[string]string, error) {
	response, err := p.tags.GetAtScope(p.ctx, scope, nil)
	if err != nil {
		return nil, err
	}
	tags := map[string]string{}
	if response.Properties != nil {
		for key, value := range response.Properties.Tags {
			if value != nil {
				tags[key] = *value
			}
		}
	}
	return tags, nil
}

func (p *Pipeline) planChanges(pipelineName string, step model.Step) (*model.PipelineChanges, error) {
	data, err := p.bucket.GetFile(model.PlanBucketKey(pipelineName))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("couldn't find plan %s in the storage account, the base image must upload it", model.PlanBucketKey(pipelineName))
	}
	return generator.ParsePlanChanges(pipelineName, step.Type, data)
}

func (p *Pipeline) StartDestroyExecution(projectName string, step model.Step) error {
	planCommand, applyCommand := model.GetDestroyCommands(step.Type)
	for _, command := range []model.ActionCommand{planCommand, applyCommand} {
		job := jobName(projectName, command)
		execution, err := p.builder.startJob(job)
		if err != nil {
			return err
		}
		if err = p.builder.waitForExecution(job, execution); err != nil {
			return fmt.Errorf("%s failed for %s: %w", command, projectName, err)
		}
	}
	return nil
}

func (p *Pipeline) DeletePipeline(_ string) error {
	return nil
}

func (p *Pipeline) CreateAgentPipelines(_, projectName, _ string, run bool) error {
	if !run {
		return nil
	}
	return p.StartAgentExecution(model.GetAgentProjectName(projectName, common.RunCommand))
}

func (p *Pipeline) StartAgentExecution(pipelineName string) error {
	_, err := p.builder.startJob(jobName(pipelineName, ""))
	return err
}
