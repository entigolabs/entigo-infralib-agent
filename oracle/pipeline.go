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
// DevOps build runs through the Builder, mirroring the local pipeline flow.
// Manual approval is a DevOps deployment gate (Gate): when the change set is not
// auto-approvable the agent blocks until an IAM-authorized user approves or
// rejects the deployment in the OCI Console; rejection fails the step so
// dependent steps never run. Build-run logs stay reviewable in the console
// during the approval wait and afterwards.
type Pipeline struct {
	ctx         context.Context
	builder     *Builder
	gate        *Gate
	logs        *Logging
	manager     model.NotificationManager
	cloudPrefix string
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

// SetCampaignId / SetPipelineIndex forward the campaign correlation to the
// Builder, which passes it as per-run build-run arguments (CAMPAIGN_ID /
// PIPELINE_INDEX) for the wrapper to read directly — same as AWS/GCloud.
func (p *Pipeline) SetCampaignId(campaignId string) {
	p.builder.SetCampaignId(campaignId)
}

func (p *Pipeline) SetPipelineIndex(index int) {
	p.builder.SetPipelineIndex(index)
}

func (p *Pipeline) CreatePipeline(projectName, stepName string, step model.Step, _ model.Bucket, _ map[string]model.SourceAuth) (*string, error) {
	if err := p.builder.ensureStepPipelines(projectName, step); err != nil {
		return nil, err
	}
	return p.StartPipelineExecution(projectName, stepName, step, "")
}

func (p *Pipeline) UpdatePipeline(projectName, _ string, step model.Step, _ string, _ map[string]model.SourceAuth) error {
	// Reconcile every command's build pipeline (params drift with image/config/CSK)
	// so the console-triggerable destroy pipelines stay current; execution itself is
	// started separately by the updater via StartPipelineExecution.
	return p.builder.ensureStepPipelines(projectName, step)
}

func (p *Pipeline) StartPipelineExecution(pipelineName, _ string, step model.Step, _ string) (*string, error) {
	planCommand, _ := model.GetCommands(step.Type)
	buildRunId, err := p.builder.launch(pipelineName, pipelineName, planCommand, step)
	if err != nil {
		return nil, err
	}
	return &buildRunId, nil
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
	changes, err := p.planChanges(pipelineName, step.Type, *executionId, since)
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

// waitForManualApproval blocks on the DevOps approval gate. The plan build run's
// logs persist, so the approver can review them in the OCI Console before deciding.
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

// StartDestroyExecution runs a step's destroy by triggering its eagerly created
// -plan-destroy then -apply-destroy build pipelines by name. It relies solely on
// the pipelines' baked-in parameter defaults, so it works in a fresh process that
// never registered the run spec (the delete command) and matches a console
// trigger. A step whose pipelines were never created surfaces as
// model.NotFoundError, which the delete flow skips.
func (p *Pipeline) StartDestroyExecution(projectName string, step model.Step) error {
	planCommand, applyCommand := model.GetDestroyCommands(step.Type)
	if err := p.triggerToCompletion(projectName, planCommand); err != nil {
		return err
	}
	return p.triggerToCompletion(projectName, applyCommand)
}

// triggerToCompletion triggers an existing pipeline by name and blocks on its
// build run, mirroring runToCompletion but without the run spec (destroy is
// agentless). No approval gate: destroy runs with ApproveForce.
func (p *Pipeline) triggerToCompletion(projectName string, command model.ActionCommand) error {
	displayName := runName(projectName, command)
	buildRunId, err := p.builder.trigger(displayName)
	if err != nil {
		return err
	}
	exitCode, err := p.builder.waitForCompletion(buildRunId)
	if err != nil {
		return fmt.Errorf("failed to wait for %s of %s: %w", command, displayName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s failed for %s (exit code %d)", command, displayName, exitCode)
	}
	return nil
}

func (p *Pipeline) DeletePipeline(projectName string) error {
	p.builder.devopsBuild.deleteStepPipelines(projectName)
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
	buildRunId, err := p.builder.launch(projectName, pipelineName, command, step)
	if err != nil {
		return err
	}
	exitCode, err := p.builder.waitForCompletion(buildRunId)
	if err != nil {
		return fmt.Errorf("failed to wait for %s of %s: %w", command, pipelineName, err)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s failed for %s (exit code %d)", command, pipelineName, exitCode)
	}
	return nil
}

// planChanges reads the plan build run's stdout back from the DevOps service log
// and parses the terraform/helm change summary. Because log ingestion is
// asynchronous, the summary may not be searchable the instant the build run
// exits, so it polls until the summary appears or the deadline passes.
func (p *Pipeline) planChanges(pipelineName string, stepType model.StepType, buildRunId string, since time.Time) (*model.PipelineChanges, error) {
	if p.logs == nil {
		return nil, fmt.Errorf("no logging service available to read plan output for %s", pipelineName)
	}
	deadline := time.After(logSearchWait)
	for {
		lines, err := p.logs.StepLogs(buildRunId, since)
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
