package oracle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	ocicommon "github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/devops"
	"github.com/oracle/oci-go-sdk/v65/ons"
)

// Each step runs through an OCI DevOps *build* pipeline: a managed OL8 build
// runner (the only OCI DevOps runner with a Docker daemon) does
// `docker run <base image>`.
//
// Why build pipelines rather than a single native plan→approve→apply pipeline:
// OCI can host "run an arbitrary container" only in BUILD pipelines (Docker, and
// native build-run logs — the good UX) and "manual approval" only in DEPLOYMENT
// pipelines (no Docker). Neither can host the other's capability, and a build
// pipeline's TRIGGER_DEPLOYMENT stage fires-and-forgets (verified live — it does
// not block on the triggered deployment). So the agent orchestrates the
// sandwich: it launches the plan build run, reads the plan from the logs, drives
// the manual-approval DevOps deployment (Gate), then launches the apply build
// run.
//
// The container environment is delivered without any bucket-side plaintext:
// non-secret values are build-pipeline PARAMETERS (exported into the build
// environment as EI_<NAME>), and secret values are the step's own Vault secrets
// injected via the build spec's `vaultVariables` (each keyed EI_<NAME>, its value
// the secret's literal OCID — OCI resolves vaultVariables at run-environment
// build time and does NOT substitute pipeline parameters into that value). A
// generic runtime loop forwards every EI_<NAME> into `docker run -e <NAME>`.
// Because the set of
// secrets/params differs per step, the build spec is generated per (step,command)
// and committed to the shared hosted repo; the spec + parameter set are
// reconciled (and the spec re-pushed) only when their content changes.
const (
	// pollInterval is how often the DevOps build-run / work-request / approval
	// polls re-check state. GetBuildRun is cheap and the step count small.
	pollInterval = 5 * time.Second
	// imageParam carries the base image per run (avoids re-pushing the spec /
	// re-declaring parameters on every version bump). Consumed by the spec, not
	// forwarded into the container (no EI_ prefix).
	imageParam = "IMAGE"
	// envParamPrefix marks a build-pipeline parameter (or vaultVariable) whose
	// value the spec forwards into the container as `docker run -e <NAME>`, with
	// the prefix stripped. Keeps our env off the runner's own environment.
	envParamPrefix = "EI_"
	// specRepoPrefix locates a step's build spec in the hosted repo — the single
	// source of truth (no bucket copy). The repo is what the build runner reads.
	specRepoPrefix = "specs/"
	// specHashTag is the build-pipeline freeform tag holding the hash of the spec
	// currently in the repo, so a reconcile pushes only when the spec changed.
	specHashTag     = "infralib-spec-hash"
	buildRunTimeout = 60 * time.Minute
	buildSpecBranch = "main"
	buildStageName  = "run"
)

// DevOpsBuilder executes infralib steps through OCI DevOps build pipelines.
// One shared project (<prefix>-infralib) holds a single hosted code repo carrying
// the per-step build specs plus one build pipeline per (step, command) — e.g.
// <prefix>-hello-plan, <prefix>-hello-apply — so a gitops engineer sees a uniquely
// named pipeline for every step action with native build-run logs. Each pipeline
// carries its non-secret env as parameters (defaults) and its secret OCIDs as
// parameters referenced by that step's spec vaultVariables. The project also hosts
// the manual-approval deployment pipelines (see Gate.UseProject).
//
// Setup mirrors the CSK bootstrap: the git push of the build specs needs a user
// (auth token), so the FIRST run (and any run that changes a spec) must be local
// (session-token or API-key auth); in-container resource-principal runs reference
// the already-pushed specs.
type DevOpsBuilder struct {
	ctx            context.Context
	client         devops.DevopsClient
	onsClient      ons.NotificationControlPlaneClient
	compartmentId  string
	region         string
	cloudPrefix    string
	once           sync.Once
	ensureErr      error
	projectId      string
	repoId         string
	repoURL        string
	gitUsername    string                 // HTTPS basic-auth username for the spec push; injected by SetGitAuth (no IAM/Vault reads here)
	gitToken       string                 // OCI auth token used as the git password; injected by SetGitAuth
	authTokenFresh bool                   // true when the injected token was just created, so pushSpec retries while it propagates
	mu             sync.Mutex             // guards the maps below; held only for short in-memory sections
	pipelines      map[string]string      // pipeline display name → build pipeline OCID (reconciled)
	pushedSpecs    map[string]string      // spec file path → hash pushed this process (dedupes redundant pushes)
	keyLocks       map[string]*sync.Mutex // per-pipeline reconcile lock, so one pipeline serializes with itself but not with others
	pushMu         sync.Mutex             // serializes git pushes to the single shared spec repo
}

// ProjectId returns the shared DevOps project OCID after Ensure has run (needed
// to enable the project's build logs and to host the approval pipelines).
func (d *DevOpsBuilder) ProjectId() string { return d.projectId }

func NewDevOpsBuilder(ctx context.Context, provider ocicommon.ConfigurationProvider, region, compartmentId, cloudPrefix string) (*DevOpsBuilder, error) {
	client, err := devops.NewDevopsClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	onsClient, err := ons.NewNotificationControlPlaneClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, err
	}
	if region != "" {
		client.SetRegion(region)
		onsClient.SetRegion(region)
	}
	return &DevOpsBuilder{
		ctx:           ctx,
		client:        client,
		onsClient:     onsClient,
		compartmentId: compartmentId,
		region:        region,
		cloudPrefix:   cloudPrefix,
		pipelines:     map[string]string{},
		pushedSpecs:   map[string]string{},
		keyLocks:      map[string]*sync.Mutex{},
	}, nil
}

// Ensure provisions (once) the shared DevOps project, its notification topic and
// the hosted build-spec repository — all list-or-create, working from any principal
// that can manage DevOps. The later git push of build specs (pushSpec) is what needs
// a user; it resolves the agent service account on demand. Per-step build pipelines +
// their specs are created lazily by launchBuildRun.
func (d *DevOpsBuilder) Ensure() error {
	d.once.Do(func() {
		d.ensureErr = d.ensure()
	})
	return d.ensureErr
}

func (d *DevOpsBuilder) ensure() error {
	topicId, err := d.ensureTopic(fmt.Sprintf("%s-approvals", d.cloudPrefix))
	if err != nil {
		return err
	}
	projectId, err := d.ensureProject(fmt.Sprintf("%s-infralib", d.cloudPrefix), topicId)
	if err != nil {
		return err
	}
	d.projectId = projectId
	repoId, repoURL, err := d.ensureRepository(repositoryName(d.cloudPrefix))
	if err != nil {
		return err
	}
	d.repoId, d.repoURL = repoId, repoURL
	return nil
}

// repositoryName is the shared hosted build-spec repo's name, referenced both when
// creating it and when scoping the agent SA's devops-repository grant to it.
func repositoryName(cloudPrefix string) string {
	return fmt.Sprintf("%s-infralib-src", cloudPrefix)
}

// ensureTopic finds or creates the notification topic the shared project (and
// thus the approval deployments hosted in it) publish to.
func (d *DevOpsBuilder) ensureTopic(name string) (string, error) {
	list, err := d.onsClient.ListTopics(d.ctx, ons.ListTopicsRequest{CompartmentId: &d.compartmentId, Name: &name})
	if err != nil {
		return "", fmt.Errorf("failed to list notification topics: %w", err)
	}
	for _, topic := range list.Items {
		if topic.LifecycleState == ons.NotificationTopicSummaryLifecycleStateActive {
			return *topic.TopicId, nil
		}
	}
	description := "Entigo infralib approval notifications"
	created, err := d.onsClient.CreateTopic(d.ctx, ons.CreateTopicRequest{
		CreateTopicDetails: ons.CreateTopicDetails{
			Name:          &name,
			CompartmentId: &d.compartmentId,
			Description:   &description,
			FreeformTags:  map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create notification topic %s: %w", name, err)
	}
	return *created.TopicId, nil
}

func (d *DevOpsBuilder) ensureProject(name, topicId string) (string, error) {
	list, err := d.client.ListProjects(d.ctx, devops.ListProjectsRequest{
		CompartmentId: &d.compartmentId,
		Name:          &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list devops projects: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, nil
	}
	description := "Entigo infralib DevOps-native execution (build pipelines + approval gate)"
	created, err := d.client.CreateProject(d.ctx, devops.CreateProjectRequest{
		CreateProjectDetails: devops.CreateProjectDetails{
			Name:               &name,
			CompartmentId:      &d.compartmentId,
			Description:        &description,
			NotificationConfig: &devops.NotificationConfig{TopicId: &topicId},
			FreeformTags:       map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create devops project %s: %w", name, err)
	}
	return *created.Id, nil
}

// ensureRepository gets or creates the hosted code repo holding the build specs.
func (d *DevOpsBuilder) ensureRepository(name string) (string, string, error) {
	list, err := d.client.ListRepositories(d.ctx, devops.ListRepositoriesRequest{
		ProjectId: &d.projectId,
		Name:      &name,
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to list repositories: %w", err)
	}
	if len(list.Items) > 0 {
		response, err := d.client.GetRepository(d.ctx, devops.GetRepositoryRequest{RepositoryId: list.Items[0].Id})
		if err != nil {
			return "", "", fmt.Errorf("failed to get repository %s: %w", *list.Items[0].Id, err)
		}
		url := ""
		if response.HttpUrl != nil {
			url = *response.HttpUrl
		}
		return *response.Id, url, nil
	}
	defaultBranch := buildSpecBranch
	description := "Entigo infralib DevOps build specs"
	created, err := d.client.CreateRepository(d.ctx, devops.CreateRepositoryRequest{
		CreateRepositoryDetails: devops.CreateRepositoryDetails{
			Name:           &name,
			ProjectId:      &d.projectId,
			RepositoryType: devops.RepositoryRepositoryTypeHosted,
			DefaultBranch:  &defaultBranch,
			Description:    &description,
			FreeformTags:   map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to create repository %s: %w", name, err)
	}
	url := ""
	if created.HttpUrl != nil {
		url = *created.HttpUrl
	}
	return *created.Id, url, nil
}

// launchBuildRun ensures the step+command build pipeline exists with the right
// parameters + spec, then starts a build run, returning its OCID (consumed by
// waitForBuildRun). Non-secret values ride the pipeline parameters; secret OCIDs
// ride parameters referenced by the spec's vaultVariables; only IMAGE and the
// portal campaign correlation are supplied per run. specFile is the shared
// per-step spec path the pipeline's stage reads.
func (d *DevOpsBuilder) launchBuildRun(displayName, specFile, image string, params, secretRefs, perRun map[string]string) (string, error) {
	pipelineId, err := d.ensurePipeline(displayName, specFile, image, params, secretRefs)
	if err != nil {
		return "", err
	}
	args := []devops.BuildRunArgument{{Name: new(imageParam), Value: &image}}
	for name, value := range perRun {
		paramName := envParamPrefix + name
		v := value
		args = append(args, devops.BuildRunArgument{Name: &paramName, Value: &v})
	}
	response, err := d.client.CreateBuildRun(d.ctx, devops.CreateBuildRunRequest{
		CreateBuildRunDetails: devops.CreateBuildRunDetails{
			BuildPipelineId:   &pipelineId,
			DisplayName:       &displayName,
			BuildRunArguments: &devops.BuildRunArgumentCollection{Items: args},
			FreeformTags:      map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create build run for %s: %w", displayName, err)
	}
	return *response.Id, nil
}

// triggerBuildRun starts a build run against an already-created (step,command)
// pipeline found by display name, relying entirely on the pipeline's baked-in
// parameter defaults (image + non-secret env + secret OCIDs). Used by
// agentless/fresh-process flows — console-triggerable destroy — that no longer
// hold the run spec, so it neither reconciles parameters nor passes IMAGE per run.
// A missing pipeline returns model.NotFoundError so the destroy flow can skip a
// step that was never created. perRun carries only the portal campaign
// correlation, when present.
func (d *DevOpsBuilder) triggerBuildRun(displayName string, perRun map[string]string) (string, error) {
	d.mu.Lock()
	pipelineId, ok := d.pipelines[displayName]
	d.mu.Unlock()
	if !ok {
		list, err := d.client.ListBuildPipelines(d.ctx, devops.ListBuildPipelinesRequest{
			ProjectId:   &d.projectId,
			DisplayName: &displayName,
		})
		if err != nil {
			return "", fmt.Errorf("failed to list build pipelines: %w", err)
		}
		if len(list.Items) == 0 {
			return "", model.NewNotFoundError(fmt.Sprintf("build pipeline %s", displayName))
		}
		pipelineId = *list.Items[0].Id
	}
	args := make([]devops.BuildRunArgument, 0, len(perRun))
	for name, value := range perRun {
		paramName := envParamPrefix + name
		v := value
		args = append(args, devops.BuildRunArgument{Name: &paramName, Value: &v})
	}
	response, err := d.client.CreateBuildRun(d.ctx, devops.CreateBuildRunRequest{
		CreateBuildRunDetails: devops.CreateBuildRunDetails{
			BuildPipelineId:   &pipelineId,
			DisplayName:       &displayName,
			BuildRunArguments: &devops.BuildRunArgumentCollection{Items: args},
			FreeformTags:      map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create build run for %s: %w", displayName, err)
	}
	return *response.Id, nil
}

// ensurePipeline reconciles the (step,command) pipeline: creates it with the
// desired parameters, ensures its stage points at the step's spec file, and
// (re)pushes the spec when the specs tree changes. Cached per process — a step's
// params/secrets are stable within one run.
func (d *DevOpsBuilder) ensurePipeline(displayName, specFile, image string, params, secretRefs map[string]string) (string, error) {
	// Serialize only this pipeline with itself (idempotent create), so different
	// steps reconcile — and wait on their async work requests — concurrently.
	lock := d.keyLock(displayName)
	lock.Lock()
	defer lock.Unlock()
	d.mu.Lock()
	id, ok := d.pipelines[displayName]
	d.mu.Unlock()
	if ok {
		return id, nil
	}
	desiredParams := buildPipelineParameters(image, params)
	// The spec is per-STEP, not per-command: it depends only on the env-var key set
	// and secrets (identical across plan/apply/destroy) — COMMAND is a parameter, not
	// baked in — so all of a step's command pipelines share one specs/<step>.yaml.
	spec := buildSpecYAMLFor(forwardNames(params, secretRefs), vaultVariables(secretRefs))
	specHash := hashSpec(spec)
	pipelineId, priorHash, err := d.getOrCreatePipeline(displayName, desiredParams)
	if err != nil {
		return "", err
	}
	if err = d.ensureStage(pipelineId, specFile); err != nil {
		return "", err
	}
	if err = d.ensureSpecPushed(specFile, spec, specHash, priorHash); err != nil {
		return "", err
	}
	// Reconcile params (non-secret defaults + secret OCIDs drift with config/CSK)
	// and record the spec hash now living in the repo.
	if err = d.updatePipeline(pipelineId, desiredParams, specHash); err != nil {
		return "", err
	}
	d.mu.Lock()
	d.pipelines[displayName] = pipelineId
	d.mu.Unlock()
	return pipelineId, nil
}

// keyLock returns the reconcile mutex for a pipeline display name, creating it on
// first use, so ensurePipeline serializes each pipeline with itself while letting
// distinct pipelines run in parallel.
func (d *DevOpsBuilder) keyLock(displayName string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	lock, ok := d.keyLocks[displayName]
	if !ok {
		lock = &sync.Mutex{}
		d.keyLocks[displayName] = lock
	}
	return lock
}

// ensureSpecPushed pushes the step's shared spec to the hosted repo when it has
// changed. The repo is the single source of truth, so it pushes only when this
// pipeline doesn't already reference the current spec (priorHash) AND a sibling
// command of the same step hasn't already pushed the shared file this process. A
// freshly created pipeline (priorHash == "") therefore forces the push, so the
// shared specs/<step>.yaml always exists before its first stage reads it. All
// pushes are serialized on pushMu because they target one shared git repo.
// In-container runs (no user) can't push, so a genuinely changed/new spec there is
// a loud error from pushSpec — a new or changed step must be introduced locally.
func (d *DevOpsBuilder) ensureSpecPushed(specFile, spec, specHash, priorHash string) error {
	d.pushMu.Lock()
	defer d.pushMu.Unlock()
	d.mu.Lock()
	pushed := d.pushedSpecs[specFile]
	d.mu.Unlock()
	if priorHash == specHash || pushed == specHash {
		return nil
	}
	if err := d.pushSpec(specFile, spec); err != nil {
		return err
	}
	d.mu.Lock()
	d.pushedSpecs[specFile] = specHash
	d.mu.Unlock()
	return nil
}

// getOrCreatePipeline returns the pipeline OCID and the spec hash recorded on it
// (empty for a freshly created one or one predating the tag), without mutating an
// existing pipeline's parameters — ensurePipeline does that via updatePipeline.
func (d *DevOpsBuilder) getOrCreatePipeline(name string, params []devops.BuildPipelineParameter) (string, string, error) {
	list, err := d.client.ListBuildPipelines(d.ctx, devops.ListBuildPipelinesRequest{
		ProjectId:   &d.projectId,
		DisplayName: &name,
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to list build pipelines: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, list.Items[0].FreeformTags[specHashTag], nil
	}
	description := "Runs an infralib step by docker-running the base image on a managed build runner"
	created, err := d.client.CreateBuildPipeline(d.ctx, devops.CreateBuildPipelineRequest{
		CreateBuildPipelineDetails: devops.CreateBuildPipelineDetails{
			ProjectId:               &d.projectId,
			DisplayName:             &name,
			Description:             &description,
			BuildPipelineParameters: &devops.BuildPipelineParameterCollection{Items: params},
			FreeformTags:            map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to create build pipeline %s: %w", name, err)
	}
	// Creation is async; a build run started before the work request finishes
	// gets a 404 ("ensure completion of any work request for the Build Pipeline").
	if err = d.waitForWorkRequest(created.OpcWorkRequestId); err != nil {
		return "", "", err
	}
	return *created.Id, "", nil
}

// updatePipeline refreshes the pipeline's parameters and stamps the spec hash now
// in the repo onto its freeform tags.
func (d *DevOpsBuilder) updatePipeline(id string, params []devops.BuildPipelineParameter, specHash string) error {
	_, err := d.client.UpdateBuildPipeline(d.ctx, devops.UpdateBuildPipelineRequest{
		BuildPipelineId: &id,
		UpdateBuildPipelineDetails: devops.UpdateBuildPipelineDetails{
			BuildPipelineParameters: &devops.BuildPipelineParameterCollection{Items: params},
			FreeformTags: map[string]string{
				model.ResourceTagKey: model.ResourceTagValue,
				specHashTag:          specHash,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update build pipeline %s: %w", id, err)
	}
	return nil
}

func (d *DevOpsBuilder) ensureStage(pipelineId, specFile string) error {
	stageName := buildStageName
	list, err := d.client.ListBuildPipelineStages(d.ctx, devops.ListBuildPipelineStagesRequest{
		BuildPipelineId: &pipelineId,
		DisplayName:     &stageName,
	})
	if err != nil {
		return fmt.Errorf("failed to list build pipeline stages: %w", err)
	}
	if len(list.Items) > 0 {
		return nil
	}
	sourceName := "build-spec"
	buildSpec := specFile
	branch := buildSpecBranch
	created, err := d.client.CreateBuildPipelineStage(d.ctx, devops.CreateBuildPipelineStageRequest{
		CreateBuildPipelineStageDetails: devops.CreateBuildStageDetails{
			BuildPipelineId: &pipelineId,
			DisplayName:     &stageName,
			// First stage's predecessor is the pipeline itself.
			BuildPipelineStagePredecessorCollection: &devops.BuildPipelineStagePredecessorCollection{
				Items: []devops.BuildPipelineStagePredecessor{{Id: &pipelineId}},
			},
			Image:              devops.BuildStageImageOl8X8664Standard10,
			BuildSpecFile:      &buildSpec,
			PrimaryBuildSource: &sourceName,
			BuildSourceCollection: &devops.BuildSourceCollection{
				Items: []devops.BuildSource{devops.DevopsCodeRepositoryBuildSource{
					Name:          &sourceName,
					RepositoryUrl: &d.repoURL,
					RepositoryId:  &d.repoId,
					Branch:        &branch,
				}},
			},
			FreeformTags: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create build pipeline stage: %w", err)
	}
	// Adding the stage updates the pipeline via another async work request; wait
	// so the first build run doesn't race it (same 404 as pipeline creation).
	return d.waitForWorkRequest(created.OpcWorkRequestId)
}

// SetGitAuth injects the build-spec push credentials the agent resolved once at
// startup (provisionBackendCredentials), so pushSpec neither reads the Vault nor
// makes IAM calls. Both values are empty on a non-admin/consume run that never
// bootstrapped them; pushSpec then fails loudly only if a spec actually changed.
// fresh is true when the token was just created, so the push retries while it
// propagates to the git endpoint.
func (d *DevOpsBuilder) SetGitAuth(username, token string, fresh bool) {
	d.gitUsername, d.gitToken, d.authTokenFresh = username, token, fresh
}

// pushSpec commits the step's build spec to the hosted repo (the single source of
// truth) at specFile, authenticating as the agent SA with credentials injected at
// startup by SetGitAuth — no Vault or IAM calls here.
// A genuine spec change on a run without those provisioned (a non-admin/consume run
// that never bootstrapped) is a loud error and must be introduced from an admin run.
func (d *DevOpsBuilder) pushSpec(specFile, spec string) error {
	if d.repoURL == "" {
		return fmt.Errorf("hosted repository has no http url to push to")
	}
	if d.gitUsername == "" || d.gitToken == "" {
		return fmt.Errorf("build spec %s changed but no DevOps git credentials are provisioned; "+
			"run the agent once as an admin to bootstrap them", specFile)
	}
	auth := &githttp.BasicAuth{Username: d.gitUsername, Password: d.gitToken}
	log.Printf("Pushing DevOps build spec %s into %s as git user %q\n", specFile, d.repoURL, d.gitUsername)
	if err := d.commitSpecWithRetry(specFile, spec, auth, d.authTokenFresh); err != nil {
		return fmt.Errorf("%w; if this is a 401, the git username form may differ for this tenancy "+
			"(e.g. identity-domain tenancies need \"<tenancy>/<domain>/%s\") — adjust deriveGitUsername",
			err, d.gitUsername)
	}
	return nil
}

const (
	// gitAuthPropagationTimeout bounds how long a freshly created auth token is given
	// to propagate to the DevOps git endpoint (like the CSK, it's eventually consistent).
	gitAuthPropagationTimeout = 5 * time.Minute
	gitAuthRetryInterval      = 15 * time.Second
)

// commitSpecWithRetry pushes the spec, tolerating the propagation delay of a
// just-created auth token: OCI returns the token immediately but the git endpoint
// only accepts it after an asynchronous delay, so an initial 401 is expected and we
// retry until it takes. A REUSED token is already propagated, so a 401 there is a
// genuine auth problem (wrong username form, revoked token) — fail fast.
func (d *DevOpsBuilder) commitSpecWithRetry(specFile, spec string, auth transport.AuthMethod, freshToken bool) error {
	deadline := time.Now().Add(gitAuthPropagationTimeout)
	for {
		err := commitSpecFile(d.ctx, d.repoURL, auth, buildSpecBranch, specFile, []byte(spec))
		if err == nil {
			return nil
		}
		if !freshToken || !isGitAuthError(err) || time.Now().After(deadline) {
			return err
		}
		log.Printf("DevOps git rejected the new auth token (not yet propagated); retrying in %s\n", gitAuthRetryInterval)
		select {
		case <-d.ctx.Done():
			return d.ctx.Err()
		case <-time.After(gitAuthRetryInterval):
		}
	}
}

// isGitAuthError reports a 401/403 from the git endpoint (go-git wraps the transport
// sentinels with %w, so errors.Is sees them through commitSpecFile's wrapping). A
// still-propagating fresh token can surface as either, so both must be retried.
func isGitAuthError(err error) bool {
	return errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed)
}

// commitSpecFile clones the hosted repo (init-ing a fresh one if the remote is
// still empty), writes the single spec file, and pushes only if it changed. The
// repo is the source of truth — cloning preserves every other step's spec, so no
// bucket copy is needed.
func commitSpecFile(ctx context.Context, url string, auth transport.AuthMethod, branch, path string, content []byte) error {
	dir, err := os.MkdirTemp("", "oracle-buildspec-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	repo, fresh, err := cloneOrInit(ctx, dir, url, auth, branch)
	if err != nil {
		return err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}
	if dir := pathDir(path); dir != "" {
		if err = worktree.Filesystem.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	file, err := worktree.Filesystem.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if _, err = worktree.Add(path); err != nil {
		return err
	}
	status, err := worktree.Status()
	if err != nil {
		return err
	}
	if status.IsClean() {
		return nil // already up to date in the repo
	}
	if _, err = worktree.Commit("Update infralib build spec "+path, &git.CommitOptions{
		Author: &object.Signature{Name: "Entigo Infralib Agent", Email: "no-reply@localhost", When: time.Now().UTC()},
	}); err != nil {
		return err
	}
	head, err := repo.Head()
	if err != nil {
		return err
	}
	// A fresh (empty-remote) repo has an unrelated history, so force-push; a clone
	// fast-forwards.
	spec := fmt.Sprintf("%s:refs/heads/%s", head.Name().String(), branch)
	if fresh {
		spec = "+" + spec
	}
	err = repo.PushContext(ctx, &git.PushOptions{Auth: auth, RefSpecs: []gitconfig.RefSpec{gitconfig.RefSpec(spec)}})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("failed to push build spec: %w", err)
	}
	log.Printf("Pushed build spec commit %s to %s\n", head.Hash().String()[:8], branch)
	return nil
}

// cloneOrInit clones the branch, or initialises a fresh repo + remote when the
// hosted repository is still empty (first-ever push). fresh reports the latter.
func cloneOrInit(ctx context.Context, dir, url string, auth transport.AuthMethod, branch string) (*git.Repository, bool, error) {
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           url,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
	})
	if err == nil {
		return repo, false, nil
	}
	if !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, false, fmt.Errorf("failed to clone build spec repo: %w", err)
	}
	repo, err = git.PlainInit(dir, false)
	if err != nil {
		return nil, false, err
	}
	if _, err = repo.CreateRemote(&gitconfig.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{url}}); err != nil {
		return nil, false, err
	}
	return repo, true, nil
}

func pathDir(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[:i]
	}
	return ""
}

// hashSpec is the content hash of a build spec, stamped on the pipeline's
// spec-hash tag so a reconcile pushes only when the spec changed.
func hashSpec(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(sum[:])
}

// waitForWorkRequest blocks until a DevOps work request finishes. A nil id (no
// async work) returns immediately.
func (d *DevOpsBuilder) waitForWorkRequest(workRequestId *string) error {
	if workRequestId == nil {
		return nil
	}
	for {
		response, err := d.client.GetWorkRequest(d.ctx, devops.GetWorkRequestRequest{WorkRequestId: workRequestId})
		if err != nil {
			return err
		}
		switch response.Status {
		case devops.OperationStatusSucceeded:
			return nil
		case devops.OperationStatusFailed, devops.OperationStatusCanceled, devops.OperationStatusCanceling,
			devops.OperationStatusNeedsAttention:
			return fmt.Errorf("devops work request %s ended as %s", *workRequestId, response.Status)
		}
		select {
		case <-d.ctx.Done():
			return d.ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// waitForBuildRun polls the build run to completion and returns a process-style
// exit code (0 = SUCCEEDED, 1 = FAILED/CANCELED).
func (d *DevOpsBuilder) waitForBuildRun(buildRunId string) (int, error) {
	for {
		response, err := d.client.GetBuildRun(d.ctx, devops.GetBuildRunRequest{BuildRunId: &buildRunId})
		if err != nil {
			return 0, err
		}
		switch response.LifecycleState {
		case devops.BuildRunLifecycleStateSucceeded:
			return 0, nil
		case devops.BuildRunLifecycleStateFailed, devops.BuildRunLifecycleStateCanceled:
			return 1, nil
		}
		select {
		case <-d.ctx.Done():
			return 0, d.ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (d *DevOpsBuilder) DeleteBuildResources() {
	name := fmt.Sprintf("%s-infralib", d.cloudPrefix)
	projectId := d.findProject(name)
	if projectId != "" {
		d.deleteRepositories(projectId)
		if _, err := d.client.DeleteProject(d.ctx, devops.DeleteProjectRequest{ProjectId: &projectId}); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf(
				"failed to delete DevOps project %s, if caused by child resources, try again: %s", name, err)))
		}
	}
	d.deleteTopic(fmt.Sprintf("%s-approvals", d.cloudPrefix))
}

// Resolve locates the shared project by name WITHOUT creating or enabling anything,
// so read-only / destroy flows (GetResources) can trigger already-provisioned
// pipelines. Unlike Ensure it enables no logs, grants no IAM and pushes no specs;
// if the project doesn't exist the id stays empty and triggers return NotFoundError.
func (d *DevOpsBuilder) Resolve() {
	d.projectId = d.findProject(fmt.Sprintf("%s-infralib", d.cloudPrefix))
}

func (d *DevOpsBuilder) findProject(name string) string {
	list, err := d.client.ListProjects(d.ctx, devops.ListProjectsRequest{CompartmentId: &d.compartmentId, Name: &name})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to look up DevOps project %s: %s", name, err)))
		return ""
	}
	if len(list.Items) == 0 {
		return ""
	}
	return *list.Items[0].Id
}

func (d *DevOpsBuilder) deleteRepositories(projectId string) {
	list, err := d.client.ListRepositories(d.ctx, devops.ListRepositoriesRequest{ProjectId: &projectId})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list DevOps repositories: %s", err)))
		return
	}
	for _, repo := range list.Items {
		if repo.Id == nil {
			continue
		}
		if _, err = d.client.DeleteRepository(d.ctx, devops.DeleteRepositoryRequest{RepositoryId: repo.Id}); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete DevOps repository %s: %s", *repo.Id, err)))
			continue
		}
		log.Printf("Deleted DevOps code repository %s\n", repositoryName(d.cloudPrefix))
	}
}

func (d *DevOpsBuilder) deleteTopic(name string) {
	list, err := d.onsClient.ListTopics(d.ctx, ons.ListTopicsRequest{CompartmentId: &d.compartmentId, Name: &name})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to look up notification topic %s: %s", name, err)))
		return
	}
	for _, topic := range list.Items {
		if _, err = d.onsClient.DeleteTopic(d.ctx, ons.DeleteTopicRequest{TopicId: topic.TopicId}); err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete notification topic %s: %s", name, err)))
			continue
		}
		log.Printf("Deleted notification topic %s\n", name)
	}
}

// deleteStepPipelines removes a removed step's build pipelines — one per action
// command it may have run. The step type is unknown here, so every command's
// pipeline name is tried. Best-effort so a removed step never blocks the delete
// flow. (The step's approval deployment pipeline is a separate, documented
// cleanup gap.)
func (d *DevOpsBuilder) deleteStepPipelines(step string) {
	for _, command := range []model.ActionCommand{
		model.PlanCommand, model.ApplyCommand, model.PlanDestroyCommand, model.ApplyDestroyCommand,
		model.ArgoCDPlanCommand, model.ArgoCDApplyCommand, model.ArgoCDPlanDestroyCommand, model.ArgoCDApplyDestroyCommand,
	} {
		name := runName(step, command)
		d.mu.Lock()
		delete(d.pipelines, name)
		d.mu.Unlock()
		list, err := d.client.ListBuildPipelines(d.ctx, devops.ListBuildPipelinesRequest{
			ProjectId:   &d.projectId,
			DisplayName: &name,
		})
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list build pipelines for %s: %s", name, err)))
			continue
		}
		for _, item := range list.Items {
			d.deleteBuildPipeline(*item.Id)
		}
	}
}

// deleteBuildPipeline removes a build pipeline, first deleting its stages — OCI
// rejects DeleteBuildPipeline with 409 ("has active stages") while any remain.
// Best-effort per stage.
func (d *DevOpsBuilder) deleteBuildPipeline(pipelineId string) {
	stages, err := d.client.ListBuildPipelineStages(d.ctx, devops.ListBuildPipelineStagesRequest{
		BuildPipelineId: &pipelineId,
	})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to list stages of build pipeline %s: %s", pipelineId, err)))
	} else {
		for _, stage := range stages.Items {
			if _, err = d.client.DeleteBuildPipelineStage(d.ctx, devops.DeleteBuildPipelineStageRequest{
				BuildPipelineStageId: stage.GetId(),
			}); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete build pipeline stage %s: %s", *stage.GetId(), err)))
			}
		}
	}
	if _, err = d.client.DeleteBuildPipeline(d.ctx, devops.DeleteBuildPipelineRequest{BuildPipelineId: &pipelineId}); err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete build pipeline %s: %s", pipelineId, err)))
	}
}

// --- build spec + parameter generation ---

// forwardNames is the sorted set of container env var names the spec forwards
// (docker run -e <NAME>): every non-secret parameter and every secret.
func forwardNames(params, secretRefs map[string]string) []string {
	names := make([]string, 0, len(params)+len(secretRefs))
	for name := range params {
		names = append(names, name)
	}
	for name := range secretRefs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// vaultVariables maps each secret's exported env-var name (EI_<NAME>) to the
// secret's literal OCID. OCI resolves vaultVariables at run-environment build
// time and requires an OCID value here — it does NOT substitute pipeline
// parameters (${...}) into this field.
func vaultVariables(secretRefs map[string]string) map[string]string {
	vars := make(map[string]string, len(secretRefs))
	for name, ocid := range secretRefs {
		vars[envParamPrefix+name] = ocid
	}
	return vars
}

// buildPipelineParameters declares the pipeline's parameters: IMAGE and EI_<NAME>
// for each non-secret value (default = value). Secrets are NOT parameters — they
// ride the spec's vaultVariables as literal OCIDs. OCI rejects an empty default,
// so blanks fall back to a placeholder (the real value always arrives, and empty
// non-secrets don't occur).
func buildPipelineParameters(image string, params map[string]string) []devops.BuildPipelineParameter {
	items := []devops.BuildPipelineParameter{makeParam(imageParam, image)}
	for name, value := range params {
		items = append(items, makeParam(envParamPrefix+name, value))
	}
	sort.Slice(items, func(i, j int) bool { return *items[i].Name < *items[j].Name })
	return items
}

func makeParam(name, defaultValue string) devops.BuildPipelineParameter {
	if defaultValue == "" {
		defaultValue = "unset"
	}
	n, v := name, defaultValue
	return devops.BuildPipelineParameter{Name: &n, DefaultValue: &v}
}

// buildSpecYAMLFor generates a step's build spec: a vaultVariables block for its
// secrets, then a generic loop that forwards every EI_<NAME> value (pipeline
// parameter or fetched secret) into `docker run -e <NAME>`, forwards the runner's
// resource-principal vars (bind-mounting file-path ones), and docker-runs $IMAGE.
func buildSpecYAMLFor(names []string, vaultVars map[string]string) string {
	var b strings.Builder
	b.WriteString("version: 0.1\ncomponent: build\ntimeoutInSeconds: 6000\nshell: bash\n")
	if len(vaultVars) > 0 {
		b.WriteString("env:\n  vaultVariables:\n")
		keys := make([]string, 0, len(vaultVars))
		for k := range vaultVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "    %s: %s\n", k, vaultVars[k])
		}
	}
	b.WriteString("steps:\n  - type: Command\n    name: \"Run infralib step in base image\"\n")
	fmt.Fprintf(&b, "    timeoutInSeconds: %d\n", int(buildRunTimeout.Seconds()))
	b.WriteString("    command: |\n")
	body := []string{
		"set -euo pipefail",
		"names=(" + strings.Join(names, " ") + ")",
		"docker_args=()",
		`for n in "${names[@]}"; do`,
		`  ei="` + envParamPrefix + `$n"`,
		`  docker_args+=(-e "$n=${!ei-}")`,
		"done",
		"# Forward the runner's resource-principal vars into the container so the",
		"# in-container oci provider authenticates as one, bind-mounting file paths.",
		`echo "Runner resource-principal vars present:"; env | grep -o '^OCI_RESOURCE_PRINCIPAL[A-Z_]*' || echo "  (none)"`,
		"for v in OCI_RESOURCE_PRINCIPAL_VERSION OCI_RESOURCE_PRINCIPAL_REGION OCI_RESOURCE_PRINCIPAL_RPST OCI_RESOURCE_PRINCIPAL_PRIVATE_PEM OCI_RESOURCE_PRINCIPAL_PRIVATE_PEM_PASSPHRASE; do",
		`  val="${!v:-}"`,
		`  [ -z "$val" ] && continue`,
		`  if [ -f "$val" ]; then docker_args+=(-v "$val:$val:ro"); fi`,
		`  docker_args+=(-e "$v=$val")`,
		"done",
		`echo "Running $` + imageParam + `"`,
		`docker run --rm "${docker_args[@]}" "$` + imageParam + `"`,
	}
	for _, line := range body {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

// specFileFor is the hosted-repo path of a step's shared build spec. All of a
// step's command pipelines (plan/apply and destroy variants) read this one file —
// the spec is command-independent (COMMAND rides a pipeline parameter, not the
// spec), so there is no reason to duplicate it per command.
func specFileFor(projectName string) string {
	return specRepoPrefix + projectName + ".yaml"
}
