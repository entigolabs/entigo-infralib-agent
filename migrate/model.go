package migrate

import (
	"encoding/json"
)

type typeIdentification struct {
	Identification string   `yaml:"identification"`
	Types          []string `yaml:"types"`
}

type typesConfig struct {
	TypeIdentifications []typeIdentification `yaml:"typeIdentifications"`
}

type stateV4 struct {
	Version          int                      `json:"version"`
	TerraformVersion string                   `json:"terraform_version"`
	Serial           uint64                   `json:"serial"`
	Lineage          string                   `json:"lineage"`
	RootOutputs      map[string]outputStateV4 `json:"outputs"`
	Resources        []resourceStateV4        `json:"resources"`
	CheckResults     []checkResultsV4         `json:"check_results"`
}

type outputStateV4 struct {
	ValueRaw     json.RawMessage `json:"value"`
	ValueTypeRaw json.RawMessage `json:"type"`
	Sensitive    bool            `json:"sensitive,omitempty"`
}

type resourceStateV4 struct {
	Module         string                  `json:"module,omitempty"`
	Mode           string                  `json:"mode"`
	Type           string                  `json:"type"`
	Name           string                  `json:"name"`
	EachMode       string                  `json:"each,omitempty"`
	ProviderConfig string                  `json:"provider,omitempty"`
	Instances      []instanceObjectStateV4 `json:"instances"`
}

type instanceObjectStateV4 struct {
	IndexKey       interface{} `json:"index_key,omitempty"`
	Status         string      `json:"status,omitempty"`
	Deposed        string      `json:"deposed,omitempty"`
	ProviderConfig string      `json:"provider,omitempty"`

	SchemaVersion           uint64            `json:"schema_version"`
	AttributesRaw           json.RawMessage   `json:"attributes,omitempty"`
	AttributesFlat          map[string]string `json:"attributes_flat,omitempty"`
	AttributeSensitivePaths json.RawMessage   `json:"sensitive_attributes,omitempty"`

	PrivateRaw []byte `json:"private,omitempty"`

	Dependencies []string `json:"dependencies,omitempty"`

	CreateBeforeDestroy bool `json:"create_before_destroy,omitempty"`
}

type checkResultsV4 struct {
	ObjectKind string                 `json:"object_kind"`
	ConfigAddr string                 `json:"config_addr"`
	Status     string                 `json:"status"`
	Objects    []checkResultsObjectV4 `json:"objects"`
}

type checkResultsObjectV4 struct {
	ObjectAddr      string   `json:"object_addr"`
	Status          string   `json:"status"`
	FailureMessages []string `json:"failure_messages,omitempty"`
}

type plan struct {
	FormatVersion    string                  `json:"format_version"`
	TerraformVersion string                  `json:"terraform_version"`
	Variables        map[string]variablePlan `json:"variables"`
	PlannedValues    valuesPlan              `json:"planned_values"`
	ResourceChanges  []resourceChangePlan    `json:"resource_changes"`
	ResourceDrifts   []resourceChangePlan    `json:"resource_drift"`
	Configuration    configurationPlan       `json:"configuration"`
}

type variablePlan struct {
	Value json.RawMessage `json:"value"`
}

type valuesPlan struct {
	RootModule modulePlan `json:"root_module"`
}

type modulePlan struct {
	Resources    []resourcePlan `json:"resources"`
	ChildModules []modulePlan   `json:"child_modules,omitempty"`
}

type resourcePlan struct {
	Address       string          `json:"address"`
	Mode          string          `json:"mode"`
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	Index         interface{}     `json:"index,omitempty"`
	ProviderName  string          `json:"provider_name"`
	SchemaVersion int             `json:"schema_version"`
	Values        json.RawMessage `json:"values"`
}

type resourceChangePlan struct {
	Address      string      `json:"address"`
	Mode         string      `json:"mode"`
	Type         string      `json:"type"`
	Name         string      `json:"name"`
	Index        interface{} `json:"index,omitempty"`
	ProviderName string      `json:"provider_name"`
	Change       changePlan  `json:"change"`
}

type changePlan struct {
	Actions         []string        `json:"actions"`
	Before          json.RawMessage `json:"before"`
	After           json.RawMessage `json:"after"`
	AfterUnknown    json.RawMessage `json:"after_unknown"`
	BeforeSensitive json.RawMessage `json:"before_sensitive,omitempty"`
	AfterSensitive  json.RawMessage `json:"after_sensitive,omitempty"`
}

type changedValuePlan struct {
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

type configurationPlan struct {
	ProviderConfigs map[string]providerConfigPlan `json:"provider_configs"`
	RootModule      moduleConfigPlan              `json:"root_module"`
}

type providerConfigPlan struct {
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
}

type moduleConfigPlan struct {
	Resources    []resourceConfigPlan `json:"resources"`
	ChildModules []moduleConfigPlan   `json:"child_modules,omitempty"`
}

type resourceConfigPlan struct {
	Address      string          `json:"address"`
	Mode         string          `json:"mode"`
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	ProviderName string          `json:"provider_name"`
	Expressions  json.RawMessage `json:"expressions"`
}

type importConfig struct {
	Import []importItem `yaml:"import"`
}

type importItem struct {
	Type        string `yaml:"type"`
	Source      module `yaml:"source"`
	Destination module `yaml:"destination"`
}

type module struct {
	Module    string        `yaml:"module"`
	Name      string        `yaml:"name,omitempty"`
	IndexKey  interface{}   `yaml:"index_key,omitempty"`
	IndexKeys []interface{} `yaml:"index_keys,omitempty"`
}

type KeyPair struct {
	Key1 interface{}
	Key2 interface{}
}

func newKeyPair(key1, key2 interface{}) KeyPair {
	return KeyPair{Key1: key1, Key2: key2}
}
