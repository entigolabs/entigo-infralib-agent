package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/logging"
	"github.com/oracle/oci-go-sdk/v65/loggingsearch"
)

// Container Instances are NOT a supported OCI Logging "service log" source, so
// there is no toggle that streams their stdout to the Logging service. Instead
// the in-container wrapper pushes stdout to a custom Log via the ingestion API
// (see wrapper/oci_log.go), mirroring how the app-side push is the sanctioned
// pattern for container instances. The agent owns that Log (found-or-created by
// prefixed name, like the buckets/network) and reads it back with Log Search to
// parse plan changes for the approval gate — the same role CloudWatch/Cloud
// Logging play for the AWS/GCloud providers.
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
func (l *Logging) logName() string      { return fmt.Sprintf("%s-logs", l.cloudPrefix) }

// EnsureLog find-or-creates the log group and custom log, returning the Log OCID
// the wrapper ingests into. Idempotent across processes (matched by display name)
// so no OCID is persisted.
func (l *Logging) EnsureLog() (string, error) {
	groupId, err := l.getOrCreateLogGroup()
	if err != nil {
		return "", err
	}
	logId, err := l.getOrCreateLog(groupId)
	if err != nil {
		return "", err
	}
	l.logGroupId = groupId
	l.logId = logId
	return logId, nil
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
	log.Printf("Created log group %s for container execution logs\n", name)
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

func (l *Logging) getOrCreateLog(groupId string) (string, error) {
	name := l.logName()
	if id, err := l.findLogId(groupId, name); err != nil || id != "" {
		return id, err
	}
	retention := logRetentionDays
	response, err := l.mgmt.CreateLog(l.ctx, logging.CreateLogRequest{
		LogGroupId: &groupId,
		CreateLogDetails: logging.CreateLogDetails{
			DisplayName:       &name,
			LogType:           logging.CreateLogDetailsLogTypeCustom,
			IsEnabled:         ocicommon.Bool(true),
			RetentionDuration: &retention,
			FreeformTags:      map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create log %s: %w", name, err)
	}
	if err = l.waitForWorkRequest(response.OpcWorkRequestId); err != nil {
		return "", err
	}
	log.Printf("Created custom log %s (%d-day retention)\n", name, logRetentionDays)
	id, err := l.findLogId(groupId, name)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("log %s not found after creation", name)
	}
	return id, nil
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

// StepLogs returns the ingested stdout lines for one step execution, isolated by
// the subject the wrapper stamps (prefixStep/command) so parallel steps sharing
// the log don't bleed into each other. Ordered oldest-first for change parsing.
func (l *Logging) StepLogs(prefixStep string, command model.ActionCommand, since time.Time) ([]string, error) {
	subject := logSubject(prefixStep, command)
	query := fmt.Sprintf("search %q", fmt.Sprintf("%s/%s/%s", l.compartmentId, l.logGroupId, l.logId))
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
		return nil, fmt.Errorf("failed to search logs for %s: %w", prefixStep, err)
	}
	return extractLines(response.Results, subject), nil
}

func logSubject(prefixStep string, command model.ActionCommand) string {
	return fmt.Sprintf("%s/%s", prefixStep, command)
}

// StepLogHint returns an OCI Log Search query that narrows the shared log to a
// single command execution, so a gitops engineer can paste it into the console's
// Log Search instead of scrolling the whole log group. The subject is the same
// tag the wrapper stamps on every line (prefixStep/command).
func (l *Logging) StepLogHint(prefixStep string, command model.ActionCommand) string {
	return fmt.Sprintf("search %q | where subject = '%s'",
		fmt.Sprintf("%s/%s/%s", l.compartmentId, l.logGroupId, l.logId), logSubject(prefixStep, command))
}

// extractLines pulls the log line out of each matching search result, keeping
// only records the wrapper tagged with our subject, ordered by time. A result's
// fields live under "logContent" (siblings: datetime, regionId), and the pushed
// line is under logContent.data — stored as a raw string for JSON payloads or,
// for our plain text, wrapped by OCI as {"message": "<line>"}.
func extractLines(results []loggingsearch.SearchResult, subject string) []string {
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
		if s, _ := content["subject"].(string); s != subject {
			continue
		}
		data := logLineData(content["data"])
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
