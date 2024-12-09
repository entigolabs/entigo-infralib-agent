package common

import "fmt"

func (f *Flags) validate(cmd Command) error {
	if f.Pipeline.Type != "" && f.Pipeline.Type != string(PipelineTypeLocal) && f.Pipeline.Type != string(PipelineTypeCloud) {
		return fmt.Errorf("pipeline type must be either 'local' or 'cloud'")
	}
	switch cmd {
	case RunCommand:
		fallthrough
	case UpdateCommand:
		fallthrough
	case DeleteCommand:
		fallthrough
	case SACommand:
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
