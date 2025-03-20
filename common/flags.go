package common

import (
	"github.com/urfave/cli/v2"
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
	Steps                   cli.StringSlice
	Pipeline                Pipeline
	GCloud                  GCloud
	AWS                     AWS
	Delete                  DeleteFlags
	Params                  Params
	Migrate                 Migrate
}

func (f *Flags) Setup(cmd Command) error {
	return f.validate(cmd)
}

type GCloud struct {
	ProjectId string
	Location  string
	Zone      string
}

type AWS struct {
	RoleArn string
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
	TerraformCache bool
	AllowParallel  bool
}

type Migrate struct {
	StateFile  string
	ImportFile string
	PlanFile   string
	TypesFile  string
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
