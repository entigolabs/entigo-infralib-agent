package model

type ApplyStatus string

const (
	ApplyStatusSuccess  ApplyStatus = "success"
	ApplyStatusFailure  ApplyStatus = "failure"
	ApplyStatusSkipped  ApplyStatus = "skipped"
	ApplyStatusProgress ApplyStatus = "progress"
)

type ModulesRequest struct {
	Status  ApplyStatus     `json:"status"`
	Step    string          `json:"step"`
	Modules []ModuleRequest `json:"modules"`
}

type ModuleRequest struct {
	Name           string  `json:"name"`
	AppliedVersion *string `json:"applied_version,omitempty"`
	Version        string  `json:"version"`
}

func ToModulesRequest(status ApplyStatus, stepState StateStep) ModulesRequest {
	modules := make([]ModuleRequest, 0)
	for _, module := range stepState.Modules {
		modules = append(modules, ModuleRequest{
			Name:           module.Name,
			AppliedVersion: module.AppliedVersion,
			Version:        module.Version,
		})
	}
	return ModulesRequest{
		Status:  status,
		Step:    stepState.Name,
		Modules: modules,
	}
}
