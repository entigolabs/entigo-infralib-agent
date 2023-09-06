package model

import (
	"github.com/hashicorp/go-version"
	"time"
)

type Config struct {
	Prefix       string `yaml:"prefix"`
	Source       string `yaml:"source"`
	Version      string `yaml:"version"`
	AgentVersion string `yaml:"agent_version"`
	Steps        []Step `yaml:"steps"`
}

type Step struct {
	Name      string   `yaml:"name"`
	Type      StepType `yaml:"type"`
	Workspace string   `yaml:"workspace"`
	Approve   Approve  `yaml:"approve,omitempty"`
	Modules   []Module `yaml:"modules"`
	Version   string   `yaml:"version,omitempty"`
	VpcPrefix string   `yaml:"vpc_prefix,omitempty"`
}

type Module struct {
	Name    string                 `yaml:"name"`
	Source  string                 `yaml:"source"`
	Version string                 `yaml:"version"`
	Inputs  map[string]interface{} `yaml:"inputs,omitempty"`
}

type StepType string

const (
	StepTypeTerraform StepType = "terraform"
	StepTypeArgoCD             = "argocd-apps"
	StepTypeAgent              = "agent"
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
