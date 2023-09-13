package model

import (
	"github.com/hashicorp/go-version"
	"time"
)

type Config struct {
	BaseConfig   BaseConfig `yaml:"base_config"`
	Prefix       string     `yaml:"prefix,omitempty"`
	Source       string     `yaml:"source"`
	Version      string     `yaml:"version,omitempty"`
	AgentVersion string     `yaml:"agent_version,omitempty"`
	Steps        []Step     `yaml:"steps,omitempty"`
}

type BaseConfig struct {
	Version string `yaml:"version,omitempty"`
	Profile string `yaml:"profile"`
}

type Step struct {
	Name         string   `yaml:"name"`
	Type         StepType `yaml:"type,omitempty"`
	Workspace    string   `yaml:"workspace"`
	Approve      Approve  `yaml:"approve,omitempty"`
	Remove       bool     `yaml:"remove,omitempty"`
	Version      string   `yaml:"version,omitempty"`
	VpcPrefix    string   `yaml:"vpc_prefix,omitempty"`
	ArgoCDPrefix string   `yaml:"argocd_prefix,omitempty"`
	Modules      []Module `yaml:"modules,omitempty"`
}

type Module struct {
	Name    string                 `yaml:"name"`
	Source  string                 `yaml:"source,omitempty"`
	Version string                 `yaml:"version,omitempty"`
	Remove  bool                   `yaml:"remove,omitempty"`
	Inputs  map[string]interface{} `yaml:"inputs,omitempty"`
}

type StepType string

const (
	StepTypeTerraform       StepType = "terraform"
	StepTypeArgoCD                   = "argocd-apps"
	StepTypeTerraformCustom          = "terraform-custom"
)

type Approve string

const (
	ApproveMinor  Approve = "minor"
	ApproveMajor          = "major"
	ApproveAlways         = "always"
	ApproveNever          = "never"
)

type State struct {
	Steps []*StateStep `yaml:"steps"`
}

type StateStep struct {
	Name      string         `yaml:"name"`
	Workspace string         `yaml:"workspace"`
	AppliedAt time.Time      `yaml:"applied_at,omitempty"`
	Modules   []*StateModule `yaml:"modules"`
}

type StateModule struct {
	Name        string           `yaml:"name"`
	Version     *version.Version `yaml:"version,omitempty"`
	AutoApprove bool             `yaml:"-"` // always omit
}
