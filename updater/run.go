package updater

import (
	"bytes"
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
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
	"net/url"
	"os"
	"strings"
)

const branchName = "main"

func Run(flags *common.Flags) {
	config := getConfig(flags.Config)
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
		switch step.Type {
		case "terraform":
			provider, err := getTerraformProvider(step)
			if err != nil {
				common.Logger.Fatalf("Failed to create terraform provider: %s", err)
			}
			parentCommitId = putFile(codeCommit, repoName, fmt.Sprintf("%s-%s/provider.tf", config.Prefix, step.Name), provider, parentCommitId)
			main, err := getTerraformMain(step, config.Source, releaseTag)
			if err != nil {
				common.Logger.Fatalf("Failed to create terraform main: %s", err)
			}
			parentCommitId = putFile(codeCommit, repoName, fmt.Sprintf("%s-%s/main.tf", config.Prefix, step.Name), main, parentCommitId)
			break
		case "argocd-apps":
			for _, module := range step.Modules {
				inputs := module.Inputs
				if len(inputs) == 0 {
					continue
				}
				yamlBytes, err := yaml.Marshal(inputs)
				if err != nil {
					common.Logger.Fatalf("Failed to marshal helm values: %s", err)
				}
				parentCommitId = putFile(codeCommit, repoName,
					fmt.Sprintf("%s-%s/%s-values.yaml", config.Prefix, step.Name, module.Name),
					yamlBytes, parentCommitId)
				break
			}
		}
	}
}

func getTerraformProvider(step model.Steps) ([]byte, error) {
	file, err := ReadTerraformFile("base.tf")
	if err != nil {
		return nil, err
	}
	body := file.Body()
	err = injectEKS(body, step)
	if err != nil {
		return nil, err
	}
	return hclwrite.Format(file.Bytes()), nil
}

func getTerraformMain(step model.Steps, source string, releaseTag string) ([]byte, error) {
	file := hclwrite.NewEmptyFile()
	body := file.Body()
	for _, module := range step.Modules {
		newModule := body.AppendNewBlock("module", []string{module.Name})
		moduleBody := newModule.Body()
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("git::%s/%s.git?ref=%s", source, module.Source, releaseTag)))
		if module.Inputs == nil {
			continue
		}
		for name, value := range module.Inputs {
			if value == nil {
				continue
			}
			moduleBody.SetAttributeRaw(name, getTokens(value))
		}
	}
	return file.Bytes(), nil
}

func injectEKS(body *hclwrite.Body, step model.Steps) error {
	hasEKSModule := false
	for _, module := range step.Modules {
		if module.Name == "eks" {
			hasEKSModule = true
			break
		}
	}
	if !hasEKSModule {
		return nil
	}
	file, err := ReadTerraformFile("eks.tf")
	if err != nil {
		return err
	}
	body.AppendBlock(file.Body().Blocks()[0])
	body.AppendNewline()
	terraformBlock := body.FirstMatchingBlock("terraform", []string{})
	if terraformBlock == nil {
		return fmt.Errorf("terraform block not found")
	}
	providersBlock := terraformBlock.Body().FirstMatchingBlock("required_providers", []string{})
	if providersBlock == nil {
		providersBlock = terraformBlock.Body().AppendNewBlock("required_providers", []string{})
	}
	kubernetesProvider := map[string]string{
		"source":  "hashicorp/kubernetes",
		"version": "~>2.0",
	}
	providerBytes, err := createKeyValuePairs(kubernetesProvider)
	if err != nil {
		return err
	}
	providersBlock.Body().SetAttributeRaw("kubernetes", getBytesTokens(providerBytes))
	return nil
}

func createKeyValuePairs(m map[string]string) ([]byte, error) {
	b := new(bytes.Buffer)
	b.Write([]byte("{\n"))
	for key, value := range m {
		_, err := fmt.Fprintf(b, "%s=\"%s\"\n", key, value)
		if err != nil {
			return nil, err
		}
	}
	b.Write([]byte("}"))
	return bytes.TrimRight(b.Bytes(), ", "), nil
}

func getTokens(value interface{}) hclwrite.Tokens {
	return getBytesTokens([]byte(fmt.Sprintf("%v", value)))
}

func getBytesTokens(bytes []byte) hclwrite.Tokens {
	return hclwrite.Tokens{
		{
			Type:  hclsyntax.TokenIdent,
			Bytes: bytes,
		},
	}
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
	fileBytes, err := os.ReadFile(configFile)
	if err != nil {
		common.Logger.Fatal(&common.PrefixedError{Reason: err})
	}
	var config model.Config
	err = yaml.Unmarshal(fileBytes, &config)
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

func ReadTerraformFile(fileName string) (*hclwrite.File, error) {
	file, err := os.ReadFile(fileName)
	if err != nil {
		return nil, err
	}
	hclFile, diags := hclwrite.ParseConfig(file, fileName, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}
	return hclFile, nil
}
