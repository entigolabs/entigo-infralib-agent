package gcloud

import (
	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"context"
	"fmt"
)

type Logging struct {
	ctx      context.Context
	client   *logging.Client
	resource string
}

func NewLogging(ctx context.Context, projectId string) (*Logging, error) {
	client, err := logging.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &Logging{
		ctx:      ctx,
		client:   client,
		resource: fmt.Sprintf("projects/%s", projectId),
	}, nil
}

func (l *Logging) GetJobExecutionLogs(job string, execution string, location string) *logging.LogEntryIterator {
	filter := fmt.Sprintf(`resource.type = "cloud_run_job" resource.labels.job_name = "%s" 
		labels."run.googleapis.com/execution_name" = "%s" resource.labels.location = "%s"`, job, execution, location)
	return l.client.ListLogEntries(l.ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{l.resource},
		Filter:        filter,
	})
}
