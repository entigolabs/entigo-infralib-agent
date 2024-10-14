package common

import "fmt"

func (f *Flags) validate(cmd Command) error {
	switch cmd {
	case RunCommand:
		fallthrough
	case UpdateCommand:
		fallthrough
	case DeleteCommand:
		fallthrough
	case BootstrapCommand:
		if f.GCloud.ProjectId != "" {
			if f.GCloud.Location == "" || f.GCloud.Zone == "" {
				return fmt.Errorf("gcloud location and zone must be set")
			}
		}
		fallthrough
	default:
		return nil
	}
}
