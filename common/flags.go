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

	OracleCompartmentIdEnv = "OCI_COMPARTMENT_ID"
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
	Oracle                  Oracle
	ServiceAccount          ServiceAccount
	Delete                  DeleteFlags
	Params                  Params
	Migrate                 Migrate
	Wrapper                 Wrapper
	OfflineTrustBundle      string
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

// Oracle holds OCI (Oracle Cloud) resource placement. CompartmentId selects the
// Oracle provider when set. Credentials are resolved ambiently by the SDK
// (~/.oci/config or OCI_CONFIG_FILE / config env vars), or via resource principal
// in-container — see oracle.newConfigProvider.
type Oracle struct {
	Region        string
	CompartmentId string
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
	Config        string
	Step          string
	Command       string
	Entrypoint    string
	PrefixStep    string
	PlanPath      string
	CampaignId    string
	PipelineIndex string
	Insecure      bool
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
