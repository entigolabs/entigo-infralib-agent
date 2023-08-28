package model

import "time"

type Config struct {
	Prefix  string  `yaml:"prefix"`
	Source  string  `yaml:"source"`
	Version string  `yaml:"version"`
	Steps   []Steps `yaml:"steps"`
}

type Steps struct {
	Name      string    `yaml:"name"`
	Type      string    `yaml:"type"`
	Workspace string    `yaml:"workspace"`
	Approve   Approve   `yaml:"approve,omitempty"`
	Modules   []Modules `yaml:"modules"`
	Version   string    `yaml:"version,omitempty"`
	VpcPrefix string    `yaml:"vpc_prefix,omitempty"`
}

type Modules struct {
	Name    string                 `yaml:"name"`
	Source  string                 `yaml:"source"`
	Version string                 `yaml:"version"`
	Inputs  map[string]interface{} `yaml:"inputs,omitempty"`
}

type Approve string

const (
	ApproveMinor  Approve = "minor"
	ApproveMajor          = "major"
	ApproveAlways         = "always"
	ApproveNever          = "never"
)

type State struct {
	Version            string    `yaml:"version"`
	VersionPublishedAt time.Time `yaml:"version_published_at"`
	VersionAppliedAt   time.Time `yaml:"applied_at"`
}
