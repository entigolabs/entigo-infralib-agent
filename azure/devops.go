package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	devOpsResourceID = "499b84ac-1321-427f-aa17-267ca6975798" // Azure DevOps resource ID
	apiVersion       = "7.1"
)

type DevOpsClient struct {
	ctx          context.Context
	credential   *azidentity.DefaultAzureCredential
	organization string
	project      string
	baseURL      string
	httpClient   *http.Client
}

type Approval struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	CreatedOn  time.Time `json:"createdOn"`
	RunID      int       `json:"pipeline,omitempty"`
	PipelineID int       `json:"pipelineId,omitempty"`
}

type ApprovalResponse struct {
	Count int        `json:"count"`
	Value []Approval `json:"value"`
}

type ApprovalUpdateRequest struct {
	ApprovalID string `json:"approvalId"`
	Status     string `json:"status"`
	Comment    string `json:"comment,omitempty"`
}

type PipelineRun struct {
	ID         int       `json:"id"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Result     string    `json:"result,omitempty"`
	CreatedOn  time.Time `json:"createdDate"`
	FinishedOn time.Time `json:"finishedDate,omitempty"`
	URL        string    `json:"url"`
}

type PipelineRunResponse struct {
	Count int           `json:"count"`
	Value []PipelineRun `json:"value"`
}

type PipelineDefinition struct {
	ID            int    `json:"id,omitempty"`
	Name          string `json:"name"`
	Folder        string `json:"folder,omitempty"`
	Configuration struct {
		Type       string `json:"type"`
		Path       string `json:"path,omitempty"`
		Repository struct {
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
			Type string `json:"type"`
		} `json:"repository,omitempty"`
	} `json:"configuration,omitempty"`
}

type PipelineListResponse struct {
	Count int                  `json:"count"`
	Value []PipelineDefinition `json:"value"`
}

type RunPipelineRequest struct {
	Variables          map[string]PipelineVariable `json:"variables,omitempty"`
	TemplateParameters map[string]string           `json:"templateParameters,omitempty"`
}

type PipelineVariable struct {
	Value    string `json:"value"`
	IsSecret bool   `json:"isSecret,omitempty"`
}

type BuildLog struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	LineCount int    `json:"lineCount"`
	CreatedOn string `json:"createdOn"`
}

type BuildLogResponse struct {
	Count int        `json:"count"`
	Value []BuildLog `json:"value"`
}

type RepositoryCreateRequest struct {
	Name    string               `json:"name"`
	Project RepositoryProjectRef `json:"project"`
}

type RepositoryProjectRef struct {
	ID string `json:"id"`
}

type Repository struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	URL           string `json:"url"`
	RemoteURL     string `json:"remoteUrl"`
	WebURL        string `json:"webUrl"`
}

type GitPush struct {
	RefUpdates []RefUpdate `json:"refUpdates"`
	Commits    []GitCommit `json:"commits"`
}

type RefUpdate struct {
	Name        string `json:"name"`
	OldObjectID string `json:"oldObjectId"`
}

type GitCommit struct {
	Comment string      `json:"comment"`
	Changes []GitChange `json:"changes"`
}

type GitChange struct {
	ChangeType string      `json:"changeType"`
	Item       GitItem     `json:"item"`
	NewContent *GitContent `json:"newContent,omitempty"`
}

type GitItem struct {
	Path string `json:"path"`
}

type GitContent struct {
	Content     string `json:"content"`
	ContentType string `json:"contentType"`
}

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func NewDevOpsClient(ctx context.Context, credential *azidentity.DefaultAzureCredential, organization, project string) (*DevOpsClient, error) {
	if organization == "" || project == "" {
		return nil, fmt.Errorf("azure DevOps organization and project are required")
	}

	return &DevOpsClient{
		ctx:          ctx,
		credential:   credential,
		organization: organization,
		project:      project,
		baseURL:      fmt.Sprintf("https://dev.azure.com/%s", organization),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *DevOpsClient) getAuthHeader() (string, error) {
	token, err := c.credential.GetToken(c.ctx, policy.TokenRequestOptions{
		Scopes: []string{devOpsResourceID + "/.default"},
	})
	if err == nil {
		return "Bearer " + token.Token, nil
	}
	return "", fmt.Errorf("no Azure DevOps authentication available: Azure AD auth failed and no PAT configured")
}

func (c *DevOpsClient) doRequest(method, url string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequestWithContext(c.ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	authHeader, err := c.getAuthHeader()
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func (c *DevOpsClient) GetProject() (*Project, error) {
	url := fmt.Sprintf("%s/_apis/projects/%s?api-version=%s", c.baseURL, c.project, apiVersion)
	respBody, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	var project Project
	if err := json.Unmarshal(respBody, &project); err != nil {
		return nil, fmt.Errorf("failed to parse project response: %w", err)
	}

	return &project, nil
}

func (c *DevOpsClient) GetPendingApprovals(runID int) ([]Approval, error) {
	url := fmt.Sprintf("%s/%s/_apis/pipelines/approvals?api-version=%s",
		c.baseURL, c.project, apiVersion)

	respBody, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	var response ApprovalResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse approvals response: %w", err)
	}

	// Filter by run ID if specified
	if runID > 0 {
		var filtered []Approval
		for _, a := range response.Value {
			if a.RunID == runID && a.Status == "pending" {
				filtered = append(filtered, a)
			}
		}
		return filtered, nil
	}

	return response.Value, nil
}

func (c *DevOpsClient) UpdateApproval(approvalID, status, comment string) error {
	url := fmt.Sprintf("%s/%s/_apis/pipelines/approvals?api-version=%s",
		c.baseURL, c.project, apiVersion)

	updates := []ApprovalUpdateRequest{{
		ApprovalID: approvalID,
		Status:     status,
		Comment:    comment,
	}}

	_, err := c.doRequest("PATCH", url, updates)
	return err
}

func (c *DevOpsClient) GetPipelineRun(pipelineID, runID int) (*PipelineRun, error) {
	url := fmt.Sprintf("%s/%s/_apis/pipelines/%d/runs/%d?api-version=%s",
		c.baseURL, c.project, pipelineID, runID, apiVersion)

	respBody, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	var run PipelineRun
	if err := json.Unmarshal(respBody, &run); err != nil {
		return nil, fmt.Errorf("failed to parse pipeline run response: %w", err)
	}

	return &run, nil
}

func (c *DevOpsClient) GetPipelineByName(name string) (*PipelineDefinition, error) {
	url := fmt.Sprintf("%s/%s/_apis/pipelines?api-version=%s",
		c.baseURL, c.project, apiVersion)

	respBody, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	var response PipelineListResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to parse pipelines response: %w", err)
	}

	for _, p := range response.Value {
		if p.Name == name {
			return &p, nil
		}
	}

	return nil, nil
}

func (c *DevOpsClient) CreatePipeline(name, repoID, yamlPath string) (*PipelineDefinition, error) {
	url := fmt.Sprintf("%s/%s/_apis/pipelines?api-version=%s",
		c.baseURL, c.project, apiVersion)

	pipeline := map[string]interface{}{
		"name":   name,
		"folder": "\\entigo-infralib",
		"configuration": map[string]interface{}{
			"type": "yaml",
			"path": yamlPath,
			"repository": map[string]interface{}{
				"id":   repoID,
				"type": "azureReposGit",
			},
		},
	}

	respBody, err := c.doRequest("POST", url, pipeline)
	if err != nil {
		return nil, err
	}

	var created PipelineDefinition
	if err := json.Unmarshal(respBody, &created); err != nil {
		return nil, fmt.Errorf("failed to parse created pipeline response: %w", err)
	}

	return &created, nil
}

func (c *DevOpsClient) StartPipelineRun(pipelineID int, variables map[string]string) (*PipelineRun, error) {
	url := fmt.Sprintf("%s/%s/_apis/pipelines/%d/runs?api-version=%s",
		c.baseURL, c.project, pipelineID, apiVersion)

	req := RunPipelineRequest{
		Variables: make(map[string]PipelineVariable),
	}
	for k, v := range variables {
		req.Variables[k] = PipelineVariable{Value: v}
	}

	respBody, err := c.doRequest("POST", url, req)
	if err != nil {
		return nil, err
	}

	var run PipelineRun
	if err := json.Unmarshal(respBody, &run); err != nil {
		return nil, fmt.Errorf("failed to parse pipeline run response: %w", err)
	}

	return &run, nil
}

func (c *DevOpsClient) GetBuildLogs(buildID int) ([]string, error) {
	// First get the list of logs
	url := fmt.Sprintf("%s/%s/_apis/build/builds/%d/logs?api-version=%s",
		c.baseURL, c.project, buildID, apiVersion)

	respBody, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	var logList BuildLogResponse
	if err := json.Unmarshal(respBody, &logList); err != nil {
		return nil, fmt.Errorf("failed to parse log list response: %w", err)
	}

	var allLogs []string
	for _, log := range logList.Value {
		// Get each log's content
		logURL := fmt.Sprintf("%s/%s/_apis/build/builds/%d/logs/%d?api-version=%s",
			c.baseURL, c.project, buildID, log.ID, apiVersion)

		logContent, err := c.doRequest("GET", logURL, nil)
		if err != nil {
			continue // Skip logs we can't fetch
		}

		lines := strings.Split(string(logContent), "\n")
		allLogs = append(allLogs, lines...)
	}

	return allLogs, nil
}

func (c *DevOpsClient) GetOrCreateRepository(name string) (*Repository, error) {
	// Try to get existing repository
	url := fmt.Sprintf("%s/%s/_apis/git/repositories/%s?api-version=%s",
		c.baseURL, c.project, name, apiVersion)

	respBody, err := c.doRequest("GET", url, nil)
	if err == nil {
		var repo Repository
		if err := json.Unmarshal(respBody, &repo); err == nil {
			return &repo, nil
		}
	}

	// Get project ID for creating repository
	project, err := c.GetProject()
	if err != nil {
		return nil, fmt.Errorf("failed to get project: %w", err)
	}

	// Create new repository
	createURL := fmt.Sprintf("%s/%s/_apis/git/repositories?api-version=%s",
		c.baseURL, c.project, apiVersion)

	createReq := RepositoryCreateRequest{
		Name: name,
		Project: RepositoryProjectRef{
			ID: project.ID,
		},
	}

	respBody, err = c.doRequest("POST", createURL, createReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create repository: %w", err)
	}

	var repo Repository
	if err := json.Unmarshal(respBody, &repo); err != nil {
		return nil, fmt.Errorf("failed to parse repository response: %w", err)
	}

	return &repo, nil
}

func (c *DevOpsClient) PushFileToRepository(repoID, branch, filePath, content, commitMessage string) error {
	// Get the latest commit to determine oldObjectId
	refURL := fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?filter=heads/%s&api-version=%s",
		c.baseURL, c.project, repoID, branch, apiVersion)

	var oldObjectID string

	respBody, err := c.doRequest("GET", refURL, nil)
	if err == nil {
		var refs struct {
			Value []struct {
				ObjectID string `json:"objectId"`
			} `json:"value"`
		}
		if json.Unmarshal(respBody, &refs) == nil && len(refs.Value) > 0 {
			oldObjectID = refs.Value[0].ObjectID
		}
	}

	// If no existing ref, this is an initial commit
	if oldObjectID == "" {
		oldObjectID = "0000000000000000000000000000000000000000"
	}

	// Determine change type based on whether file exists
	changeType := "add"
	if oldObjectID != "0000000000000000000000000000000000000000" {
		// Check if file exists
		itemURL := fmt.Sprintf("%s/%s/_apis/git/repositories/%s/items?path=%s&api-version=%s",
			c.baseURL, c.project, repoID, filePath, apiVersion)
		_, err := c.doRequest("GET", itemURL, nil)
		if err == nil {
			changeType = "edit"
		}
	}

	pushURL := fmt.Sprintf("%s/%s/_apis/git/repositories/%s/pushes?api-version=%s",
		c.baseURL, c.project, repoID, apiVersion)

	push := GitPush{
		RefUpdates: []RefUpdate{{
			Name:        "refs/heads/" + branch,
			OldObjectID: oldObjectID,
		}},
		Commits: []GitCommit{{
			Comment: commitMessage,
			Changes: []GitChange{{
				ChangeType: changeType,
				Item:       GitItem{Path: filePath},
				NewContent: &GitContent{
					Content:     content,
					ContentType: "rawtext",
				},
			}},
		}},
	}

	_, err = c.doRequest("POST", pushURL, push)
	return err
}

func (c *DevOpsClient) GetPipelineLink(pipelineName string) string {
	return fmt.Sprintf("https://dev.azure.com/%s/%s/_build?definitionName=%s",
		c.organization, c.project, pipelineName)
}

func (c *DevOpsClient) GetRunLink(pipelineID, runID int) string {
	return fmt.Sprintf("https://dev.azure.com/%s/%s/_build/results?buildId=%d&view=results",
		c.organization, c.project, runID)
}
