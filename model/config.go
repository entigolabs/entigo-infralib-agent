package model

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Prefix           string               `yaml:"prefix,omitempty"`
	Sources          []ConfigSource       `yaml:"sources,omitempty"`
	AgentVersion     string               `yaml:"agent_version,omitempty"`
	BaseImageSource  string               `yaml:"base_image_source,omitempty"`
	BaseImageVersion string               `yaml:"base_image_version,omitempty"`
	Destinations     []ConfigDestination  `yaml:"destinations,omitempty"`
	Notifications    []ConfigNotification `yaml:"notifications,omitempty"`
	Schedule         Schedule             `yaml:"schedule,omitempty"`
	Provider         Provider             `yaml:"provider,omitempty"`
	Steps            []Step               `yaml:"steps,omitempty"`
	Certs            []File               `yaml:"-"`
}

type ConfigSource struct {
	URL          string   `yaml:"url"`
	Version      string   `yaml:"version,omitempty"`
	ForceVersion bool     `yaml:"force_version,omitempty"`
	Include      []string `yaml:"include,omitempty"`
	Exclude      []string `yaml:"exclude,omitempty"`
	Username     string   `yaml:"username,omitempty"`
	Password     string   `yaml:"password,omitempty"`
	Insecure     bool     `yaml:"insecure,omitempty"`
	RepoPath     string   `yaml:"repo_path,omitempty"`
	CAFile       string   `yaml:"ca_file,omitempty"`
}

func (s ConfigSource) GetSourceKey() SourceKey {
	if s.ForceVersion {
		return SourceKey{URL: s.URL, ForcedVersion: s.Version}
	}
	return SourceKey{URL: s.URL}
}

func GetAgentPrefix(prefix string) string {
	return prefix + "-agent"
}

func GetAgentProjectName(agentPrefix string, cmd common.Command) string {
	return fmt.Sprintf("%s-%s", agentPrefix, cmd)
}

type ConfigDestination struct {
	Name string `yaml:"name,omitempty"`
	Git  *Git   `yaml:"git,omitempty"`
}

type Git struct {
	URL             string `yaml:"url,omitempty"`
	Key             string `yaml:"key,omitempty"`
	KeyPassword     string `yaml:"key_password,omitempty"`
	InsecureHostKey bool   `yaml:"insecure_host_key,omitempty"`
	Username        string `yaml:"username,omitempty"`
	Password        string `yaml:"password,omitempty"`
	AuthorName      string `yaml:"author_name,omitempty"`
	AuthorEmail     string `yaml:"author_email,omitempty"`
	Insecure        bool   `yaml:"insecure,omitempty"`
	CAFile          string `yaml:"ca_file,omitempty"`
}

type ConfigNotification struct {
	Name         string           `yaml:"name,omitempty"`
	Context      string           `yaml:"context,omitempty"`
	MessageTypes []MessageType    `yaml:"message_types,omitempty"`
	Slack        *Slack           `yaml:"slack,omitempty"`
	Teams        *Teams           `yaml:"teams,omitempty"`
	Api          *NotificationApi `yaml:"api,omitempty"`
}

type Slack struct {
	Token     string `yaml:"token,omitempty"`
	ChannelId string `yaml:"channel_id,omitempty"`
}

type Teams struct {
	WebhookUrl string `yaml:"webhook_url,omitempty"`
}

type NotificationApi struct {
	URL string `yaml:"url,omitempty"`
	Key string `yaml:"key,omitempty"`
}

type Schedule struct {
	UpdateCron string `yaml:"update_cron,omitempty"`
}

type Step struct {
	Name                  string        `yaml:"name"`
	Type                  StepType      `yaml:"type,omitempty"`
	Approve               Approve       `yaml:"approve,omitempty"`
	RunApprove            ManualApprove `yaml:"manual_approve_run,omitempty"`
	UpdateApprove         ManualApprove `yaml:"manual_approve_update,omitempty"`
	BaseImageSource       string        `yaml:"base_image_source,omitempty"`
	BaseImageVersion      string        `yaml:"base_image_version,omitempty"`
	Vpc                   VPC           `yaml:"vpc,omitempty"`
	KubernetesClusterName string        `yaml:"kubernetes_cluster_name,omitempty"`
	ArgocdNamespace       string        `yaml:"argocd_namespace,omitempty"`
	Provider              Provider      `yaml:"provider,omitempty"`
	Modules               []Module      `yaml:"modules,omitempty"`
	Files                 []File        `yaml:"-"`
}

func NewStepsChecksums() StepsChecksums {
	return StepsChecksums{
		PreviousChecksums: make(map[string]StepChecksums),
		CurrentChecksums:  make(map[string]StepChecksums),
	}
}

type StepsChecksums struct {
	PreviousChecksums map[string]StepChecksums
	CurrentChecksums  map[string]StepChecksums
}

type StepChecksums struct {
	ModuleChecksums map[string][]byte
	FileChecksums   map[string][]byte
}

type VPC struct {
	Attach           *bool  `yaml:"attach,omitempty"`
	Id               string `yaml:"id,omitempty"`
	SubnetIds        string `yaml:"subnet_ids,omitempty"`
	SecurityGroupIds string `yaml:"security_group_ids,omitempty"`
}

type Provider struct {
	Inputs     map[string]interface{} `yaml:"inputs,omitempty"`
	Aws        AwsProvider            `yaml:"aws,omitempty"`
	Kubernetes KubernetesProvider     `yaml:"kubernetes,omitempty"`
}

type AwsProvider struct {
	IgnoreTags  AwsIgnoreTags     `yaml:"ignore_tags,omitempty"`
	DefaultTags AwsDefaultTags    `yaml:"default_tags,omitempty"`
	Endpoints   map[string]string `yaml:"endpoints,omitempty"`
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
	Name     string `yaml:"-"`
	Content  []byte `yaml:"-"`
	Checksum []byte `yaml:"-"`
}

func (p Provider) IsEmpty() bool {
	return p.Aws.IsEmpty() && p.Kubernetes.IsEmpty()
}

func (a AwsProvider) IsEmpty() bool {
	return a.IgnoreTags.IsEmpty() && a.DefaultTags.IsEmpty() && len(a.Endpoints) == 0
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
	Name           string                 `yaml:"name"`
	Source         string                 `yaml:"source,omitempty"`
	HttpUsername   string                 `yaml:"http_username,omitempty"`
	HttpPassword   string                 `yaml:"http_password,omitempty"`
	Version        string                 `yaml:"version,omitempty"`
	DefaultModule  bool                   `yaml:"default_module,omitempty"`
	Inputs         map[string]interface{} `yaml:"inputs,omitempty"`
	InputsChecksum []byte                 `yaml:"-"`
	InputsFile     string                 `yaml:"-"`
	FileContent    []byte                 `yaml:"-"`
	Metadata       map[string]string      `yaml:"-"`
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
	ReplaceTypeStep            ReplaceType = "step"
	ReplaceTypeModuleType      ReplaceType = "tmodule"
	ReplaceTypeModuleOptional  ReplaceType = "toptmodule"
	ReplaceTypeStepModule      ReplaceType = "tsmodule"
	ReplaceTypeModule          ReplaceType = "module"
	ReplaceTypeInput           ReplaceType = "input"
	ReplaceTypeSelfOutput      ReplaceType = "sout"
)

type AgentReplaceType string

const (
	AgentReplaceTypeVersion   AgentReplaceType = "version"
	AgentReplaceTypeAccountId AgentReplaceType = "accountId"
	AgentReplaceTypeRegion    AgentReplaceType = "region"
)

type Approve string

const (
	ApproveMinor  Approve = "minor"
	ApproveMajor  Approve = "major"
	ApproveAlways Approve = "always"
	ApproveNever  Approve = "never"
	ApproveForce  Approve = "force"
	ApproveReject Approve = "reject"
)

type ManualApprove string

const (
	ManualApproveAlways  ManualApprove = "always"
	ManualApproveChanges ManualApprove = "changes"
	ManualApproveRemoves ManualApprove = "removes"
	ManualApproveNever   ManualApprove = "never"
	ManualApproveReject  ManualApprove = "reject"
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

type SourceKey struct {
	URL           string
	ForcedVersion string
}

func (s SourceKey) String() string {
	if s.ForcedVersion != "" {
		return fmt.Sprintf("%s@%s", s.URL, s.ForcedVersion)
	}
	return s.URL
}

func (s SourceKey) IsEmpty() bool {
	return s.URL == "" && s.ForcedVersion == ""
}

type Source struct {
	URL               string
	Version           *version.Version
	ForcedVersion     string
	Storage           Storage
	Auth              SourceAuth
	NewestVersion     *version.Version
	StableVersion     *version.Version
	Releases          []*version.Version
	Modules           Set[string]
	PreviousChecksums map[string][]byte
	CurrentChecksums  map[string][]byte
	Includes          Set[string]
	Excludes          Set[string]
}

type SourceAuth struct {
	Username string
	Password string
}

type Storage interface {
	GetFile(path, release string) ([]byte, error)
	FileExists(path, release string) bool
	PathExists(path, release string) (bool, error)
	CalculateChecksums(release string) (map[string][]byte, error)
}

type ModuleVersion struct {
	Version string
	Changed bool
	Source  SourceKey
}

type V1Agent struct {
	Version     string            `json:"version" yaml:"version"`
	Metadata    map[string]string `json:"metadata" yaml:"metadata"`
	ModuleTypes []string          `json:"module_types" yaml:"module_types"`
}

func UnmarshalAgentYaml(yamlData []byte) (interface{}, error) {
	var genericMap map[string]interface{}
	err := yaml.Unmarshal(yamlData, &genericMap)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal agent file yaml: %v", err)
	}

	metaVersion, ok := genericMap["version"].(string)
	if !ok {
		slog.Debug("agent file version not found in metadata, defaulting to v1")
		metaVersion = "v1"
	}

	switch metaVersion {
	case "v1":
		var v1 V1Agent
		err = yaml.Unmarshal(yamlData, &v1)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal v1 agent file: %v", err)
		}
		return v1, nil
	default:
		return nil, fmt.Errorf("unsupported version: %s", metaVersion)
	}
}
