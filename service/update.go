package service

import (
	"bytes"
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
	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"
	"log"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	stateFile = "state.yaml"
	ssmPrefix = "/entigo-infralib"
)

type Updater interface {
	Process(command common.Command) error
}

type updater struct {
	cmd           common.Command
	config        model.Config
	steps         []model.Step
	stepChecksums model.StepsChecksums
	resources     model.Resources
	terraform     terraform.Terraform
	destinations  map[string]model.Destination
	state         *model.State
	stateLock     sync.Mutex
	pipelineFlags common.Pipeline
	localPipeline *LocalPipeline
	callback      Callback
	moduleSources map[string]string
	sources       map[string]*model.Source
	firstRunDone  map[string]bool
}

func NewUpdater(ctx context.Context, flags *common.Flags, resources model.Resources, notifiers []model.Notifier) (Updater, error) {
	config, err := GetFullConfig(resources.GetSSM(), resources.GetCloudPrefix(), flags.Config, resources.GetBucket())
	if err != nil {
		return nil, err
	}
	state, err := getLatestState(resources.GetBucket())
	if err != nil {
		return nil, err
	}
	if err = ValidateConfig(config, state); err != nil {
		return nil, err
	}
	ProcessConfig(&config, resources.GetProviderType())
	steps, err := getRunnableSteps(config, flags.Steps)
	if err != nil {
		return nil, err
	}
	sources, moduleSources, err := createSources(ctx, steps, config, state, resources.GetSSM())
	if err != nil {
		return nil, err
	}
	destinations, err := createDestinations(ctx, config)
	if err != nil {
		return nil, err
	}
	pipeline := ProcessPipelineFlags(flags.Pipeline)
	resources.GetPipeline().AddNotifiers(notifiers)
	return &updater{
		config:        config,
		steps:         steps,
		stepChecksums: model.NewStepsChecksums(),
		resources:     resources,
		terraform:     terraform.NewTerraform(resources.GetProviderType(), config.Sources, sources),
		destinations:  destinations,
		state:         state,
		pipelineFlags: pipeline,
		localPipeline: getLocalPipeline(resources, pipeline, flags.GCloud, notifiers),
		callback:      NewCallback(ctx, config.Callback),
		moduleSources: moduleSources,
		sources:       sources,
		firstRunDone:  make(map[string]bool),
	}, nil
}

func getLatestState(bucket model.Bucket) (*model.State, error) {
	file, err := bucket.GetFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get state file: %v", err)
	}
	if file == nil {
		return &model.State{}, nil
	}
	var state model.State
	err = yaml.Unmarshal(file, &state)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal state file: %v", err)
	}
	return &state, nil
}

func getRunnableSteps(config model.Config, stepsFlag cli.StringSlice) ([]model.Step, error) {
	if len(stepsFlag.Value()) == 0 {
		return config.Steps, nil
	}
	steps := model.NewSet[string]()
	for _, step := range stepsFlag.Value() {
		steps.Add(step)
	}
	runnableSteps := make([]model.Step, 0)
	for _, step := range config.Steps {
		if steps.Contains(step.Name) {
			runnableSteps = append(runnableSteps, step)
			steps.Remove(step.Name)
		}
	}
	if len(steps) > 0 {
		return nil, fmt.Errorf("runnable steps not found: %s", steps.String())
	}
	return runnableSteps, nil
}

func createSources(ctx context.Context, steps []model.Step, config model.Config, state *model.State, ssm model.SSM) (map[string]*model.Source, map[string]string, error) {
	sources := make(map[string]*model.Source)
	for _, source := range config.Sources {
		var stableVersion *version.Version
		var storage model.Storage
		if util.IsLocalSource(source.URL) {
			storage = git.NewLocalPath(source.URL)
		} else {
			CABundle, err := getCABundle(source.CAFile, config.Certs)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get CABundle for source %s: %v", source.CAFile, err)
			}
			sourceClient, err := git.NewSourceClient(ctx, source, CABundle)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get CABundle for source %s: %v", source.CAFile, err)
			}
			storage = sourceClient
			if !source.ForceVersion {
				stableVersion, err = sourceClient.GetLatestReleaseTag()
				if err != nil {
					return nil, nil, fmt.Errorf("failed to get latest release for %s: %s", source.URL, err)
				}
			}
		}
		if source.Username != "" {
			if err := upsertSourceCredentials(source, ssm); err != nil {
				return nil, nil, err
			}
		}
		sources[source.URL] = &model.Source{
			URL:           source.URL,
			StableVersion: stableVersion,
			Modules:       model.NewSet[string](),
			Includes:      model.ToSet(source.Include),
			Excludes:      model.ToSet(source.Exclude),
			Storage:       storage,
			Auth:          model.SourceAuth{Username: source.Username, Password: source.Password},
		}
	}
	moduleSources, err := addSourceModules(steps, config.Sources, sources)
	if err != nil {
		return nil, nil, err
	}
	err = addSourceReleases(steps, config.Sources, state, sources)
	return sources, moduleSources, err
}

func getCABundle(file string, certs []model.File) ([]byte, error) {
	if file == "" {
		return nil, nil
	}
	for _, cert := range certs {
		if filepath.Base(cert.Name) == file {
			return cert.Content, nil
		}
	}
	return nil, fmt.Errorf("CA file %s not found", file)
}

func upsertSourceCredentials(source model.ConfigSource, ssm model.SSM) error {
	hash := util.HashCode(source.URL)
	err := ssm.PutSecret(fmt.Sprintf(model.GitSourceFormat, hash), source.URL)
	if err != nil {
		return fmt.Errorf("failed to upsert secret %s: %v", fmt.Sprintf(model.GitSourceFormat, hash), err)
	}
	err = ssm.PutSecret(fmt.Sprintf(model.GitUsernameFormat, hash), source.Username)
	if err != nil {
		return fmt.Errorf("failed to upsert secret %s: %v", fmt.Sprintf(model.GitUsernameFormat, hash), err)
	}
	err = ssm.PutSecret(fmt.Sprintf(model.GitPasswordFormat, hash), source.Password)
	if err != nil {
		return fmt.Errorf("failed to upsert secret %s: %v", fmt.Sprintf(model.GitPasswordFormat, hash), err)
	}
	return nil
}

func addSourceModules(steps []model.Step, configSources []model.ConfigSource, sources map[string]*model.Source) (map[string]string, error) {
	moduleSources := make(map[string]string)
	for _, step := range steps {
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				continue
			}
			if moduleSources[module.Source] != "" {
				continue
			}
			moduleSource, err := getModuleSource(configSources, step, module, sources)
			if err != nil {
				return nil, fmt.Errorf("module %s in step %s is not included in any Source", module.Name, step.Name)
			}
			moduleSources[module.Source] = moduleSource
		}
	}
	return moduleSources, nil
}

func getModuleSource(configSources []model.ConfigSource, step model.Step, module model.Module, sources map[string]*model.Source) (string, error) {
	for _, configSource := range configSources {
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
		var release string
		if source.ForcedVersion != "" {
			release = source.ForcedVersion
		} else if source.StableVersion != nil {
			release = source.StableVersion.Original()
		}
		if source.Storage.PathExists(moduleKey, release) {
			sources[source.URL].Modules.Add(module.Source)
			return source.URL, nil
		}
	}
	return "", fmt.Errorf("module %s source not found", module.Name)
}

func addSourceReleases(steps []model.Step, configSources []model.ConfigSource, state *model.State, sources map[string]*model.Source) error {
	for _, cSource := range configSources {
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
				return fmt.Errorf("failed to parse version %s: %s", cSource.Version, err)
			}
		}
		source.Version = upperVersion
		if len(source.Modules) == 0 {
			log.Printf("No modules found for Source %s\n", cSource.URL)
		}
		newestVersion, releases, err := getSourceReleases(steps, source, state)
		if err != nil {
			return fmt.Errorf("failed to get releases: %v", err)
		}
		source.NewestVersion = newestVersion
		source.Releases = releases
	}
	return nil
}

func createDestinations(ctx context.Context, config model.Config) (map[string]model.Destination, error) {
	dests := make(map[string]model.Destination)
	for _, destination := range config.Destinations {
		if destination.Git == nil {
			continue
		}
		CABundle, err := getCABundle(destination.Git.CAFile, config.Certs)
		if err != nil {
			return nil, fmt.Errorf("failed to get CABundle for destination %s: %v", destination.Name, err)
		}
		client, err := git.NewDestClient(ctx, destination.Name, *destination.Git, CABundle)
		if err != nil {
			return nil, fmt.Errorf("destination %s failed to create git client: %v", destination.Name, err)
		}
		dests[destination.Name] = client
	}
	return dests, nil
}

func getLocalPipeline(resources model.Resources, pipeline common.Pipeline, gcloudFlags common.GCloud, notifiers []model.Notifier) *LocalPipeline {
	if pipeline.Type == string(common.PipelineTypeLocal) {
		return NewLocalPipeline(resources, pipeline, gcloudFlags, notifiers)
	}
	return nil
}

func (u *updater) Process(command common.Command) error {
	index := 0
	mostReleases := 1
	if command == common.UpdateCommand {
		index = 1
		mostReleases = u.getMostReleases()
		if mostReleases < 2 {
			log.Println("No updates found")
			return nil
		}
	}
	u.cmd = command
	for ; index < mostReleases; index++ {
		err := u.processRelease(index, command)
		if err != nil {
			return fmt.Errorf("failed to process release: %v", err)
		}
	}
	return nil
}

func (u *updater) processRelease(index int, command common.Command) error {
	u.logReleases(index)
	u.updateState()
	if command == common.UpdateCommand {
		if err := u.updateChecksums(index); err != nil {
			return err
		}
	}
	wg := new(model.SafeCounter)
	errChan := make(chan error, len(u.steps))
	failed := false
	retrySteps := make([]model.Step, 0)
	for _, step := range u.steps {
		retry, err := u.processStep(index, step, wg, errChan)
		if err != nil {
			slog.Error(common.PrefixError(err))
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
	err := u.putStateFileOrDie()
	if err != nil {
		return err
	}
	if _, ok := <-errChan; ok || failed {
		return errors.New("one or more steps failed to apply")
	}
	err = u.retrySteps(index, retrySteps, wg)
	if err != nil {
		return err
	}
	if command == common.UpdateCommand {
		for i, source := range u.sources {
			u.sources[i].PreviousChecksums = source.CurrentChecksums
		}
	}
	return nil
}

func (u *updater) getMostReleases() int {
	mostReleases := 0
	for _, source := range u.sources {
		if source.ForcedVersion != "" {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Source %s has forced version %s", source.URL,
				source.ForcedVersion)))
		}
		if len(source.Releases) > mostReleases {
			mostReleases = len(source.Releases)
		}
	}
	return mostReleases
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
		u.postCallback(model.ApplyStatusFailure, *stepState)
		return false, err
	}
	step, err = u.processModules(step, moduleVersions)
	if err != nil {
		u.postCallback(model.ApplyStatusFailure, *stepState)
		return false, err
	}
	step, err = u.replaceConfigStepValues(step, index)
	if err != nil {
		u.postCallback(model.ApplyStatusFailure, *stepState)
		var parameterError *model.ParameterNotFoundError
		if wg.HasCount() && errors.As(err, &parameterError) {
			slog.Warn(common.PrefixWarning(err.Error()))
			log.Printf("Step %s will be retried if others succeed\n", step.Name)
			return true, nil
		}
		return false, err
	}
	if !u.firstRunDone[step.Name] {
		err = u.updateCertFiles(step.Name)
		if err != nil {
			u.postCallback(model.ApplyStatusFailure, *stepState)
			return false, err
		}
	}
	executePipelines, providers, files, err := u.updateStepFiles(step, moduleVersions, index)
	if err != nil {
		u.postCallback(model.ApplyStatusFailure, *stepState)
		return false, err
	}
	u.updateStepChecksums(step, files)
	err = u.applyRelease(!u.firstRunDone[step.Name], executePipelines, step, stepState, index, providers, wg, errChan, files)
	if err != nil {
		return false, err
	}
	u.firstRunDone[step.Name] = true
	return false, nil
}

func (u *updater) retrySteps(index int, retrySteps []model.Step, wg *model.SafeCounter) error {
	if len(retrySteps) == 0 {
		return nil
	}
	u.pipelineFlags.AllowParallel = false
	for _, step := range retrySteps {
		log.Printf("Retrying step %s\n", step.Name)
		_, err := u.processStep(index, step, wg, nil)
		if err != nil {
			slog.Error(common.PrefixError(err))
			return fmt.Errorf("failed to apply step %s", step.Name)
		}
	}
	return u.putStateFileOrDie()
}

func (u *updater) updateDestinationsPlanFiles(step model.Step, files map[string]model.File) {
	u.updateDestinationsFiles(step, git.PlanBranch, files)
}

func (u *updater) updateDestinationsApplyFiles(step model.Step, files map[string]model.File) {
	u.updateDestinationsFiles(step, git.ApplyBranch, files)
}

func (u *updater) updateDestinationsFiles(step model.Step, branch string, files map[string]model.File) {
	folder := fmt.Sprintf("steps/%s-%s", u.resources.GetCloudPrefix(), step.Name)
	for name, destination := range u.destinations {
		log.Printf("Step %s updating %s files for destination %s\n", step.Name, branch, name)
		err := destination.UpdateFiles(branch, folder, files)
		if err != nil {
			slog.Warn(common.PrefixWarning(fmt.Sprintf("Step %s failed to update %s files for destination %s: %s",
				step.Name, branch, name, err)))
			return
		}
	}
}

func (u *updater) applyRelease(firstRun bool, executePipelines bool, step model.Step, stepState *model.StateStep, index int, providers map[string]model.Set[string], wg *model.SafeCounter, errChan chan<- error, files map[string]model.File) error {
	if !executePipelines && !firstRun {
		log.Printf("Skipping step %s because all applied module versions are newer or older than current releases\n", step.Name)
		return nil
	}
	u.updateDestinationsPlanFiles(step, files)
	if !firstRun {
		if !u.hasChanged(step, providers) {
			log.Printf("Skipping step %s\n", step.Name)
			return u.putAppliedStateFile(stepState, step, model.ApplyStatusSkipped, index)
		}
		return u.executePipeline(firstRun, step, stepState, index, files)
	}
	if !u.pipelineFlags.AllowParallel || !u.appliedVersionMatchesRelease(step, *stepState, index) {
		return u.executePipeline(firstRun, step, stepState, index, files)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := u.executePipeline(firstRun, step, stepState, index, files)
		if err != nil {
			slog.Error(common.PrefixError(err))
			errChan <- err
		}
	}()
	return nil
}

func (u *updater) hasChanged(step model.Step, providers map[string]model.Set[string]) bool {
	changed := u.getChangedProviders(providers)
	if len(changed) > 0 {
		log.Printf("Step %s providers have changed: %s\n", step.Name, strings.Join(changed, ", "))
		return true
	}
	changed = u.getChangedModules(step)
	if len(changed) > 0 {
		log.Printf("Step %s modules have changed: %s\n", step.Name, strings.Join(changed, ", "))
		return true
	}
	changed = u.getChangedStepModules(step)
	if len(changed) > 0 {
		log.Printf("Step %s module inputs have changed: %s\n", step.Name, strings.Join(changed, ", "))
		return true
	}
	changed = u.getChangedStepFiles(step)
	if len(changed) > 0 {
		log.Printf("Step %s files have changed: %s\n", step.Name, strings.Join(changed, ", "))
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
			if !bytes.Equal(previousChecksum, currentChecksum) {
				slog.Debug(fmt.Sprintf("Provider %s has changed, previous %s, current %s", provider,
					string(previousChecksum), string(currentChecksum)))
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
			slog.Debug(fmt.Sprintf("Module %s source is missing checksums", module.Name))
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
			slog.Debug(fmt.Sprintf("Module %s not found in previous checksums", module.Name))
			changed = append(changed, module.Name)
			continue
		}
		currentChecksum, ok := moduleSource.CurrentChecksums[moduleKey]
		if !ok {
			slog.Debug(fmt.Sprintf("Module %s not found in current checksums", module.Name))
			changed = append(changed, module.Name)
			continue
		}
		if !bytes.Equal(previousChecksum, currentChecksum) {
			slog.Debug(fmt.Sprintf("Module %s has changed, previous %s, current %s", module.Name,
				string(previousChecksum), string(currentChecksum)))
			changed = append(changed, module.Name)
		}
	}
	return changed
}

func (u *updater) getChangedStepModules(step model.Step) []string {
	changed := make([]string, 0)
	if step.Modules == nil {
		return changed
	}
	previousChecksums, exists := u.stepChecksums.PreviousChecksums[step.Name]
	if !exists {
		slog.Debug(fmt.Sprintf("Step %s is missing previous checksums", step.Name))
		for _, module := range step.Modules {
			changed = append(changed, module.Name)
		}
		return changed
	}
	currentChecksums, exists := u.stepChecksums.CurrentChecksums[step.Name]
	if !exists {
		slog.Debug(fmt.Sprintf("Step %s is missing current checksums", step.Name))
		for _, module := range step.Modules {
			changed = append(changed, module.Name)
		}
		return changed
	}
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			changed = append(changed, module.Name)
		}
		previous := previousChecksums.ModuleChecksums[module.Name]
		current := currentChecksums.ModuleChecksums[module.Name]
		if !bytes.Equal(previous, current) {
			changed = append(changed, module.Name)
			slog.Debug(fmt.Sprintf("Module %s inputs have changed, previous %s, current %s", module.Name,
				string(previous), string(current)))

		}
	}
	return changed
}

func (u *updater) getChangedStepFiles(step model.Step) []string {
	changed := make([]string, 0)
	if step.Files == nil {
		return changed
	}
	previousChecksums, exists := u.stepChecksums.PreviousChecksums[step.Name]
	if !exists {
		slog.Debug(fmt.Sprintf("Step %s is missing previous checksums", step.Name))
		for _, file := range step.Files {
			changed = append(changed, file.Name)
		}
		return changed
	}
	currentChecksums, exists := u.stepChecksums.CurrentChecksums[step.Name]
	if !exists {
		slog.Debug(fmt.Sprintf("Step %s is missing current checksums", step.Name))
		for _, file := range step.Files {
			changed = append(changed, file.Name)
		}
		return changed
	}
	for name, file := range currentChecksums.FileChecksums {
		previous := previousChecksums.FileChecksums[name]
		if !bytes.Equal(previous, file) {
			changed = append(changed, name)
			slog.Debug(fmt.Sprintf("File %s has changed, previous %s, current %s", name, string(previous),
				string(file)))
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

func (u *updater) executePipeline(firstRun bool, step model.Step, stepState *model.StateStep, index int, files map[string]model.File) error {
	log.Printf("Applying release for step %s\n", step.Name)
	autoApprove := getAutoApprove(*stepState)
	var err error
	if u.pipelineFlags.Type == string(common.PipelineTypeLocal) {
		err = u.localPipeline.executeLocalPipeline(step, autoApprove, u.getStepAuthSources(step), u.getManualApproval(step))
	} else if firstRun {
		err = u.createExecuteStepPipelines(step, autoApprove, index)
	} else {
		err = u.executeStepPipelines(step, autoApprove, index)
	}
	if err != nil {
		u.postCallback(model.ApplyStatusFailure, *stepState)
		return err
	}
	log.Printf("release applied successfully for step %s\n", step.Name)
	err = u.putAppliedStateFile(stepState, step, model.ApplyStatusSuccess, index)
	if err == nil {
		u.updateDestinationsApplyFiles(step, files)
	}
	return err
}

func (u *updater) getStepState(step model.Step) (*model.StateStep, error) {
	stepState := GetStepState(u.state, step.Name)
	if stepState == nil {
		return nil, fmt.Errorf("failed to get state for step %s", step.Name)
	}
	return stepState, nil
}

func (u *updater) updateCertFiles(stepName string) error {
	folder := fmt.Sprintf("steps/%s-%s", u.resources.GetCloudPrefix(), stepName)
	if len(u.config.Certs) == 0 {
		return removeFolder(u.resources.GetBucket(), fmt.Sprintf("%s/%s", folder, certsFolder))
	}
	allFiles := model.NewSet[string]()
	for _, file := range u.config.Certs {
		filePath := fmt.Sprintf("%s/%s", folder, file.Name)
		err := u.resources.GetBucket().PutFile(filePath, file.Content)
		if err != nil {
			return err
		}
		allFiles.Add(filePath)
	}
	bucketFiles, err := u.resources.GetBucket().ListFolderFiles(fmt.Sprintf("%s/%s", folder, certsFolder))
	if err != nil {
		return fmt.Errorf("failed to list folder allFiles: %s", err)
	}
	for _, bucketFile := range bucketFiles {
		if allFiles.Contains(bucketFile) {
			continue
		}
		err = u.resources.GetBucket().DeleteFile(bucketFile)
		if err != nil {
			return fmt.Errorf("failed to delete file %s: %s", bucketFile, err)
		}
	}
	return nil
}

func (u *updater) updateStepChecksums(step model.Step, files map[string]model.File) {
	sums, exists := u.stepChecksums.CurrentChecksums[step.Name]
	if exists {
		u.stepChecksums.PreviousChecksums[step.Name] = sums
	}
	moduleChecksums := make(map[string][]byte)
	for _, module := range step.Modules {
		moduleChecksums[module.Name] = module.InputsChecksum
	}
	fileChecksums := make(map[string][]byte)
	for _, file := range step.Files {
		fileChecksums[file.Name] = file.Checksum
	}
	for _, file := range files {
		if file.Checksum == nil {
			continue
		}
		fileChecksums[file.Name] = file.Checksum
	}
	stepChecksum := model.StepChecksums{
		ModuleChecksums: moduleChecksums,
		FileChecksums:   fileChecksums,
	}
	u.stepChecksums.CurrentChecksums[step.Name] = stepChecksum
}

func (u *updater) updateStepFiles(step model.Step, moduleVersions map[string]model.ModuleVersion, index int) (bool, map[string]model.Set[string], map[string]model.File, error) {
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

func (u *updater) createExecuteStepPipelines(step model.Step, autoApprove bool, index int) error {
	bucket := u.resources.GetBucket()
	repoMetadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return err
	}

	stepName := fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name)

	vpcConfig := u.getVpcConfig(step)
	imageVersion, imageSource := u.getBaseImage(step, index)
	sources := u.getStepAuthSources(step)
	err = u.resources.GetBuilder().CreateProject(stepName, repoMetadata.URL, stepName, step, imageVersion, imageSource,
		vpcConfig, sources)
	if err != nil {
		return fmt.Errorf("failed to create CodeBuild project: %w", err)
	}
	return u.createExecutePipelines(stepName, stepName, step, autoApprove, bucket, sources)
}

func (u *updater) getVpcConfig(step model.Step) *model.VpcConfig {
	if u.pipelineFlags.Type == string(common.PipelineTypeLocal) {
		return nil
	}
	if !*step.Vpc.Attach {
		return nil
	}
	return &model.VpcConfig{
		VpcId:            &step.Vpc.Id,
		Subnets:          util.ToList(step.Vpc.SubnetIds),
		SecurityGroupIds: util.ToList(step.Vpc.SecurityGroupIds),
	}
}

func (u *updater) createExecutePipelines(projectName string, stepName string, step model.Step, autoApprove bool, bucket model.Bucket, authSources map[string]model.SourceAuth) error {
	executionId, err := u.resources.GetPipeline().CreatePipeline(projectName, stepName, step, bucket, authSources)
	if err != nil {
		return fmt.Errorf("failed to create pipeline %s: %w", projectName, err)
	}
	err = u.resources.GetPipeline().WaitPipelineExecution(projectName, projectName, executionId, autoApprove, step, u.getManualApproval(step))
	if err != nil {
		return fmt.Errorf("failed to wait for pipeline %s execution: %w", projectName, err)
	}
	return nil
}

func (u *updater) getManualApproval(step model.Step) model.ManualApprove {
	if step.Approve != "" {
		return ""
	}
	if step.RunApprove == "" && step.UpdateApprove == "" {
		return ""
	}
	if u.cmd == common.RunCommand {
		if step.RunApprove == "" {
			return model.ManualApproveChanges
		}
		return step.RunApprove
	} else {
		if step.UpdateApprove == "" {
			return model.ManualApproveRemoves
		}
		return step.UpdateApprove
	}
}

func (u *updater) getStepAuthSources(step model.Step) map[string]model.SourceAuth {
	authSources := make(map[string]model.SourceAuth)
	for _, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		moduleSource := u.getModuleSource(module.Source)
		if moduleSource.Auth.Username == "" {
			continue
		}
		authSources[moduleSource.URL] = moduleSource.Auth
	}
	return authSources
}

func (u *updater) executeStepPipelines(step model.Step, autoApprove bool, index int) error {
	stepName := fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name)
	vpcConfig := u.getVpcConfig(step)
	imageVersion, imageSource := u.getBaseImage(step, index)
	bucket := u.resources.GetBucket()
	repoMetadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return err
	}
	sources := u.getStepAuthSources(step)
	err = u.resources.GetBuilder().UpdateProject(stepName, repoMetadata.URL, stepName, step, imageVersion, imageSource,
		vpcConfig, sources)
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
	return u.resources.GetPipeline().WaitPipelineExecution(stepName, stepName, executionId, autoApprove, step, u.getManualApproval(step))
}

func getAutoApprove(state model.StateStep) bool {
	for _, module := range state.Modules {
		if !module.AutoApprove {
			return false
		}
	}
	return true
}

func getSourceReleases(steps []model.Step, source *model.Source, state *model.State) (*version.Version, []*version.Version, error) {
	sourceClient := source.Storage.(*git.SourceClient)
	oldestVersion, err := getOldestVersion(steps, source, state)
	if err != nil {
		return nil, nil, err
	}
	if oldestVersion == StableVersion || oldestVersion == source.StableVersion.Original() {
		latestRelease := source.StableVersion
		log.Printf("Latest release for %s is %s\n", source.URL, latestRelease.Original())
		return latestRelease, []*version.Version{latestRelease}, nil
	}
	oldestRelease, err := sourceClient.GetRelease(getFormattedVersionString(oldestVersion))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get oldest release %s: %w", oldestVersion, err)
	}
	log.Printf("Oldest module version for %s is %s\n", source.URL, oldestRelease.Original())

	newestVersion, err := getNewestVersion(steps, source)
	if err != nil {
		return nil, nil, err
	}
	var newestRelease *version.Version
	if newestVersion != StableVersion {
		newestRelease, err = sourceClient.GetRelease(getFormattedVersionString(newestVersion))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get newest release %s: %w", oldestVersion, err)
		}
		log.Printf("Newest module version for %s is %s\n", source.URL, newestRelease.Original())
	}

	releases, err := sourceClient.GetReleases(oldestRelease, newestRelease)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get newer releases: %w", err)
	}
	return releases[len(releases)-1], releases, nil
}

func getOldestVersion(steps []model.Step, source *model.Source, state *model.State) (string, error) {
	oldestVersion := source.Version.Original()
	var err error
	for _, step := range steps {
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
	} else if oldestVersion == StableVersion {
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

func getNewestVersion(steps []model.Step, source *model.Source) (string, error) {
	newestVersion := ""
	var err error
	for _, step := range steps {
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

func (u *updater) updateTerraformFiles(step model.Step, moduleVersions map[string]model.ModuleVersion, index int) (bool, map[string]model.Set[string], map[string]model.File, error) {
	files := make(map[string]model.File)
	mainPath, mainFile, err := u.createBackendConf(fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name), u.resources.GetBucket())
	if err != nil {
		return false, nil, nil, err
	}
	files[mainPath] = model.File{Content: mainFile}
	changed, mainPath, mainBytes, err := u.createTerraformMain(step, moduleVersions)
	if err != nil {
		return false, nil, nil, err
	}
	files[mainPath] = model.File{Content: mainBytes}
	err = u.updateIncludedStepFiles(step, ReservedTFFiles, model.ToSet([]string{terraformCache, certsFolder}), files)
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
	modifiedProvider, delayedKeyTypes, err := u.replaceStringValues(step, string(provider), index, make(paramCache))
	if err != nil {
		return false, nil, nil, fmt.Errorf("failed to replace provider values: %s", err)
	}
	providerChecksum := util.CalculateHash([]byte(modifiedProvider))
	modifiedProvider, err = u.replaceDelayedStringValues(step, modifiedProvider, index, make(paramCache), delayedKeyTypes)
	if err != nil {
		return false, nil, nil, fmt.Errorf("failed to replace delayed provider values: %s", err)
	}
	providerFile := fmt.Sprintf("steps/%s-%s/provider.tf", u.resources.GetCloudPrefix(), step.Name)
	files[providerFile] = model.File{Content: []byte(modifiedProvider), Checksum: providerChecksum}
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

func (u *updater) updateArgoCDFiles(step model.Step, moduleVersions map[string]model.ModuleVersion) (bool, map[string]model.File, error) {
	executePipeline := false
	files := make(map[string]model.File)
	for _, module := range step.Modules {
		moduleVersion, found := moduleVersions[module.Name]
		if !found {
			return false, nil, fmt.Errorf("module %s version not found", module.Name)
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
		files[filePath] = model.File{Content: file}
	}
	err := u.updateIncludedStepFiles(step, ReservedAppsFiles, model.ToSet([]string{certsFolder}), files)
	return executePipeline, files, err
}

func getModuleInputBytes(inputs map[string]interface{}) ([]byte, error) {
	if len(inputs) == 0 {
		return []byte{}, nil
	}
	inputBytes, err := yaml.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal inputs: %s", err)
	}
	return inputBytes, nil
}

func (u *updater) createBackendConf(path string, bucket model.Bucket) (string, []byte, error) {
	key := fmt.Sprintf("%s/terraform.tfstate", path)
	backendConfig := u.resources.GetBackendConfigVars(key)
	confBytes, err := util.CreateKeyValuePairs(backendConfig, "", "")
	if err != nil {
		return "", nil, fmt.Errorf("failed to convert backend config values: %w", err)
	}
	filePath := fmt.Sprintf("steps/%s/backend.conf", path)
	return filePath, confBytes, bucket.PutFile(filePath, confBytes)
}

func (u *updater) putStateFileOrDie() error {
	err := u.putStateFile()
	if err != nil {
		state, _ := yaml.Marshal(u.state)
		if state != nil {
			log.Println(string(state))
			log.Println("Update the state file manually to avoid reapplying steps")
		}
		return fmt.Errorf("failed to put state file: %v", err)
	}
	return nil
}

func (u *updater) putStateFile() error {
	stateBytes, err := yaml.Marshal(u.state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	return u.resources.GetBucket().PutFile(stateFile, stateBytes)
}

func (u *updater) putAppliedStateFile(stepState *model.StateStep, step model.Step, status model.ApplyStatus, index int) error {
	u.stateLock.Lock()
	defer u.stateLock.Unlock()

	stepState.AppliedAt = time.Now().UTC()
	for _, module := range stepState.Modules {
		module.AppliedVersion = &module.Version
	}
	if u.callback == nil {
		return u.putStateFile()
	}
	modifiedStep, err := u.replaceStepMetadataValues(step, index)
	if err == nil {
		u.postCallbackWithStep(status, *stepState, &modifiedStep)
	} else {
		slog.Error(common.PrefixError(fmt.Errorf("error replacing step %s metadata values: %v", step.Name, err)))
	}
	return u.putStateFile()
}

func (u *updater) postCallback(status model.ApplyStatus, stepState model.StateStep) {
	u.postCallbackWithStep(status, stepState, nil)
}

func (u *updater) postCallbackWithStep(status model.ApplyStatus, stepState model.StateStep, step *model.Step) {
	if u.callback == nil {
		return
	}
	log.Printf("Posting step %s status '%s' to callback", stepState.Name, status)
	err := u.callback.PostStepState(status, stepState, step)
	if err != nil {
		slog.Error(common.PrefixError(fmt.Errorf("error posting step %s status '%s' to callback: %v",
			stepState.Name, status, err)))
	}
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
	appBytes, err := argocd.GetApplicationFile(moduleSource.Storage, module, moduleSource.URL, moduleVersion, values,
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
	u.postCallback(model.ApplyStatusStarting, *stepState)
	return moduleVersions, nil
}

func (u *updater) getModuleVersion(module model.Module, stepState *model.StateStep, index int, approve model.Approve) (string, bool, error) {
	moduleVersion := module.Version
	moduleState, err := getModuleState(stepState, module)
	if err != nil {
		return "", false, err
	}
	moduleState.AutoApprove = getStepAutoApprove(approve)
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

func getStepAutoApprove(approve model.Approve) bool {
	if approve == model.ApproveNever || approve == model.ApproveForce {
		return true
	}
	if approve == "" || approve == model.ApproveAlways || approve == model.ApproveReject {
		return false
	}
	return true
}

func getModuleAutoApprove(moduleVersion *version.Version, releaseTag *version.Version, approve model.Approve) bool {
	if approve == model.ApproveNever || approve == model.ApproveForce {
		return true
	}
	if approve == "" || approve == model.ApproveAlways || approve == model.ApproveReject {
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

func (u *updater) getModuleSource(moduleSource string) *model.Source {
	sourceUrl := u.moduleSources[moduleSource]
	return u.sources[sourceUrl]
}

func (u *updater) updatePipelines(projectName string, step model.Step, bucket string) error {
	stepName := fmt.Sprintf("%s-%s", u.resources.GetCloudPrefix(), step.Name)
	sources := u.getStepAuthSources(step)
	err := u.resources.GetPipeline().UpdatePipeline(projectName, stepName, step, bucket, sources)
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

func (u *updater) updateChecksums(index int) error {
	for url, source := range u.sources {
		if index != 1 && len(source.Releases)-1 < index {
			continue
		}
		release := source.ForcedVersion
		if release == "" {
			if index >= len(source.Releases) {
				release = source.Releases[len(source.Releases)-1].Original()
			} else {
				release = source.Releases[index].Original()
			}
		}
		checksums, err := source.Storage.CalculateChecksums(release)
		if err != nil {
			return fmt.Errorf("failed to get checksums for %s: %s", url, err)
		}
		source.CurrentChecksums = checksums
		u.sources[url] = source
	}
	return nil
}

func (u *updater) updateIncludedStepFiles(step model.Step, reservedFiles, excludedFolders model.Set[string], includedFiles map[string]model.File) error {
	files := model.Set[string]{}
	folder := fmt.Sprintf("steps/%s-%s", u.resources.GetCloudPrefix(), step.Name)
	for _, file := range step.Files {
		target := fmt.Sprintf("%s/%s", folder, file.Name)
		err := u.resources.GetBucket().PutFile(target, file.Content)
		if err != nil {
			return err
		}
		files.Add(target)
		includedFiles[target] = model.File{Content: file.Content}
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

func (u *updater) processModules(step model.Step, moduleVersions map[string]model.ModuleVersion) (model.Step, error) {
	for i, module := range step.Modules {
		if util.IsClientModule(module) {
			continue
		}
		moduleVersion, found := moduleVersions[module.Name]
		if !found {
			return step, fmt.Errorf("module %s version not found", module.Name)
		}
		source := u.getModuleSource(module.Source)
		moduleSource := module.Source
		if step.Type == model.StepTypeArgoCD {
			moduleSource = fmt.Sprintf("k8s/%s", module.Source)
		}
		inputs, err := u.getModuleInputs(module, moduleSource, source, moduleVersion.Version)
		if err != nil {
			return step, err
		}
		step.Modules[i].Inputs = inputs
		metadata, err := u.getModuleMetadata(module, moduleSource, source, moduleVersion.Version)
		if err != nil {
			return step, err
		}
		step.Modules[i].Metadata = metadata
	}
	return step, nil
}

func (u *updater) getModuleInputs(module model.Module, moduleSource string, source *model.Source, moduleVersion string) (map[string]interface{}, error) {
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
	return replaceModuleValues(module, inputs)
}

func (u *updater) getModuleDefaultInputs(filePath string, moduleSource *model.Source, moduleVersion string) (map[string]interface{}, error) {
	defaultInputsRaw, err := moduleSource.Storage.GetFile(filePath, moduleVersion)
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

func (u *updater) getModuleMetadata(module model.Module, moduleSource string, source *model.Source, moduleVersion string) (map[string]string, error) {
	if u.callback == nil {
		return nil, nil
	}
	filePath := fmt.Sprintf("modules/%s/agent.yaml", moduleSource)
	metadataRaw, err := source.Storage.GetFile(filePath, moduleVersion)
	if err != nil {
		var fileError model.FileNotFoundError
		if errors.As(err, &fileError) {
			slog.Debug(fmt.Sprintf("Module %s agent file not found", module.Name))
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get module file %s: %v", filePath, err)
	}
	agentFile, err := model.UnmarshalAgentYaml(metadataRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal module %s agent: %v", module.Name, err)
	}
	switch v := agentFile.(type) {
	case model.V1Agent:
		return v.Metadata, nil
	default:
		return nil, fmt.Errorf("unsupported agent file version: %T", v)
	}
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
