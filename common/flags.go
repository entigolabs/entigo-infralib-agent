package common

type Flags struct {
	LoggingLevel string
	Config       string
	Branch       string
	AWSPrefix    string
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
