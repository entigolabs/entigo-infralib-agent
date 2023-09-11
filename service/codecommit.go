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
	CreateRepository() (bool, error)
	GetLatestCommitId() (*string, error)
	GetRepoMetadata() *types.RepositoryMetadata
	PutFile(file string, content []byte)
	GetFile(file string) []byte
}

type codeCommit struct {
	codecommit     *codecommit.Client
	repo           *string
	branch         *string
	repoMetadata   *types.RepositoryMetadata
	parentCommitId *string
}

func NewCodeCommit(awsConfig aws.Config, repoName string, branchName string) CodeCommit {
	return &codeCommit{
		codecommit: codecommit.NewFromConfig(awsConfig),
		repo:       aws.String(repoName),
		branch:     aws.String(branchName),
	}
}

func (c *codeCommit) CreateRepository() (bool, error) {
	input := &codecommit.CreateRepositoryInput{
		RepositoryName: c.repo,
	}
	result, err := c.codecommit.CreateRepository(context.Background(), input)
	if err != nil {
		var awsError *types.RepositoryNameExistsException
		if errors.As(err, &awsError) {
			common.Logger.Printf("Repository %s already exists. Continuing...\n", *c.repo)
			c.parentCommitId, err = c.GetLatestCommitId()
			c.repoMetadata = c.GetRepoMetadata()
			return false, err
		} else {
			common.Logger.Fatalf("Failed to create CodeCommit repository: %s", err)
			return false, err
		}
	} else {
		common.Logger.Printf("Repository created with name: %s\n", *result.RepositoryMetadata.RepositoryName)
		c.repoMetadata = result.RepositoryMetadata
		return true, nil
	}
}

func (c *codeCommit) GetRepoMetadata() *types.RepositoryMetadata {
	if c.repoMetadata != nil {
		return c.repoMetadata
	}
	result, err := c.codecommit.GetRepository(context.Background(), &codecommit.GetRepositoryInput{
		RepositoryName: c.repo,
	})
	if err != nil {
		common.Logger.Fatalf("Failed to get CodeCommit repository: %s", err)
	}
	return result.RepositoryMetadata
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
	putFileOutput, err := c.codecommit.PutFile(context.Background(), &codecommit.PutFileInput{
		BranchName:     c.branch,
		CommitMessage:  aws.String(fmt.Sprintf("Add %s", file)),
		RepositoryName: c.repo,
		FileContent:    content,
		FilePath:       aws.String(file),
		ParentCommitId: c.parentCommitId,
	})
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

func (c *codeCommit) GetFile(file string) []byte {
	common.Logger.Printf("Getting file %s from repository\n", file)
	output, err := c.codecommit.GetFile(context.Background(), &codecommit.GetFileInput{
		CommitSpecifier: c.branch,
		RepositoryName:  c.repo,
		FilePath:        aws.String(file),
	})
	if err != nil {
		var awsError *types.FileDoesNotExistException
		if errors.As(err, &awsError) {
			return nil
		}
		common.Logger.Fatalf("Failed to get %s from repository: %s", file, err)
	}
	return output.FileContent
}
