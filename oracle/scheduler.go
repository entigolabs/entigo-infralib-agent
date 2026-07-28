package oracle

import (
	"context"
	"fmt"
	"log"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/resourcescheduler"
)

// Scheduler manages the OCI Resource Schedule that invokes the scheduler Function
// on cron. Resource Scheduler is the only native cron in OCI, and among its targets
// only a Function can run arbitrary logic — so the schedule's START_RESOURCE action
// on the function OCID is what fires the agent-update pipeline. The schedule is
// found-or-created by prefixed display name (same name AWS/GCloud use).
type Scheduler struct {
	ctx           context.Context
	client        resourcescheduler.ScheduleClient
	compartmentId string
	scheduleName  string
}

func NewScheduler(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, cloudPrefix string) (*Scheduler, error) {
	client, err := resourcescheduler.NewScheduleClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
	}
	return &Scheduler{
		ctx:           ctx,
		client:        client,
		compartmentId: compartmentId,
		scheduleName:  getScheduleName(cloudPrefix),
	}, nil
}

func getScheduleName(cloudPrefix string) string {
	return model.GetAgentProjectName(model.GetAgentPrefix(cloudPrefix), common.UpdateCommand)
}

// getUpdateSchedule returns the existing update schedule, or nil if none exists.
func (s *Scheduler) getUpdateSchedule() (*resourcescheduler.ScheduleSummary, error) {
	list, err := s.client.ListSchedules(s.ctx, resourcescheduler.ListSchedulesRequest{
		CompartmentId: &s.compartmentId,
		DisplayName:   &s.scheduleName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list schedules: %w", err)
	}
	for _, schedule := range list.Items {
		if schedule.LifecycleState != resourcescheduler.ScheduleLifecycleStateDeleted &&
			schedule.LifecycleState != resourcescheduler.ScheduleLifecycleStateDeleting {
			return &schedule, nil
		}
	}
	return nil, nil
}

func (s *Scheduler) createUpdateSchedule(cron, functionId string) error {
	_, err := s.client.CreateSchedule(s.ctx, resourcescheduler.CreateScheduleRequest{
		CreateScheduleDetails: resourcescheduler.CreateScheduleDetails{
			CompartmentId:     &s.compartmentId,
			DisplayName:       &s.scheduleName,
			Action:            resourcescheduler.CreateScheduleDetailsActionStartResource,
			RecurrenceType:    resourcescheduler.CreateScheduleDetailsRecurrenceTypeCron,
			RecurrenceDetails: &cron,
			Resources:         []resourcescheduler.Resource{{Id: &functionId}},
			FreeformTags:      map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create schedule %s: %w", s.scheduleName, err)
	}
	log.Printf("Created resource schedule %s\n", s.scheduleName)
	return nil
}

func (s *Scheduler) updateUpdateSchedule(scheduleId, cron, functionId string) error {
	_, err := s.client.UpdateSchedule(s.ctx, resourcescheduler.UpdateScheduleRequest{
		ScheduleId: &scheduleId,
		UpdateScheduleDetails: resourcescheduler.UpdateScheduleDetails{
			Action:            resourcescheduler.UpdateScheduleDetailsActionStartResource,
			RecurrenceType:    resourcescheduler.UpdateScheduleDetailsRecurrenceTypeCron,
			RecurrenceDetails: &cron,
			Resources:         []resourcescheduler.Resource{{Id: &functionId}},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update schedule %s: %w", s.scheduleName, err)
	}
	log.Printf("Updated resource schedule %s\n", s.scheduleName)
	return nil
}

func (s *Scheduler) deleteUpdateSchedule(scheduleId string) error {
	_, err := s.client.DeleteSchedule(s.ctx, resourcescheduler.DeleteScheduleRequest{ScheduleId: &scheduleId})
	if err != nil {
		return fmt.Errorf("failed to delete schedule %s: %w", s.scheduleName, err)
	}
	log.Printf("Deleted resource schedule %s\n", s.scheduleName)
	return nil
}
