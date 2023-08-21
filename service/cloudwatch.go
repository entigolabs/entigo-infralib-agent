package service

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
	common.Logger.Printf("Creating log group %s\n", logGroupName)
	_, err := c.cloudwatchlogs.CreateLogGroup(context.Background(), &cloudwatchlogs.CreateLogGroupInput{
		LogGroupName: aws.String(logGroupName),
	})
	if err != nil {
		var awsError *types.ResourceAlreadyExistsException
		if errors.As(err, &awsError) {
			common.Logger.Printf("Log group %s already exists. Continuing...\n", logGroupName)
		} else {
			return "", err
		}
	}
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
	common.Logger.Printf("Creating log stream %s\n", logStreamName)
	_, err := c.cloudwatchlogs.CreateLogStream(context.Background(), &cloudwatchlogs.CreateLogStreamInput{
		LogGroupName:  aws.String(logGroupName),
		LogStreamName: aws.String(logStreamName),
	})
	var awsError *types.ResourceAlreadyExistsException
	if err != nil && errors.As(err, &awsError) {
		common.Logger.Printf("Log group stream %s already exists. Continuing...\n", logGroupName)
		return nil
	}
	return err
}
