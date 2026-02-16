package model

import "time"

type NotificationManager interface {
	HasNotifier(messageType MessageType) bool
	Message(messageType MessageType, message string, params map[string]string)
	ManualApproval(pipelineName string, changes PipelineChanges, link string)
	StepState(status ApplyStatus, stepState StateStep, step *Step, err error)
	Modules(accountId, region string, provider ProviderType, config Config)
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
)

type BaseNotifier struct {
	Name         string
	Context      string
	MessageTypes Set[MessageType]
}

type Notifier interface {
	GetName() string
	Includes(messageType MessageType) bool
	Message(messageType MessageType, message string, params map[string]string) error
	ManualApproval(pipelineName string, changes PipelineChanges, link string) error
	StepState(status ApplyStatus, stepState StateStep, step *Step, err error) error
	Modules(accountId string, region string, provider ProviderType, config Config) error
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

type StepStatusRequest struct {
	Status    ApplyStatus          `json:"status"`
	StatusAt  time.Time            `json:"status_at"`
	Step      string               `json:"step"`
	Error     string               `json:"error,omitempty"`
	AppliedAt time.Time            `json:"applied_at"`
	Modules   []ModuleStatusEntity `json:"modules"`
}

type ModuleStatusEntity struct {
	Name           string            `json:"name"`
	AppliedVersion *string           `json:"applied_version,omitempty"`
	Version        string            `json:"version"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type MessageRequest struct {
	Type    string            `json:"type"`
	Message string            `json:"message"`
	Params  map[string]string `json:"params,omitempty"`
}

type ApprovalRequest struct {
	Name string     `json:"name"`
	Plan PlanEntity `json:"plan"`
	Link string     `json:"link,omitempty"`
}

type PlanEntity struct {
	Imported  int `json:"imported,omitempty"`
	Added     int `json:"added,omitempty"`
	Changed   int `json:"changed,omitempty"`
	Destroyed int `json:"destroyed,omitempty"`
}

type ModulesRequest struct {
	Id             string       `json:"id"`
	Region         string       `json:"region"`
	UpdateSchedule string       `json:"updateSchedule,omitempty"`
	Provider       ProviderType `json:"provider"`
	Steps          []StepEntity `json:"steps"`
}

type StepEntity struct {
	Name    string         `json:"name"`
	Modules []ModuleEntity `json:"modules"`
}

type ModuleEntity struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}
