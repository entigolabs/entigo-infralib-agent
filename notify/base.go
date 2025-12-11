package notify

import (
	"bytes"
	"fmt"

	"github.com/entigolabs/entigo-infralib-agent/model"
)

type BaseNotifier struct {
	model.BaseNotifier
	MessageFunc func(message string) error
}

func (b *BaseNotifier) Message(messageType model.MessageType, message string) error {
	if messageType == model.MessageTypeFailure {
		message = fmt.Sprintf("ERROR %s", message)
	}
	if b.Context != "" {
		message = fmt.Sprintf("%s %s", b.Context, message)
	}
	return b.MessageFunc(message)
}

func (b *BaseNotifier) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) error {
	imported := ""
	if changes.Imported != 0 {
		imported = fmt.Sprintf(" %d to import, ", changes.Imported)
	}
	formattedChanges := fmt.Sprintf("Plan: %s%d to add, %d to change, %d to destroy.", imported, changes.Added, changes.Changed, changes.Destroyed)
	message := fmt.Sprintf("Waiting for manual approval of pipeline %s\n%s", pipelineName, formattedChanges)
	if link != "" {
		message += fmt.Sprintf("\nPipeline: %s", link)
	}
	if b.Context != "" {
		message = fmt.Sprintf("%s %s", b.Context, message)
	}
	return b.MessageFunc(message)
}

func (b *BaseNotifier) StepState(status model.ApplyStatus, stepState model.StateStep, _ *model.Step, err error) error {
	var buffer bytes.Buffer
	if b.Context != "" {
		buffer.WriteString(fmt.Sprintf("%s ", b.Context))
	}
	buffer.WriteString(fmt.Sprintf("Step '%s' status: %s", stepState.Name, status))
	if err != nil {
		buffer.WriteString(fmt.Sprintf(", error: %s", err.Error()))
	}
	for _, module := range stepState.Modules {
		buffer.WriteString(fmt.Sprintf("\nModule '%s' version: %s", module.Name, module.Version))
		if module.AppliedVersion != nil {
			buffer.WriteString(fmt.Sprintf(", applied version: %s", *module.AppliedVersion))
		}
	}
	return b.MessageFunc(buffer.String())
}

func (b *BaseNotifier) Modules(accountId string, region string, _ model.ProviderType, config model.Config) error {
	var buffer bytes.Buffer
	if b.Context != "" {
		buffer.WriteString(fmt.Sprintf("%s ", b.Context))
	}
	buffer.WriteString(fmt.Sprintf("Steps for account %s in region %s", accountId, region))
	for _, step := range config.Steps {
		buffer.WriteString(fmt.Sprintf("\nStep '%s':", step.Name))
		for _, module := range step.Modules {
			buffer.WriteString(fmt.Sprintf("\n- Module '%s' source: %s", module.Name, module.Source))
		}
	}
	return b.MessageFunc(buffer.String())
}
