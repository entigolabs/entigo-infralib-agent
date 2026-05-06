package notify

import (
	"fmt"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/model"
)

var _ model.Notifier = (*BaseNotifier)(nil)

type BaseNotifier struct {
	model.BaseNotifier
	MessageFunc func(message string) error
}

func (b *BaseNotifier) HandleCampaign(msg model.CampaignMessage) error {
	provider := msg.Resources.GetProviderType()
	message := ""
	if msg.Err != nil {
		message = fmt.Sprintf("ERROR %s\n", msg.Err.Error())
	}
	message += fmt.Sprintf("Agent %s %s: prefix %s %s ", msg.Command, msg.Status,
		msg.Resources.GetCloudPrefix(), provider)
	if provider == model.GCLOUD {
		message += fmt.Sprintf("project Id %s, location %s", msg.Resources.GetAccount(), msg.Resources.GetRegion())
	} else {
		message += fmt.Sprintf("account Id %s, region %s", msg.Resources.GetAccount(), msg.Resources.GetRegion())
	}
	return b.sendMessage(message)
}

func (b *BaseNotifier) HandleSchedule(msg model.ScheduleMessage) error {
	message := fmt.Sprintf("Update schedule %s: %s", msg.Action, msg.Schedule)
	return b.sendMessage(message)
}

func (b *BaseNotifier) HandleApproval(msg model.ApprovalMessage) error {
	message := fmt.Sprintf("Pipeline %s was approved", msg.PipelineName)
	if msg.ApprovedBy != "" {
		message += fmt.Sprintf("\nApproved by %s", msg.ApprovedBy)
	}
	return b.sendMessage(message)
}

func (b *BaseNotifier) HandleManualApproval(msg model.ManualApprovalMessage) error {
	imported := ""
	if msg.Changes.Imported != 0 {
		imported = fmt.Sprintf(" %d to import, ", msg.Changes.Imported)
	}
	formattedChanges := fmt.Sprintf("Plan: %s%d to add, %d to change, %d to destroy.",
		imported, msg.Changes.Added, msg.Changes.Changed, msg.Changes.Destroyed)
	message := fmt.Sprintf("Waiting for manual approval of pipeline %s\n%s", msg.PipelineName, formattedChanges)
	if msg.Link != "" {
		message += fmt.Sprintf("\nPipeline: %s", msg.Link)
	}
	return b.sendMessage(message)
}

func (b *BaseNotifier) HandleStepState(msg model.StepStateMessage) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Step '%s' status: %s", msg.StateStep.Name, msg.Status)
	if msg.Err != nil {
		fmt.Fprintf(&sb, ", error: %s", msg.Err.Error())
	}
	for _, module := range msg.StateStep.Modules {
		fmt.Fprintf(&sb, "\nModule '%s' version: %s", module.Name, module.Version)
		if module.AppliedVersion != nil {
			fmt.Fprintf(&sb, ", applied version: %s", *module.AppliedVersion)
		}
	}
	return b.sendMessage(sb.String())
}

func (b *BaseNotifier) HandleModules(msg model.ModulesMessage) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Steps for account %s in region %s", msg.Resources.GetAccount(), msg.Resources.GetRegion())
	for _, step := range msg.Config.Steps {
		fmt.Fprintf(&sb, "\nStep '%s':", step.Name)
		for _, module := range step.Modules {
			fmt.Fprintf(&sb, "\n- Module '%s' source: %s", module.Name, module.Source)
		}
	}
	return b.sendMessage(sb.String())
}

func (b *BaseNotifier) HandleSources(msg model.SourcesMessage) error {
	var sb strings.Builder
	fmt.Fprint(&sb, "Configured sources:")
	for _, source := range msg.Sources {
		fmt.Fprintf(&sb, "\n- Source: %s", source.URL)
		if source.Version != nil {
			fmt.Fprintf(&sb, "\n  Version: %s", source.Version)
		}
		if source.ForcedVersion != "" {
			fmt.Fprintf(&sb, "\n  Forced version: %s", source.ForcedVersion)
		}
		if len(source.Releases) > 0 {
			releases := make([]string, 0)
			for _, release := range source.Releases {
				if release == nil {
					continue
				}
				releases = append(releases, release.Original())
			}
			fmt.Fprintf(&sb, "\n  Releases: %s", strings.Join(releases, ", "))
		}
		if len(source.Modules) > 0 {
			fmt.Fprintf(&sb, "\n  Modules: %s", strings.Join(source.Modules.ToSlice(), ", "))
		}
	}
	return b.sendMessage(sb.String())
}

func (b *BaseNotifier) HandlePipelineState(msg model.PipelineStateMessage) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pipeline status: %s", msg.Status)
	if msg.Err != nil {
		fmt.Fprintf(&sb, ", error: %s", msg.Err.Error())
	}
	for _, source := range msg.SourceVersions {
		fmt.Fprintf(&sb, "\n- Source: %s", source.URL)
		if source.Version != nil {
			fmt.Fprintf(&sb, ", Version: %s", source.Version)
		}
		if source.ForcedVersion != "" {
			fmt.Fprintf(&sb, ", Forced version: %s", source.ForcedVersion)
		}
	}
	return b.sendMessage(sb.String())
}

func (b *BaseNotifier) sendMessage(message string) error {
	if b.Context != "" {
		message = fmt.Sprintf("%s %s", b.Context, message)
	}
	return b.MessageFunc(message)
}
