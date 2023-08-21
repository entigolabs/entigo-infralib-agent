package service

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codecommit"
	"github.com/aws/aws-sdk-go-v2/service/codecommit/types"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

type CodeCommit interface {
	CreateRepository() error
	GetLatestCommitId() (*string, error)
	GetRepoArn() *string
	PutFile(file string, content []byte)
}

type codeCommit struct {
	codecommit     *codecommit.Client
	repo           *string
	repoArn        *string
	branch         *string
	parentCommitId *string
}

func NewCodeCommit(awsConfig aws.Config, repoName string, branchName string) CodeCommit {
	return &codeCommit{
		codecommit: codecommit.NewFromConfig(awsConfig),
		repo:       aws.String(repoName),
		branch:     aws.String(branchName),
	}
}

func (c *codeCommit) CreateRepository() error {
	input := &codecommit.CreateRepositoryInput{
		RepositoryName: c.repo,
	}
	result, err := c.codecommit.CreateRepository(context.Background(), input)
	if err != nil {
		var awsError *types.RepositoryNameExistsException
		if errors.As(err, &awsError) {
			common.Logger.Printf("Repository %s already exists. Continuing...\n", *c.repo)
			c.parentCommitId, err = c.GetLatestCommitId()
			c.repoArn = c.GetRepoArn()
			return err
		} else {
			common.Logger.Fatalf("Failed to create CodeCommit repository: %s", err)
			return err
		}
	} else {
		common.Logger.Printf("Repository created with name: %s\n", *result.RepositoryMetadata.RepositoryName)
		c.repoArn = result.RepositoryMetadata.Arn
		return nil
	}
}

func (c *codeCommit) GetRepoArn() *string {
	if c.repoArn != nil {
		return c.repoArn
	}
	result, err := c.codecommit.GetRepository(context.Background(), &codecommit.GetRepositoryInput{
		RepositoryName: c.repo,
	})
	if err != nil {
		common.Logger.Fatalf("Failed to get CodeCommit repository: %s", err)
	}
	return result.RepositoryMetadata.Arn
}

func (c *codeCommit) GetLatestCommitId() (*string, error) {
	input := &codecommit.GetBranchInput{
		RepositoryName: c.repo,
		BranchName:     c.branch,
	}

	result, err := c.codecommit.GetBranch(context.Background(), input)
	if err != nil {
		return nil, err
	}

	return result.Branch.CommitId, nil
}

func (c *codeCommit) PutFile(file string, content []byte) {
	common.Logger.Printf("Adding file %s to repository\n", file)
	putFileInput := &codecommit.PutFileInput{
		BranchName:     c.branch,
		CommitMessage:  aws.String(fmt.Sprintf("Add %s", file)),
		RepositoryName: c.repo,
		FileContent:    content,
		FilePath:       aws.String(file),
		ParentCommitId: c.parentCommitId,
	}

	putFileOutput, err := c.codecommit.PutFile(context.Background(), putFileInput)
	if err != nil {
		var awsError *types.SameFileContentException
		if errors.As(err, &awsError) {
			common.Logger.Printf("%s already exists in repository. Continuing...\n", file)
			return
		} else {
			common.Logger.Fatalf("Failed to put file %s to repository: %s", file, err)
		}
	}
	c.parentCommitId = putFileOutput.CommitId
}
