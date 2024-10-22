package common

const (
	AwsPrefixEnv       = "AWS_PREFIX"
	GCloudProjectIdEnv = "PROJECT_ID"
	GCloudLocationEnv  = "LOCATION"
	GCloudZoneEnv      = "ZONE"
)

type Flags struct {
	LogLevel      string
	Config        string
	Prefix        string
	AllowParallel bool
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
	DeleteBucket     bool
	SkipConfirmation bool
}

func (f *Flags) Setup(cmd Command) error {
	return f.validate(cmd)
}
