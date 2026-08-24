package gcloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	scheduler "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
)

var locationPrefixFallbacks = map[string]string{
	"africa":       "europe-west1", // Currently unsupported, fallback to nearest continent
	"asia":         "asia-east1",
	"australia":    "australia-southeast1",
	"eu":           "europe-west1",
	"europe":       "europe-west1",
	"me":           "me-central2",
	"northamerica": "northamerica-northeast1",
	"southamerica": "southamerica-east1",
	"us":           "us-central1",
}

type Scheduler struct {
	ctx                context.Context
	updateScheduleName string
	updateSchedule     string
	client             *scheduler.CloudSchedulerClient
	project            string
	location           string
	schedulerLocation  string
}

func NewScheduler(ctx context.Context, options []option.ClientOption, project, location, prefix string) (*Scheduler, error) {
	client, err := scheduler.NewCloudSchedulerClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	schedulerLocation, err := getValidSchedulerLocation(ctx, client, project, location)
	if err != nil {
		return nil, err
	}
	return &Scheduler{
		ctx:                ctx,
		updateScheduleName: getScheduleName(prefix, common.UpdateCommand),
		updateSchedule:     getScheduleFullName(prefix, project, schedulerLocation, common.UpdateCommand),
		client:             client,
		project:            project,
		location:           location,
		schedulerLocation:  schedulerLocation,
	}, nil
}

func getScheduleName(prefix string, cmd common.Command) string {
	return model.GetAgentProjectName(model.GetAgentPrefix(prefix), cmd)
}

func getScheduleFullName(prefix, project, location string, cmd common.Command) string {
	return fmt.Sprintf("projects/%s/locations/%s/jobs/%s", project, location, getScheduleName(prefix, cmd))
}

func getValidSchedulerLocation(ctx context.Context, client *scheduler.CloudSchedulerClient, project, location string) (string, error) {
	supportedLocations := model.NewSet[string]()
	it := client.ListLocations(ctx, &locationpb.ListLocationsRequest{
		Name: fmt.Sprintf("projects/%s", project),
	})
	for {
		loc, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("failed to list locations: %w", err)
		}
		supportedLocations.Add(loc.LocationId)
	}
	if supportedLocations.Contains(location) {
		return location, nil
	}
	prefix := strings.Split(location, "-")[0]
	if fallback, ok := locationPrefixFallbacks[prefix]; ok {
		if supportedLocations.Contains(fallback) {
			return fallback, nil
		}
		return "", fmt.Errorf("location %q is not supported and its fallback %q is also not listed", location, fallback)
	}
	return "", fmt.Errorf("location %q is not supported and no fallback rule exists", location)
}

func (s *Scheduler) getUpdateSchedule() (*schedulerpb.Job, error) {
	job, err := s.client.GetJob(s.ctx, &schedulerpb.GetJobRequest{Name: s.updateSchedule})
	if err != nil {
		if isNotFoundError(err) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

func (s *Scheduler) createUpdateSchedule(cron, agentJob, serviceAccount string) error {
	_, err := s.client.CreateJob(s.ctx, &schedulerpb.CreateJobRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", s.project, s.schedulerLocation),
		Job:    s.job(cron, agentJob, serviceAccount),
	})
	if err == nil {
		log.Printf("Created Cloud Scheduler job: %s\n", s.updateScheduleName)
	}
	return err
}

func (s *Scheduler) deleteUpdateSchedule() error {
	err := s.client.DeleteJob(s.ctx, &schedulerpb.DeleteJobRequest{Name: s.updateSchedule})
	if err == nil {
		log.Printf("Deleted Cloud Scheduler job: %s\n", s.updateScheduleName)
		return nil
	}
	if isNotFoundError(err) {
		return nil
	}
	return err
}

func (s *Scheduler) updateUpdateSchedule(cron, agentJob, serviceAccount string) error {
	_, err := s.client.UpdateJob(s.ctx, &schedulerpb.UpdateJobRequest{
		Job: s.job(cron, agentJob, serviceAccount),
	})
	if err == nil {
		log.Printf("Updated Cloud Scheduler job: %s\n", s.updateScheduleName)
	}
	return err
}

func (s *Scheduler) job(cron, agentJob, serviceAccount string) *schedulerpb.Job {
	runUri := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s:run",
		s.project, s.location, agentJob)
	return &schedulerpb.Job{
		Name:     s.updateSchedule,
		Schedule: cron,
		Target: &schedulerpb.Job_HttpTarget{
			HttpTarget: &schedulerpb.HttpTarget{
				Uri:        runUri,
				HttpMethod: schedulerpb.HttpMethod_POST,
				AuthorizationHeader: &schedulerpb.HttpTarget_OauthToken{
					OauthToken: &schedulerpb.OAuthToken{
						ServiceAccountEmail: serviceAccount,
						Scope:               "https://www.googleapis.com/auth/cloud-platform",
					},
				},
			},
		},
		RetryConfig: &schedulerpb.RetryConfig{
			RetryCount: 0,
		},
	}
}
