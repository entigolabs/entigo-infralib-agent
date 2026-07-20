package oracle

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const (
	// Plan stdout must propagate through OCI Logging ingestion before it's
	// searchable; poll until it appears or the wait elapses.
	logSearchWait = 3 * time.Minute
	logSearchPoll = 10 * time.Second
)

// Pipeline orchestrates a step's plan → approve → apply cycle by launching
// Container Instances through the Builder, mirroring the local pipeline flow.
// Manual approval is a DevOps deployment gate (Gate): when the change set is not
// auto-approvable the agent blocks until an IAM-authorized user approves or
// rejects the deployment in the OCI Console; rejection fails the step so
// dependent steps never run. Instances persist between runs (the Builder
// restarts them when the spec is unchanged), so plan logs stay reviewable in
// the console during the approval wait and afterwards.
type Pipeline struct {
	ctx           context.Context
	builder       *Builder
	gate          *Gate
	logs          *Logging
	manager       model.NotificationManager
	cloudPrefix   string
	campaignId    string
	pipelineIndex int
}

func NewPipeline(ctx context.Context, builder *Builder, gate *Gate, logs *Logging, cloudPrefix string, manager model.NotificationManager) *Pipeline {
	return &Pipeline{
		ctx:         ctx,
		builder:     builder,
		gate:        gate,
		logs:        logs,
		manager:     manager,
		cloudPrefix: cloudPrefix,
	}
}

// SetCampaignId stores the campaign correlation, which is forwarded to step
// containers OUT-OF-BAND (Builder.putRunContext → config-bucket object read
// and deleted by the wrapper), never via env: the campaign id is a fresh UUID
// per agent process and container env is immutable and part of the reuse spec
// hash, so a per-run env value would force a recreate on every execution — and
// OCI offers no per-execution overrides (StartContainerInstance takes only the
// instance OCID).
func (p *Pipeline) SetCampaignId(campaignId string) {
	p.campaignId = campaignId
}

func (p *Pipeline) SetPipelineIndex(index int) {
	p.pipelineIndex = index
}

func (p *Pipeline) CreatePipeline(projectName, stepName string, step model.Step, _ model.Bucket, _ map[string]model.SourceAuth) (*string, error) {
	return p.StartPipelineExecution(projectName, stepName, step, "")
}

func (p *Pipeline) UpdatePipeline(_, _ string, _ model.Step, _ string, _ map[string]model.SourceAuth) error {
	// No persistent pipeline resource to update — the Builder recreates an
	// instance on launch whenever its spec (image, env, networking) has changed.
	return nil
}

func (p *Pipeline) StartPipelineExecution(pipelineName, _ string, step model.Step, _ string) (*string, error) {
	planCommand, _ := model.GetCommands(step.Type)
	if err := p.builder.putRunContext(pipelineName, planCommand, p.campaignId, p.pipelineIndex); err != nil {
		return nil, err
	}
	instanceId, err := p.builder.launch(pipelineName, pipelineName, planCommand, step)
	if err != nil {
		return nil, err
	}
	p.logStepHint(pipelineName, planCommand)
	return &instanceId, nil
}

// logStepHint prints an OCI Log Search query scoped to this one execution so a
// gitops engineer can jump straight to its logs in the shared log group.
func (p *Pipeline) logStepHint(prefixStep string, command model.ActionCommand) {
	if p.logs == nil {
		return
	}
	log.Printf("Logs for %s %s — OCI Log Search: %s\n", prefixStep, command, p.logs.StepLogHint(prefixStep, command))
}

func (p *Pipeline) WaitPipelineExecution(pipelineName, projectName string, executionId *string, autoApprove bool, step model.Step, approve model.ManualApprove) error {
	if executionId == nil {
		return fmt.Errorf("no execution id for pipeline %s", pipelineName)
	}
	since := time.Now()
	exitCode, err := p.builder.waitForCompletion(*executionId)
	if err != nil {
		return fmt.Errorf("failed to wait for plan of %s: %w", pipelineName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("plan failed for %s (exit code %d)", pipelineName, exitCode)
	}
	planCommand, _ := model.GetCommands(step.Type)
	changes, err := p.planChanges(pipelineName, step.Type, planCommand, since)
	if err != nil {
		return err
	}
	// Mirrors LocalPipeline.getApproval: reject halts, no-changes skips apply.
	if step.Approve == model.ApproveReject || approve == model.ManualApproveReject {
		return fmt.Errorf("stopped because step approve type is 'reject'")
	}
	if changes.NoChanges {
		log.Printf("No changes detected for %s, skipping apply", pipelineName)
		return nil
	}
	if !util.ShouldApprovePipeline(*changes, step.Approve, autoApprove, approve) {
		if err = p.waitForManualApproval(pipelineName, step, changes); err != nil {
			return err
		}
	}
	_, applyCommand := model.GetCommands(step.Type)
	return p.runToCompletion(projectName, pipelineName, applyCommand, step)
}

// waitForManualApproval blocks on the DevOps approval gate. The plan container
// instance persists, so the approver can review the plan logs in the OCI
// Console before deciding.
func (p *Pipeline) waitForManualApproval(pipelineName string, step model.Step, changes *model.PipelineChanges) error {
	if p.gate == nil {
		return fmt.Errorf("manual approval required for %s but no approval gate is available", pipelineName)
	}
	summary := fmt.Sprintf("%s: %d to add, %d to change, %d to destroy", pipelineName,
		changes.Added, changes.Changed, changes.Destroyed)
	deploymentId, err := p.gate.RequestApproval(pipelineName, summary)
	if err != nil {
		return err
	}
	if p.manager != nil {
		p.manager.ManualApproval(pipelineName, step.Name, *changes, "")
	}
	if err = p.gate.WaitForApproval(deploymentId); err != nil {
		return fmt.Errorf("manual approval for %s: %w", pipelineName, err)
	}
	log.Printf("Approved %s\n", pipelineName)
	return nil
}

func (p *Pipeline) StartDestroyExecution(projectName string, step model.Step) error {
	planCommand, applyCommand := model.GetDestroyCommands(step.Type)
	if err := p.runToCompletion(projectName, projectName, planCommand, step); err != nil {
		return err
	}
	return p.runToCompletion(projectName, projectName, applyCommand, step)
}

// DeletePipeline removes the persistent container instances of a removed step.
// The approval pipeline in the DevOps project is left behind (known gap).
func (p *Pipeline) DeletePipeline(projectName string) error {
	p.builder.deleteProjectInstances(projectName)
	return nil
}

func (p *Pipeline) CreateAgentPipelines(_, projectName, _ string, run bool) error {
	if !run {
		return nil
	}
	// Mirrors gcloud: the registered agent project name carries the command suffix.
	return p.StartAgentExecution(model.GetAgentProjectName(projectName, common.RunCommand))
}

func (p *Pipeline) StartAgentExecution(pipelineName string) error {
	_, err := p.builder.launch(pipelineName, pipelineName, "", model.Step{})
	return err
}

func (p *Pipeline) runToCompletion(projectName, pipelineName string, command model.ActionCommand, step model.Step) error {
	if err := p.builder.putRunContext(pipelineName, command, p.campaignId, p.pipelineIndex); err != nil {
		return err
	}
	instanceId, err := p.builder.launch(projectName, pipelineName, command, step)
	if err != nil {
		return err
	}
	p.logStepHint(pipelineName, command)
	exitCode, err := p.builder.waitForCompletion(instanceId)
	if err != nil {
		return fmt.Errorf("failed to wait for %s of %s: %w", command, pipelineName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s failed for %s (exit code %d)", command, pipelineName, exitCode)
	}
	return nil
}

// planChanges reads the plan container's stdout back from OCI Logging and parses
// the terraform/helm change summary. Because log ingestion is asynchronous, the
// summary may not be searchable the instant the container exits, so it polls
// until the summary appears or the deadline passes.
func (p *Pipeline) planChanges(pipelineName string, stepType model.StepType, command model.ActionCommand, since time.Time) (*model.PipelineChanges, error) {
	if p.logs == nil {
		return nil, fmt.Errorf("no logging service available to read plan output for %s", pipelineName)
	}
	deadline := time.After(logSearchWait)
	for {
		lines, err := p.logs.StepLogs(pipelineName, command, since)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to read logs for %s: %s", pipelineName, err)))
		} else {
			changes, err := parseChanges(pipelineName, stepType, lines)
			if err != nil {
				return nil, err
			}
			if changes != nil {
				return changes, nil
			}
		}
		select {
		case <-p.ctx.Done():
			return nil, p.ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("couldn't find plan output in logs for %s within %s", pipelineName, logSearchWait)
		case <-time.After(logSearchPoll):
		}
	}
}

// parseChanges scans plan stdout lines for the terraform/helm change summary,
// reusing the same line parsers the local and cloud pipelines of other providers
// use.
func parseChanges(pipelineName string, stepType model.StepType, lines []string) (*model.PipelineChanges, error) {
	var parser func(string, string) (*model.PipelineChanges, error)
	switch stepType {
	case model.StepTypeTerraform:
		parser = terraform.ParseLogChanges
	case model.StepTypeArgoCD:
		parser = argocd.ParseLogChanges
	default:
		return nil, fmt.Errorf("unsupported step type %s", stepType)
	}
	for _, line := range lines {
		changes, err := parser(pipelineName, line)
		if err != nil {
			return nil, err
		}
		if changes != nil {
			return changes, nil
		}
	}
	return nil, nil
}
