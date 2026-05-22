package common

import (
	"strconv"
)

const (
	AwsPrefixEnv       = "AWS_PREFIX"
	PrefixEnv          = "PREFIX"
	GCloudProjectIdEnv = "PROJECT_ID"
	GCloudLocationEnv  = "LOCATION"
	GCloudZoneEnv      = "ZONE"
)

type Flags struct {
	LogLevel                string
	Config                  string
	Prefix                  string
	Force                   bool
	SkipBucketCreationDelay bool
	Start                   bool
	Steps                   []string
	RotateCredentials       bool
	Pipeline                Pipeline
	GCloud                  GCloud
	AWS                     AWS
	ServiceAccount          ServiceAccount
	Delete                  DeleteFlags
	Params                  Params
	Migrate                 Migrate
	Wrapper                 Wrapper
}

func (f *Flags) Setup(cmd Command) error {
	return f.validate(cmd)
}

type GCloud struct {
	ProjectId       string
	Location        string
	Zone            string
	CredentialsJson string
}

type AWS struct {
	RoleArn string
}

type ServiceAccount struct {
	RemoveUser        bool
	RotateCredentials bool
	TrustRole         string
}

type DeleteFlags struct {
	DeleteBucket         bool
	DeleteServiceAccount bool
	SkipConfirmation     bool
}

type Pipeline struct {
	Type           string
	LogsPath       string
	PrintLogs      bool
	TerraformCache BoolPtrFlag
	AllowParallel  bool
}

type Migrate struct {
	StateFile  string
	ImportFile string
	PlanFile   string
	TypesFile  string
}

type Wrapper struct {
	Config     string
	Step       string
	Command    string
	Entrypoint string
}

type PipelineType string

const (
	PipelineTypeLocal PipelineType = "local"
	PipelineTypeCloud PipelineType = "cloud"
)

type Params struct {
	Key       string
	Value     string
	Overwrite bool
}

type BoolPtrFlag struct {
	Value *bool
}

func (b *BoolPtrFlag) Get() interface{} {
	return b.Value
}

func (b *BoolPtrFlag) Set(value string) error {
	boolValue, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	b.Value = &boolValue
	return nil
}

func (b *BoolPtrFlag) String() string {
	if b.Value == nil {
		return ""
	}
	return strconv.FormatBool(*b.Value)
}
