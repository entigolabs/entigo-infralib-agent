package common

const (
	AwsPrefixEnv       = "AWS_PREFIX"
	GCloudProjectIdEnv = "PROJECT_ID"
	GCloudLocationEnv  = "LOCATION"
	GCloudZoneEnv      = "ZONE"
)

type Flags struct {
	LoggingLevel  string
	Config        string
	BaseConfig    string
	Branch        string
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
	if err := f.validate(cmd); err != nil {
		return err
	}
	f.cmdSpecificSetup(cmd)
	return nil
}

func (f *Flags) cmdSpecificSetup(cmd Command) {
	// currently empty
}
