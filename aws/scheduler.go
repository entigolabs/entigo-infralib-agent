package aws

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type Scheduler struct {
	ctx            context.Context
	updateSchedule string
	client         scheduler.Client
}

func NewScheduler(ctx context.Context, awsConfig aws.Config, prefix string) *Scheduler {
	return &Scheduler{
		ctx:            ctx,
		updateSchedule: getScheduleName(prefix, common.UpdateCommand),
		client:         *scheduler.NewFromConfig(awsConfig),
	}
}

func getScheduleName(prefix string, cmd common.Command) string {
	return model.GetAgentProjectName(model.GetAgentPrefix(prefix), cmd)
}

func getCronExpression(cron string) string {
	return fmt.Sprintf("cron(%s)", cron)
}

func (s Scheduler) getUpdateSchedule() (*scheduler.GetScheduleOutput, error) {
	schedule, err := s.client.GetSchedule(s.ctx, &scheduler.GetScheduleInput{Name: &s.updateSchedule})
	if err != nil {
		var awsError *types.ResourceNotFoundException
		if errors.As(err, &awsError) {
			return nil, nil
		}
		return nil, err
	}
	return schedule, nil
}

func (s Scheduler) createUpdateSchedule(cron, pipelineArn, roleArn string) error {
	_, err := s.client.CreateSchedule(s.ctx, &scheduler.CreateScheduleInput{
		Name:               &s.updateSchedule,
		FlexibleTimeWindow: &types.FlexibleTimeWindow{Mode: types.FlexibleTimeWindowModeOff},
		ScheduleExpression: aws.String(getCronExpression(cron)),
		Target: &types.Target{
			Arn:     &pipelineArn,
			RoleArn: &roleArn,
			RetryPolicy: &types.RetryPolicy{
				MaximumEventAgeInSeconds: aws.Int32(60),
				MaximumRetryAttempts:     aws.Int32(0),
			},
		},
	})
	if err == nil {
		log.Printf("Created EventBridge update schedule: %s\n", s.updateSchedule)
	}
	return err
}

func (s Scheduler) deleteUpdateSchedule() error {
	_, err := s.client.DeleteSchedule(s.ctx, &scheduler.DeleteScheduleInput{Name: &s.updateSchedule})
	if err != nil {
		var awsError *types.ResourceNotFoundException
		if errors.As(err, &awsError) {
			return nil
		}
	}
	if err == nil {
		log.Printf("Deleted EventBridge update schedule: %s\n", s.updateSchedule)
	}
	return err
}

func (s Scheduler) updateUpdateSchedule(cron, pipelineArn, roleArn string) error {
	_, err := s.client.UpdateSchedule(s.ctx, &scheduler.UpdateScheduleInput{
		Name:               &s.updateSchedule,
		FlexibleTimeWindow: &types.FlexibleTimeWindow{Mode: types.FlexibleTimeWindowModeOff},
		ScheduleExpression: aws.String(getCronExpression(cron)),
		Target: &types.Target{
			Arn:     &pipelineArn,
			RoleArn: &roleArn,
			RetryPolicy: &types.RetryPolicy{
				MaximumEventAgeInSeconds: aws.Int32(60),
				MaximumRetryAttempts:     aws.Int32(0),
			},
		},
	})
	if err == nil {
		log.Printf("Updated EventBridge update schedule: %s\n", s.updateSchedule)
	}
	return err
}
