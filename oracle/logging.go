package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/logging"
	"github.com/oracle/oci-go-sdk/v65/loggingsearch"
)

// OCI won't run a DevOps build until the project has a log enabled, and that same
// service log (`<prefix>-infralib-logs`) captures the build runner's command
// output — our docker container's stdout. The agent reads it back with Log Search
// to parse each step's plan change summary for the approval gate — the same role
// CloudWatch/Cloud Logging play for the AWS/GCloud providers. Found-or-created by
// prefixed name, like the buckets, so no OCID is persisted.
const (
	logRetentionDays  = 180 // matches the AWS CloudWatch retention (aws/cloudwatch.go)
	workRequestPoll   = 3 * time.Second
	workRequestWait   = 3 * time.Minute
	logSearchLimit    = 1000
	logSearchLookback = 5 * time.Minute
)

type Logging struct {
	ctx           context.Context
	mgmt          logging.LoggingManagementClient
	search        loggingsearch.LogSearchClient
	compartmentId string
	cloudPrefix   string
	logGroupId    string
	logId         string
}

func NewLogging(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, cloudPrefix string) (*Logging, error) {
	mgmt, err := logging.NewLoggingManagementClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	search, err := loggingsearch.NewLogSearchClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		mgmt.SetRegion(region)
		search.SetRegion(region)
	}
	return &Logging{
		ctx:           ctx,
		mgmt:          mgmt,
		search:        search,
		compartmentId: compartmentId,
		cloudPrefix:   cloudPrefix,
	}, nil
}

func (l *Logging) logGroupName() string { return fmt.Sprintf("%s-logs", l.cloudPrefix) }
func (l *Logging) buildLogName() string { return fmt.Sprintf("%s-infralib-logs", l.cloudPrefix) }

// EnsureDevOpsBuildLog enables the OCI service log for the DevOps build project
// and records its ids (StepLogs reads plan output back from it). OCI refuses to
// start any build run until the project has logs enabled (CreateBuildRun → 409
// "Logs need to be enabled"), and that same service log captures the build
// runner's command output — i.e. our docker container's stdout — so it doubles as
// the source the agent parses plan changes from. Found-or-created by name.
func (l *Logging) EnsureDevOpsBuildLog(projectId string) error {
	groupId, err := l.getOrCreateLogGroup()
	if err != nil {
		return err
	}
	l.logGroupId = groupId
	name := l.buildLogName()
	id, err := l.findLogId(groupId, name)
	if err != nil {
		return err
	}
	if id != "" {
		l.logId = id
		return nil
	}
	retention := logRetentionDays
	service := "devops"
	category := "all"
	response, err := l.mgmt.CreateLog(l.ctx, logging.CreateLogRequest{
		LogGroupId: &groupId,
		CreateLogDetails: logging.CreateLogDetails{
			DisplayName:       &name,
			LogType:           logging.CreateLogDetailsLogTypeService,
			IsEnabled:         ocicommon.Bool(true),
			RetentionDuration: &retention,
			Configuration: &logging.Configuration{
				CompartmentId: &l.compartmentId,
				Source: logging.OciService{
					Service:  &service,
					Resource: &projectId,
					Category: &category,
				},
			},
			FreeformTags: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create devops build log %s: %w", name, err)
	}
	if err = l.waitForWorkRequest(response.OpcWorkRequestId); err != nil {
		return err
	}
	id, err = l.findLogId(groupId, name)
	if err != nil {
		return err
	}
	if id == "" {
		return fmt.Errorf("devops build log %s not found after creation", name)
	}
	l.logId = id
	log.Printf("Enabled DevOps build logs (%s) for project\n", name)
	return nil
}

// Delete removes the agent's service log (<prefix>-infralib-logs) and then its
// log group (<prefix>-logs). Deletions are asynchronous work requests, so the log
// is removed and awaited before its group. Best-effort: each failure warns and
// continues, and a missing group is a no-op.
func (l *Logging) Delete() {
	groupName := l.logGroupName()
	groupId, err := l.lookupLogGroup(groupName)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to look up log group %s: %s", groupName, err)))
		return
	}
	if groupId == "" {
		return
	}
	// Delete every log in the group, not just the agent's <prefix>-infralib-logs: OCI
	// rejects DeleteLogGroup with 409 ("Log group not empty") while any log remains,
	// and the group is agent-owned (named by prefix), so anything in it is ours to clear.
	l.deleteAllLogs(groupId)
	response, err := l.mgmt.DeleteLogGroup(l.ctx, logging.DeleteLogGroupRequest{LogGroupId: &groupId})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete log group %s: %s", groupName, err)))
		return
	}
	if err = l.waitForWorkRequest(response.OpcWorkRequestId); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed waiting for log group %s deletion: %s", groupName, err)))
		return
	}
	log.Printf("Deleted log group %s\n", groupName)
}

// deleteAllLogs removes every log in the group, waiting on each deletion's work
// request so the group is empty before the caller deletes it. Best-effort and
// paginated.
func (l *Logging) deleteAllLogs(groupId string) {
	var page *string
	for {
		list, err := l.mgmt.ListLogs(l.ctx, logging.ListLogsRequest{LogGroupId: &groupId, Page: page})
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list logs in group %s: %s", groupId, err)))
			return
		}
		for _, item := range list.Items {
			if item.Id == nil {
				continue
			}
			response, err := l.mgmt.DeleteLog(l.ctx, logging.DeleteLogRequest{LogGroupId: &groupId, LogId: item.Id})
			if err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete log %s: %s", *item.Id, err)))
				continue
			}
			if err = l.waitForWorkRequest(response.OpcWorkRequestId); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("failed waiting for log %s deletion: %s", *item.Id, err)))
			}
		}
		if list.OpcNextPage == nil {
			return
		}
		page = list.OpcNextPage
	}
}

// lookupLogGroup returns the log group OCID for the given name, or "" when absent
// (unlike findLogGroupId, which treats absence as an error).
func (l *Logging) lookupLogGroup(name string) (string, error) {
	list, err := l.mgmt.ListLogGroups(l.ctx, logging.ListLogGroupsRequest{
		CompartmentId: &l.compartmentId,
		DisplayName:   &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list log groups: %w", err)
	}
	for _, group := range list.Items {
		if group.DisplayName != nil && *group.DisplayName == name {
			return *group.Id, nil
		}
	}
	return "", nil
}

func (l *Logging) getOrCreateLogGroup() (string, error) {
	name := l.logGroupName()
	list, err := l.mgmt.ListLogGroups(l.ctx, logging.ListLogGroupsRequest{
		CompartmentId: &l.compartmentId,
		DisplayName:   &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list log groups: %w", err)
	}
	for _, group := range list.Items {
		if group.DisplayName != nil && *group.DisplayName == name {
			return *group.Id, nil
		}
	}
	response, err := l.mgmt.CreateLogGroup(l.ctx, logging.CreateLogGroupRequest{
		CreateLogGroupDetails: logging.CreateLogGroupDetails{
			CompartmentId: &l.compartmentId,
			DisplayName:   &name,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create log group %s: %w", name, err)
	}
	if err = l.waitForWorkRequest(response.OpcWorkRequestId); err != nil {
		return "", err
	}
	log.Printf("Created log group %s for step execution logs\n", name)
	return l.findLogGroupId(name)
}

func (l *Logging) findLogGroupId(name string) (string, error) {
	list, err := l.mgmt.ListLogGroups(l.ctx, logging.ListLogGroupsRequest{
		CompartmentId: &l.compartmentId,
		DisplayName:   &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list log groups: %w", err)
	}
	for _, group := range list.Items {
		if group.DisplayName != nil && *group.DisplayName == name {
			return *group.Id, nil
		}
	}
	return "", fmt.Errorf("log group %s not found after creation", name)
}

func (l *Logging) findLogId(groupId, name string) (string, error) {
	list, err := l.mgmt.ListLogs(l.ctx, logging.ListLogsRequest{
		LogGroupId:  &groupId,
		DisplayName: &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list logs: %w", err)
	}
	for _, item := range list.Items {
		if item.DisplayName != nil && *item.DisplayName == name {
			return *item.Id, nil
		}
	}
	return "", nil
}

func (l *Logging) waitForWorkRequest(id *string) error {
	if id == nil {
		return nil
	}
	deadline := time.After(workRequestWait)
	for {
		response, err := l.mgmt.GetWorkRequest(l.ctx, logging.GetWorkRequestRequest{WorkRequestId: id})
		if err != nil {
			return fmt.Errorf("failed to get work request %s: %w", *id, err)
		}
		switch response.Status {
		case logging.OperationStatusSucceeded:
			return nil
		case logging.OperationStatusFailed, logging.OperationStatusCanceled:
			return fmt.Errorf("logging work request %s ended in %s", *id, response.Status)
		}
		select {
		case <-l.ctx.Done():
			return l.ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out waiting for logging work request %s", *id)
		case <-time.After(workRequestPoll):
		}
	}
}

// StepLogs returns the build runner's command-output lines for one build run,
// isolated by the DevOps service log's `subject` field (which OCI sets to the
// build run OCID) so parallel steps sharing the log don't bleed into each other.
// Ordered oldest-first for change parsing.
func (l *Logging) StepLogs(buildRunId string, since time.Time) ([]string, error) {
	// Sort newest-first so the logSearchLimit cap keeps the most recent entries: a
	// step's plan change summary ("Plan: N to add, …") is emitted at the tail of the
	// run, so a large plan whose output exceeds the cap must not have its summary
	// truncated away. extractLines re-sorts the kept rows oldest-first for parsing.
	query := fmt.Sprintf("search %q | sort by datetime desc", fmt.Sprintf("%s/%s/%s", l.compartmentId, l.logGroupId, l.logId))
	start := ocicommon.SDKTime{Time: since.Add(-logSearchLookback)}
	end := ocicommon.SDKTime{Time: time.Now().Add(time.Minute)}
	limit := logSearchLimit
	response, err := l.search.SearchLogs(l.ctx, loggingsearch.SearchLogsRequest{
		Limit: &limit,
		SearchLogsDetails: loggingsearch.SearchLogsDetails{
			TimeStart:   &start,
			TimeEnd:     &end,
			SearchQuery: &query,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to search logs for build run %s: %w", buildRunId, err)
	}
	return extractLines(response.Results, buildRunId), nil
}

// execPrefix is the DevOps build runner's prefix on every command-output line in
// the service log (e.g. "EXEC: Plan: 2 to add, …"). It is stripped before parsing
// so the shared terraform/argocd parsers — some of which anchor on the line start
// (`strings.HasPrefix("No changes.")`) — see the raw output.
const execPrefix = "EXEC: "

// extractLines pulls the command-output line out of each matching search result,
// keeping only records whose subject is our build run, ordered by time. A result's
// fields live under "logContent"; the line is under logContent.data.message and is
// stripped of the runner's EXEC prefix.
func extractLines(results []loggingsearch.SearchResult, buildRunId string) []string {
	type row struct {
		time time.Time
		data string
	}
	rows := make([]row, 0, len(results))
	for _, result := range results {
		if result.Data == nil {
			continue
		}
		record, ok := (*result.Data).(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := record["logContent"].(map[string]interface{})
		if !ok {
			continue
		}
		if s, _ := content["subject"].(string); s != buildRunId {
			continue
		}
		data := strings.TrimPrefix(logLineData(content["data"]), execPrefix)
		if data == "" {
			continue
		}
		t, _ := content["time"].(string)
		parsed, _ := time.Parse(time.RFC3339Nano, t)
		rows = append(rows, row{time: parsed, data: data})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].time.Before(rows[j].time) })
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.data
	}
	return lines
}

// logLineData recovers the pushed line from a search result's data field: OCI
// keeps it as a raw string when the entry was JSON, but wraps plain text (our
// case) as {"message": "<line>"}. Any other object is re-encoded so no line is
// silently dropped.
func logLineData(data interface{}) string {
	switch d := data.(type) {
	case string:
		return d
	case map[string]interface{}:
		if msg, ok := d["message"].(string); ok {
			return msg
		}
		if encoded, err := json.Marshal(d); err == nil {
			return string(encoded)
		}
	}
	return ""
}
