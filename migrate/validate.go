package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
	"log/slog"
)

type Validator interface {
	Validate()
}

type validator struct {
	ctx    context.Context
	state  stateV4
	plan   plan
	config importConfig
}

func NewValidator(ctx context.Context, flags common.Migrate) Validator {
	state := getState(flags.StateFile)
	if state.Version != 4 {
		log.Fatalf("Unsupported state version: %d", state.Version)
	}
	return &validator{
		ctx:    ctx,
		state:  state,
		plan:   getPlan(flags.PlanFile),
		config: getConfig(flags.ImportFile),
	}
}

func (v *validator) Validate() {
	log.Println("Validating state resources")
	v.validatePlanState()
	log.Println("Validating import config types")
	v.validateConfigTypes()
	log.Println("Validating changed values")
	v.validateChangedValues()
}

func (v *validator) validatePlanState() {
	resources := model.NewSet[string]()
	for _, resource := range v.state.Resources {
		if resource.Mode != "managed" {
			continue
		}
		resources.Add(fmt.Sprintf("%s.%s", resource.Type, resource.Name))
	}
	actionTypes := model.ToSet[string]([]string{"create", "delete", "replace"})
	validateChangeState(resources, actionTypes, v.plan.ResourceChanges)
	validateChangeState(resources, actionTypes, v.plan.ResourceDrifts)
}

func validateChangeState(resources, actionTypes model.Set[string], changes []resourceChangePlan) {
	for _, change := range changes {
		if change.Mode != "managed" {
			continue
		}
		if !resources.Contains(fmt.Sprintf("%s.%s", change.Type, change.Name)) {
			continue
		}
		var actionType string
		for _, action := range change.Change.Actions {
			if actionTypes.Contains(action) {
				actionType = action
				break
			}
		}
		if actionType == "" {
			continue
		}
		slog.Error(fmt.Sprintf("Resource '%s' change action: %s", change.Address, actionType))
	}
}

func (v *validator) validateConfigTypes() {
	types := model.NewSet[string]()
	for _, item := range v.config.Import {
		types.Add(item.Type)
	}
	actionTypes := model.ToSet[string]([]string{"create", "delete", "replace"})
	validateConfigTypes(types, actionTypes, v.plan.ResourceChanges)
	validateConfigTypes(types, actionTypes, v.plan.ResourceDrifts)
}

func validateConfigTypes(types, actionTypes model.Set[string], changes []resourceChangePlan) {
	for _, change := range changes {
		if change.Mode != "managed" {
			continue
		}
		if !types.Contains(change.Type) {
			continue
		}
		var actionType string
		for _, action := range change.Change.Actions {
			if actionTypes.Contains(action) {
				actionType = action
				break
			}
		}
		if actionType == "" {
			continue
		}
		slog.Warn(fmt.Sprintf("Resource '%s' drift action: %s", change.Address, actionType))
	}
}

func (v *validator) validateChangedValues() {
	actionTypes := model.ToSet[string]([]string{"update", "replace"})
	validateChangedResourceValues(actionTypes, v.plan.ResourceDrifts)
	validateChangedResourceValues(actionTypes, v.plan.ResourceChanges)
}

func validateChangedResourceValues(actionTypes model.Set[string], changes []resourceChangePlan) {
	for _, change := range changes {
		if change.Mode != "managed" {
			continue
		}
		correctType := false
		for _, action := range change.Change.Actions {
			if actionTypes.Contains(action) {
				correctType = true
				break
			}
		}
		if !correctType {
			continue
		}
		if change.Change.Before != nil && change.Change.After != nil {
			validateValueChange(change, change.Change.Before, change.Change.After)
		}
		if change.Change.BeforeSensitive != nil && change.Change.AfterSensitive != nil {
			validateValueChange(change, change.Change.BeforeSensitive, change.Change.AfterSensitive)
		}
	}
}

func validateValueChange(change resourceChangePlan, beforeJson, afterJson json.RawMessage) {
	var before changedValuePlan
	var after changedValuePlan
	if err := json.Unmarshal(beforeJson, &before); err != nil {
		slog.Error(fmt.Sprintf("Failed to unmarshal before value for %s: %s", change.Address, err))
		return
	}
	if err := json.Unmarshal(afterJson, &after); err != nil {
		slog.Error(fmt.Sprintf("Failed to unmarshal after value for %s: %s", change.Address, err))
		return
	}
	if before.Value != after.Value {
		slog.Warn(fmt.Sprintf("Resource '%s' value changed: %s -> %s", change.Address, before.Value, after.Value))
	}
}
