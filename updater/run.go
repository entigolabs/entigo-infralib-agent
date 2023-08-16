package updater

import (
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/codecommit"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"log"
	"os"
)

func Run(flags *common.Flags) {
	// Initialize a session using Amazon SDK
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(os.Getenv("AWS_REGION")),
	})
	if err != nil {
		log.Fatalf("Failed to initialize AWS session: %s", err)
	}

	// Get AWS account number using STS
	stsService := sts.New(sess)
	stsInput := &sts.GetCallerIdentityInput{}
	stsOutput, err := stsService.GetCallerIdentity(stsInput)
	if err != nil {
		log.Fatalf("Failed to get AWS account number: %s", err)
	}
	accountID := *stsOutput.Account

	// Create CodeCommit repository with name containing the AWS account number
	repoName := fmt.Sprintf("entigo-infralib-%s", accountID)
	codeCommitService := codecommit.New(sess)
	input := &codecommit.CreateRepositoryInput{
		RepositoryName: aws.String(repoName),
	}

	result, err := codeCommitService.CreateRepository(input)
	if err != nil {
		awsErr, ok := err.(awserr.Error)
		if ok && awsErr.Code() == "RepositoryNameExistsException" {
			// Repository already exists, log and continue
			fmt.Printf("Repository %s already exists. Continuing...\n", repoName)
		} else {
			// Any other error, fail the operation
			log.Fatalf("Failed to create CodeCommit repository: %s", err)
		}
	} else {
		fmt.Printf("Repository created with name: %s\n", *result.RepositoryMetadata.RepositoryName)
	}

	putFileInput := &codecommit.PutFileInput{
		BranchName:     aws.String("main"), // The default branch that gets created
		CommitMessage:  aws.String("Add README.md"),
		RepositoryName: aws.String(repoName),
		FileContent:    []byte("# My New Repository\nThis is the README file."),
		FilePath:       aws.String("/README.md"),
	}

	_, err = codeCommitService.PutFile(putFileInput)
	if err != nil {
		log.Fatalf("Failed to put README.md file to repository: %s", err)
	}

	fmt.Println("README.md added to repository.")
}
