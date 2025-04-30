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

func NewValidator(ctx context.Context, flags common.Migrate) (Validator, error) {
	state, err := getState(flags.StateFile)
	if err != nil {
		return nil, err
	}
	if state.Version != 4 {
		return nil, fmt.Errorf("unsupported state version: %d", state.Version)
	}
	planFile, err := getPlan(flags.PlanFile)
	if err != nil {
		return nil, err
	}
	config, err := getConfig(flags.ImportFile)
	if err != nil {
		return nil, err
	}
	return &validator{
		ctx:    ctx,
		state:  state,
		plan:   planFile,
		config: config,
	}, nil
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
	fmt.Println("State resources:")
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
		fmt.Printf("Resource '%s' change action: %s\n", change.Address, actionType)
	}
}

func (v *validator) validateConfigTypes() {
	types := model.NewSet[string]()
	for _, item := range v.config.Import {
		types.Add(item.Type)
	}
	actionTypes := model.ToSet[string]([]string{"create", "delete", "replace"})
	fmt.Println("Import config types:")
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
		fmt.Printf("Resource '%s' drift action: %s\n", change.Address, actionType)
	}
}

func (v *validator) validateChangedValues() {
	actionTypes := model.ToSet[string]([]string{"update", "replace"})
	fmt.Println("Changed values:")
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
		slog.Error(common.PrefixError(fmt.Errorf("failed to unmarshal before value for %s: %s", change.Address, err)))
		return
	}
	if err := json.Unmarshal(afterJson, &after); err != nil {
		slog.Error(common.PrefixError(fmt.Errorf("failed to unmarshal after value for %s: %s", change.Address, err)))
		return
	}
	if before.Value != after.Value {
		fmt.Printf("Resource '%s' value changed: %s -> %s\n", change.Address, before.Value, after.Value)
	}
}
