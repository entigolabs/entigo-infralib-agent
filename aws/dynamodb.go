package aws

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

func CreateDynamoDBTable(ctx context.Context, awsConfig aws.Config, tableName string) (*types.TableDescription, error) {
	dynamodbClient := dynamodb.NewFromConfig(awsConfig)
	table, err := dynamodbClient.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(tableName),
		BillingMode: types.BillingModePayPerRequest,
		KeySchema: []types.KeySchemaElement{{
			AttributeName: aws.String("LockID"),
			KeyType:       types.KeyTypeHash,
		}},
		AttributeDefinitions: []types.AttributeDefinition{{
			AttributeName: aws.String("LockID"),
			AttributeType: types.ScalarAttributeTypeS,
		}},
		Tags: []types.Tag{{
			Key:   aws.String(model.ResourceTagKey),
			Value: aws.String(model.ResourceTagValue),
		}},
	})
	if err != nil {
		var resourceError *types.ResourceInUseException
		var tableError *types.TableAlreadyExistsException
		if errors.As(err, &tableError) || errors.As(err, &resourceError) {
			return GetExistingDynamoDBTable(ctx, dynamodbClient, tableName)
		} else {
			return nil, err
		}
	}
	log.Printf("Created DynamoDB table %s\n", tableName)
	err = pollUntilTableActive(ctx, dynamodbClient, tableName)
	if err != nil {
		return nil, err
	}
	return table.TableDescription, nil
}

func pollUntilTableActive(ctx context.Context, client *dynamodb.Client, name string) error {
	wait := 3
	for {
		select {
		case <-ctx.Done():
			return errors.New("context cancelled while waiting for DynamoDB table to become active")
		case <-time.After(time.Duration(wait) * time.Second):
			table, err := GetExistingDynamoDBTable(ctx, client, name)
			if err != nil {
				return err
			}
			if table.TableStatus == types.TableStatusActive {
				return nil
			}
			log.Printf("Waiting for DynamoDB table %s to become active\n", name)
		}
	}
}

func DeleteDynamoDBTable(ctx context.Context, awsConfig aws.Config, tableName string) error {
	client := dynamodb.NewFromConfig(awsConfig)
	_, err := client.DeleteTable(ctx, &dynamodb.DeleteTableInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		var resourceError *types.ResourceNotFoundException
		if errors.As(err, &resourceError) {
			return nil
		}
		return err
	}
	log.Printf("Deleted DynamoDB table %s\n", tableName)
	return nil
}

func GetDynamoDBTable(ctx context.Context, awsConfig aws.Config, tableName string) (*types.TableDescription, error) {
	dynamodbClient := dynamodb.NewFromConfig(awsConfig)
	table, err := GetExistingDynamoDBTable(ctx, dynamodbClient, tableName)
	if err != nil {
		var resourceError *types.ResourceNotFoundException
		if errors.As(err, &resourceError) {
			return nil, nil
		}
		return nil, err
	}
	return table, nil
}

func GetExistingDynamoDBTable(ctx context.Context, client *dynamodb.Client, tableName string) (*types.TableDescription, error) {
	table, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		return nil, err
	}
	return table.Table, nil
}
