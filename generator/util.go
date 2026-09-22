package generator

import (
	"fmt"
	"log/slog"

	"github.com/entigolabs/entigo-infralib-agent/model"
)

func ParsePlanChanges(pipelineName string, stepType model.StepType, data []byte) (*model.PipelineChanges, error) {
	slog.Debug("Parsing json plan changes", "pipeline", pipelineName, "stepType", stepType)
	switch stepType {
	case model.StepTypeTerraform:
		return ParseTfChanges(pipelineName, data)
	case model.StepTypeArgoCD:
		return ParseArgoCDPlan(pipelineName, data)
	}
	return nil, fmt.Errorf("unsupported plan step type %s", stepType)
}

func ParseLogChanges(pipelineName string, stepType model.StepType, logRow string) (*model.PipelineChanges, error) {
	switch stepType {
	case model.StepTypeTerraform:
		return ParseTfLog(pipelineName, logRow)
	case model.StepTypeArgoCD:
		return ParseArgoCDLog(pipelineName, logRow)
	}
	return nil, fmt.Errorf("unsupported log step type %s", stepType)
}
