package oracle

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/functions"
)

// functionMemoryMB is the smallest supported OCI Function memory size — the
// scheduler function only issues one DevOps CreateBuildRun call.
const functionMemoryMB int64 = 128

// Functions manages the OCI Function (and its Application) the Resource Scheduler
// invokes to trigger the agent-update build pipeline on cron. Both are found-or-
// created by prefixed display name.
type Functions struct {
	ctx           context.Context
	client        functions.FunctionsManagementClient
	compartmentId string
	region        string
	cloudPrefix   string
}

func NewFunctions(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, cloudPrefix string) (*Functions, error) {
	client, err := functions.NewFunctionsManagementClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
	}
	return &Functions{
		ctx:           ctx,
		client:        client,
		compartmentId: compartmentId,
		region:        region,
		cloudPrefix:   cloudPrefix,
	}, nil
}

func (f *Functions) appName() string      { return fmt.Sprintf("%s-infralib-fn", f.cloudPrefix) }
func (f *Functions) functionName() string { return fmt.Sprintf("%s-agent-update-fn", f.cloudPrefix) }

// EnsureApplication find-or-creates the Function Application in the given subnet
// and returns its OCID.
func (f *Functions) EnsureApplication(subnetId string) (string, error) {
	name := f.appName()
	list, err := f.client.ListApplications(f.ctx, functions.ListApplicationsRequest{
		CompartmentId: &f.compartmentId,
		DisplayName:   &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list function applications: %w", err)
	}
	for _, app := range list.Items {
		if app.LifecycleState != functions.ApplicationLifecycleStateDeleted && app.LifecycleState != functions.ApplicationLifecycleStateDeleting {
			return *app.Id, nil
		}
	}
	created, err := f.client.CreateApplication(f.ctx, functions.CreateApplicationRequest{
		CreateApplicationDetails: functions.CreateApplicationDetails{
			CompartmentId: &f.compartmentId,
			DisplayName:   &name,
			SubnetIds:     []string{subnetId},
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create function application %s: %w", name, err)
	}
	log.Printf("Created function application %s for scheduler\n", name)
	return *created.Id, nil
}

// EnsureFunction find-or-creates the scheduler function in the application with the
// given image and config, reconciling image/config drift on an existing one, and
// returns its OCID.
func (f *Functions) EnsureFunction(appId, image string, config map[string]string) (string, error) {
	name := f.functionName()
	existing, err := f.findFunction(appId, name)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return f.reconcileFunction(*existing, image, config)
	}
	created, err := f.client.CreateFunction(f.ctx, functions.CreateFunctionRequest{
		CreateFunctionDetails: functions.CreateFunctionDetails{
			DisplayName:   &name,
			ApplicationId: &appId,
			Image:         &image,
			MemoryInMBs:   new(functionMemoryMB),
			Config:        config,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create scheduler function %s: %w", name, err)
	}
	log.Printf("Created scheduler function %s\n", name)
	return *created.Id, nil
}

func (f *Functions) findFunction(appId, name string) (*functions.FunctionSummary, error) {
	list, err := f.client.ListFunctions(f.ctx, functions.ListFunctionsRequest{
		ApplicationId: &appId,
		DisplayName:   &name,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list functions: %w", err)
	}
	for _, fn := range list.Items {
		if fn.LifecycleState != functions.FunctionLifecycleStateDeleted && fn.LifecycleState != functions.FunctionLifecycleStateDeleting {
			return &fn, nil
		}
	}
	return nil, nil
}

// reconcileFunction updates the function only when its image drifted (a version
// bump); config carries the update pipeline OCID which is stable, so an unchanged
// image is a no-op. Returns the function OCID.
func (f *Functions) reconcileFunction(fn functions.FunctionSummary, image string, config map[string]string) (string, error) {
	if fn.Image != nil && *fn.Image == image {
		return *fn.Id, nil
	}
	_, err := f.client.UpdateFunction(f.ctx, functions.UpdateFunctionRequest{
		FunctionId: fn.Id,
		UpdateFunctionDetails: functions.UpdateFunctionDetails{
			Image:  &image,
			Config: config,
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to update scheduler function %s: %w", *fn.Id, err)
	}
	return *fn.Id, nil
}

// DeleteFunction and DeleteApplication tear down the scheduler function stack,
// best-effort (warn-and-continue). The function must go before its application.
func (f *Functions) DeleteFunction(appId string) {
	fn, err := f.findFunction(appId, f.functionName())
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to find scheduler function for teardown: %s", err)))
		return
	}
	if fn == nil {
		return
	}
	if _, err = f.client.DeleteFunction(f.ctx, functions.DeleteFunctionRequest{FunctionId: fn.Id}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete scheduler function: %s", err)))
		return
	}
	// Wait for the async delete to settle: OCI refuses to delete the application
	// while it still owns a function.
	f.waitForFunctionDeleted(*fn.Id)
}

func (f *Functions) waitForFunctionDeleted(functionId string) {
	deadline := time.After(networkPollTimeout)
	for {
		response, err := f.client.GetFunction(f.ctx, functions.GetFunctionRequest{FunctionId: &functionId})
		if err != nil || response.LifecycleState == functions.FunctionLifecycleStateDeleted {
			return
		}
		select {
		case <-f.ctx.Done():
			return
		case <-deadline:
			return
		case <-time.After(networkPollInterval):
		}
	}
}

func (f *Functions) DeleteApplication() {
	name := f.appName()
	list, err := f.client.ListApplications(f.ctx, functions.ListApplicationsRequest{
		CompartmentId: &f.compartmentId,
		DisplayName:   &name,
	})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list function applications for teardown: %s", err)))
		return
	}
	for _, app := range list.Items {
		if app.LifecycleState == functions.ApplicationLifecycleStateDeleted || app.LifecycleState == functions.ApplicationLifecycleStateDeleting {
			continue
		}
		if _, err = f.client.DeleteApplication(f.ctx, functions.DeleteApplicationRequest{ApplicationId: app.Id}); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete function application: %s", err)))
		}
	}
}

// ApplicationId returns the OCID of an existing application, or "" if none exists
// (nothing to tear down). Used by DeleteResources to scope the function delete.
func (f *Functions) ApplicationId() (string, error) {
	name := f.appName()
	list, err := f.client.ListApplications(f.ctx, functions.ListApplicationsRequest{
		CompartmentId: &f.compartmentId,
		DisplayName:   &name,
	})
	if err != nil {
		return "", err
	}
	for _, app := range list.Items {
		if app.LifecycleState != functions.ApplicationLifecycleStateDeleted && app.LifecycleState != functions.ApplicationLifecycleStateDeleting {
			return *app.Id, nil
		}
	}
	return "", nil
}

// FunctionConfig builds the function config map the invoked function reads from its
// environment: the update build pipeline to trigger and the region for the SDK/RP.
func FunctionConfig(updatePipelineId, region string) map[string]string {
	config := map[string]string{"UPDATE_PIPELINE_ID": updatePipelineId}
	if region != "" {
		config[model.OracleRegion] = region
	}
	return config
}

// ocirRegionKey maps an OCI region identifier (eu-frankfurt-1) to its OCIR registry
// key (fra), used to build the function image host <key>.ocir.io. The SDK's own
// key→region table is unexported, so a commercial-realm map is kept here; an
// unknown region falls back to the identifier with a warning (the image is Entigo-
// CI published per region and is provisional until that CI lands).
var ocirRegionKey = map[string]string{
	"ap-chuncheon-1": "yny", "ap-hyderabad-1": "hyd", "ap-melbourne-1": "mel",
	"ap-mumbai-1": "bom", "ap-osaka-1": "kix", "ap-seoul-1": "icn",
	"ap-sydney-1": "syd", "ap-tokyo-1": "nrt", "ap-singapore-1": "sin",
	"ca-montreal-1": "yul", "ca-toronto-1": "yyz", "eu-amsterdam-1": "ams",
	"eu-frankfurt-1": "fra", "eu-zurich-1": "zrh", "eu-madrid-1": "mad",
	"eu-marseille-1": "mrs", "eu-milan-1": "lin", "eu-paris-1": "cdg",
	"eu-stockholm-1": "arn", "me-abudhabi-1": "auh", "me-dubai-1": "dxb",
	"me-jeddah-1": "jed", "il-jerusalem-1": "mtz", "sa-santiago-1": "scl",
	"sa-saopaulo-1": "gru", "sa-vinhedo-1": "vcp", "af-johannesburg-1": "jnb",
	"uk-cardiff-1": "cwl", "uk-london-1": "lhr", "us-ashburn-1": "iad",
	"us-phoenix-1": "phx", "us-sanjose-1": "sjc",
}

// SchedulerFunctionImage returns the OCIR image reference for the scheduler
// function in the given region.
func SchedulerFunctionImage(region string) string {
	key, ok := ocirRegionKey[strings.ToLower(region)]
	if !ok {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("no known OCIR region key for %q; using region as registry host", region)))
		key = region
	}
	return fmt.Sprintf(model.SchedulerFunctionImageOracle, key)
}
