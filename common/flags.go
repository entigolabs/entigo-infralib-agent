package common

type Flags struct {
	LoggingLevel string
	Config       string
}

func (f *Flags) Setup(cmd Command) error {
	if err := f.validate(cmd); err != nil {
		return err
	}
	f.cmdSpecificSetup(cmd)
	return nil
}

func (f *Flags) cmdSpecificSetup(cmd Command) {
	switch cmd {
	case UpdateCommand: // currently empty
	}
}
