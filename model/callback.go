package model

import "time"

type ApplyStatus string

const (
	ApplyStatusSuccess  ApplyStatus = "success"
	ApplyStatusFailure  ApplyStatus = "failure"
	ApplyStatusSkipped  ApplyStatus = "skipped"
	ApplyStatusStarting ApplyStatus = "starting"
)

type ModulesRequest struct {
	Status    ApplyStatus     `json:"status"`
	StatusAt  time.Time       `json:"status_at"`
	Step      string          `json:"step"`
	AppliedAt time.Time       `json:"applied_at"`
	Modules   []ModuleRequest `json:"modules"`
}

type ModuleRequest struct {
	Name           string            `json:"name"`
	AppliedVersion *string           `json:"applied_version,omitempty"`
	Version        string            `json:"version"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

func ToModulesRequest(status ApplyStatus, stepState StateStep, step *Step) ModulesRequest {
	modules := make([]ModuleRequest, 0)
	for _, module := range stepState.Modules {
		var metadata map[string]string
		if step != nil {
			for _, m := range step.Modules {
				if m.Name == module.Name {
					metadata = m.Metadata
					break
				}
			}
		}
		modules = append(modules, ModuleRequest{
			Name:           module.Name,
			AppliedVersion: module.AppliedVersion,
			Version:        module.Version,
			Metadata:       metadata,
		})
	}
	return ModulesRequest{
		Status:    status,
		StatusAt:  time.Now().UTC(),
		Step:      stepState.Name,
		AppliedAt: stepState.AppliedAt,
		Modules:   modules,
	}
}
