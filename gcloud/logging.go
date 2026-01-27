package gcloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"time"

	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Logging struct {
	ctx      context.Context
	client   *logging.Client
	resource string
}

func NewLogging(ctx context.Context, options []option.ClientOption, projectId string) (*Logging, error) {
	client, err := logging.NewClient(ctx, options...)
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
		OrderBy:       "timestamp desc",
	})
}

func (l *Logging) GetLogRow(logIterator *logging.LogEntryIterator) (string, error) {
	backoff := 2
	var err error
	var entry *loggingpb.LogEntry
	ctx, cancel := context.WithTimeout(l.ctx, 90*time.Second)
	defer cancel()
	first := true

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", errors.New("reading log entries timed out")
			}
			return "", ctx.Err()
		default:
			entry, err = logIterator.Next()
			if err == nil {
				return entry.GetTextPayload(), nil
			}
			if status.Code(err) == codes.ResourceExhausted {
				if first {
					slog.Error(common.PrefixError(err))
					first = false
				}
				log.Printf("Log api resource exhausted, retrying in %d seconds", backoff)
				time.Sleep(time.Duration(backoff) * time.Second)
				backoff = util.MinInt(16, backoff*2)
				continue
			}
			return "", err
		}
	}
}
