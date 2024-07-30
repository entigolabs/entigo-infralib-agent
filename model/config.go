package model

import (
	"github.com/hashicorp/go-version"
	"time"
)

type Config struct {
	BaseConfig       BaseConfig `yaml:"base_config"`
	Prefix           string     `yaml:"prefix,omitempty" fake:"{word}"`
	Source           string     `yaml:"source" fake:"{url}"`
	Version          string     `yaml:"version,omitempty" fake:"{version}"`
	AgentVersion     string     `yaml:"agent_version,omitempty" fake:"{version}"`
	BaseImageVersion string     `yaml:"base_image_version,omitempty"`
	Steps            []Step     `yaml:"steps,omitempty" fakesize:"1"`
}

type BaseConfig struct {
	Version string `yaml:"version,omitempty" fake:"{version}"`
	Profile string `yaml:"profile" fake:"{word}"`
}

type Step struct {
	Name                  string   `yaml:"name" fake:"{word}"`
	Type                  StepType `yaml:"type,omitempty" fake:"{stepType}"`
	Workspace             string   `yaml:"workspace"`
	Before                string   `yaml:"before,omitempty" fake:"skip"`
	Approve               Approve  `yaml:"approve,omitempty"`
	Remove                bool     `yaml:"remove,omitempty" fake:"skip"`
	Version               string   `yaml:"version,omitempty" fake:"{version}"`
	BaseImageVersion      string   `yaml:"base_image_version,omitempty" fake:"{version}"`
	VpcId                 string   `yaml:"vpc_id,omitempty"`
	VpcSubnetIds          string   `yaml:"vpc_subnet_ids,omitempty"`
	VpcSecurityGroupIds   string   `yaml:"vpc_security_group_ids,omitempty"`
	KubernetesClusterName string   `yaml:"kubernetes_cluster_name,omitempty"`
	ArgocdNamespace       string   `yaml:"argocd_namespace,omitempty"`
	RepoUrl               string   `yaml:"repo_url,omitempty"`
	Provider              Provider `yaml:"provider,omitempty"`
	Modules               []Module `yaml:"modules,omitempty" fakesize:"1"`
}

type Provider struct {
	Inputs     map[string]interface{} `yaml:"inputs,omitempty"`
	Aws        AwsProvider            `yaml:"aws"`
	Kubernetes KubernetesProvider     `yaml:"kubernetes"`
}

type AwsProvider struct {
	IgnoreTags  AwsIgnoreTags  `yaml:"ignore_tags,omitempty"`
	DefaultTags AwsDefaultTags `yaml:"default_tags,omitempty"`
}

type AwsIgnoreTags struct {
	KeyPrefixes []string `yaml:"key_prefixes,omitempty"`
	Keys        []string `yaml:"keys,omitempty"`
}

type AwsDefaultTags struct {
	Tags map[string]string `yaml:"tags,omitempty"`
}

type KubernetesProvider struct {
	IgnoreAnnotations []string `yaml:"ignore_annotations,omitempty"`
	IgnoreLabels      []string `yaml:"ignore_labels,omitempty"`
}

func (p Provider) IsEmpty() bool {
	return p.Aws.IsEmpty() && p.Kubernetes.IsEmpty()
}

func (a AwsProvider) IsEmpty() bool {
	return a.IgnoreTags.IsEmpty() && a.DefaultTags.IsEmpty()
}

func (i AwsIgnoreTags) IsEmpty() bool {
	return len(i.KeyPrefixes) == 0 && len(i.Keys) == 0
}

func (d AwsDefaultTags) IsEmpty() bool {
	return len(d.Tags) == 0
}

func (k KubernetesProvider) IsEmpty() bool {
	return len(k.IgnoreAnnotations) == 0 && len(k.IgnoreLabels) == 0
}

type Module struct {
	Name         string                 `yaml:"name"`
	Source       string                 `yaml:"source,omitempty"`
	HttpUsername string                 `yaml:"http_username,omitempty"`
	HttpPassword string                 `yaml:"http_password,omitempty"`
	Version      string                 `yaml:"version,omitempty"`
	Remove       bool                   `yaml:"remove,omitempty" fake:"skip"`
	Inputs       map[string]interface{} `yaml:"inputs,omitempty" fakesize:"2,5"`
}

type StepType string

const (
	StepTypeTerraform       StepType = "terraform"
	StepTypeArgoCD          StepType = "argocd-apps"
	StepTypeTerraformCustom StepType = "terraform-custom"
)

type ReplaceType string

const (
	ReplaceTypeSSM          ReplaceType = "ssm"
	ReplaceTypeSSMCustom    ReplaceType = "ssm-custom"
	ReplaceTypeGCSM         ReplaceType = "gcsm"
	ReplaceTypeGCSMCustom   ReplaceType = "gcsm-custom"
	ReplaceTypeOutput       ReplaceType = "output"
	ReplaceTypeOutputCustom ReplaceType = "output-custom"
	ReplaceTypeConfig       ReplaceType = "config"
	ReplaceTypeAgent        ReplaceType = "agent"
)

type AgentReplaceType string

const (
	AgentReplaceTypeVersion   AgentReplaceType = "version"
	AgentReplaceTypeAccountId AgentReplaceType = "accountId"
)

type Approve string

const (
	ApproveMinor  Approve = "minor"
	ApproveMajor  Approve = "major"
	ApproveAlways Approve = "always"
	ApproveNever  Approve = "never"
)

type State struct {
	BaseConfig StateConfig  `yaml:"base_config"`
	Steps      []*StateStep `yaml:"steps"`
}

type StateConfig struct {
	Version        *version.Version `yaml:"version,omitempty"`
	AppliedVersion *version.Version `yaml:"applied_version,omitempty"`
}

type StateStep struct {
	Name      string         `yaml:"name"`
	Workspace string         `yaml:"workspace"`
	AppliedAt time.Time      `yaml:"applied_at,omitempty"`
	Modules   []*StateModule `yaml:"modules"`
}

type StateModule struct {
	Name           string      `yaml:"name"`
	Version        string      `yaml:"version,omitempty"`
	AppliedVersion *string     `yaml:"applied_version,omitempty"`
	Type           *ModuleType `yaml:"type,omitempty"`
	AutoApprove    bool        `yaml:"-"` // always omit
}

type ModuleType string

const (
	ModuleTypeCustom ModuleType = "custom"
)
