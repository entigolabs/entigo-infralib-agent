package aws

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

type CloudWatch interface {
	CreateLogGroup(logGroupName string) (string, error)
	CreateLogStream(logGroupName string, logStreamName string) error
	GetLogs(logGroupName string, logStreamName string, limit int32) ([]string, error)
}

type cloudWatch struct {
	cloudwatchlogs *cloudwatchlogs.Client
}

func NewCloudWatch(awsConfig aws.Config) CloudWatch {
	return &cloudWatch{
		cloudwatchlogs: cloudwatchlogs.NewFromConfig(awsConfig),
	}
}

func (c *cloudWatch) CreateLogGroup(logGroupName string) (string, error) {
	_, err := c.cloudwatchlogs.CreateLogGroup(context.Background(), &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(logGroupName),
	})
	if err != nil {
		var awsError *types.ResourceAlreadyExistsException
		if errors.As(err, &awsError) {
			return c.GetLogGroup(logGroupName)
		} else {
			return "", err
		}
	}
	common.Logger.Printf("Created log group %s\n", logGroupName)
	return c.GetLogGroup(logGroupName)
}

func (c *cloudWatch) GetLogGroup(logGroupName string) (string, error) {
	groups, err := c.cloudwatchlogs.DescribeLogGroups(context.Background(), &cloudwatchlogs.DescribeLogGroupsInput{
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

func (c *cloudWatch) CreateLogStream(logGroupName string, logStreamName string) error {
	_, err := c.cloudwatchlogs.CreateLogStream(context.Background(), &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
	})
	var awsError *types.ResourceAlreadyExistsException
	if err != nil && errors.As(err, &awsError) {
		return nil
	}
	common.Logger.Printf("Created log stream %s\n", logStreamName)
	return err
}

func (c *cloudWatch) GetLogs(logGroupName string, logStreamName string, limit int32) ([]string, error) {
	response, err := c.cloudwatchlogs.GetLogEvents(context.Background(), &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
		Limit:         aws.Int32(limit),
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
