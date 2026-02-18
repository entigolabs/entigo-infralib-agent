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
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type Logging struct {
	ctx          context.Context
	client       *logging.Client
	configClient *logging.ConfigClient
	resource     string
	projectId    string
	location     string
	logBucketId  string
}

func NewLogging(ctx context.Context, options []option.ClientOption, projectId, location string) (*Logging, error) {
	client, err := logging.NewClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	configClient, err := logging.NewConfigClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	return &Logging{
		ctx:          ctx,
		client:       client,
		configClient: configClient,
		resource:     fmt.Sprintf("projects/%s", projectId),
		projectId:    projectId,
		location:     location,
	}, nil
}

func (l *Logging) GetJobExecutionLogs(job string, execution string, location string) *logging.LogEntryIterator {
	resourceName := l.resource
	if l.logBucketId != "" {
		resourceName = fmt.Sprintf("projects/%s/locations/%s/buckets/%s/views/_AllLogs", l.projectId, l.location, l.logBucketId)
	}
	filter := fmt.Sprintf(`resource.type = "cloud_run_job" resource.labels.job_name = "%s"
		labels."run.googleapis.com/execution_name" = "%s" resource.labels.location = "%s"`, job, execution, location)
	return l.client.ListLogEntries(l.ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{resourceName},
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

func (l *Logging) CreateLogBucket(bucketId, kmsKeyName string) error {
	existing, err := l.getLogBucket(bucketId)
	if err != nil {
		return err
	}
	l.logBucketId = bucketId
	if existing != nil {
		return nil
	}
	bucket := &loggingpb.LogBucket{
		Description:   "Entigo infralib agent logs",
		RetentionDays: 30,
	}
	if kmsKeyName != "" {
		bucket.CmekSettings = &loggingpb.CmekSettings{
			KmsKeyName: kmsKeyName,
		}
	}
	_, err = l.configClient.CreateBucket(l.ctx, &loggingpb.CreateBucketRequest{
		Parent:   fmt.Sprintf("projects/%s/locations/%s", l.projectId, l.location),
		BucketId: bucketId,
		Bucket:   bucket,
	})
	if err != nil {
		return fmt.Errorf("failed to create log bucket %s: %w", bucketId, err)
	}
	log.Printf("Created log bucket %s\n", bucketId)
	return nil
}

func (l *Logging) getLogBucket(bucketId string) (*loggingpb.LogBucket, error) {
	bucket, err := l.configClient.GetBucket(l.ctx, &loggingpb.GetBucketRequest{
		Name: fmt.Sprintf("projects/%s/locations/%s/buckets/%s", l.projectId, l.location, bucketId),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return bucket, nil
}

func (l *Logging) CreateLogSink(sinkName, bucketId, filter string) error {
	existing, err := l.getLogSink(sinkName)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	destination := fmt.Sprintf("logging.googleapis.com/projects/%s/locations/%s/buckets/%s", l.projectId, l.location, bucketId)
	_, err = l.configClient.CreateSink(l.ctx, &loggingpb.CreateSinkRequest{
		Parent: l.resource,
		Sink: &loggingpb.LogSink{
			Name:        sinkName,
			Destination: destination,
			Filter:      filter,
			Description: "Entigo infralib agent log sink",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create log sink %s: %w", sinkName, err)
	}
	log.Printf("Created log sink %s\n", sinkName)
	return nil
}

func (l *Logging) UpdateLogSinkDestination(sinkName, bucketId string) error {
	sinkFullName := fmt.Sprintf("projects/%s/sinks/%s", l.projectId, sinkName)
	destination := fmt.Sprintf("logging.googleapis.com/projects/%s/locations/%s/buckets/%s", l.projectId, l.location, bucketId)
	_, err := l.configClient.UpdateSink(l.ctx, &loggingpb.UpdateSinkRequest{
		SinkName: sinkFullName,
		Sink: &loggingpb.LogSink{
			Name:        sinkName,
			Destination: destination,
		},
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{"destination"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update log sink %s destination: %w", sinkName, err)
	}
	log.Printf("Updated log sink %s destination to bucket %s\n", sinkName, bucketId)
	return nil
}

func (l *Logging) getLogSink(sinkName string) (*loggingpb.LogSink, error) {
	sink, err := l.configClient.GetSink(l.ctx, &loggingpb.GetSinkRequest{
		SinkName: fmt.Sprintf("projects/%s/sinks/%s", l.projectId, sinkName),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return sink, nil
}

func (l *Logging) CreateDefaultSinkExclusion(exclusionName, filter string) error {
	_, err := l.configClient.GetExclusion(l.ctx, &loggingpb.GetExclusionRequest{
		Name: fmt.Sprintf("projects/%s/exclusions/%s", l.projectId, exclusionName),
	})
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}
	_, err = l.configClient.CreateExclusion(l.ctx, &loggingpb.CreateExclusionRequest{
		Parent: l.resource,
		Exclusion: &loggingpb.LogExclusion{
			Name:        exclusionName,
			Description: "Exclude entigo infralib agent logs from _Default sink",
			Filter:      filter,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create exclusion %s: %w", exclusionName, err)
	}
	log.Printf("Created _Default sink exclusion %s\n", exclusionName)
	return nil
}

func (l *Logging) DeleteLogResources(bucketIds []string, sinkName, exclusionName string) {
	err := l.configClient.DeleteExclusion(l.ctx, &loggingpb.DeleteExclusionRequest{
		Name: fmt.Sprintf("projects/%s/exclusions/%s", l.projectId, exclusionName),
	})
	if err != nil && !isNotFound(err) {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete exclusion %s: %s", exclusionName, err)))
	}
	err = l.configClient.DeleteSink(l.ctx, &loggingpb.DeleteSinkRequest{
		SinkName: fmt.Sprintf("projects/%s/sinks/%s", l.projectId, sinkName),
	})
	if err != nil && !isNotFound(err) {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete log sink %s: %s", sinkName, err)))
	}
	for _, bucketId := range bucketIds {
		err = l.configClient.DeleteBucket(l.ctx, &loggingpb.DeleteBucketRequest{
			Name: fmt.Sprintf("projects/%s/locations/%s/buckets/%s", l.projectId, l.location, bucketId),
		})
		if err != nil && !isNotFound(err) {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to delete log bucket %s: %s", bucketId, err)))
		}
	}
}

func isNotFound(err error) bool {
	var apiError *apierror.APIError
	if errors.As(err, &apiError) && apiError.GRPCStatus().Code() == codes.NotFound {
		return true
	}
	return status.Code(err) == codes.NotFound
}
