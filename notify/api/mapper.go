package api

import (
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/google/uuid"
)

func toCampaignNotification(context string, msg model.CampaignMessage) (Notification, error) {
	notification := CampaignNotification{
		Kind:           Campaign,
		Context:        contextPtr(context),
		NotificationId: uuid.New(),
		Timestamp:      time.Now().UTC(),
		Command:        Command(msg.Command),
		Provider:       ProviderType(msg.Resources.GetProviderType()),
		Id:             msg.Resources.GetAccount(),
		Region:         msg.Resources.GetRegion(),
		Status:         CampaignStatus(msg.Status),
	}
	if msg.Err != nil {
		notification.Error = new(msg.Err.Error())
	}
	var n Notification
	if err := n.FromCampaignNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toScheduleNotification(context string, msg model.ScheduleMessage) (Notification, error) {
	notification := ScheduleNotification{
		Kind:           Schedule,
		Context:        contextPtr(context),
		NotificationId: uuid.New(),
		Timestamp:      time.Now().UTC(),
		Action:         ScheduleNotificationAction(msg.Action),
		Command:        Command(msg.Command),
	}
	if msg.Schedule != "" {
		notification.Schedule = &msg.Schedule
	}
	var n Notification
	if err := n.FromScheduleNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toApprovalNotification(context string, msg model.ApprovalMessage) (Notification, error) {
	notification := ApprovalNotification{
		Kind:           Approval,
		Context:        contextPtr(context),
		NotificationId: uuid.New(),
		PipelineIndex:  int(msg.PipelineIndex),
		Timestamp:      time.Now().UTC(),
		Name:           msg.PipelineName,
		Step:           msg.Step,
	}
	if msg.ApprovedBy != "" {
		notification.ApprovedBy = &msg.ApprovedBy
	}
	var n Notification
	if err := n.FromApprovalNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toManualApprovalNotification(context string, msg model.ManualApprovalMessage) (Notification, error) {
	notification := ManualApprovalNotification{
		Kind:           ManualApproval,
		PipelineIndex:  int(msg.PipelineIndex),
		Timestamp:      time.Now().UTC(),
		NotificationId: uuid.New(),
		Context:        contextPtr(context),
		Name:           msg.PipelineName,
		Step:           msg.Step,
		Plan: PlanEntity{
			Added:     msg.Changes.Added,
			Changed:   msg.Changes.Changed,
			Destroyed: msg.Changes.Destroyed,
			Imported:  msg.Changes.Imported,
		},
	}
	if msg.Link != "" {
		notification.Link = &msg.Link
	}
	var n Notification
	if err := n.FromManualApprovalNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toStepStateNotification(context string, msg model.StepStateMessage) (Notification, error) {
	notification := StepStateNotification{
		Kind:           StepState,
		PipelineIndex:  int(msg.PipelineIndex),
		Timestamp:      time.Now().UTC(),
		NotificationId: uuid.New(),
		Context:        contextPtr(context),
		Status:         ApplyStatus(msg.Status),
		Step:           msg.StateStep.Name,
		Modules:        toStepModuleStatuses(msg.StateStep.Modules, msg.Step),
	}
	if msg.Err != nil {
		notification.Error = new(msg.Err.Error())
	}
	if !msg.StateStep.AppliedAt.IsZero() {
		notification.AppliedAt = &msg.StateStep.AppliedAt
	}
	var n Notification
	if err := n.FromStepStateNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toPipelineStateNotification(context string, msg model.PipelineStateMessage) (Notification, error) {
	versions := make([]PipelineVersion, 0, len(msg.SourceVersions))
	for _, sv := range msg.SourceVersions {
		v := PipelineVersion{Url: sv.URL}
		if sv.Version != nil {
			v.Version = new(sv.Version.Original())
		}
		if sv.ForcedVersion != "" {
			v.ForcedVersion = &sv.ForcedVersion
		}
		versions = append(versions, v)
	}
	notification := PipelineStateNotification{
		Kind:           PipelineState,
		Timestamp:      time.Now().UTC(),
		NotificationId: uuid.New(),
		Context:        contextPtr(context),
		Status:         ApplyStatus(msg.Status),
		PipelineIndex:  int(msg.Index),
		Versions:       versions,
	}
	if msg.Err != nil {
		notification.Error = new(msg.Err.Error())
	}
	var n Notification
	if err := n.FromPipelineStateNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toModulesNotification(context string, msg model.ModulesMessage) (Notification, error) {
	steps := make([]StepEntity, 0, len(msg.Config.Steps))
	for _, step := range msg.Config.Steps {
		modules := make([]ModuleEntity, 0, len(step.Modules))
		for _, m := range step.Modules {
			modules = append(modules, ModuleEntity{Name: m.Name, Source: m.Source})
		}
		steps = append(steps, StepEntity{
			Name:    step.Name,
			Type:    StepEntityType(step.Type),
			Modules: modules,
		})
	}
	notification := ModulesNotification{
		Kind:           Modules,
		Timestamp:      time.Now().UTC(),
		NotificationId: uuid.New(),
		Context:        contextPtr(context),
		Id:             msg.Resources.GetAccount(),
		Region:         msg.Resources.GetRegion(),
		Provider:       ProviderType(msg.Resources.GetProviderType()),
		Command:        Command(msg.Command),
		Steps:          steps,
	}
	if msg.Config.Schedule.UpdateCron != "" {
		notification.UpdateSchedule = &msg.Config.Schedule.UpdateCron
	}
	var n Notification
	if err := n.FromModulesNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toSourcesNotification(context string, msg model.SourcesMessage) (Notification, error) {
	sources := make([]SourceEntity, 0, len(msg.Sources))
	for _, src := range msg.Sources {
		releases := make([]string, 0, len(src.Releases))
		for _, r := range src.Releases {
			if r == nil {
				continue
			}
			releases = append(releases, r.Original())
		}
		entity := SourceEntity{
			Url:      src.URL,
			Releases: &releases,
		}
		if src.Version != nil {
			entity.Version = new(src.Version.Original())
		}
		if src.ForcedVersion != "" {
			entity.ForcedVersion = &src.ForcedVersion
		}
		if src.Modules != nil {
			entity.Modules = new(src.Modules.ToSlice())
		}
		sources = append(sources, entity)
	}
	notification := SourcesNotification{
		Kind:           Sources,
		Timestamp:      time.Now().UTC(),
		NotificationId: uuid.New(),
		Context:        contextPtr(context),
		Sources:        sources,
	}
	var n Notification
	if err := n.FromSourcesNotification(notification); err != nil {
		return Notification{}, err
	}
	return n, nil
}

func toStepModuleStatuses(stepModules []*model.StateModule, step *model.Step) []StepModuleStatus {
	modules := make([]StepModuleStatus, 0, len(stepModules))
	for _, m := range stepModules {
		var metadata map[string]string
		if step != nil {
			for _, sm := range step.Modules {
				if sm.Name == m.Name {
					metadata = sm.Metadata
					break
				}
			}
		}
		s := StepModuleStatus{
			Name:           m.Name,
			AppliedVersion: m.AppliedVersion,
			Version:        m.Version,
		}
		if len(metadata) != 0 {
			s.Metadata = &metadata
		}
		modules = append(modules, s)
	}
	return modules
}

func contextPtr(context string) *string {
	if context == "" {
		return nil
	}
	return new(context)
}
