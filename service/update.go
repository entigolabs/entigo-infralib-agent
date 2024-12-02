package service

import (
	"context"
	"dario.cat/mergo"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/git"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"gopkg.in/yaml.v3"
	"log"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	stateFile     = "state.yaml"
	checksumsFile = "checksums.sha256"

	ssmPrefix = "/entigo-infralib"
)

type Updater interface {
	Run()
	Update()
}

type updater struct {
	config        model.Config
	provider      model.CloudProvider
	resources     model.Resources
	terraform     terraform.Terraform
	github        git.Github
	destinations  map[string]model.Destination
	state         *model.State
	stateLock     sync.Mutex
	moduleSources map[string]string
	sources       map[string]*model.Source
	firstRunDone  map[string]bool
	allowParallel bool
}

func NewUpdater(ctx context.Context, flags *common.Flags) Updater {
	provider := GetCloudProvider(ctx, flags)
	resources := provider.SetupResources()
	config := GetConfig(resources.GetSSM(), resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	state := getLatestState(resources.GetBucket())
	ValidateConfig(config, state)
	ProcessSteps(&config, resources.GetProviderType())
	githubClient := git.NewGithub(ctx, flags.GithubToken)
	sources, moduleSources := createSources(githubClient, config, state)
	destinations := createDestinations(ctx, config.Destinations)
	return &updater{
		config:        config,
		provider:      provider,
		resources:     resources,
		terraform:     terraform.NewTerraform(resources.GetProviderType(), config.Sources, sources, githubClient),
		github:        githubClient,
		destinations:  destinations,
		state:         state,
		moduleSources: moduleSources,
		sources:       sources,
		firstRunDone:  make(map[string]bool),
		allowParallel: flags.AllowParallel,
	}
}

func getLatestState(bucket model.Bucket) *model.State {
	file, err := bucket.GetFile(stateFile)
	if err != nil {
		log.Fatalf("Failed to get state file: %v", err)
	}
	if file == nil {
		return &model.State{}
	}
	var state model.State
	err = yaml.Unmarshal(file, &state)
	if err != nil {
		log.Fatalf("Failed to unmarshal state file: %v", err)
	}
	return &state
}

func createSources(githubClient git.Github, config model.Config, state *model.State) (map[string]*model.Source, map[string]string) {
	sources := make(map[string]*model.Source)
	for _, source := range config.Sources {
		var release string
		if source.ForceVersion {
			release = source.Version
		} else {
			stableVersion := getLatestRelease(githubClient, source.URL)
			release = stableVersion.Original()
		}
		checksums, err := getChecksums(githubClient, source.URL, release)
		if err != nil {
			log.Fatalf("Failed to get checksums for source %s: %v", source.URL, err)
		}
		sources[source.URL] = &model.Source{
			URL:              source.URL,
			StableVersion:    getLatestRelease(githubClient, source.URL),
			Modules:          model.NewSet[string](),
			Includes:         model.ToSet(source.Include),
			Excludes:         model.ToSet(source.Exclude),
			CurrentChecksums: checksums,
		}
	}
	moduleSources := addSourceModules(config, sources)
	addSourceReleases(githubClient, config, state, sources)
	return sources, moduleSources
}

func addSourceModules(config model.Config, sources map[string]*model.Source) map[string]string {
	moduleSources := make(map[string]string)
	for _, step := range config.Steps {
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				continue
			}
			if moduleSources[module.Source] != "" {
				continue
			}
			moduleSource, err := getModuleSource(config, step, module, sources)
			if err != nil {
				log.Fatalf("Module %s in step %s is not included in any Source", module.Name, step.Name)
			}
			moduleSources[module.Source] = moduleSource
		}
	}
	return moduleSources
}

func getModuleSource(config model.Config, step model.Step, module model.Module, sources map[string]*model.Source) (string, error) {
	for _, configSource := range config.Sources {
		source := sources[configSource.URL]
		moduleSource := module.Source
		if len(source.Includes) > 0 {
			if source.Includes.Contains(module.Source) {
				sources[source.URL].Modules.Add(module.Source)
				return source.URL, nil
			}
			continue
		}
		if source.Excludes.Contains(moduleSource) {
			continue
		}
		if step.Type == model.StepTypeArgoCD {
			moduleSource = fmt.Sprintf("k8s/%s", moduleSource)
		}
		moduleKey := fmt.Sprintf("modules/%s", moduleSource)
		if source.CurrentChecksums[moduleKey] != "" {
			sources[source.URL].Modules.Add(module.Source)
			return source.URL, nil
		}
	}
	return "", fmt.Errorf("module %s source not found", module.Name)
}

func addSourceReleases(githubClient git.Github, config model.Config, state *model.State, sources map[string]*model.Source) {
	for _, cSource := range config.Sources {
		source := sources[cSource.URL]
		if cSource.ForceVersion {
			source.ForcedVersion = cSource.Version
			continue
		}
		upperVersion := source.StableVersion
		if cSource.Version != "" && cSource.Version != StableVersion {
			var err error
			upperVersion, err = version.NewVersion(cSource.Version)
			if err != nil {
				log.Fatalf("Failed to parse version %s: %s", cSource.Version, err)
			}
		}
		source.Version = upperVersion
		if len(source.Modules) == 0 {
			log.Printf("No modules found for Source %s\n", cSource.URL)
		}
		newestVersion, releases, err := getSourceReleases(githubClient, config, source, state)
		if err != nil {
			log.Fatalf("Failed to get releases: %v", err)
		}
		source.NewestVersion = newestVersion
		source.Releases = releases
	}
}

func createDestinations(ctx context.Context, destinations []model.ConfigDestination) map[string]model.Destination {
	dests := make(map[string]model.Destination)
	for _, destination := range destinations {
		if destination.Git != nil {
			client, err := git.NewGitClient(ctx, destination.Name, *destination.Git)
			if err != nil {
				log.Fatalf("Destination %s failed to create git client: %v", destination.Name, err)
			}
			dests[destination.Name] = client
		}
	}
	return dests
}

func (u *updater) Run() {
	u.updateAgentJob(common.RunCommand)
	index := 0
	u.logReleases(index)
	u.updateState()
	wg := new(model.SafeCounter)
	errChan := make(chan error, 1)
	failed := false
	retrySteps := make([]model.Step, 0)
	for _, step := range u.config.Steps {
		retry, err := u.processStep(index, step, wg, errChan)
		if err != nil {
			common.PrintError(err)
			failed = true
			break
		}
		if retry {
			retrySteps = append(retrySteps, step)
		}
	}
	wg.Wait()
	close(errChan)
	time.Sleep(1 * time.Second)
	u.putStateFileOrDie()
	if _, ok := <-errChan; ok || failed {
		log.Fatalf("One or more steps failed to apply")
	}
	u.retrySteps(index, retrySteps, wg)
}

func (u *updater) logReleases(index int) {
	var sourceReleases []string
	for url, source := range u.sources {
		if source.ForcedVersion != "" {
			sourceReleases = append(sourceReleases, fmt.Sprintf("%s %s", url, source.ForcedVersion))
			continue
		}
		if index < len(source.Releases) {
			release := source.Releases[index]
			sourceReleases = append(sourceReleases, fmt.Sprintf("%s %s", url, release.Original()))
		}
	}
	log.Printf("Applying releases: %s", strings.Join(sourceReleases, ", "))
}

func (u *updater) Update() {
	u.updateAgentJob(common.UpdateCommand)
	mostReleases := u.getMostReleases()
	if mostReleases < 2 {
		log.Println("No updates found")
		return
	}
	for index := 1; index < mostReleases; index++ {
		u.logReleases(index)
		u.updateState()
		u.GetChecksums(index)
		wg := new(model.SafeCounter)
		errChan := make(chan error, 1)
		failed := false
		retrySteps := make([]model.Step, 0)
		for _, step := range u.config.Steps {
			retry, err := u.processStep(index, step, wg, errChan)
			if err != nil {
				common.PrintError(err)
				failed = true
				break
			}
			if retry {
				retrySteps = append(retrySteps, step)
			}
		}
		wg.Wait()
		close(errChan)
		time.Sleep(1 * time.Second)
		u.putStateFileOrDie()
		if _, ok := <-errChan; ok || failed {
			log.Fatalf("One or more steps failed to apply")
		}
		u.retrySteps(index, retrySteps, wg)
		for i, source := range u.sources {
			u.sources[i].PreviousChecksums = source.CurrentChecksums
		}
	}
}

func (u *updater) getMostReleases() int {
	mostReleases := 0
	for _, source := range u.sources {
		if len(source.Releases) > mostReleases {
			mostReleases = len(source.Releases)
		}
	}
	return mostReleases
}

func (u *updater) updateState() {
	if len(u.state.Steps) == 0 {
		createState(u.config, u.state)
		return
	}
	removeUnusedSteps(u.resources.GetCloudPrefix(), u.config, u.state, u.resources.GetBucket())
	addNewSteps(u.config, u.state)
}

func (u *updater) processStep(index int, step model.Step, wg *model.SafeCounter, errChan chan<- error) (bool, error) {
	stepState, err := u.getStepState(step)
	if err != nil {
		return false, err
	}
	moduleVersions, err := u.updateModuleVersions(step, stepState, index)
	if err != nil {
		return false, err
	}
	step, err = u.mergeModuleInputs(step, moduleVersions)
	if err != nil {
		return false, err
	}
	step, err = u.replaceConfigStepValues(step, index)
	if err != nil {
		var parameterError *model.ParameterNotFoundError
		if wg.HasCount() && errors.As(err, &parameterError) {
			common.PrintWarning(err.Error())
			log.Printf("Step %s will be retried if others succeed\n", step.Name)
			return true, nil
		}
		return false, err
	}
	var executePipelines bool
	var providers map[string]model.Set[string]
	var files map[string][]byte
	if !u.firstRunDone[step.Name] {
		executePipelines, files, err = u.createStepFiles(step, moduleVersions, index)
	} else {
		executePipelines, providers, files, err = u.updateStepFiles(step, moduleVersions, index)
	}
	if err != nil {
		return false, err
	}
	err = u.applyRelease(!u.firstRunDone[step.Name], executePipelines, step, stepState, index, providers, wg, errChan, files)
	if err != nil {
		return false, err
	}
	u.firstRunDone[step.Name] = true
	return false, nil
}

func (u *updater) retrySteps(index int, retrySteps []model.Step, wg *model.SafeCounter) {
	if len(retrySteps) == 0 {
		return
	}
	u.allowParallel = false
	for _, step := range retrySteps {
		log.Printf("Retrying step %s\n", step.Name)
		_, err := u.processStep(index, step, wg, nil)
		if err != nil {
			common.PrintError(err)
			log.Fatalf("Failed to apply step %s", step.Name)
		}
	}
	u.putStateFileOrDie()
}

func (u *updater) updateDestinationsPlanFiles(step model.Step, files map[string][]byte) {
	u.updateDestinationsFiles(step, git.PlanBranch, files)
}

func (u *updater) updateDestinationsApplyFiles(step model.Step, files map[string][]byte) {
	u.updateDestinationsFiles(step, git.ApplyBranch, files)
}

func (u *updater) updateDestinationsFiles(step model.Step, branch string, files map[string][]byte) {
	folder := fmt.Sprintf("steps/%s-%s", u.resources.GetCloudPrefix(), step.Name)
	for name, destination := range u.destinations {
		log.Printf("Step %s updating %s files for destination %s\n", step.Name, branch, name)
		err := destination.UpdateFiles(branch, folder, files)
		if err != nil {
			slog.Warn(fmt.Sprintf("Step %s failed to update %s files for destination %s: %s", step.Name, branch,
				name, err))
			return
		}
	}
}

func (u *updater) applyRelease(firstRun bool, executePipelines bool, step model.Step, stepState *model.StateStep, index int, providers map[string]model.Set[string], wg *model.SafeCounter, errChan chan<- error, files map[string][]byte) error {
	if !executePipelines {
		return nil
	}
	u.updateDestinationsPlanFiles(step, files)
	if !firstRun {
		if !u.hasChanged(step, providers) {
			log.Printf("Skipping step %s\n", step.Name)
			return u.putAppliedStateFile(stepState)
		}
		u.updateDestinationsApplyFiles(step, files)
		return nil //u.executePipeline(firstRun, step, stepState, index) // TODO Enable all!
	}
	if !u.allowParallel || !u.appliedVersionMatchesRelease(step, *stepState, index) {
		u.updateDestinationsApplyFiles(step, files)
		return nil //u.executePipeline(firstRun, step, stepState, index)
	}
	wg.Add(1)
	go func() {
		//defer wg.Done()
		//err := u.executePipeline(firstRun, step, stepState, index)
		//if err != nil {
		//	common.PrintError(err)
		//	errChan <- err
		//} else {
		//	u.updateDestinationsApplyFiles(step, files)
		//}
		u.updateDestinationsApplyFiles(step, files)
	}()
	return nil
}

func (u *updater) hasChanged(step model.Step, providers map[string]model.Set[string]) bool {
	changed := u.getChangedProviders(providers)
	if len(changed) > 0 {
		log.Printf("Step %s providers have changed: %s\n", step.Name,
			strings.Join(changed, ", "))
		return true
	}
	changed = u.getChangedModules(step)
	if len(changed) > 0 {
		log.Printf("Step %s modules have changed: %s\n", step.Name,
			strings.Join(changed, ", "))
		return true
	}
	return false
}

func (u *updater) getChangedProviders(repoProviders map[string]model.Set[string]) []string {
	changed := make([]string, 0)
	if repoProviders == nil {
		return changed
	}
	for repoURL, providers := range repoProviders {
		providerSource := u.sources[repoURL]
		for provider := range providers {
			providerKey := fmt.Sprintf("providers/%s.tf", provider)
			previousChecksum, ok := providerSource.PreviousChecksums[providerKey]
			if !ok {
				slog.Debug(fmt.Sprintf("Provider %s not found in previous checksums", provider))
				changed = append(changed, provider)
				continue
			}
			currentChecksum, ok := providerSource.CurrentChecksums[providerKey]
			if !ok {
				slog.Debug(fmt.Sprintf("Provider %s not found in current checksums", provider))
				changed = append(changed, provider)
				continue
			}
			if previousChecksum != currentChecksum {
				slog.Debug(fmt.Sprintf("Provider %s has changed, previous %s, current %s", provider,
					previousChecksum, currentChecksum))
				changed = append(changed, provider)
			}
		}
	}
	return changed
}

func (u *updater) getChangedModules(step model.Step) []string {
	changed := make([]string, 0)
	if step.Modules == nil {
		return changed
	}
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		moduleSource := u.getModuleSource(module.Source)
		if moduleSource.PreviousChecksums == nil || moduleSource.CurrentChecksums == nil {
			changed = append(changed, module.Name)
			continue
		}
		source := module.Source
		if step.Type == model.StepTypeArgoCD {
			source = fmt.Sprintf("k8s/%s", module.Source)
		}
		moduleKey := fmt.Sprintf("modules/%s", source)
		previousChecksum, ok := moduleSource.PreviousChecksums[moduleKey]
		if !ok {
			changed = append(changed, module.Name)
			continue
		}
		currentChecksum, ok := moduleSource.CurrentChecksums[moduleKey]
		if !ok {
			changed = append(changed, module.Name)
			continue
		}
		if previousChecksum != currentChecksum {
			changed = append(changed, module.Name)
		}
	}
	return changed
}

func (u *updater) appliedVersionMatchesRelease(step model.Step, stepState model.StateStep, index int) bool {
	for _, moduleState := range stepState.Modules {
		if moduleState.Type != nil && *moduleState.Type == model.ModuleTypeCustom {
			continue
		}
		if moduleState.AppliedVersion == nil {
			return false
		}
		module := getModule(moduleState.Name, step.Modules)
		moduleSource := u.getModuleSource(module.Source)
		if moduleSource.ForcedVersion != "" {
			return moduleSource.ForcedVersion == *moduleState.AppliedVersion
		}
		release := moduleSource.Releases[util.MinInt(index, len(moduleSource.Releases)-1)].Original()
		if *moduleState.AppliedVersion != release {
			return false
		}
	}
	return true
}

func (u *updater) executePipeline(firstRun bool, step model.Step, stepState *model.StateStep, index int) error {
	log.Printf("Applying release for step %s\n", step.Name)
	var err error
	if firstRun {
		err = u.createExecuteStepPipelines(step, *stepState, index)
	} else {
		err = u.executeStepPipelines(step, *stepState, index)
	}
	if err != nil {
		return err
	}
	log.Printf("release applied successfully for step %s\n", step.Name)
	return u.putAppliedStateFile(stepState)
}

func (u *updater) updateAgentJob(cmd common.Command) {
	agent := NewAgent(u.resources)
	err := agent.UpdateProjectImage(u.config.AgentVersion, cmd)
	if err != nil {
		log.Fatalf("Failed to update agent codebuild: %s", err)
	}
}

func (u *updater) getStepState(step model.Step) (*model.StateStep, error) {
	stepState := GetStepState(u.state, step.Name)
	if stepState == nil {
		return nil, fmt.Errorf("failed to get state for step %s", step.Name)
	}
	return stepState, nil
}

func (u *updater) createStepFiles(step model.Step, moduleVersions map[string]model.ModuleVersion, index int) (bool, map[string][]byte, error) {
	switch step.Type {
	case model.StepTypeTerraform:
		return u.createTerraformFiles(step, moduleVersions, index)
	case model.StepTypeArgoCD:
		return u.updateArgoCDFiles(step, moduleVersions)
	default:
		return false, nil, fmt.Errorf("step type %s not supported", step.Type)
	}
}

func (u *updater) updateStepFiles(step model.Step, moduleVersions map[string]model.ModuleVersion, index int) (bool, map[string]model.Set[string], map[string][]byte, error) {
	switch step.Type {
	case model.StepTypeTerraform:
		return u.updateTerraformFiles(step, moduleVersions, index)
	case model.StepTypeArgoCD:
		execute, files, err := u.updateArgoCDFiles(step, moduleVersions)
		return execute, nil, files, err
	default:
		return false, nil, nil, fmt.Errorf("step type %s not supported", step.Type)
	}
}

func (u *updater) createExecuteStepPipelines(step model.Step, stepState model.StateStep, index int) error {
	bucket := u.resources.GetBucket()
	repoMetadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return err
	}

	stepName := fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name)

	vpcConfig := getVpcConfig(step)
	imageVersion, imageSource := u.getBaseImage(step, index)
	err = u.resources.GetBuilder().CreateProject(stepName, repoMetadata.URL, stepName, step, imageVersion, imageSource, vpcConfig)
	if err != nil {
		return fmt.Errorf("failed to create CodeBuild project: %w", err)
	}
	autoApprove := getAutoApprove(stepState)
	return u.createExecuteTerraformPipelines(stepName, stepName, step, autoApprove, bucket)
}

func getVpcConfig(step model.Step) *model.VpcConfig {
	if !*step.Vpc.Attach {
		return nil
	}
	return &model.VpcConfig{
		VpcId:            &step.Vpc.Id,
		Subnets:          util.ToList(step.Vpc.SubnetIds),
		SecurityGroupIds: util.ToList(step.Vpc.SecurityGroupIds),
	}
}

func (u *updater) createExecuteTerraformPipelines(projectName string, stepName string, step model.Step, autoApprove bool, bucket model.Bucket) error {
	executionId, err := u.resources.GetPipeline().CreatePipeline(projectName, stepName, step, bucket)
	if err != nil {
		return fmt.Errorf("failed to create pipeline %s: %w", projectName, err)
	}
	err = u.resources.GetPipeline().WaitPipelineExecution(projectName, projectName, executionId, autoApprove, step.Type)
	if err != nil {
		return fmt.Errorf("failed to wait for pipeline %s execution: %w", projectName, err)
	}
	return nil
}

func (u *updater) executeStepPipelines(step model.Step, stepState model.StateStep, index int) error {
	stepName := fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name)
	vpcConfig := getVpcConfig(step)
	imageVersion, imageSource := u.getBaseImage(step, index)
	bucket := u.resources.GetBucket()
	repoMetadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return err
	}
	err = u.resources.GetBuilder().UpdateProject(stepName, repoMetadata.URL, stepName, step, imageVersion, imageSource, vpcConfig)
	if err != nil {
		return err
	}
	err = u.updatePipelines(stepName, step, repoMetadata.Name)
	if err != nil {
		return err
	}
	executionId, err := u.resources.GetPipeline().StartPipelineExecution(stepName, stepName, step, repoMetadata.Name)
	if err != nil {
		return fmt.Errorf("failed to start pipeline %s execution: %w", stepName, err)
	}
	autoApprove := getAutoApprove(stepState)
	return u.resources.GetPipeline().WaitPipelineExecution(stepName, stepName, executionId, autoApprove, step.Type)
}

func getAutoApprove(state model.StateStep) bool {
	for _, module := range state.Modules {
		if !module.AutoApprove {
			return false
		}
	}
	return true
}

func getSourceReleases(githubClient git.Github, config model.Config, source *model.Source, state *model.State) (*version.Version, []*version.Version, error) {
	oldestVersion, err := getOldestVersion(config, source, state)
	if err != nil {
		return nil, nil, err
	}
	if oldestVersion == StableVersion || oldestVersion == source.StableVersion.Original() {
		latestRelease := source.StableVersion
		log.Printf("Latest release for %s is %s\n", source.URL, latestRelease.Original())
		return latestRelease, []*version.Version{latestRelease}, nil
	}
	oldestRelease, err := githubClient.GetReleaseByTag(source.URL, getFormattedVersionString(oldestVersion))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get oldest release %s: %w", oldestVersion, err)
	}
	log.Printf("Oldest module version for %s is %s\n", source.URL, oldestRelease.Tag)

	newestVersion, err := getNewestVersion(config, source)
	if err != nil {
		return nil, nil, err
	}
	var newestRelease *git.Release
	if newestVersion != StableVersion {
		newestRelease, err = githubClient.GetReleaseByTag(source.URL, getFormattedVersionString(newestVersion))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get newest release %s: %w", oldestVersion, err)
		}
		log.Printf("Newest module version for %s is %s\n", source.URL, newestRelease.Tag)
	}

	releases, err := githubClient.GetReleases(source.URL, *oldestRelease, newestRelease)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get newer releases: %w", err)
	}
	return releases[len(releases)-1], releases, nil
}

func getLatestRelease(githubClient git.Github, repoURL string) *version.Version {
	latestRelease, err := githubClient.GetLatestReleaseTag(repoURL)
	if err != nil {
		log.Fatal(err.Error())
	}
	latestSemver, err := version.NewVersion(latestRelease.Tag)
	if err != nil {
		log.Fatalf("Failed to parse latest release version %s: %s", latestRelease.Tag, err)
	}
	return latestSemver
}

func getOldestVersion(config model.Config, source *model.Source, state *model.State) (string, error) {
	oldestVersion := source.Version.Original()
	var err error
	for _, step := range config.Steps {
		stepState := GetStepState(state, step.Name)
		for _, module := range step.Modules {
			if util.IsClientModule(module) || !source.Modules.Contains(module.Source) {
				continue
			}
			oldestVersion, err = getOlderVersion(oldestVersion, module.Version)
			if err != nil {
				return "", err
			}
			if stepState == nil {
				continue
			}
			moduleState := GetModuleState(stepState, module.Name)
			if moduleState == nil {
				continue
			}
			if moduleState.Source != source.URL {
				continue
			}
			moduleStateVersion := ""
			if moduleState.AppliedVersion != nil {
				moduleStateVersion = *moduleState.AppliedVersion
			} else if moduleState.Version != "" {
				moduleStateVersion = moduleState.Version
			}
			oldestVersion, err = getOlderVersion(oldestVersion, moduleStateVersion)
			if err != nil {
				return "", err
			}
		}
	}
	return oldestVersion, nil
}

func getOlderVersion(oldestVersion string, compareVersion string) (string, error) {
	if compareVersion == "" || oldestVersion != StableVersion && compareVersion == StableVersion ||
		oldestVersion == StableVersion && compareVersion == StableVersion {
		return oldestVersion, nil
	} else if oldestVersion == StableVersion && compareVersion != StableVersion {
		return compareVersion, nil
	}
	version1, err := version.NewVersion(oldestVersion)
	if err != nil {
		return "", fmt.Errorf("failed to parse version %s: %w", oldestVersion, err)
	}
	version2, err := version.NewVersion(compareVersion)
	if err != nil {
		return "", fmt.Errorf("failed to parse version %s: %w", compareVersion, err)
	}
	if version1.LessThan(version2) {
		return oldestVersion, nil
	} else {
		return compareVersion, nil
	}
}

func getNewestVersion(config model.Config, source *model.Source) (string, error) {
	newestVersion := ""
	var err error
	for _, step := range config.Steps {
		for _, module := range step.Modules {
			if util.IsClientModule(module) || !source.Modules.Contains(module.Source) {
				continue
			}
			if module.Version == StableVersion {
				return StableVersion, nil
			}
			moduleVersion := module.Version
			if moduleVersion == "" {
				moduleVersion = source.Version.Original()
			}
			newestVersion, err = getNewerVersion(newestVersion, moduleVersion)
			if err != nil {
				return "", err
			}
		}
	}
	return newestVersion, nil
}

func getNewerVersion(newestVersion string, moduleVersion string) (string, error) {
	if newestVersion == "" {
		return moduleVersion, nil
	}
	version1, err := version.NewVersion(newestVersion)
	if err != nil {
		return "", fmt.Errorf("failed to parse version %s: %w", newestVersion, err)
	}
	version2, err := version.NewVersion(moduleVersion)
	if err != nil {
		return "", fmt.Errorf("failed to parse version %s: %w", moduleVersion, err)
	}
	if version1.GreaterThan(version2) {
		return newestVersion, nil
	} else {
		return moduleVersion, nil
	}
}

func (u *updater) createTerraformFiles(step model.Step, moduleVersions map[string]model.ModuleVersion, index int) (bool, map[string][]byte, error) {
	execute, _, files, err := u.updateTerraformFiles(step, moduleVersions, index)
	return execute, files, err
}

func (u *updater) updateTerraformFiles(step model.Step, moduleVersions map[string]model.ModuleVersion, index int) (bool, map[string]model.Set[string], map[string][]byte, error) {
	files := make(map[string][]byte)
	mainPath, mainFile, err := u.createBackendConf(fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name), u.resources.GetBucket())
	if err != nil {
		return false, nil, nil, err
	}
	files[mainPath] = mainFile
	changed, mainPath, mainBytes, err := u.createTerraformMain(step, moduleVersions)
	if err != nil {
		return false, nil, nil, err
	}
	files[mainPath] = mainBytes
	err = u.updateIncludedStepFiles(step, ReservedTFFiles, model.ToSet([]string{terraformCache}), files)
	if err != nil {
		return false, nil, nil, err
	}
	if len(moduleVersions) == 0 {
		return false, nil, nil, errors.New("no module versions found")
	}
	sourceVersions, err := u.getSourceVersions(step, moduleVersions, index)
	if err != nil {
		return false, nil, nil, err
	}
	provider, providers, err := u.terraform.GetTerraformProvider(step, moduleVersions, sourceVersions)
	if err != nil {
		return false, nil, nil, fmt.Errorf("failed to create terraform provider: %s", err)
	}
	modifiedProvider, err := u.replaceStringValues(step, string(provider), index, make(paramCache))
	if err != nil {
		return false, nil, nil, fmt.Errorf("failed to replace provider values: %s", err)
	}
	providerFile := fmt.Sprintf("steps/%s-%s/provider.tf", u.resources.GetCloudPrefix(), step.Name)
	files[providerFile] = []byte(modifiedProvider)
	err = u.resources.GetBucket().PutFile(providerFile, []byte(modifiedProvider))
	return changed || len(step.Files) > 0, providers, files, err
}

func (u *updater) getSourceVersions(step model.Step, moduleVersions map[string]model.ModuleVersion, index int) (map[string]string, error) {
	sourceVersions := make(map[string]string)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		source := moduleVersions[module.Name].SourceURL
		moduleVersion, err := version.NewVersion(moduleVersions[module.Name].Version)
		if err != nil {
			continue
		}
		if sourceVersions[source] == "" {
			sourceVersions[source] = moduleVersion.Original()
		}
		sourceVersion, err := version.NewVersion(sourceVersions[source])
		if err != nil {
			continue
		}
		if moduleVersion.GreaterThan(sourceVersion) {
			sourceVersions[source] = moduleVersion.Original()
		}
	}
	for sourceURL, source := range u.sources {
		if source.ForcedVersion != "" {
			sourceVersions[sourceURL] = source.ForcedVersion
			continue
		}
		_, exists := sourceVersions[sourceURL]
		if exists {
			continue
		}
		sourceVersions[sourceURL] = source.Releases[util.MinInt(index, len(source.Releases)-1)].Original()
	}
	return sourceVersions, nil
}

func (u *updater) updateArgoCDFiles(step model.Step, moduleVersions map[string]model.ModuleVersion) (bool, map[string][]byte, error) {
	executePipeline := false
	files := make(map[string][]byte)
	for _, module := range step.Modules {
		moduleVersion, found := moduleVersions[module.Name]
		if !found {
			return false, nil, fmt.Errorf("module %s version not found", module.Name)
		}
		if moduleVersion.Changed {
			executePipeline = true
		}
		inputs := module.Inputs
		if len(inputs) == 0 {
			inputs = make(map[string]interface{})
		}
		prefix := fmt.Sprintf("%s-%s-%s", u.resources.GetCloudPrefix(), step.Name, module.Name)
		err := util.SetChildStringValue(inputs, prefix, false, "global", "prefix")
		if err != nil {
			return false, nil, fmt.Errorf("failed to set prefix: %s", err)
		}
		inputBytes, err := getModuleInputBytes(inputs)
		if err != nil {
			return false, nil, err
		}
		executePipeline = true
		filePath, file, err := u.createArgoCDApp(module, step, moduleVersion.Version, inputBytes)
		if err != nil {
			return false, nil, err
		}
		files[filePath] = file
	}
	err := u.updateIncludedStepFiles(step, ReservedAppsFiles, model.NewSet[string](), files)
	return executePipeline, files, err
}

func getModuleInputBytes(inputs map[string]interface{}) ([]byte, error) {
	if len(inputs) == 0 {
		return []byte{}, nil
	}
	bytes, err := yaml.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal inputs: %s", err)
	}
	return bytes, nil
}

func (u *updater) createBackendConf(path string, bucket model.Bucket) (string, []byte, error) {
	key := fmt.Sprintf("%s/terraform.tfstate", path)
	backendConfig := u.resources.GetBackendConfigVars(key)
	bytes, err := util.CreateKeyValuePairs(backendConfig, "", "")
	if err != nil {
		return "", nil, fmt.Errorf("failed to convert backend config values: %w", err)
	}
	filePath := fmt.Sprintf("steps/%s/backend.conf", path)
	return filePath, bytes, bucket.PutFile(filePath, bytes)
}

func (u *updater) putStateFileOrDie() {
	err := u.putStateFile()
	if err != nil {
		state, _ := yaml.Marshal(u.state)
		if state != nil {
			log.Println(string(state))
			log.Println("Update the state file manually to avoid reapplying steps")
		}
		log.Fatalf("Failed to put state file: %v", err)
	}
}

func (u *updater) putStateFile() error {
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	return u.resources.GetBucket().PutFile(stateFile, bytes)
}

func (u *updater) putAppliedStateFile(stepState *model.StateStep) error {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()

	stepState.AppliedAt = time.Now()
	for _, module := range stepState.Modules {
		module.AppliedVersion = &module.Version
	}
	return u.putStateFile()
}

func (u *updater) createTerraformMain(step model.Step, moduleVersions map[string]model.ModuleVersion) (bool, string, []byte, error) {
	file := hclwrite.NewEmptyFile()
	body := file.Body()
	changed := false
	for _, module := range step.Modules {
		moduleVersion, found := moduleVersions[module.Name]
		if !found {
			return false, "", nil, fmt.Errorf("module %s version not found", module.Name)
		}
		if moduleVersion.Changed {
			changed = true
		}
		err := u.terraform.AddModule(u.resources.GetCloudPrefix(), body, step, module, moduleVersion)
		if err != nil {
			return false, "", nil, err
		}
		body.AppendNewline()
	}
	filePath := fmt.Sprintf("steps/%s-%s/main.tf", u.resources.GetCloudPrefix(), step.Name)
	fileBytes := file.Bytes()
	if changed {
		err := u.resources.GetBucket().PutFile(filePath, fileBytes)
		if err != nil {
			return false, "", nil, err
		}
	}
	return changed, filePath, fileBytes, nil
}

func (u *updater) createArgoCDApp(module model.Module, step model.Step, moduleVersion string, values []byte) (string, []byte, error) {
	moduleSource := u.getModuleSource(module.Source)
	appBytes, err := argocd.GetApplicationFile(u.github, module, moduleSource.URL, step.RepoUrl, moduleVersion, values,
		u.resources.GetProviderType())
	if err != nil {
		return "", nil, fmt.Errorf("failed to create application file: %w", err)
	}
	filePath := fmt.Sprintf("steps/%s-%s/%s.yaml", u.resources.GetCloudPrefix(), step.Name, module.Name)
	return filePath, appBytes, u.resources.GetBucket().PutFile(filePath, appBytes)
}

func (u *updater) updateModuleVersions(step model.Step, stepState *model.StateStep, index int) (map[string]model.ModuleVersion, error) {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()

	moduleVersions := make(map[string]model.ModuleVersion)
	for _, module := range step.Modules {
		moduleVersion, changed, err := u.getModuleVersion(module, stepState, index, step.Approve)
		if err != nil {
			return nil, err
		}
		moduleVersions[module.Name] = model.ModuleVersion{
			Version:   moduleVersion,
			Changed:   changed,
			SourceURL: u.moduleSources[module.Source],
		}
	}
	err := u.putStateFile()
	if err != nil {
		return nil, err
	}
	return moduleVersions, nil
}

func (u *updater) getModuleVersion(module model.Module, stepState *model.StateStep, index int, approve model.Approve) (string, bool, error) {
	moduleVersion := module.Version
	moduleState, err := getModuleState(stepState, module)
	if err != nil {
		return "", false, err
	}
	if util.IsClientModule(module) {
		moduleState.Version = module.Version
		return module.Version, true, nil
	}
	moduleSource := u.getModuleSource(module.Source)
	if moduleSource.ForcedVersion != "" {
		if moduleState.Source != moduleSource.URL {
			moduleState.Source = moduleSource.URL
			moduleState.AppliedVersion = nil
		}
		moduleState.Version = moduleSource.ForcedVersion
		return moduleSource.ForcedVersion, true, nil
	}
	var moduleSemver *version.Version
	if moduleVersion == "" {
		moduleSemver = moduleSource.Version
	} else if moduleVersion == StableVersion {
		moduleSemver = moduleSource.NewestVersion
	} else {
		moduleSemver, err = version.NewVersion(moduleVersion)
		if err != nil {
			moduleSemver = moduleSource.NewestVersion
		}
	}
	moduleState.AutoApprove = true
	if index > len(moduleSource.Releases)-1 {
		return getFormattedVersion(moduleSemver), false, nil
	}
	releaseTag := moduleSource.Releases[index]
	if moduleState.AppliedVersion == nil || moduleState.Source != moduleSource.URL {
		moduleState.Source = moduleSource.URL
		moduleState.AppliedVersion = nil
		if moduleSemver.GreaterThan(releaseTag) {
			moduleState.Version = getFormattedVersion(releaseTag)
			return getFormattedVersion(releaseTag), true, nil
		} else {
			moduleState.Version = getFormattedVersion(moduleSemver)
			return getFormattedVersion(moduleSemver), true, nil
		}
	}
	var moduleStateSemver *version.Version
	moduleStateSemver, err = version.NewVersion(*moduleState.AppliedVersion)
	if err != nil {
		return "", false, fmt.Errorf("failed to parse module state applied version %s: %s", moduleVersion, err)
	}
	if moduleSemver.Equal(moduleStateSemver) && moduleSemver.LessThan(releaseTag) {
		return getFormattedVersion(moduleStateSemver), false, nil
	}
	if moduleStateSemver.GreaterThan(releaseTag) {
		return getFormattedVersion(moduleStateSemver), false, nil
	} else {
		moduleState.AutoApprove = getModuleAutoApprove(moduleStateSemver, releaseTag, approve)
		moduleState.Version = getFormattedVersion(releaseTag)
		return getFormattedVersion(releaseTag), true, nil
	}
}

func (u *updater) getModuleSource(moduleSource string) *model.Source {
	sourceUrl := u.moduleSources[moduleSource]
	return u.sources[sourceUrl]
}

func (u *updater) updatePipelines(projectName string, step model.Step, bucket string) error {
	stepName := fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name)
	err := u.resources.GetPipeline().UpdatePipeline(projectName, stepName, step, bucket)
	if err != nil {
		return fmt.Errorf("failed to update pipeline %s: %w", projectName, err)
	}
	return nil
}

func (u *updater) getBaseImage(step model.Step, index int) (string, string) {
	release := model.LatestImageVersion
	if step.BaseImageVersion != "" {
		release = step.BaseImageVersion
	} else if u.config.BaseImageVersion != "" {
		release = u.config.BaseImageVersion
	} else {
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				continue
			}
			source := u.getModuleSource(module.Source)
			if !strings.Contains(source.URL, EntigoSource) {
				continue
			}
			if source.ForcedVersion != "" {
				break
			} else {
				release = getFormattedVersion(source.Releases[util.MinInt(index, len(source.Releases)-1)])
				break
			}
		}
	}
	imageSource := ""
	if step.BaseImageSource != "" {
		imageSource = step.BaseImageSource
	} else if u.config.BaseImageSource != "" {
		imageSource = u.config.BaseImageSource
	}
	return release, imageSource
}

func (u *updater) GetChecksums(index int) {
	for url, source := range u.sources {
		if len(source.Releases)-1 < index {
			continue
		}
		checksums, err := getChecksums(u.github, url, source.Releases[index].Original())
		if err != nil {
			log.Fatalf("Failed to get checksums for %s: %s", url, err)
		}
		source.CurrentChecksums = checksums
	}
}

func getChecksums(githubClient git.Github, sourceURL string, release string) (map[string]string, error) {
	content, err := githubClient.GetRawFileContent(sourceURL, checksumsFile, release)
	if err != nil {
		var fileError model.FileNotFoundError
		if errors.As(err, &fileError) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	checksums := make(map[string]string)
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			log.Printf("Invalid line: %s\n", line)
			continue
		}
		checksums[strings.TrimRight(parts[0], ":")] = parts[1]
	}
	return checksums, nil
}

func (u *updater) updateIncludedAppsStepFiles(step model.Step) (model.Set[string], error) {
	files := model.Set[string]{}
	folder := fmt.Sprintf("steps/%s-%s", u.resources.GetCloudPrefix(), step.Name)
	for _, file := range step.Files {
		target := fmt.Sprintf("%s/%s", folder, file.Name)
		err := u.resources.GetBucket().PutFile(target, file.Content)
		if err != nil {
			return nil, err
		}
		files.Add(target)
	}
	return files, nil
}

func (u *updater) updateIncludedStepFiles(step model.Step, reservedFiles, excludedFolders model.Set[string], includedFiles map[string][]byte) error {
	files := model.Set[string]{}
	folder := fmt.Sprintf("steps/%s-%s", u.resources.GetCloudPrefix(), step.Name)
	for _, file := range step.Files {
		target := fmt.Sprintf("%s/%s", folder, file.Name)
		err := u.resources.GetBucket().PutFile(target, file.Content)
		if err != nil {
			return err
		}
		files.Add(target)
		includedFiles[target] = file.Content
	}
	folderFiles, err := u.resources.GetBucket().ListFolderFilesWithExclude(folder, excludedFolders)
	if err != nil {
		return err
	}
	for _, file := range folderFiles {
		relativeFile := strings.TrimPrefix(file, folder+"/")
		if reservedFiles.Contains(relativeFile) {
			continue
		}
		if !files.Contains(file) {
			err = u.resources.GetBucket().DeleteFile(file)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (u *updater) mergeModuleInputs(step model.Step, moduleVersions map[string]model.ModuleVersion) (model.Step, error) {
	for i, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		moduleVersion, found := moduleVersions[module.Name]
		if !found {
			return step, fmt.Errorf("module %s version not found", module.Name)
		}
		moduleSource := u.getModuleSource(module.Source)
		inputs, err := u.getModuleInputs(step.Type, module, moduleSource, moduleVersion.Version)
		if err != nil {
			return step, err
		}
		step.Modules[i].Inputs = inputs
	}
	return step, nil
}

func (u *updater) getModuleInputs(stepType model.StepType, module model.Module, source *model.Source, moduleVersion string) (map[string]interface{}, error) {
	moduleSource := module.Source
	if stepType == model.StepTypeArgoCD {
		moduleSource = fmt.Sprintf("k8s/%s", module.Source)
	}
	filePath := fmt.Sprintf("modules/%s/agent_input.yaml", moduleSource)
	defaultInputs, err := u.getModuleDefaultInputs(filePath, source, moduleVersion)
	if err != nil {
		return nil, err
	}

	providerType := u.resources.GetProviderType()
	if providerType == model.AWS {
		providerType = "aws"
	} else if providerType == model.GCLOUD {
		providerType = "google"
	}
	filePath = fmt.Sprintf("modules/%s/agent_input_%s.yaml", moduleSource, providerType)
	providerInputs, err := u.getModuleDefaultInputs(filePath, source, moduleVersion)
	if err != nil {
		return nil, err
	}

	inputs, err := mergeInputs(defaultInputs, providerInputs)
	if err != nil {
		return nil, fmt.Errorf("failed to merge inputs: %v", err)
	}
	inputs, err = mergeInputs(inputs, module.Inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to merge inputs: %v", err)
	}
	return inputs, nil
}

func (u *updater) getModuleDefaultInputs(filePath string, moduleSource *model.Source, moduleVersion string) (map[string]interface{}, error) {
	defaultInputsRaw, err := u.github.GetRawFileContent(moduleSource.URL, filePath, moduleVersion)
	if err != nil {
		var fileError model.FileNotFoundError
		if errors.As(err, &fileError) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get module file %s: %v", filePath, err)
	}
	var defaultInputs map[string]interface{}
	err = yaml.Unmarshal(defaultInputsRaw, &defaultInputs)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal default inputs: %v", err)
	}
	return defaultInputs, nil
}

func mergeInputs(baseInputs map[string]interface{}, patchInputs map[string]interface{}) (map[string]interface{}, error) {
	if baseInputs != nil && patchInputs == nil {
		return baseInputs, nil
	} else if baseInputs == nil && patchInputs != nil {
		return patchInputs, nil
	} else if baseInputs == nil {
		return nil, nil
	}
	err := mergo.Merge(&baseInputs, patchInputs, mergo.WithOverride)
	if err != nil {
		return nil, err
	}
	return baseInputs, nil
}

func getFormattedVersion(version *version.Version) string {
	if version == nil {
		return ""
	}
	return getFormattedVersionString(version.Original())
}

func getFormattedVersionString(original string) string {
	if strings.HasPrefix(original, "v") {
		return original
	}
	return fmt.Sprintf("v%s", original)
}

func getModuleState(stepState *model.StateStep, module model.Module) (*model.StateModule, error) {
	moduleState := GetModuleState(stepState, module.Name)
	if moduleState == nil {
		return nil, fmt.Errorf("failed to get state for module %s", module.Name)
	}
	return moduleState, nil
}

func getModuleAutoApprove(moduleVersion *version.Version, releaseTag *version.Version, approve model.Approve) bool {
	if approve == model.ApproveNever {
		return true
	}
	if approve == "" || approve == model.ApproveAlways {
		return false
	}
	releaseSegments := releaseTag.Segments()
	moduleSegments := moduleVersion.Segments()
	if approve == model.ApproveMajor {
		return moduleSegments[0] >= releaseSegments[0]
	}
	if approve == model.ApproveMinor {
		return moduleSegments[0] >= releaseSegments[0] && moduleSegments[1] >= releaseSegments[1]
	}
	return false
}
