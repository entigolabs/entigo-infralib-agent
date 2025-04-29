package notify

import (
	"bytes"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type BaseNotifier struct {
	model.NotifierType
	MessageFunc func(message string) error
}

func (b *BaseNotifier) Message(messageType model.MessageType, message string) error {
	if messageType == model.MessageTypeFailure {
		message = fmt.Sprintf("ERROR %s", message)
	}
	return b.MessageFunc(message)
}

func (b *BaseNotifier) ManualApproval(pipelineName string, changes model.PipelineChanges, link string) error {
	formattedChanges := fmt.Sprintf("Plan: %d to add, %d to change, %d to destroy.", changes.Added, changes.Changed, changes.Destroyed)
	message := fmt.Sprintf("Waiting for manual approval of pipeline %s\n%s", pipelineName, formattedChanges)
	if link != "" {
		message += fmt.Sprintf("\nPipeline: %s", link)
	}
	return b.MessageFunc(message)
}

func (b *BaseNotifier) StepState(status model.ApplyStatus, stepState model.StateStep, _ *model.Step) error {
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintf("Step '%s' status: %s", stepState.Name, status))
	for _, module := range stepState.Modules {
		buffer.WriteString(fmt.Sprintf("\nModule '%s' version: %s", module.Name, module.Version))
		if module.AppliedVersion != nil {
			buffer.WriteString(fmt.Sprintf(", applied version: %s", *module.AppliedVersion))
		}
	}
	return b.MessageFunc(buffer.String())
}
