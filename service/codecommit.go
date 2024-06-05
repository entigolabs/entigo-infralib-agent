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

type codeCommit struct {
	codecommit     *codecommit.Client
	repo           *string
	branch         *string
	repoMetadata   *types.RepositoryMetadata
	parentCommitId *string
}

func NewCodeCommit(awsConfig aws.Config, repoName string, branchName string) CodeRepo {
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
			c.parentCommitId, err = c.GetLatestCommitId()
			if err != nil {
				return false, err
			}
			c.repoMetadata, err = c.GetRepoMetadata()
			return false, err
		} else {
			common.Logger.Fatalf("Failed to create CodeRepo repository: %s", err)
			return false, err
		}
	} else {
		common.Logger.Printf("Repository created with name: %s\n", *result.RepositoryMetadata.RepositoryName)
		c.repoMetadata = result.RepositoryMetadata
		return true, nil
	}
}

func (c *codeCommit) GetRepoMetadata() (*types.RepositoryMetadata, error) {
	if c.repoMetadata != nil {
		return c.repoMetadata, nil
	}
	result, err := c.codecommit.GetRepository(context.Background(), &codecommit.GetRepositoryInput{
		RepositoryName: c.repo,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get CodeRepo repository: %w", err)
	}
	return result.RepositoryMetadata, nil
}

func (c *codeCommit) GetLatestCommitId() (*string, error) {
	result, err := c.codecommit.GetBranch(context.Background(), &codecommit.GetBranchInput{
		RepositoryName: c.repo,
		BranchName:     c.branch,
	})
	if err != nil {
		return nil, err
	}
	return result.Branch.CommitId, nil
}

func (c *codeCommit) updateLatestCommitId() error {
	commitId, err := c.GetLatestCommitId()
	if err != nil {
		return err
	}
	c.parentCommitId = commitId
	return nil
}

func (c *codeCommit) PutFile(file string, content []byte) error {
	for {
		putFileOutput, err := c.codecommit.PutFile(context.Background(), &codecommit.PutFileInput{
			BranchName:     c.branch,
			CommitMessage:  aws.String(fmt.Sprintf("Add %s", file)),
			RepositoryName: c.repo,
			FileContent:    content,
			FilePath:       aws.String(file),
			ParentCommitId: c.parentCommitId,
		})
		if err != nil {
			var sameFileError *types.SameFileContentException
			var commitIdError *types.ParentCommitIdOutdatedException
			if errors.As(err, &sameFileError) {
				return nil
			} else if errors.As(err, &commitIdError) {
				err = c.updateLatestCommitId()
				if err != nil {
					return fmt.Errorf("failed to update latest commit id: %w", err)
				}
				continue
			}
			return fmt.Errorf("failed to put file %s to repository: %w", file, err)
		}
		common.Logger.Printf("Added file %s to repository\n", file)
		c.parentCommitId = putFileOutput.CommitId
		return nil
	}
}

func (c *codeCommit) GetFile(file string) ([]byte, error) {
	common.Logger.Printf("Getting file %s from repository\n", file)
	output, err := c.codecommit.GetFile(context.Background(), &codecommit.GetFileInput{
		CommitSpecifier: c.branch,
		RepositoryName:  c.repo,
		FilePath:        aws.String(file),
	})
	if err != nil {
		var awsError *types.FileDoesNotExistException
		if errors.As(err, &awsError) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get %s from repository: %w", file, err)
	}
	return output.FileContent, nil
}

func (c *codeCommit) DeleteFile(file string) error {
	for {
		deleteFileOutput, err := c.codecommit.DeleteFile(context.Background(), &codecommit.DeleteFileInput{
			BranchName:     c.branch,
			CommitMessage:  aws.String(fmt.Sprintf("Delete %s", file)),
			RepositoryName: c.repo,
			FilePath:       aws.String(file),
			ParentCommitId: c.parentCommitId,
		})
		if err != nil {
			var awsError *types.FileDoesNotExistException
			var commitIdError *types.ParentCommitIdOutdatedException
			if errors.As(err, &awsError) {
				return nil
			} else if errors.As(err, &commitIdError) {
				err = c.updateLatestCommitId()
				if err != nil {
					return fmt.Errorf("failed to update latest commit id: %w", err)
				}
				continue
			} else {
				return fmt.Errorf("failed to delete %s from repository: %w", file, err)
			}
		}
		common.Logger.Printf("Deleted file %s from repository\n", file)
		c.parentCommitId = deleteFileOutput.CommitId
		return nil
	}
}

func (c *codeCommit) CheckFolderExists(folder string) (bool, error) {
	output, err := c.codecommit.GetFolder(context.Background(), &codecommit.GetFolderInput{
		CommitSpecifier: c.branch,
		RepositoryName:  c.repo,
		FolderPath:      aws.String(folder),
	})
	if err != nil {
		var awsError *types.FolderDoesNotExistException
		if errors.As(err, &awsError) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get %s from repository: %w", folder, err)
	}
	return len(output.Files) > 0 || len(output.SubFolders) > 0, nil
}

func (c *codeCommit) ListFolderFiles(folder string) ([]string, error) {
	output, err := c.codecommit.GetFolder(context.Background(), &codecommit.GetFolderInput{
		CommitSpecifier: c.branch,
		RepositoryName:  c.repo,
		FolderPath:      aws.String(folder),
	})
	if err != nil {
		var awsError *types.FolderDoesNotExistException
		if errors.As(err, &awsError) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to get %s from repository: %w", folder, err)
	}
	files := make([]string, 0)
	for _, file := range output.Files {
		files = append(files, *file.RelativePath)
	}
	return files, nil
}
