package updater

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/codecommit"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/google/go-github/v54/github"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
	"net/url"
	"os"
	"strings"
)

const branchName = "main"

// Type Terraform tekitab terraformi faili
// Step name = kausta nimi, koos parent prefixiga, kõigile stepidele
// Failid provider.tf, moodulinimi.tf
// Helmi puhul input väljundisse
// Terraformi puhul module nagu test module, source midagi muud
func Run(flags *common.Flags) {
	config := getConfig(flags.Config)
	fmt.Println(config)
	// Initialize a session using Amazon SDK
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(os.Getenv("AWS_REGION")),
	})
	if err != nil {
		common.Logger.Fatalf("Failed to initialize AWS session: %s", err)
	}

	// Get AWS account number using STS
	stsService := sts.New(sess)
	stsInput := &sts.GetCallerIdentityInput{}
	stsOutput, err := stsService.GetCallerIdentity(stsInput)
	if err != nil {
		common.Logger.Fatalf("Failed to get AWS account number: %s", err)
	}
	accountID := *stsOutput.Account

	// Create CodeCommit repository with name containing the AWS account number
	repoName := fmt.Sprintf("entigo-infralib-%s", accountID)
	codeCommit := codecommit.New(sess)

	created := createRepository(codeCommit, repoName)

	var parentCommitId *string = nil
	if !created {
		parentCommitId, err = getLatestCommitId(codeCommit, repoName)
		if err != nil {
			common.Logger.Fatalf("Failed to get latest commit id: %s", err)
		}
	}

	parentCommitId = putFile(codeCommit, repoName, "README.md", []byte("# My New Repository\nThis is the README file."), parentCommitId)

	release, err := getLatestRelease(config.Source)
	if err != nil {
		common.Logger.Fatalf("Failed to get latest release: %s", err)
	}
	releaseTag := release.GetTagName()

	for _, step := range config.Steps {
		if step.Type == "terraform" {
			provider, err := getTerraformProvider(step, config.Source, releaseTag)
			if err != nil {
				common.Logger.Fatalf("Failed to create terraform provider: %s", err)
			}
			parentCommitId = putFile(codeCommit, repoName, fmt.Sprintf("%s-%s/provider.tf", config.Prefix, step.Name), provider, parentCommitId)
			inputs := getHelmValues(step)
			if len(inputs) == 0 {
				continue
			}
			yamlBytes, err := yaml.Marshal(inputs)
			if err != nil {
				common.Logger.Fatalf("Failed to marshal helm values: %s", err)
			}
			parentCommitId = putFile(codeCommit, repoName, fmt.Sprintf("%s-%s/values.yaml", config.Prefix, step.Name), yamlBytes, parentCommitId)
		}
	}
}

func getTerraformProvider(step model.Steps, source string, releaseTag string) ([]byte, error) {
	baseFile, err := ReadTerraformFile("base.tf")
	if err != nil {
		return nil, err
	}
	newFile := hclwrite.NewEmptyFile()
	body := newFile.Body()
	for _, module := range step.Modules {
		newModule := body.AppendNewBlock("module", []string{module.Name})
		moduleBody := newModule.Body()
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("git::%s/%s.git?ref=%s", source, module.Source, releaseTag)))
	}
	return hclwrite.Format(append(baseFile.Bytes, newFile.Bytes()...)), nil
}

func getLatestRelease(url string) (*github.RepositoryRelease, error) {
	client := github.NewClient(nil)
	owner, repo, err := getGithubOwnerAndRepo(url)
	if err != nil {
		return nil, err
	}
	release, _, err := client.Repositories.GetLatestRelease(context.Background(), owner, repo)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, fmt.Errorf("no releases found for %s/%s", owner, repo)
	}

	return release, nil
}

func getGithubOwnerAndRepo(repoURL string) (string, string, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", err
	}
	if u.Hostname() != "github.com" {
		return "", "", fmt.Errorf("not a GitHub URL: %s", repoURL)
	}

	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid GitHub URL: %s", repoURL)
	}

	return parts[0], parts[1], nil
}

func getHelmValues(step model.Steps) map[string]interface{} {
	inputs := make(map[string]interface{})
	for _, module := range step.Modules {
		for name, value := range module.Inputs {
			inputs[name] = value
		}
	}
	return inputs
}

func getLatestCommitId(codeCommit *codecommit.CodeCommit, repoName string) (*string, error) {
	input := &codecommit.GetBranchInput{
		RepositoryName: aws.String(repoName),
		BranchName:     aws.String(branchName),
	}

	result, err := codeCommit.GetBranch(input)
	if err != nil {
		return nil, err
	}

	return result.Branch.CommitId, nil
}

func putFile(codeCommit *codecommit.CodeCommit, repoName string, file string, content []byte, parentCommitId *string) *string {
	putFileInput := &codecommit.PutFileInput{
		BranchName:     aws.String(branchName),
		CommitMessage:  aws.String(fmt.Sprintf("Add %s", file)),
		RepositoryName: aws.String(repoName),
		FileContent:    content,
		FilePath:       aws.String(file),
		ParentCommitId: parentCommitId,
	}

	putFileOutput, err := codeCommit.PutFile(putFileInput)
	if err != nil {
		var awsErr awserr.Error
		ok := errors.As(err, &awsErr)
		if ok && awsErr.Code() == codecommit.ErrCodeSameFileContentException {
			common.Logger.Printf("%s already exists in repository. Continuing...\n", file)
			return parentCommitId
		} else {
			common.Logger.Fatalf("Failed to put README.md file to repository: %s", err)
			return nil
		}
	}
	common.Logger.Printf("File %s added to repository\n", file)
	return putFileOutput.CommitId
}

func createRepository(codeCommit *codecommit.CodeCommit, repoName string) bool {
	input := &codecommit.CreateRepositoryInput{
		RepositoryName: aws.String(repoName),
	}

	result, err := codeCommit.CreateRepository(input)
	if err != nil {
		var awsErr awserr.Error
		ok := errors.As(err, &awsErr)
		if ok && awsErr.Code() == codecommit.ErrCodeRepositoryNameExistsException {
			common.Logger.Printf("Repository %s already exists. Continuing...\n", repoName)
			return false
		} else {
			common.Logger.Fatalf("Failed to create CodeCommit repository: %s", err)
			return false
		}
	} else {
		common.Logger.Printf("Repository created with name: %s\n", *result.RepositoryMetadata.RepositoryName)
		return true
	}
}

func getConfig(configFile string) model.Config {
	bytes, err := os.ReadFile(configFile)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	var config model.Config
	err = yaml.Unmarshal(bytes, &config)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	validateConfig(config)
	return config
}

func validateConfig(config model.Config) {
	moduleNames := model.NewSet[string]()
	for _, step := range config.Steps {
		for _, module := range step.Modules {
			if moduleNames.Contains(module.Name) {
				common.Logger.Fatal(&common.PrefixedError{Reason: fmt.Errorf("module name %s is not unique",
					module.Name)})
			}
			moduleNames.Add(module.Name)
		}
	}
}

func ReadTerraformFile(fileName string) (*hcl.File, error) {
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCLFile(fileName)
	if diags.HasErrors() {
		return nil, diags
	}
	return file, nil
}
