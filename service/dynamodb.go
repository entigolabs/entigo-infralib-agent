package service

import (
	"context"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

func CreateDynamoDBTable(awsConfig aws.Config, tableName string) (string, error) {
	common.Logger.Printf("Creating DynamoDB table %s\n", tableName)
	dynamodbClient := dynamodb.NewFromConfig(awsConfig)
	table, err := dynamodbClient.CreateTable(context.Background(), &dynamodb.CreateTableInput{
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
			common.Logger.Printf("DynamoDB table %s already exists. Continuing...\n", tableName)
			return GetDynamoDBTable(dynamodbClient, tableName)
		} else {
			return "", err
		}
	}
	return *table.TableDescription.TableArn, nil
}

func GetDynamoDBTable(client *dynamodb.Client, tableName string) (string, error) {
	table, err := client.DescribeTable(context.Background(), &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	})
	if err != nil {
		return "", err
	}
	return *table.Table.TableArn, nil
}
