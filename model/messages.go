package model

import (
	"context"

	"github.com/entigolabs/entigo-infralib-agent/common"
)

type Message interface {
	Type() MessageType
	Dispatch(Notifier) error
}

type CampaignStatus string

const (
	CampaignStatusStarted    CampaignStatus = "started"
	CampaignStatusSuccess    CampaignStatus = "success"
	CampaignStatusFailure    CampaignStatus = "failure"
	CampaignStatusTerminated CampaignStatus = "terminated"
)

type CampaignMessage struct {
	Ctx       context.Context
	Status    CampaignStatus
	Resources Resources
	Command   common.Command
	Err       error
}

func (m CampaignMessage) Type() MessageType {
	switch m.Status {
	case CampaignStatusStarted:
		return MessageTypeStarted
	case CampaignStatusSuccess:
		return MessageTypeSuccess
	case CampaignStatusFailure, CampaignStatusTerminated:
		return MessageTypeFailure
	default:
		return MessageTypeUnknown
	}
}

func (m CampaignMessage) Dispatch(n Notifier) error {
	return n.HandleCampaign(m)
}

type ScheduleAction string

const (
	ScheduleAdded    ScheduleAction = "added"
	ScheduleModified ScheduleAction = "modified"
	ScheduleRemoved  ScheduleAction = "removed"
)

type ScheduleMessage struct {
	Command  common.Command
	Action   ScheduleAction
	Schedule string
}

func (m ScheduleMessage) Type() MessageType {
	return MessageTypeSchedule
}

func (m ScheduleMessage) Dispatch(n Notifier) error {
	return n.HandleSchedule(m)
}

type ApprovalMessage struct {
	PipelineIndex int32
	PipelineName  string
	Step          string
	ApprovedBy    string
}

func (m ApprovalMessage) Type() MessageType {
	return MessageTypeApprovals
}

func (m ApprovalMessage) Dispatch(n Notifier) error {
	return n.HandleApproval(m)
}

type ManualApprovalMessage struct {
	PipelineIndex int32
	PipelineName  string
	Step          string
	Changes       PipelineChanges
	Link          string
}

func (ManualApprovalMessage) Type() MessageType {
	return MessageTypeApprovals
}

func (m ManualApprovalMessage) Dispatch(n Notifier) error {
	return n.HandleManualApproval(m)
}

type StepStateMessage struct {
	PipelineIndex int32
	Status        ApplyStatus
	StateStep     StateStep
	Step          *Step
	Err           error
}

func (StepStateMessage) Type() MessageType {
	return MessageTypeProgress
}

func (m StepStateMessage) Dispatch(n Notifier) error {
	return n.HandleStepState(m)
}

type PipelineStateMessage struct {
	Index          int32
	Status         ApplyStatus
	SourceVersions []SourceVersion
	Err            error
}

func (PipelineStateMessage) Type() MessageType {
	return MessageTypeProgress
}

func (m PipelineStateMessage) Dispatch(n Notifier) error {
	return n.HandlePipelineState(m)
}

type ModulesMessage struct {
	Resources Resources
	Command   common.Command
	Config    Config
}

func (ModulesMessage) Type() MessageType {
	return MessageTypeModules
}

func (m ModulesMessage) Dispatch(n Notifier) error {
	return n.HandleModules(m)
}

type SourcesMessage struct {
	Sources map[SourceKey]*Source
}

func (SourcesMessage) Type() MessageType {
	return MessageTypeSources
}

func (m SourcesMessage) Dispatch(n Notifier) error {
	return n.HandleSources(m)
}
