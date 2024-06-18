package common

const AwsPrefixEnv = "AWS_PREFIX"

type Flags struct {
	LoggingLevel string
	Config       string
	BaseConfig   string
	Branch       string
	AWSPrefix    string
	GCloud       GCloud
}

type GCloud struct {
	ProjectId string
	Location  string
	Zone      string
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
