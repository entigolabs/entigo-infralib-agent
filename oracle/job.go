package oracle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/containerinstances"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

const (
	containerShape  = "CI.Standard.E4.Flex"
	containerOcpus  = float32(1)
	containerMemory = float32(8)
	// OCI runs the container as the image's USER (infralib images: non-root UID
	// 1000) and, unlike CodeBuild's $CODEBUILD_SRC_DIR or Cloud Run's mounted
	// volume at /project, provides no writable working dir — a root-owned /project
	// is why plan failed with EACCES. /tmp is world-writable (mode 1777) in any
	// standard image, so it works for a non-root user with no volume. We point at
	// /tmp itself, NOT a subdir: OCI pre-creates workingDirectory root-owned, so a
	// subdir would just move the permission error down one level. The entrypoint
	// creates and cd's into its own work dir (e.g. /tmp/project) under it.
	containerWorkdir = "/tmp"
	// Poll for container completion often enough that the agent reacts promptly
	// when the container exits (a 15s interval added a noticeable lag before the
	// next step / apply launched); GetContainerInstance is cheap and the step
	// count is small, so 5s is comfortably within API limits.
	pollInterval = 5 * time.Second
	// specHashTag records the hash of the launch spec on the instance. OCI can
	// only Start an INACTIVE instance as-is (env, image and VNIC are immutable —
	// UpdateContainerInstance/UpdateContainer touch nothing but display name and
	// tags), so an instance is reused only when the desired spec hashes to the
	// same value; otherwise it is deleted and recreated. Only consulted when
	// reuseInstances is on.
	specHashTag = "entigo-spec-hash"
)

// reuseInstances toggles the persistent-instance optimisation. When true a
// step's INACTIVE instance is restarted on the next run (reuseInstance) and
// left alive between runs; when false every execution creates a fresh instance
// and deletes it as soon as its container has exited. Fresh-per-run is the
// default: reusing an INACTIVE instance added noticeable latency (the start
// work-request round-trip) and the ContainerInstance's INACTIVE transition
// lagged the container's actual exit, so exit-state capture was slow. The
// reuse machinery is kept intact behind this flag so we can switch back. See
// waitForCompletion (polls the Container object, not the ContainerInstance,
// because the container's lifecycleState/exitCode update the instant the
// process finishes).
const reuseInstances = false

// Builder runs infralib steps as OCI Container Instances. Unlike AWS CodeBuild /
// GCloud Cloud Run Jobs there is no persistent job definition, so
// CreateProject/UpdateProject record the run spec (image, networking, auth) that
// the Pipeline uses to launch instances. Specs are persisted to the config
// bucket so destroy/delete and agent flows work from a fresh process, and cached
// in-memory behind a mutex because steps run in parallel goroutines on the first
// execution cycle. The instances themselves are the nearest thing to a job:
// one per (project, command), kept INACTIVE between runs and restarted when the
// spec is unchanged — both to skip provisioning time and so gitops engineers can
// re-run a step manually from the console (Start) without the agent, the same
// way AWS CodeBuild projects and Cloud Run jobs allow.
type Builder struct {
	ctx                context.Context
	client             containerinstances.ContainerInstanceClient
	identity           identity.IdentityClient
	configStore        objectStore
	compartmentId      string
	region             string
	bucket             string
	s3Endpoint         string
	accessKey          string
	secretKey          string
	enableOpenTofu     bool
	terraformCache     bool
	cloudPrefix        string
	defaultSubnet      string
	logId              string
	availabilityDomain string
	mu                 sync.Mutex
	projects           map[string]*containerProject
	wrapperConfigOnce  sync.Once
	wrapperConfig      string
}

// containerProject is persisted as JSON in the config bucket (projectSpecFormat).
// AuthSources carries git credentials; the config bucket already holds the SSM
// secrets/ tree and the customer secret key, so the trust boundary is unchanged.
type containerProject struct {
	Image       string                      `json:"image"`
	StepType    model.StepType              `json:"stepType,omitempty"`
	VpcConfig   *model.VpcConfig            `json:"vpcConfig,omitempty"`
	AuthSources map[string]model.SourceAuth `json:"authSources,omitempty"`
	AgentCmd    common.Command              `json:"agentCmd,omitempty"` // set for agent projects
}

const projectSpecFormat = "projects/%s"

func NewBuilder(ctx context.Context, provider ocicommon.ConfigurationProvider, configStore objectStore, region, compartmentId, bucket, s3Endpoint, accessKey, secretKey string, enableOpenTofu, terraformCache bool, cloudPrefix, defaultSubnet, logId string) (*Builder, error) {
	client, err := containerinstances.NewContainerInstanceClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	idClient, err := identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
		idClient.SetRegion(region)
	}
	return &Builder{
		ctx:            ctx,
		client:         client,
		identity:       idClient,
		configStore:    configStore,
		compartmentId:  compartmentId,
		region:         region,
		bucket:         bucket,
		s3Endpoint:     s3Endpoint,
		accessKey:      accessKey,
		secretKey:      secretKey,
		enableOpenTofu: enableOpenTofu,
		terraformCache: terraformCache,
		cloudPrefix:    cloudPrefix,
		defaultSubnet:  defaultSubnet,
		logId:          logId,
		projects:       map[string]*containerProject{},
	}, nil
}

func getImage(imageVersion, imageSource string) string {
	if imageSource == "" {
		imageSource = model.ProjectImageOracle
	}
	return fmt.Sprintf("%s:%s", imageSource, imageVersion)
}

func (b *Builder) CreateProject(projectName, _, _ string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	return b.putProject(projectName, &containerProject{
		Image:       getImage(imageVersion, imageSource),
		StepType:    step.Type,
		VpcConfig:   vpcConfig,
		AuthSources: authSources,
	})
}

func (b *Builder) UpdateProject(projectName, repoURL, stepName string, step model.Step, imageVersion, imageSource string, vpcConfig *model.VpcConfig, authSources map[string]model.SourceAuth) error {
	return b.CreateProject(projectName, repoURL, stepName, step, imageVersion, imageSource, vpcConfig, authSources)
}

func (b *Builder) CreateAgentProject(projectName string, _ string, imageVersion string, cmd common.Command) error {
	return b.putProject(projectName, &containerProject{
		Image:    getImage(imageVersion, model.AgentImageOracle),
		AgentCmd: cmd,
	})
}

func (b *Builder) UpdateAgentProject(projectName, version, _ string) error {
	project, err := b.getProject(projectName)
	if err != nil {
		return err
	}
	if project == nil {
		return b.CreateAgentProject(projectName, b.cloudPrefix, version, common.RunCommand)
	}
	updated := *project
	updated.Image = getImage(version, model.AgentImageOracle)
	return b.putProject(projectName, &updated)
}

func (b *Builder) GetProject(projectName string) (*model.Project, error) {
	project, err := b.getProject(projectName)
	if err != nil || project == nil {
		return nil, err
	}
	return &model.Project{
		Name:           projectName,
		Image:          project.Image,
		TerraformCache: strconv.FormatBool(b.terraformCache),
	}, nil
}

func (b *Builder) DeleteProject(projectName string, _ model.Step) error {
	b.mu.Lock()
	delete(b.projects, projectName)
	b.mu.Unlock()
	return b.configStore.DeleteFile(fmt.Sprintf(projectSpecFormat, projectName))
}

func (b *Builder) putProject(projectName string, project *containerProject) error {
	data, err := json.Marshal(project)
	if err != nil {
		return err
	}
	if err = b.configStore.PutFile(fmt.Sprintf(projectSpecFormat, projectName), data); err != nil {
		return fmt.Errorf("failed to persist project spec %s: %w", projectName, err)
	}
	b.mu.Lock()
	b.projects[projectName] = project
	b.mu.Unlock()
	return nil
}

// wrapperConfigValue returns the portal wrapper config yaml that
// upsertWrapperConfig stored in the config bucket, or "" when portal
// notifications aren't configured. Read once per process: it is written before
// any pipeline runs and stable afterwards.
func (b *Builder) wrapperConfigValue() string {
	b.wrapperConfigOnce.Do(func() {
		content, err := b.configStore.GetFile(secretKey(model.WrapperConfigSecretName(b.cloudPrefix)))
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to read wrapper config secret: %s", err)))
			return
		}
		b.wrapperConfig = string(content)
	})
	return b.wrapperConfig
}

// putRunContext persists the campaign correlation for the container execution
// about to start; the wrapper deletes the object after reading it. With no
// active campaign any stale context (from an agent that died between write and
// launch) is removed instead, so the next execution can't report under it.
func (b *Builder) putRunContext(prefixStep string, command model.ActionCommand, campaignId string, pipelineIndex int) error {
	key := fmt.Sprintf(model.OracleRunContextFormat, prefixStep, command)
	if campaignId == "" {
		return b.configStore.DeleteFile(key)
	}
	data, err := json.Marshal(model.OracleRunContext{CampaignId: campaignId, PipelineIndex: pipelineIndex})
	if err != nil {
		return err
	}
	return b.configStore.PutFile(key, data)
}

// getProject returns the cached spec, falling back to the persisted copy so
// destroy/delete and agent flows work in processes that never called CreateProject.
func (b *Builder) getProject(projectName string) (*containerProject, error) {
	b.mu.Lock()
	project, ok := b.projects[projectName]
	b.mu.Unlock()
	if ok {
		return project, nil
	}
	content, err := b.configStore.GetFile(fmt.Sprintf(projectSpecFormat, projectName))
	if err != nil || content == nil {
		return nil, err
	}
	loaded := &containerProject{}
	if err = json.Unmarshal(content, loaded); err != nil {
		return nil, fmt.Errorf("failed to parse project spec %s: %w", projectName, err)
	}
	b.mu.Lock()
	b.projects[projectName] = loaded
	b.mu.Unlock()
	return loaded, nil
}

// launch runs the given command for a step in a Container Instance and returns
// its OCID. An existing INACTIVE instance with the same name and an unchanged
// spec is restarted; otherwise a new instance is created (running immediately —
// OCI has no way to create one stopped).
func (b *Builder) launch(projectName, prefixStep string, command model.ActionCommand, step model.Step) (string, error) {
	displayName := instanceName(projectName, command)
	log.Printf("Executing container instance %s\n", displayName)
	project, err := b.getProject(projectName)
	if err != nil {
		return "", err
	}
	if project == nil {
		return "", fmt.Errorf("no container project registered for %s", projectName)
	}
	subnet, publicIp, err := b.subnetId(project.VpcConfig)
	if err != nil {
		return "", err
	}
	ad, err := b.getAvailabilityDomain()
	if err != nil {
		return "", err
	}
	env := b.containerEnv(prefixStep, command, step, project)
	var nsgIds []string
	if project.VpcConfig != nil && len(project.VpcConfig.SecurityGroupIds) > 0 {
		nsgIds = project.VpcConfig.SecurityGroupIds
	}
	hash := specHash(project.Image, subnet, publicIp, nsgIds, env)
	if reuseInstances {
		if instanceId := b.reuseInstance(displayName, hash); instanceId != "" {
			return instanceId, nil
		}
	}
	vnic := containerinstances.CreateContainerVnicDetails{
		SubnetId:           &subnet,
		IsPublicIpAssigned: new(publicIp),
		NsgIds:             nsgIds,
	}
	response, err := b.client.CreateContainerInstance(b.ctx, containerinstances.CreateContainerInstanceRequest{
		CreateContainerInstanceDetails: containerinstances.CreateContainerInstanceDetails{
			CompartmentId:      &b.compartmentId,
			AvailabilityDomain: &ad,
			Shape:              new(containerShape),
			ShapeConfig: &containerinstances.CreateContainerInstanceShapeConfigDetails{
				Ocpus:       new(containerOcpus),
				MemoryInGBs: new(containerMemory),
			},
			DisplayName:            &displayName,
			ContainerRestartPolicy: containerinstances.ContainerInstanceContainerRestartPolicyNever,
			Vnics:                  []containerinstances.CreateContainerVnicDetails{vnic},
			Containers: []containerinstances.CreateContainerDetails{{
				ImageUrl:         &project.Image,
				DisplayName:      &displayName,
				WorkingDirectory: new(containerWorkdir),
				// Enable the container instance resource principal (RP v2.2) so
				// the platform injects the OCI_RESOURCE_PRINCIPAL_* tokens the
				// in-container SDK and oci provider authenticate with. Explicit
				// rather than relying on the API's default.
				IsResourcePrincipalDisabled: new(false),
				EnvironmentVariables:        env,
			}},
			FreeformTags: map[string]string{
				model.ResourceTagKey: model.ResourceTagValue,
				specHashTag:          hash,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create container instance for %s: %w", displayName, err)
	}
	return *response.Id, nil
}

// instanceName is the persistent identity of a (project, command) instance.
// Agent projects already carry the command suffix in their name and launch
// with an empty command.
func instanceName(projectName string, command model.ActionCommand) string {
	if command == "" {
		return projectName
	}
	return fmt.Sprintf("%s-%s", projectName, command)
}

// instanceSpec captures every launch input a Container Instance cannot change
// after creation. The env map covers command, credentials and git auth, so any
// of those changing forces a fresh instance. Volatile per-execution values
// (e.g. campaign correlation) must never be added to the env — OCI has no
// run-time overrides, so they would force a recreate on every run; deliver
// them out-of-band via the config bucket instead.
type instanceSpec struct {
	Image     string            `json:"image"`
	Shape     string            `json:"shape"`
	Ocpus     float32           `json:"ocpus"`
	MemoryGBs float32           `json:"memoryGBs"`
	Workdir   string            `json:"workdir"`
	Subnet    string            `json:"subnet"`
	PublicIp  bool              `json:"publicIp"`
	NsgIds    []string          `json:"nsgIds,omitempty"`
	Env       map[string]string `json:"env"`
}

func specHash(image, subnet string, publicIp bool, nsgIds []string, env map[string]string) string {
	data, _ := json.Marshal(instanceSpec{
		Image:     image,
		Shape:     containerShape,
		Ocpus:     containerOcpus,
		MemoryGBs: containerMemory,
		Workdir:   containerWorkdir,
		Subnet:    subnet,
		PublicIp:  publicIp,
		NsgIds:    nsgIds,
		Env:       env,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// reuseInstance starts an existing INACTIVE instance whose recorded spec hash
// matches the desired launch spec. Terminal instances that cannot serve the
// launch — spec changed, or FAILED (Start only works on INACTIVE) — are
// deleted so they don't pile up under the same name; non-terminal ones are
// left alone (a concurrent execution, or the agent's own instance relaunching
// itself). Best-effort: any error falls back to creating a fresh instance.
func (b *Builder) reuseInstance(displayName, hash string) string {
	instances, err := b.listInstancesNamed(displayName)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list container instances named %s: %s", displayName, err)))
		return ""
	}
	var reusable *string
	for _, instance := range instances {
		switch instance.LifecycleState {
		case containerinstances.ContainerInstanceLifecycleStateInactive:
			if reusable == nil && instance.FreeformTags[specHashTag] == hash {
				reusable = instance.Id
				continue
			}
		case containerinstances.ContainerInstanceLifecycleStateFailed:
		default:
			continue
		}
		b.deleteInstanceLogged(*instance.Id)
	}
	if reusable == nil {
		return ""
	}
	if err = b.startInstance(*reusable); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to restart container instance %s, recreating: %s", displayName, err)))
		b.deleteInstanceLogged(*reusable)
		return ""
	}
	slog.Debug(fmt.Sprintf("Reusing container instance %s\n", displayName))
	return *reusable
}

// startInstance re-runs an INACTIVE instance's container and waits for the
// start work request to finish, so waitForCompletion cannot mistake the
// pre-start INACTIVE state for the new run having already completed.
func (b *Builder) startInstance(instanceId string) error {
	response, err := b.client.StartContainerInstance(b.ctx, containerinstances.StartContainerInstanceRequest{
		ContainerInstanceId: &instanceId,
	})
	if err != nil {
		return err
	}
	if response.OpcWorkRequestId == nil {
		return fmt.Errorf("start of %s returned no work request", instanceId)
	}
	return b.waitForWorkRequest(*response.OpcWorkRequestId)
}

func (b *Builder) waitForWorkRequest(workRequestId string) error {
	for {
		response, err := b.client.GetWorkRequest(b.ctx, containerinstances.GetWorkRequestRequest{
			WorkRequestId: &workRequestId,
		})
		if err != nil {
			return err
		}
		switch response.Status {
		case containerinstances.OperationStatusSucceeded:
			slog.Debug("Job operation succeeded")
			return nil
		case containerinstances.OperationStatusFailed, containerinstances.OperationStatusCanceling,
			containerinstances.OperationStatusCanceled:
			return fmt.Errorf("work request %s ended as %s", workRequestId, response.Status)
		}
		select {
		case <-b.ctx.Done():
			return b.ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// listInstancesNamed returns the non-deleted instances this agent created with
// the given display name. Display names are not unique in OCI, so multiple
// matches are possible (e.g. a run launched while a previous one was active).
func (b *Builder) listInstancesNamed(displayName string) ([]containerinstances.ContainerInstanceSummary, error) {
	var instances []containerinstances.ContainerInstanceSummary
	var page *string
	for {
		response, err := b.client.ListContainerInstances(b.ctx, containerinstances.ListContainerInstancesRequest{
			CompartmentId: &b.compartmentId,
			DisplayName:   &displayName,
			Page:          page,
		})
		if err != nil {
			return nil, err
		}
		for _, instance := range response.Items {
			if instance.Id == nil || instance.FreeformTags[model.ResourceTagKey] != model.ResourceTagValue {
				continue
			}
			switch instance.LifecycleState {
			case containerinstances.ContainerInstanceLifecycleStateDeleting,
				containerinstances.ContainerInstanceLifecycleStateDeleted:
				continue
			}
			instances = append(instances, instance)
		}
		if response.OpcNextPage == nil {
			return instances, nil
		}
		page = response.OpcNextPage
	}
}

// waitForCompletion waits for a step's container to exit and returns its exit
// code. It polls the Container object (not the ContainerInstance): the
// container's lifecycleState flips to INACTIVE and its exitCode is populated the
// instant the process finishes, whereas the enclosing instance's INACTIVE
// transition lags behind — polling the container captures the result sooner.
// Logs are NOT read here: RetrieveLogs only works while the container is ACTIVE
// (it 409s once INACTIVE) and is capped at 256 KB, so stdout is instead pushed
// to OCI Logging by the in-container wrapper and read back via Logging.StepLogs.
// In fresh-per-run mode (reuseInstances off) the instance is deleted as soon as
// the container terminates and the exit code is captured — nothing reads the
// instance afterwards. Deletion is skipped when the wait was cut short (ctx
// cancellation, API error) so a mid-flight apply runs to completion, and in
// reuse mode the INACTIVE instance is kept for the next run.
func (b *Builder) waitForCompletion(instanceId string) (int, error) {
	containerId, err := b.waitForContainerId(instanceId)
	if err != nil {
		return 0, err
	}
	exitCode, terminated, err := b.pollContainer(containerId)
	if terminated && !reuseInstances {
		b.deleteInstanceLogged(instanceId)
	}
	return exitCode, err
}

// waitForContainerId resolves the id of the instance's single container,
// polling briefly in case the instance is still CREATING and hasn't exposed it.
func (b *Builder) waitForContainerId(instanceId string) (string, error) {
	for {
		instance, err := b.client.GetContainerInstance(b.ctx, containerinstances.GetContainerInstanceRequest{
			ContainerInstanceId: &instanceId,
		})
		if err != nil {
			return "", err
		}
		if len(instance.Containers) > 0 && instance.Containers[0].ContainerId != nil {
			return *instance.Containers[0].ContainerId, nil
		}
		switch instance.LifecycleState {
		case containerinstances.ContainerInstanceLifecycleStateFailed,
			containerinstances.ContainerInstanceLifecycleStateDeleting,
			containerinstances.ContainerInstanceLifecycleStateDeleted:
			return "", fmt.Errorf("container instance %s has no container (state %s)", instanceId, instance.LifecycleState)
		}
		select {
		case <-b.ctx.Done():
			return "", b.ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// pollContainer polls a container until it finishes, returning its exit code and
// whether it reached a terminal state (false only when the wait was interrupted
// by ctx cancellation).
//
// exitCode is NOT itself a completion signal: OCI reports exitCode=0 as a
// placeholder from the moment the container turns ACTIVE — while the process is
// still running (verified live) — so returning on a non-nil exitCode deletes the
// container ~instantly, before it does any work (no logs ever reach OCI Logging).
//
// Completion keys on lifecycleState. The observed lifecycle of a run is
// CREATING → ACTIVE → UPDATING → INACTIVE, and the exit code is already correct
// at UPDATING, so returning there shaves the UPDATING→INACTIVE teardown wait
// (~10-20s). But UPDATING is only trusted once ACTIVE has been seen: a reused
// instance (StartContainerInstance) passes through UPDATING while STARTING,
// before ACTIVE, where the exit code would be stale. INACTIVE/FAILED remain the
// terminal fallbacks (e.g. a fast container that never reports an ACTIVE poll).
// Every state change is logged so the real timeline stays observable.
func (b *Builder) pollContainer(containerId string) (int, bool, error) {
	var lastState containerinstances.ContainerLifecycleStateEnum
	sawActive := false
	for {
		container, err := b.client.GetContainer(b.ctx, containerinstances.GetContainerRequest{
			ContainerId: &containerId,
		})
		if err != nil {
			return 0, false, err
		}
		if container.LifecycleState != lastState {
			slog.Debug(fmt.Sprintf("Container %s state=%s exitCode=%s", containerId, container.LifecycleState, exitCodeString(container.ExitCode)))
			lastState = container.LifecycleState
		}
		switch container.LifecycleState {
		case containerinstances.ContainerLifecycleStateActive:
			sawActive = true
		case containerinstances.ContainerLifecycleStateUpdating:
			if sawActive {
				return exitCodeValue(container.ExitCode), true, nil
			}
		case containerinstances.ContainerLifecycleStateInactive:
			return exitCodeValue(container.ExitCode), true, nil
		case containerinstances.ContainerLifecycleStateFailed:
			return exitCodeValue(container.ExitCode), true, fmt.Errorf("container %s failed", containerId)
		case containerinstances.ContainerLifecycleStateDeleting,
			containerinstances.ContainerLifecycleStateDeleted:
			return exitCodeValue(container.ExitCode), true, fmt.Errorf("container %s was deleted before it finished", containerId)
		}
		select {
		case <-b.ctx.Done():
			return 0, false, b.ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func exitCodeValue(code *int) int {
	if code != nil {
		return *code
	}
	return 0
}

func exitCodeString(code *int) string {
	if code != nil {
		return strconv.Itoa(*code)
	}
	return "<nil>"
}

func (b *Builder) deleteInstance(instanceId string) error {
	_, err := b.client.DeleteContainerInstance(b.ctx, containerinstances.DeleteContainerInstanceRequest{
		ContainerInstanceId: &instanceId,
	})
	return err
}

func (b *Builder) deleteInstanceLogged(instanceId string) {
	if err := b.deleteInstance(instanceId); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete container instance %s: %s", instanceId, err)))
	}
}

// deleteProjectInstances removes the persistent instances a removed step left
// behind — one per action command it has executed. The step type is unknown
// here, so every command's name is tried. Best-effort so a removed step never
// blocks the delete flow.
func (b *Builder) deleteProjectInstances(projectName string) {
	for _, command := range []model.ActionCommand{
		model.PlanCommand, model.ApplyCommand, model.PlanDestroyCommand, model.ApplyDestroyCommand,
		model.ArgoCDPlanCommand, model.ArgoCDApplyCommand, model.ArgoCDPlanDestroyCommand, model.ArgoCDApplyDestroyCommand,
	} {
		instances, err := b.listInstancesNamed(instanceName(projectName, command))
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list container instances of %s: %s", projectName, err)))
			continue
		}
		for _, instance := range instances {
			b.deleteInstanceLogged(*instance.Id)
		}
	}
}

// subnetId resolves the subnet a step's container runs in and reports whether it
// is the agent's bootstrap subnet. A step that attaches its own VPC uses that
// subnet (assumed private with its own NAT egress); a step without one falls back
// to the bootstrap subnet, which is public and needs a public IP for egress.
func (b *Builder) subnetId(vpcConfig *model.VpcConfig) (string, bool, error) {
	if vpcConfig != nil && len(vpcConfig.Subnets) > 0 {
		return vpcConfig.Subnets[0], false, nil
	}
	if b.defaultSubnet != "" {
		return b.defaultSubnet, true, nil
	}
	return "", false, fmt.Errorf("oracle cloud execution requires a subnet; set the step vpc subnet ids")
}

func (b *Builder) getAvailabilityDomain() (string, error) {
	if b.availabilityDomain != "" {
		return b.availabilityDomain, nil
	}
	response, err := b.identity.ListAvailabilityDomains(b.ctx, identity.ListAvailabilityDomainsRequest{
		CompartmentId: &b.compartmentId,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list availability domains: %w", err)
	}
	if len(response.Items) == 0 || response.Items[0].Name == nil {
		return "", fmt.Errorf("no availability domains found in compartment")
	}
	b.availabilityDomain = *response.Items[0].Name
	return b.availabilityDomain, nil
}

// containerEnv mirrors service.LocalPipeline.getEnv so the base image behaves
// identically whether a step runs locally or in a Container Instance.
func (b *Builder) containerEnv(prefixStep string, command model.ActionCommand, step model.Step, project *containerProject) map[string]string {
	env := map[string]string{
		"COMMAND":             string(command),
		"TF_VAR_prefix":       prefixStep,
		"INFRALIB_BUCKET":     b.bucket,
		model.OracleRegion:    b.region,
		"AWS_REGION":          b.region,
		"AWS_ENDPOINT_URL_S3": b.s3Endpoint,
		// The OpenTofu/Terraform oci provider defaults its `auth` argument to
		// ApiKey (env MultiEnvDefaultFunc over TF_VAR_auth/OCI_AUTH), which in a
		// Container Instance finds no ~/.oci/config and fails with "did not find a
		// proper configuration for tenancy". In-container the only credential is
		// the container's resource principal, so select it explicitly; the
		// platform injects the OCI_RESOURCE_PRINCIPAL_* tokens the provider then
		// reads (container RP is enabled below). Region comes from OCI_REGION,
		// set above via model.OracleRegion — without it RP auth pins the provider
		// to the tenancy home region.
		"OCI_AUTH": "ResourcePrincipal",
	}
	// The wrapper pushes stdout to this OCI custom Log; the subject it stamps is
	// TF_VAR_prefix/COMMAND, which is exactly what Logging.StepLogs searches for.
	if b.logId != "" {
		env[model.OracleLogOCID] = b.logId
	}
	if step.Name != "" {
		env["INFRALIB_STEP"] = step.Name
	}
	// Stable env only — this map feeds the reuse spec hash, so the portal
	// wrapper config (changes only when notification config changes) is fine
	// here, while the per-run campaign correlation goes via putRunContext.
	if config := b.wrapperConfigValue(); config != "" && project.AgentCmd == "" {
		env[model.WrapperConfigEnv] = config
	}
	if project.AgentCmd != "" {
		// The relaunched agent selects the Oracle provider via the compartment id
		// and derives every bucket name from the prefix; without these it would
		// fall through to the AWS provider.
		env["COMMAND"] = string(project.AgentCmd)
		env[common.OracleCompartmentIdEnv] = b.compartmentId
		env[common.PrefixEnv] = b.cloudPrefix
	}
	if b.accessKey != "" {
		env["AWS_ACCESS_KEY_ID"] = b.accessKey
		env["AWS_SECRET_ACCESS_KEY"] = b.secretKey
	}
	for source, auth := range project.AuthSources {
		hash := util.HashCode(source)
		env[fmt.Sprintf(model.GitSourceEnvFormat, hash)] = source
		env[fmt.Sprintf(model.GitUsernameEnvFormat, hash)] = auth.Username
		env[fmt.Sprintf(model.GitPasswordEnvFormat, hash)] = auth.Password
	}
	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			env["KUBERNETES_CLUSTER_NAME"] = step.KubernetesClusterName
		}
		if step.ArgocdNamespace == "" {
			env["ARGOCD_NAMESPACE"] = "argocd"
		} else {
			env["ARGOCD_NAMESPACE"] = step.ArgocdNamespace
		}
	}
	if step.Type == model.StepTypeTerraform {
		env["TERRAFORM_CACHE"] = fmt.Sprintf("%t", b.terraformCache)
		if b.enableOpenTofu {
			env["TF_TOOL"] = string(model.TofuTfTool)
		}
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				name := strings.ToUpper(module.Name)
				env[fmt.Sprintf("GIT_AUTH_USERNAME_%s", name)] = module.HttpUsername
				env[fmt.Sprintf("GIT_AUTH_PASSWORD_%s", name)] = module.HttpPassword
				env[fmt.Sprintf("GIT_AUTH_SOURCE_%s", name)] = module.Source
			}
		}
	}
	return env
}
