package common

import "github.com/urfave/cli/v2"

const (
	AwsPrefixEnv       = "AWS_PREFIX"
	PrefixEnv          = "PREFIX"
	GCloudProjectIdEnv = "PROJECT_ID"
	GCloudLocationEnv  = "LOCATION"
	GCloudZoneEnv      = "ZONE"
)

type Flags struct {
	LogLevel      string
	Config        string
	Prefix        string
	AllowParallel bool
	GithubToken   string
	Force         bool
	Steps         cli.StringSlice
	Pipeline      Pipeline
	GCloud        GCloud
	AWS           AWS
	Delete        DeleteFlags
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
	Type      string
	LogsPath  string
	PrintLogs bool
}

type PipelineType string

const (
	PipelineTypeLocal PipelineType = "local"
	PipelineTypeCloud PipelineType = "cloud"
)

func (f *Flags) Setup(cmd Command) error {
	return f.validate(cmd)
}
