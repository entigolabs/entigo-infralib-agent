package aws

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"log"
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
	return table.TableDescription, nil
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
