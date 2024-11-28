package model

import (
	"github.com/hashicorp/go-version"
	"time"
)

type Config struct {
	Prefix           string         `yaml:"prefix,omitempty" fake:"{word}"`
	Sources          []ConfigSource `yaml:"sources,omitempty" fakesize:"1"`
	AgentVersion     string         `yaml:"agent_version,omitempty" fake:"{version}"`
	BaseImageSource  string         `yaml:"base_image_source,omitempty"`
	BaseImageVersion string         `yaml:"base_image_version,omitempty"`
	Steps            []Step         `yaml:"steps,omitempty" fakesize:"1"`
}

type ConfigSource struct {
	URL          string   `yaml:"url" fake:"{url}"`
	Version      string   `yaml:"version,omitempty"`
	ForceVersion bool     `yaml:"force_version,omitempty"`
	Include      []string `yaml:"include,omitempty"`
	Exclude      []string `yaml:"exclude,omitempty"`
}

type Step struct {
	Name                  string   `yaml:"name" fake:"{word}"`
	Type                  StepType `yaml:"type,omitempty" fake:"{stepType}"`
	Approve               Approve  `yaml:"approve,omitempty"`
	BaseImageSource       string   `yaml:"base_image_source,omitempty"`
	BaseImageVersion      string   `yaml:"base_image_version,omitempty" fake:"{version}"`
	Vpc                   VPC      `yaml:"vpc,omitempty"`
	KubernetesClusterName string   `yaml:"kubernetes_cluster_name,omitempty"`
	ArgocdNamespace       string   `yaml:"argocd_namespace,omitempty"`
	RepoUrl               string   `yaml:"repo_url,omitempty"`
	Provider              Provider `yaml:"provider,omitempty"`
	Modules               []Module `yaml:"modules,omitempty" fakesize:"1"`
	Files                 []File   `yaml:"-"`
}

type VPC struct {
	Attach           *bool  `yaml:"attach,omitempty"`
	Id               string `yaml:"id,omitempty"`
	SubnetIds        string `yaml:"subnet_ids,omitempty"`
	SecurityGroupIds string `yaml:"security_group_ids,omitempty"`
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

type File struct {
	Name    string `yaml:"-"`
	Content []byte `yaml:"-"`
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
	Inputs       map[string]interface{} `yaml:"inputs,omitempty" fakesize:"2,5"`
	InputsFile   string                 `yaml:"-"`
	FileContent  []byte                 `yaml:"-"`
}

type StepType string

const (
	StepTypeTerraform StepType = "terraform"
	StepTypeArgoCD    StepType = "argocd-apps"
)

type ReplaceType string

const (
	ReplaceTypeSSM             ReplaceType = "ssm"
	ReplaceTypeSSMCustom       ReplaceType = "ssm-custom"
	ReplaceTypeGCSM            ReplaceType = "gcsm"
	ReplaceTypeGCSMCustom      ReplaceType = "gcsm-custom"
	ReplaceTypeOutput          ReplaceType = "output"
	ReplaceTypeOutputOptional  ReplaceType = "optout"
	ReplaceTypeOutputCustom    ReplaceType = "output-custom"
	ReplaceTypeTOutput         ReplaceType = "toutput"
	ReplaceTypeTOutputOptional ReplaceType = "toptout"
	ReplaceTypeConfig          ReplaceType = "config"
	ReplaceTypeAgent           ReplaceType = "agent"
	ReplaceTypeModule          ReplaceType = "tmodule"
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
	Steps []*StateStep `yaml:"steps"`
}

type StateStep struct {
	Name      string         `yaml:"name"`
	AppliedAt time.Time      `yaml:"applied_at,omitempty"`
	Modules   []*StateModule `yaml:"modules"`
}

type StateModule struct {
	Name           string      `yaml:"name"`
	Version        string      `yaml:"version,omitempty"`
	AppliedVersion *string     `yaml:"applied_version,omitempty"`
	Source         string      `yaml:"source,omitempty"`
	Type           *ModuleType `yaml:"type,omitempty"`
	AutoApprove    bool        `yaml:"-"` // always omit
}

type ModuleType string

const (
	ModuleTypeCustom ModuleType = "custom"
)

type Source struct {
	URL               string
	Version           *version.Version
	ForcedVersion     string
	NewestVersion     *version.Version
	StableVersion     *version.Version
	Releases          []*version.Version
	Modules           Set[string]
	PreviousChecksums map[string]string
	CurrentChecksums  map[string]string
	Includes          Set[string]
	Excludes          Set[string]
}

type ModuleVersion struct {
	Version   string
	Changed   bool
	SourceURL string
}
