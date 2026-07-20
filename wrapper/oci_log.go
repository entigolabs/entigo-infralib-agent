package wrapper

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/google/uuid"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/loggingingestion"
)

// Container Instances can't stream stdout to OCI Logging natively, so the wrapper
// pushes each line to a custom Log via the ingestion API — the sanctioned
// app-side push for container instances. This is best-effort observability: the
// authoritative log paths are the process stdout and the portal stream, so every
// failure here is swallowed (logged, never returned) and a full send buffer drops
// lines rather than applying backpressure to the entrypoint. The sink is only
// built when LOG_OCID is set, which the agent injects for Oracle cloud executions
// only — local runs and the AWS/GCloud providers never touch this path.
const (
	ociLogBufferSize    = 8192
	ociLogBatchSize     = 256
	ociLogFlushInterval = 2 * time.Second
	ociLogPutTimeout    = 10 * time.Second
	ociLogSpecVersion   = "1.0"
	ociLogType          = "infralib"
)

// logLine carries the arrival time so each ingested entry keeps its own
// timestamp; without it a whole batch would share the flush time and OCI Logging
// would render lines out of order within each 2-second window.
type logLine struct {
	data string
	time time.Time
}

type ociLogSink struct {
	client  loggingingestion.LoggingClient
	logId   string
	source  string
	subject string
	lines   chan logLine
	done    chan struct{}
	dropped bool
}

// newOCILogSink returns a running sink, or nil (with a warning) if LOG_OCID is
// unset or a client can't be built. A nil sink is safe to use — its methods are
// no-ops — so callers never need to nil-check.
func newOCILogSink() *ociLogSink {
	logId := os.Getenv(model.OracleLogOCID)
	if logId == "" {
		return nil
	}
	provider, err := ociConfigProvider()
	if err != nil {
		slog.Warn("wrapper OCI log sink disabled: no config provider", "err", err)
		return nil
	}
	client, err := loggingingestion.NewLoggingClientWithConfigurationProvider(provider)
	if err != nil {
		slog.Warn("wrapper OCI log sink disabled: client init failed", "err", err)
		return nil
	}
	if region := os.Getenv(model.OracleRegion); region != "" {
		client.SetRegion(region)
	}
	s := &ociLogSink{
		client:  client,
		logId:   logId,
		source:  os.Getenv("TF_VAR_prefix"),
		subject: os.Getenv("TF_VAR_prefix") + "/" + os.Getenv("COMMAND"),
		lines:   make(chan logLine, ociLogBufferSize),
		done:    make(chan struct{}),
	}
	go s.run()
	return s
}

// ociConfigProvider mirrors oracle.newConfigProvider: a resource principal inside
// a Container Instance, else the SDK default chain.
func ociConfigProvider() (ocicommon.ConfigurationProvider, error) {
	if os.Getenv(auth.ResourcePrincipalVersionEnvVar) != "" {
		return auth.ResourcePrincipalConfigurationProvider()
	}
	return ocicommon.DefaultConfigProvider(), nil
}

func (s *ociLogSink) write(line string) {
	if s == nil {
		return
	}
	// OCI Logging rejects the ENTIRE PutLogs batch with HTTP 400 if any entry's
	// data is blank, and terraform output is full of blank lines — so a single one
	// would drop every other line in its batch. Blank/whitespace-only lines are
	// skipped here (they still reach stdout and the portal via streamPipe).
	if strings.TrimSpace(line) == "" {
		return
	}
	select {
	case s.lines <- logLine{data: line, time: time.Now()}:
	default:
		if !s.dropped {
			s.dropped = true
			slog.Warn("wrapper OCI log sink buffer full, dropping lines (stdout/portal logs unaffected)")
		}
	}
}

func (s *ociLogSink) close() {
	if s == nil {
		return
	}
	close(s.lines)
	<-s.done
}

func (s *ociLogSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(ociLogFlushInterval)
	defer ticker.Stop()
	batch := make([]logLine, 0, ociLogBatchSize)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				s.flush(batch)
				return
			}
			batch = append(batch, line)
			if len(batch) >= ociLogBatchSize {
				s.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

func (s *ociLogSink) flush(batch []logLine) {
	if len(batch) == 0 {
		return
	}
	now := ocicommon.SDKTime{Time: time.Now()}
	entries := make([]loggingingestion.LogEntry, 0, len(batch))
	for _, line := range batch {
		// A single blank entry makes OCI reject the whole batch; skip defensively.
		if strings.TrimSpace(line.data) == "" {
			continue
		}
		data := line.data
		id := uuid.NewString()
		entryTime := ocicommon.SDKTime{Time: line.time}
		entries = append(entries, loggingingestion.LogEntry{Data: &data, Id: &id, Time: &entryTime})
	}
	if len(entries) == 0 {
		return
	}
	logType := ociLogType
	ctx, cancel := context.WithTimeout(context.Background(), ociLogPutTimeout)
	defer cancel()
	_, err := s.client.PutLogs(ctx, loggingingestion.PutLogsRequest{
		LogId: &s.logId,
		PutLogsDetails: loggingingestion.PutLogsDetails{
			Specversion: ocicommon.String(ociLogSpecVersion),
			LogEntryBatches: []loggingingestion.LogEntryBatch{{
				Entries:             entries,
				Source:              &s.source,
				Subject:             &s.subject,
				Type:                &logType,
				Defaultlogentrytime: &now,
			}},
		},
	})
	if err != nil {
		slog.Warn("wrapper OCI PutLogs failed (best-effort, ignored)", "err", err)
	}
}
