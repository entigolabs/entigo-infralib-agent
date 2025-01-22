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
	ProviderConfig string                  `json:"provider"`
	Instances      []instanceObjectStateV4 `json:"instances"`
}

type instanceObjectStateV4 struct {
	IndexKey interface{} `json:"index_key,omitempty"`
	Status   string      `json:"status,omitempty"`
	Deposed  string      `json:"deposed,omitempty"`

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
