package azure

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/monitor/azquery"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/operationalinsights/armoperationalinsights"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type Logging struct {
	ctx              context.Context
	credential       *azidentity.DefaultAzureCredential
	subscriptionId   string
	resourceGroup    string
	location         string
	cloudPrefix      string
	workspacesClient *armoperationalinsights.WorkspacesClient
	logsClient       *azquery.LogsClient
	workspaceId      string
}

func NewLogging(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup, location, cloudPrefix string) (*Logging, error) {
	workspacesClient, err := armoperationalinsights.NewWorkspacesClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create workspaces client: %w", err)
	}

	logsClient, err := azquery.NewLogsClient(credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create logs client: %w", err)
	}

	l := &Logging{
		ctx:              ctx,
		credential:       credential,
		subscriptionId:   subscriptionId,
		resourceGroup:    resourceGroup,
		location:         location,
		cloudPrefix:      cloudPrefix,
		workspacesClient: workspacesClient,
		logsClient:       logsClient,
	}

	err = l.ensureWorkspaceExists()
	if err != nil {
		return nil, err
	}

	return l, nil
}

func (l *Logging) ensureWorkspaceExists() error {
	workspaceName := l.getWorkspaceName()

	resp, err := l.workspacesClient.Get(l.ctx, l.resourceGroup, workspaceName, nil)
	if err == nil {
		if resp.Properties != nil && resp.Properties.CustomerID != nil {
			l.workspaceId = *resp.Properties.CustomerID
		}
		return nil
	}

	poller, err := l.workspacesClient.BeginCreateOrUpdate(l.ctx, l.resourceGroup, workspaceName,
		armoperationalinsights.Workspace{
			Location: to.Ptr(l.location),
			Properties: &armoperationalinsights.WorkspaceProperties{
				RetentionInDays: to.Ptr(int32(30)),
				SKU: &armoperationalinsights.WorkspaceSKU{
					Name: to.Ptr(armoperationalinsights.WorkspaceSKUNameEnumPerGB2018),
				},
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to begin creating log analytics workspace: %w", err)
	}

	result, err := poller.PollUntilDone(l.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to create log analytics workspace: %w", err)
	}

	if result.Properties != nil && result.Properties.CustomerID != nil {
		l.workspaceId = *result.Properties.CustomerID
	}

	log.Printf("Created Log Analytics Workspace %s\n", workspaceName)
	return nil
}

func (l *Logging) getWorkspaceName() string {
	name := fmt.Sprintf("%s-logs", l.cloudPrefix)
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimSuffix(name, "-")
}

func (l *Logging) GetLogs(jobName, executionId string) ([]string, error) {
	if l.workspaceId == "" {
		return nil, fmt.Errorf("workspace ID not set")
	}

	query := fmt.Sprintf(`
		ContainerAppConsoleLogs_CL
		| where ContainerAppName_s == "%s"
		| where TimeGenerated > ago(24h)
		| order by TimeGenerated asc
		| project Log_s
	`, jobName)

	timespan := azquery.TimeInterval(fmt.Sprintf("PT%dH", 24))
	resp, err := l.logsClient.QueryWorkspace(l.ctx, l.workspaceId, azquery.Body{
		Query:    to.Ptr(query),
		Timespan: &timespan,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to query logs: %w", err)
	}

	var logs []string
	if len(resp.Tables) > 0 {
		table := resp.Tables[0]
		for _, row := range table.Rows {
			if len(row) > 0 {
				if logStr, ok := row[0].(string); ok {
					logs = append(logs, logStr)
				}
			}
		}
	}

	return logs, nil
}

func (l *Logging) DeleteWorkspace() error {
	workspaceName := l.getWorkspaceName()

	poller, err := l.workspacesClient.BeginDelete(l.ctx, l.resourceGroup, workspaceName, &armoperationalinsights.WorkspacesClientBeginDeleteOptions{
		Force: to.Ptr(true),
	})
	if err != nil {
		return fmt.Errorf("failed to begin deleting log analytics workspace: %w", err)
	}

	_, err = poller.PollUntilDone(l.ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to delete log analytics workspace: %w", err)
	}

	log.Printf("Deleted Log Analytics Workspace %s\n", workspaceName)
	return nil
}

func (l *Logging) WaitForLogs(jobName string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(l.ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for logs")
		case <-ticker.C:
			logs, err := l.GetLogs(jobName, "")
			if err != nil {
				continue
			}
			if len(logs) > 0 {
				return logs, nil
			}
		}
	}
}
