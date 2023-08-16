package common

func (f *Flags) validate(cmd Command) error {
	switch cmd {
	case UpdateCommand:
		return nil // nop
	}
	return nil
}
