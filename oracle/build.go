package oracle

import (
	"context"
	"encoding/base64"
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
const (
	// pollInterval is how often the DevOps build-run / work-request / approval
	// polls re-check state. GetBuildRun is cheap and the step count small.
	pollInterval = 5 * time.Second
	// runEnvParam is the single build-pipeline parameter the generic build spec
	// reads: a PAR URL to the per-run environment file in the config bucket.
	// Keeping the whole (variable, secret-bearing) env out of DevOps arguments
	// and behind a short-lived PAR means the build definition is static and the
	// build-run argument carries no secret.
	runEnvParam = "RUN_ENV_URL"
	// runEnvImageKey is the env-file key the build spec pulls the image name from
	// (double-underscore = consumed by the spec, not forwarded into the container).
	runEnvImageKey = "__IMAGE"
	// buildSpecPath is the build spec's location in the seeded repository.
	buildSpecPath   = "build_spec.yaml"
	buildRunEnvTTL  = 60 * time.Minute
	buildSpecBranch = "main"
	buildStageName  = "run"
	// devopsAuthTokenObject persists the OCI auth token used to git-push the
	// build spec, in the config bucket alongside the CSK. Same trust boundary.
	devopsAuthTokenObject = "oracle-devops-auth-token"
)

// buildSpecYAML is the generic managed-build spec seeded once into the hosted
// code repository and shared by every step's build pipeline (the per-step
// pipelines differ only in name; the command is delivered per run). It fetches
// the per-run env file via the PAR URL passed as the RUN_ENV_URL parameter,
// reconstructs the container environment (each value is base64-encoded so
// multi-line values like WRAPPER_CONFIG survive), then does a single `docker
// run` of the infralib base image. The env file already carries COMMAND, so the
// same spec runs plan or apply depending only on which env file the agent wrote.
// `set -e` + docker's exit-code propagation make a non-zero step exit fail the
// build run, which is how the agent detects a failed plan/apply. Base images are
// public on Docker Hub, so no registry login is needed. The build runner has a
// resource principal; its OCI_RESOURCE_PRINCIPAL_* vars are forwarded into the
// container so the in-container oci provider authenticates as one.
const buildSpecYAML = `version: 0.1
component: build
timeoutInSeconds: 6000
shell: bash
steps:
  - type: Command
    name: "Run infralib step in base image"
    timeoutInSeconds: 6000
    command: |
      set -euo pipefail
      echo "Fetching run environment"
      curl -fsSL "${RUN_ENV_URL}" -o /tmp/run.env
      IMAGE=""
      docker_args=()
      while IFS=' ' read -r key b64 || [ -n "$key" ]; do
        [ -z "$key" ] && continue
        val=$(printf '%s' "$b64" | base64 -d)
        if [ "$key" = "__IMAGE" ]; then IMAGE="$val"; continue; fi
        docker_args+=(-e "$key=$val")
      done < /tmp/run.env
      # The oci provider in the container authenticates as a resource principal,
      # but only the build runner has one — forward its RP env vars into the
      # container, bind-mounting any that are file paths (RPv2.2 token/key) so
      # they resolve at the same path inside.
      echo "Runner resource-principal vars present:"; env | grep -o '^OCI_RESOURCE_PRINCIPAL[A-Z_]*' || echo "  (none)"
      for v in OCI_RESOURCE_PRINCIPAL_VERSION OCI_RESOURCE_PRINCIPAL_REGION OCI_RESOURCE_PRINCIPAL_RPST OCI_RESOURCE_PRINCIPAL_PRIVATE_PEM OCI_RESOURCE_PRINCIPAL_PRIVATE_PEM_PASSPHRASE; do
        val="${!v:-}"
        [ -z "$val" ] && continue
        if [ -f "$val" ]; then docker_args+=(-v "$val:$val:ro"); fi
        docker_args+=(-e "$v=$val")
      done
      echo "Running $IMAGE"
      docker run --rm "${docker_args[@]}" "$IMAGE"
`

// DevOpsBuilder executes infralib steps through OCI DevOps build pipelines.
// One shared project (<prefix>-infralib) holds a
// single hosted code repo carrying buildSpecYAML plus one build pipeline per
// (step, command) — e.g. <prefix>-hello-plan, <prefix>-hello-apply — so a gitops
// engineer sees a uniquely named pipeline for every step action with native
// build-run logs. The per-step pipelines are created lazily and share the one
// build spec; per-run data (image + full env, incl. COMMAND) is delivered
// out-of-band via a PAR'd config-bucket object, so the build definitions never
// change and steps run concurrently as independent build runs. The project also
// hosts the manual-approval deployment pipelines (see Gate.UseProject).
//
// Setup mirrors the CSK bootstrap: the git seed of the build spec needs a user
// (auth token), so the FIRST run must be local (session-token or API-key auth);
// in-container resource-principal runs reference the already-seeded repo.
type DevOpsBuilder struct {
	ctx           context.Context
	client        devops.DevopsClient
	onsClient     ons.NotificationControlPlaneClient
	iam           *IAM
	config        *Storage
	compartmentId string
	region        string
	cloudPrefix   string
	once          sync.Once
	ensureErr     error
	projectId     string
	repoId        string
	repoURL       string
	mu            sync.Mutex
	pipelines     map[string]string // pipeline display name → build pipeline OCID (run stage ensured)
}

// ProjectId returns the shared DevOps project OCID after Ensure has run (needed
// to enable the project's build logs and to host the approval pipelines).
func (d *DevOpsBuilder) ProjectId() string { return d.projectId }

func NewDevOpsBuilder(ctx context.Context, provider ocicommon.ConfigurationProvider, iam *IAM, config *Storage, region, compartmentId, cloudPrefix string) (*DevOpsBuilder, error) {
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
		iam:           iam,
		config:        config,
		compartmentId: compartmentId,
		region:        region,
		cloudPrefix:   cloudPrefix,
		pipelines:     map[string]string{},
	}, nil
}

// Ensure provisions (once) the shared DevOps project, its notification topic and
// the hosted build-spec repository. userId (empty under in-container resource
// principals) gates only the git seed of the build spec; everything else is
// list-or-create and works from any principal that can manage DevOps. Per-step
// build pipelines are created lazily by launchBuildRun.
func (d *DevOpsBuilder) Ensure(userId string) error {
	d.once.Do(func() {
		d.ensureErr = d.ensure(userId)
	})
	return d.ensureErr
}

func (d *DevOpsBuilder) ensure(userId string) error {
	topicId, err := d.ensureTopic(fmt.Sprintf("%s-approvals", d.cloudPrefix))
	if err != nil {
		return err
	}
	projectId, err := d.ensureProject(fmt.Sprintf("%s-infralib", d.cloudPrefix), topicId)
	if err != nil {
		return err
	}
	d.projectId = projectId
	repoId, repoURL, seeded, err := d.ensureRepository(fmt.Sprintf("%s-infralib-src", d.cloudPrefix))
	if err != nil {
		return err
	}
	d.repoId, d.repoURL = repoId, repoURL
	if seeded {
		return nil
	}
	if userId == "" {
		return fmt.Errorf("build-spec repository %s-infralib-src is empty and no user is available to seed it; "+
			"run the agent once locally (session-token or API-key auth) to bootstrap the DevOps build pipelines", d.cloudPrefix)
	}
	return d.seedBuildSpec(userId)
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

// ensureRepository gets or creates the hosted code repo holding the build spec.
// seeded reports whether it already has commits (a fresh hosted repo is empty
// and must be seeded before any pipeline can read the spec).
func (d *DevOpsBuilder) ensureRepository(name string) (string, string, bool, error) {
	list, err := d.client.ListRepositories(d.ctx, devops.ListRepositoriesRequest{
		ProjectId: &d.projectId,
		Name:      &name,
	})
	if err != nil {
		return "", "", false, fmt.Errorf("failed to list repositories: %w", err)
	}
	if len(list.Items) > 0 {
		return d.resolveRepository(*list.Items[0].Id)
	}
	defaultBranch := buildSpecBranch
	description := "Entigo infralib DevOps build spec"
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
		return "", "", false, fmt.Errorf("failed to create repository %s: %w", name, err)
	}
	url := ""
	if created.HttpUrl != nil {
		url = *created.HttpUrl
	}
	return *created.Id, url, false, nil
}

// resolveRepository reads the full repository (the list summary omits
// CommitCount) to get its http url and whether it already carries the spec.
func (d *DevOpsBuilder) resolveRepository(repoId string) (string, string, bool, error) {
	response, err := d.client.GetRepository(d.ctx, devops.GetRepositoryRequest{RepositoryId: &repoId})
	if err != nil {
		return "", "", false, fmt.Errorf("failed to get repository %s: %w", repoId, err)
	}
	url := ""
	if response.HttpUrl != nil {
		url = *response.HttpUrl
	}
	seeded := response.CommitCount != nil && *response.CommitCount > 0
	return repoId, url, seeded, nil
}

// seedBuildSpec pushes buildSpecYAML to the hosted repo's default branch using
// go-git over HTTPS. Basic-auth username is the OCI code-repo username; password
// is an auth token (persisted, reused).
func (d *DevOpsBuilder) seedBuildSpec(userId string) error {
	if d.repoURL == "" {
		return fmt.Errorf("hosted repository has no http url to push to")
	}
	username, err := d.gitUsername(userId)
	if err != nil {
		return err
	}
	token, err := d.iam.EnsureAuthToken(d.config, userId, fmt.Sprintf("entigo-infralib-%s-devops", d.cloudPrefix))
	if err != nil {
		return err
	}
	auth := &githttp.BasicAuth{Username: username, Password: token}
	log.Printf("Seeding DevOps build spec into %s as git user %q\n", d.repoURL, username)
	if err = seedRepoFile(d.ctx, d.repoURL, auth, buildSpecBranch, buildSpecPath, []byte(buildSpecYAML)); err != nil {
		return fmt.Errorf("%w; if this is a 401, the git username form may differ for this tenancy — "+
			"set %s to override it (e.g. \"<tenancy>/<domain>/%s\" when a non-default identity domain is in play)",
			err, gitUsernameEnv, username)
	}
	return nil
}

// gitUsernameEnv overrides the HTTPS basic-auth username used to push the build
// spec, for tenancies whose exact form the default doesn't match.
const gitUsernameEnv = "ORACLE_GIT_USERNAME"

// gitUsername builds the OCI code-repository HTTPS username, which OCI forms as
// `<tenancy-name>/<login>` (NOT the object-storage namespace — the two differ).
// Tenancies with a non-default identity domain instead need
// `<tenancy-name>/<domain>/<login>`; the domain can't be derived from the
// Identity user alone, so that case falls back to the env override.
func (d *DevOpsBuilder) gitUsername(userId string) (string, error) {
	if override := os.Getenv(gitUsernameEnv); override != "" {
		return override, nil
	}
	tenancy, err := d.iam.TenancyName()
	if err != nil {
		return "", err
	}
	login, err := d.iam.Username(userId)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s", tenancy, login), nil
}

// seedRepoFile initialises a local repo, commits a single file and pushes it to
// the given branch of a remote that starts empty.
func seedRepoFile(ctx context.Context, url string, auth transport.AuthMethod, branch, path string, content []byte) error {
	dir, err := os.MkdirTemp("", "oracle-buildspec-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return err
	}
	if _, err = repo.CreateRemote(&gitconfig.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{url}}); err != nil {
		return err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return err
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
	if _, err = worktree.Commit("Add infralib build spec", &git.CommitOptions{
		Author: &object.Signature{Name: "Entigo Infralib Agent", Email: "no-reply@localhost", When: time.Now().UTC()},
	}); err != nil {
		return err
	}
	// Push the concrete local branch ref, not "HEAD": go-git can resolve a HEAD
	// source refspec to nothing and return NoErrAlreadyUpToDate, silently pushing
	// an empty repo (the "Unable to fetch build_spec" failure).
	head, err := repo.Head()
	if err != nil {
		return err
	}
	refSpec := gitconfig.RefSpec(fmt.Sprintf("+%s:refs/heads/%s", head.Name().String(), branch))
	err = repo.PushContext(ctx, &git.PushOptions{
		Auth:     auth,
		RefSpecs: []gitconfig.RefSpec{refSpec},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("failed to push build spec: %w", err)
	}
	log.Printf("Pushed build spec commit %s to %s\n", head.Hash().String()[:8], branch)
	return nil
}

// ensurePipeline gets or creates the build pipeline named displayName (one per
// step+command, e.g. <prefix>-hello-plan) together with its single `run` stage
// over the shared build-spec repo. Cached (steps run in parallel goroutines);
// the get-or-create is serialized, which is fine for a per-pipeline one-time
// setup call.
func (d *DevOpsBuilder) ensurePipeline(displayName string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id, ok := d.pipelines[displayName]; ok {
		return id, nil
	}
	pipelineId, err := d.getOrCreatePipeline(displayName)
	if err != nil {
		return "", err
	}
	if err = d.ensureStage(pipelineId); err != nil {
		return "", err
	}
	d.pipelines[displayName] = pipelineId
	return pipelineId, nil
}

func (d *DevOpsBuilder) getOrCreatePipeline(name string) (string, error) {
	list, err := d.client.ListBuildPipelines(d.ctx, devops.ListBuildPipelinesRequest{
		ProjectId:   &d.projectId,
		DisplayName: &name,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list build pipelines: %w", err)
	}
	if len(list.Items) > 0 {
		return *list.Items[0].Id, nil
	}
	// OCI rejects an empty defaultValue ("Invalid Parameter"); the real value is
	// always supplied per run as a build run argument, so this is just a placeholder.
	defaultValue := "unset"
	description := "Runs an infralib step by docker-running the base image on a managed build runner"
	paramDesc := "PAR URL to the per-run environment file"
	created, err := d.client.CreateBuildPipeline(d.ctx, devops.CreateBuildPipelineRequest{
		CreateBuildPipelineDetails: devops.CreateBuildPipelineDetails{
			ProjectId:   &d.projectId,
			DisplayName: &name,
			Description: &description,
			BuildPipelineParameters: &devops.BuildPipelineParameterCollection{
				Items: []devops.BuildPipelineParameter{{
					Name:         new(runEnvParam),
					DefaultValue: &defaultValue,
					Description:  &paramDesc,
				}},
			},
			FreeformTags: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create build pipeline %s: %w", name, err)
	}
	// Creation is async; a build run started before the work request finishes
	// gets a 404 ("ensure completion of any work request for the Build Pipeline").
	if err = d.waitForWorkRequest(created.OpcWorkRequestId); err != nil {
		return "", err
	}
	return *created.Id, nil
}

func (d *DevOpsBuilder) ensureStage(pipelineId string) error {
	displayName := buildStageName
	list, err := d.client.ListBuildPipelineStages(d.ctx, devops.ListBuildPipelineStagesRequest{
		BuildPipelineId: &pipelineId,
		DisplayName:     &displayName,
	})
	if err != nil {
		return fmt.Errorf("failed to list build pipeline stages: %w", err)
	}
	if len(list.Items) > 0 {
		return nil
	}
	sourceName := "build-spec"
	buildSpec := buildSpecPath
	branch := buildSpecBranch
	created, err := d.client.CreateBuildPipelineStage(d.ctx, devops.CreateBuildPipelineStageRequest{
		CreateBuildPipelineStageDetails: devops.CreateBuildStageDetails{
			BuildPipelineId: &pipelineId,
			DisplayName:     &displayName,
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

// launchBuildRun ensures the step+command build pipeline exists, writes the
// per-run env file, mints a PAR for it and starts a build run, returning its
// OCID (consumed by waitForBuildRun). displayName is the "<prefixStep>-<command>"
// identity that names both the build pipeline and the build run.
func (d *DevOpsBuilder) launchBuildRun(displayName, image string, env map[string]string) (string, error) {
	pipelineId, err := d.ensurePipeline(displayName)
	if err != nil {
		return "", err
	}
	objectName := fmt.Sprintf("buildenv/%s", displayName)
	if err = d.config.PutFile(objectName, encodeRunEnv(image, env)); err != nil {
		return "", fmt.Errorf("failed to write run environment: %w", err)
	}
	parURL, err := d.config.CreatePreauthenticatedURL(objectName, buildRunEnvTTL)
	if err != nil {
		return "", err
	}
	name := displayName
	if len(name) > 255 {
		name = name[:255]
	}
	response, err := d.client.CreateBuildRun(d.ctx, devops.CreateBuildRunRequest{
		CreateBuildRunDetails: devops.CreateBuildRunDetails{
			BuildPipelineId: &pipelineId,
			DisplayName:     &name,
			BuildRunArguments: &devops.BuildRunArgumentCollection{
				Items: []devops.BuildRunArgument{{Name: new(runEnvParam), Value: &parURL}},
			},
			FreeformTags: map[string]string{model.ResourceTagKey: model.ResourceTagValue},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create build run for %s: %w", displayName, err)
	}
	return *response.Id, nil
}

// encodeRunEnv serialises the container environment as one `KEY BASE64VALUE` line
// per variable (base64 keeps multi-line values intact), with the image under
// runEnvImageKey. The build spec decodes it back into `docker run -e` args.
func encodeRunEnv(image string, env map[string]string) []byte {
	lines := make([]string, 0, len(env)+1)
	lines = append(lines, fmt.Sprintf("%s %s", runEnvImageKey, base64.StdEncoding.EncodeToString([]byte(image))))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic output
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("%s %s", k, base64.StdEncoding.EncodeToString([]byte(env[k]))))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// waitForBuildRun polls the build run to completion and returns a process-style
// exit code (0 = SUCCEEDED, 1 = FAILED/CANCELED) so callers share the Container
// Instance exit-code contract.
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
			if _, err = d.client.DeleteBuildPipeline(d.ctx, devops.DeleteBuildPipelineRequest{BuildPipelineId: item.Id}); err != nil {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("failed to delete build pipeline %s: %s", *item.Id, err)))
			}
		}
	}
}
