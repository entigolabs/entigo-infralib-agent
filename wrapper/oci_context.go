package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

// configBucketSuffix mirrors oracle.configBucketName: the config bucket is the
// state bucket (INFRALIB_BUCKET) plus this suffix.
const configBucketSuffix = "-config"

// ApplyOracleRunContext resolves the campaign correlation for an Oracle cloud
// execution. Container Instances have immutable env and no per-run overrides
// (StartContainerInstance takes only the instance OCID), and the env feeds the
// agent's instance-reuse spec hash — so the agent passes the per-run values via
// a config-bucket object instead, written just before each launch. The object
// is deleted after reading: a manual console Start (no agent, no fresh
// context) then finds nothing and runs transparently instead of reporting
// under a stale campaign. Best-effort like the log sink: any failure warns and
// the run proceeds without campaign correlation.
func ApplyOracleRunContext(ctx context.Context, flags *common.Wrapper) {
	if os.Getenv(model.OracleLogOCID) == "" {
		return
	}
	if flags.CampaignId != "" && flags.CampaignId != model.CampaignSentinelNone {
		return
	}
	stateBucket := os.Getenv("INFRALIB_BUCKET")
	if stateBucket == "" {
		slog.Warn("wrapper run context skipped: INFRALIB_BUCKET is not set")
		return
	}
	runContext, err := fetchRunContext(ctx, stateBucket+configBucketSuffix, flags.PrefixStep, flags.Command)
	if err != nil {
		slog.Warn("wrapper run context unavailable, running transparently", "err", err)
		return
	}
	if runContext == nil {
		return // no active campaign — normal for manual console runs
	}
	flags.CampaignId = runContext.CampaignId
	flags.PipelineIndex = strconv.Itoa(runContext.PipelineIndex)
}

func fetchRunContext(ctx context.Context, bucket, prefixStep, command string) (*model.OracleRunContext, error) {
	provider, err := ociConfigProvider()
	if err != nil {
		return nil, err
	}
	client, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region := os.Getenv(model.OracleRegion); region != "" {
		client.SetRegion(region)
	}
	namespace, err := client.GetNamespace(ctx, objectstorage.GetNamespaceRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get object storage namespace: %w", err)
	}
	key := fmt.Sprintf(model.OracleRunContextFormat, prefixStep, command)
	object, err := client.GetObject(ctx, objectstorage.GetObjectRequest{
		NamespaceName: namespace.Value,
		BucketName:    &bucket,
		ObjectName:    &key,
	})
	if err != nil {
		if failure, ok := ocicommon.IsServiceError(err); ok && failure.GetHTTPStatusCode() == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = object.Content.Close() }()
	content, err := io.ReadAll(object.Content)
	if err != nil {
		return nil, err
	}
	runContext := &model.OracleRunContext{}
	if err = json.Unmarshal(content, runContext); err != nil {
		return nil, fmt.Errorf("failed to parse run context %s: %w", key, err)
	}
	// One-shot: consume the context so it can't outlive this execution.
	if _, err = client.DeleteObject(ctx, objectstorage.DeleteObjectRequest{
		NamespaceName: namespace.Value,
		BucketName:    &bucket,
		ObjectName:    &key,
	}); err != nil {
		slog.Warn("wrapper failed to delete run context after reading", "err", err)
	}
	return runContext, nil
}
