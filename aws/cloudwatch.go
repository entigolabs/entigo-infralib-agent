package aws

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"log"
)

type CloudWatch interface {
	GetLogGroup(logGroupName string) (string, error)
	CreateLogGroup(logGroupName string) (string, error)
	LogStreamExists(logGroupName string, logStreamName string) (bool, error)
	CreateLogStream(logGroupName string, logStreamName string) error
	GetLogs(logGroupName string, logStreamName string) ([]string, error)
	DeleteLogGroup(logGroupName string) error
	DeleteLogStream(logGroupName, logStreamName string) error
}

type cloudWatch struct {
	ctx            context.Context
	cloudwatchlogs *cloudwatchlogs.Client
}

func NewCloudWatch(ctx context.Context, awsConfig aws.Config) CloudWatch {
	return &cloudWatch{
		ctx:            ctx,
		cloudwatchlogs: cloudwatchlogs.NewFromConfig(awsConfig),
	}
}

func (c *cloudWatch) GetLogGroup(logGroupName string) (string, error) {
	groups, err := c.cloudwatchlogs.DescribeLogGroups(c.ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(logGroupName),
	})
	if err != nil {
		var awsError *types.ResourceNotFoundException
		if errors.As(err, &awsError) {
			return "", nil
		}
		return "", err
	}
	if len(groups.LogGroups) == 0 {
		return "", nil
	}
	return *groups.LogGroups[0].Arn, nil
}

func (c *cloudWatch) CreateLogGroup(logGroupName string) (string, error) {
	_, err := c.cloudwatchlogs.CreateLogGroup(c.ctx, &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(logGroupName),
	})
	if err != nil {
		var awsError *types.ResourceAlreadyExistsException
		if errors.As(err, &awsError) {
			return c.getLogGroup(logGroupName)
		}
		return "", err
	}
	log.Printf("Created log group %s\n", logGroupName)
	return c.getLogGroup(logGroupName)
}

func (c *cloudWatch) getLogGroup(logGroupName string) (string, error) {
	groups, err := c.cloudwatchlogs.DescribeLogGroups(c.ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		LogGroupNamePrefix: aws.String(logGroupName),
	})
	if err != nil {
		return "", err
	}
	if len(groups.LogGroups) != 1 {
		return "", fmt.Errorf("expected 1 log group, got %d", len(groups.LogGroups))
	}
	return *groups.LogGroups[0].Arn, nil
}

func (c *cloudWatch) LogStreamExists(logGroupName string, logStreamName string) (bool, error) {
	streams, err := c.cloudwatchlogs.DescribeLogStreams(c.ctx, &cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName:        aws.String(logGroupName),
		LogStreamNamePrefix: aws.String(logStreamName),
	})
	if err != nil {
		var awsError *types.ResourceNotFoundException
		if errors.As(err, &awsError) {
			return false, nil
		}
		return false, err
	}
	return len(streams.LogStreams) > 0, nil
}

func (c *cloudWatch) CreateLogStream(logGroupName string, logStreamName string) error {
	_, err := c.cloudwatchlogs.CreateLogStream(c.ctx, &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
	})
	var awsError *types.ResourceAlreadyExistsException
	if err != nil && errors.As(err, &awsError) {
		return nil
	}
	log.Printf("Created log stream %s\n", logStreamName)
	return err
}

func (c *cloudWatch) GetLogs(logGroupName string, logStreamName string) ([]string, error) {
	response, err := c.cloudwatchlogs.GetLogEvents(c.ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
	})
	if err != nil {
		return nil, err
	}
	var logs []string
	for _, event := range response.Events {
		logs = append(logs, *event.Message)
	}
	return logs, nil
}

func (c *cloudWatch) DeleteLogGroup(logGroupName string) error {
	_, err := c.cloudwatchlogs.DeleteLogGroup(c.ctx, &cloudwatchlogs.DeleteLogGroupInput{
		LogGroupName: aws.String(logGroupName),
	})
	if err != nil {
		var awsError *types.ResourceNotFoundException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	log.Printf("Deleted log group %s\n", logGroupName)
	return nil
}

func (c *cloudWatch) DeleteLogStream(logGroupName, logStreamName string) error {
	_, err := c.cloudwatchlogs.DeleteLogStream(c.ctx, &cloudwatchlogs.DeleteLogStreamInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
	})
	if err != nil {
		var awsError *types.ResourceNotFoundException
		if errors.As(err, &awsError) {
			return nil
		}
		return err
	}
	log.Printf("Deleted log stream %s\n", logStreamName)
	return nil
}
