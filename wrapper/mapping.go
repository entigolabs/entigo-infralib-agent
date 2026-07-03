package wrapper

import (
	"github.com/entigolabs/entigo-infralib-agent/gen/wrapper/v1alpha1"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

// Returns "" for unrecognized commands — callers treat that as a hard error.
func getStepType(command model.ActionCommand) model.StepType {
	switch command {
	case model.PlanCommand, model.ApplyCommand,
		model.PlanDestroyCommand, model.ApplyDestroyCommand:
		return model.StepTypeTerraform
	case model.ArgoCDPlanCommand, model.ArgoCDApplyCommand,
		model.ArgoCDPlanDestroyCommand, model.ArgoCDApplyDestroyCommand:
		return model.StepTypeArgoCD
	default:
		return ""
	}
}

func protoCommand(cmd model.ActionCommand) v1alpha1.Command {
	switch cmd {
	case model.PlanCommand:
		return v1alpha1.Command_COMMAND_PLAN
	case model.ApplyCommand:
		return v1alpha1.Command_COMMAND_APPLY
	case model.PlanDestroyCommand:
		return v1alpha1.Command_COMMAND_PLAN_DESTROY
	case model.ApplyDestroyCommand:
		return v1alpha1.Command_COMMAND_APPLY_DESTROY
	case model.ArgoCDPlanCommand:
		return v1alpha1.Command_COMMAND_ARGOCD_PLAN
	case model.ArgoCDApplyCommand:
		return v1alpha1.Command_COMMAND_ARGOCD_APPLY
	case model.ArgoCDPlanDestroyCommand:
		return v1alpha1.Command_COMMAND_ARGOCD_PLAN_DESTROY
	case model.ArgoCDApplyDestroyCommand:
		return v1alpha1.Command_COMMAND_ARGOCD_APPLY_DESTROY
	default:
		return v1alpha1.Command_COMMAND_UNSPECIFIED
	}
}

func protoStepType(t model.StepType) v1alpha1.StepType {
	switch t {
	case model.StepTypeTerraform:
		return v1alpha1.StepType_STEP_TYPE_TERRAFORM
	case model.StepTypeArgoCD:
		return v1alpha1.StepType_STEP_TYPE_ARGOCD_APPS
	default:
		return v1alpha1.StepType_STEP_TYPE_UNSPECIFIED
	}
}
