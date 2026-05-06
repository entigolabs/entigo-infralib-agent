package model

import (
	"context"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/hashicorp/go-version"
)

type NotificationManager interface {
	SetCurrentPipelineIndex(index int)
	HasNotifier(messageType MessageType) bool
	Campaign(ctx context.Context, status CampaignStatus, resources Resources, command common.Command, err error)
	Schedule(command common.Command, status ScheduleAction, schedule string)
	Approval(pipeline, step, approvedBy string)
	ManualApproval(pipelineName, step string, changes PipelineChanges, link string)
	StepState(status ApplyStatus, stepState StateStep, step *Step, err error)
	Modules(resources Resources, command common.Command, config Config)
	Sources(sources map[SourceKey]*Source)
	PipelineState(status ApplyStatus, sourceVersions []SourceVersion, err error)
}

type Notifier interface {
	GetName() string
	Includes(MessageType) bool

	HandleCampaign(CampaignMessage) error
	HandleApproval(ApprovalMessage) error
	HandleManualApproval(ManualApprovalMessage) error
	HandleStepState(StepStateMessage) error
	HandlePipelineState(PipelineStateMessage) error
	HandleModules(ModulesMessage) error
	HandleSources(SourcesMessage) error
	HandleSchedule(ScheduleMessage) error
}

type MessageType string

const (
	MessageTypeStarted   MessageType = "started"
	MessageTypeProgress  MessageType = "progress"
	MessageTypeApprovals MessageType = "approvals"
	MessageTypeSuccess   MessageType = "success"
	MessageTypeFailure   MessageType = "failure"
	MessageTypeModules   MessageType = "modules"
	MessageTypeSchedule  MessageType = "schedule"
	MessageTypeSources   MessageType = "sources"
	MessageTypeUnknown   MessageType = "unknown" // Meta invalid type for handling message structs with multiple types
)

type BaseNotifier struct {
	Name         string
	Context      string
	MessageTypes Set[MessageType]
}

func (n BaseNotifier) GetName() string {
	return n.Name
}

func (n BaseNotifier) GetContext() string {
	return n.Context
}

func (n BaseNotifier) Includes(messageType MessageType) bool {
	return n.MessageTypes.Contains(messageType)
}

type ApplyStatus string

const (
	ApplyStatusSuccess  ApplyStatus = "success"
	ApplyStatusFailure  ApplyStatus = "failure"
	ApplyStatusSkipped  ApplyStatus = "skipped"
	ApplyStatusStarting ApplyStatus = "starting"
)

type SourceVersion struct {
	URL           string
	Version       *version.Version
	ForcedVersion string
}
