package common

import "fmt"

func (f *Flags) validate(cmd Command) error {
	switch cmd {
	case RunCommand:
		fallthrough
	case UpdateCommand:
		if f.Pipeline.Type != "" && f.Pipeline.Type != string(PipelineTypeLocal) && f.Pipeline.Type != string(PipelineTypeCloud) {
			return fmt.Errorf("pipeline type must be either 'local' or 'cloud'")
		}
		fallthrough
	case DeleteCommand:
		fallthrough
	case BootstrapCommand:
		fallthrough
	case PullCommand:
		if f.Config == "" && f.Prefix == "" {
			return fmt.Errorf("config or prefix must be set")
		}
		fallthrough
	case SACommand, AddCustomCommand, DeleteCustomCommand, GetCustomCommand, ListCustomCommand:
		if f.GCloud.ProjectId != "" {
			if f.GCloud.Location == "" || f.GCloud.Zone == "" {
				return fmt.Errorf("gcloud location and zone must be set")
			}
		}
		return nil
	default:
		return nil
	}
}
