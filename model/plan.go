package model

import "encoding/json"

// Plan is the OpenTofu/Terraform `terraform show -json <planfile>` output
// schema. Only the fields downstream code reads are listed; unmodelled fields
// pass through Unmarshal unread.
type Plan struct {
	FormatVersion    string                  `json:"format_version"`
	TerraformVersion string                  `json:"terraform_version"`
	Variables        map[string]PlanVariable `json:"variables"`
	PlannedValues    PlanValues              `json:"planned_values"`
	ResourceChanges  []ResourceChange        `json:"resource_changes"`
	ResourceDrifts   []ResourceChange        `json:"resource_drift"`
	OutputChanges    map[string]OutputChange `json:"output_changes"`
	Configuration    PlanConfiguration       `json:"configuration"`
}

type PlanVariable struct {
	Value json.RawMessage `json:"value"`
}

type PlanValues struct {
	RootModule PlanModule `json:"root_module"`
}

type PlanModule struct {
	Resources    []PlanResource `json:"resources"`
	ChildModules []PlanModule   `json:"child_modules,omitempty"`
}

type PlanResource struct {
	Address       string          `json:"address"`
	Mode          string          `json:"mode"`
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	Index         interface{}     `json:"index,omitempty"`
	ProviderName  string          `json:"provider_name"`
	SchemaVersion int             `json:"schema_version"`
	Values        json.RawMessage `json:"values"`
}

// ResourceChange is one entry of the plan's resource_changes / resource_drift
// arrays. ModuleAddress is set for resources nested inside a module call;
// PreviousAddress is set when the resource was moved.
type ResourceChange struct {
	Address         string      `json:"address"`
	ModuleAddress   string      `json:"module_address,omitempty"`
	PreviousAddress string      `json:"previous_address,omitempty"`
	Mode            string      `json:"mode"`
	Type            string      `json:"type"`
	Name            string      `json:"name"`
	Index           interface{} `json:"index,omitempty"`
	ProviderName    string      `json:"provider_name"`
	Change          Change      `json:"change"`
}

// Change describes how a single resource will be modified. Actions is the
// authoritative discriminator; possible values include "no-op", "create",
// "read", "update", "delete", "forget", and the two-element replace forms
// ["delete","create"] and ["create","delete"].
type Change struct {
	Actions         []string         `json:"actions"`
	Before          json.RawMessage  `json:"before"`
	After           json.RawMessage  `json:"after"`
	AfterUnknown    json.RawMessage  `json:"after_unknown"`
	BeforeSensitive json.RawMessage  `json:"before_sensitive,omitempty"`
	AfterSensitive  json.RawMessage  `json:"after_sensitive,omitempty"`
	Importing       *ImportingChange `json:"importing,omitempty"`
}

// ImportingChange marks a resource that will be imported as part of this plan.
type ImportingChange struct {
	ID string `json:"id"`
}

// OutputChange is one entry of the plan's output_changes map. On plan, Before
// and After may be absent; Actions is always set.
type OutputChange struct {
	Actions   []string        `json:"actions"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
	Sensitive bool            `json:"sensitive,omitempty"`
}

// ChangedValue is the subset of an aws_ssm_parameter resource value that the
// migrate validator reads when diffing before/after for changed resources.
type ChangedValue struct {
	AllowedPattern string            `json:"allowed_pattern"`
	Arn            string            `json:"arn"`
	DataType       string            `json:"data_type"`
	Description    string            `json:"description"`
	ID             string            `json:"id"`
	InsecureValue  interface{}       `json:"insecure_value"`
	KeyID          string            `json:"key_id"`
	Name           string            `json:"name"`
	Overwrite      interface{}       `json:"overwrite"`
	Tags           map[string]string `json:"tags"`
	TagsAll        map[string]string `json:"tags_all"`
	Tier           string            `json:"tier"`
	Type           string            `json:"type"`
	Value          interface{}       `json:"value"`
	Version        int               `json:"version"`
}

type PlanConfiguration struct {
	ProviderConfigs map[string]PlanProviderConfig `json:"provider_configs"`
	RootModule      PlanModuleConfig              `json:"root_module"`
}

type PlanProviderConfig struct {
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
}

type PlanModuleConfig struct {
	Resources    []PlanResourceConfig `json:"resources"`
	ChildModules []PlanModuleConfig   `json:"child_modules,omitempty"`
}

type PlanResourceConfig struct {
	Address      string          `json:"address"`
	Mode         string          `json:"mode"`
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	ProviderName string          `json:"provider_name"`
	Expressions  json.RawMessage `json:"expressions"`
}
