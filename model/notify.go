package model

import "time"

type NotificationManager interface {
	HasNotifier(messageType MessageType) bool
	Message(messageType MessageType, message string)
	ManualApproval(pipelineName string, changes PipelineChanges, link string)
	StepState(status ApplyStatus, stepState StateStep, step *Step, err error)
}

type MessageType string

const (
	MessageTypeStarted   MessageType = "started"
	MessageTypeProgress  MessageType = "progress"
	MessageTypeApprovals MessageType = "approvals"
	MessageTypeSuccess   MessageType = "success"
	MessageTypeFailure   MessageType = "failure"
)

type BaseNotifier struct {
	Name         string
	Context      string
	MessageTypes Set[MessageType]
}

type Notifier interface {
	GetName() string
	Includes(messageType MessageType) bool
	Message(messageType MessageType, message string) error
	ManualApproval(pipelineName string, changes PipelineChanges, link string) error
	StepState(status ApplyStatus, stepState StateStep, step *Step, err error) error
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

type ModulesRequest struct {
	Status    ApplyStatus    `json:"status"`
	StatusAt  time.Time      `json:"status_at"`
	Step      string         `json:"step"`
	Error     string         `json:"error,omitempty"`
	AppliedAt time.Time      `json:"applied_at"`
	Modules   []ModuleEntity `json:"modules"`
}

type ModuleEntity struct {
	Name           string            `json:"name"`
	AppliedVersion *string           `json:"applied_version,omitempty"`
	Version        string            `json:"version"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type MessageRequest struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type PipelineRequest struct {
	Name string     `json:"name"`
	Plan PlanEntity `json:"plan"`
	Link string     `json:"link,omitempty"`
}

type PlanEntity struct {
	Import  int `json:"imported,omitempty"`
	Add     int `json:"added,omitempty"`
	Change  int `json:"changed,omitempty"`
	Destroy int `json:"removed,omitempty"`
}
