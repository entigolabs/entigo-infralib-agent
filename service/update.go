package service

import (
	"context"
	"errors"
	"fmt"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	stateFile     = "state.yaml"
	checksumsFile = "checksums.sha256"

	replaceRegex        = `{{(.*?)}}`
	parameterIndexRegex = `(\w+)(\[(\d+)(-(\d+))?])?`
	ssmPrefix           = "/entigo-infralib"
)

type Updater interface {
	ProcessSteps()
}

type updater struct {
	config        model.Config
	patchConfig   model.Config
	provider      model.CloudProvider
	resources     model.Resources
	terraform     terraform.Terraform
	github        github.Github
	state         *model.State
	moduleSources map[string]string
	sources       map[string]*model.Source
	allowParallel bool
}

func NewUpdater(ctx context.Context, flags *common.Flags) Updater {
	provider := GetCloudProvider(ctx, flags)
	resources := provider.SetupResources()
	config := GetConfig(flags.Config, resources.GetBucket())
	state := getLatestState(resources.GetBucket())
	ValidateConfig(config, state)
	githubClient := github.NewGithub(ctx)
	moduleSources, sources, err := getSources(githubClient, config, state)
	if err != nil {
		common.Logger.Fatalf("Failed to get module sources: %s", err)
	}
	return &updater{
		config:        config,
		patchConfig:   config,
		provider:      provider,
		resources:     resources,
		terraform:     terraform.NewTerraform(githubClient),
		github:        githubClient,
		state:         state,
		moduleSources: moduleSources,
		sources:       sources,
		allowParallel: flags.AllowParallel,
	}
}

func getLatestState(codeCommit model.Bucket) *model.State {
	file, err := codeCommit.GetFile(stateFile)
	if err != nil {
		common.Logger.Fatalf("Failed to get state file: %v", err)
	}
	if file == nil {
		return &model.State{}
	}
	var state model.State
	err = yaml.Unmarshal(file, &state)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal state file: %v", err)
	}
	return &state
}

func getSources(githubClient github.Github, config model.Config, state *model.State) (map[string]string, map[string]*model.Source, error) {
	moduleSources, sources, err := getModuleSources(config, githubClient)
	if err != nil {
		return nil, nil, err
	}
	for _, cSource := range config.Sources {
		source := sources[cSource.URL]
		upperVersion := source.StableVersion
		if cSource.Version != "" && cSource.Version != StableVersion {
			upperVersion, err = version.NewVersion(cSource.Version)
			if err != nil {
				common.Logger.Fatalf("Failed to parse version %s: %s", cSource.Version, err)
			}
		}
		source.Version = upperVersion
		if source.Modules == nil || len(source.Modules) == 0 {
			common.Logger.Printf("No modules found for Source %s\n", cSource.URL)
		}
		newestVersion, releases, err := getSourceReleases(githubClient, config, source, state)
		if err != nil {
			common.Logger.Fatalf("Failed to get releases: %v", err)
		}
		source.NewestVersion = newestVersion
		source.Releases = releases
	}
	return moduleSources, sources, nil
}

func getModuleSources(config model.Config, githubClient github.Github) (map[string]string, map[string]*model.Source, error) {
	moduleSources := make(map[string]string)
	sources := make(map[string]*model.Source)

	sourceIncludes := make(map[string]model.Set[string])
	sourceExcludes := make(map[string]model.Set[string])
	for _, source := range config.Sources {
		if source.Include == nil || len(source.Include) == 0 {
			sourceIncludes[source.URL] = model.NewSet[string]()
		} else {
			sourceIncludes[source.URL] = model.ToSet(source.Include)
		}
		if source.Exclude == nil || len(source.Exclude) == 0 {
			sourceExcludes[source.URL] = model.NewSet[string]()
		} else {
			sourceExcludes[source.URL] = model.ToSet(source.Exclude)
		}
		latestVersion := getLatestRelease(githubClient, source.URL)
		sources[source.URL] = &model.Source{
			URL:           source.URL,
			StableVersion: latestVersion,
			Modules:       model.NewSet[string](),
		}
	}
	GetChecksums(sources, githubClient, nil)
	for _, source := range sources {
		if source.CurrentChecksums == nil {
			return nil, nil, fmt.Errorf("failed to get checksums for source %s", source.URL)
		}
	}

	for _, step := range config.Steps {
		for _, module := range step.Modules {
			if moduleSources[module.Source] != "" {
				continue
			}
			found := false
			for _, source := range config.Sources {
				if sourceIncludes[source.URL].Contains(module.Name) {
					moduleSources[module.Source] = source.URL
					sources[source.URL].Modules.Add(module.Name)
					found = true
					break
				}
			}
			if found {
				continue
			}
			for _, source := range sources {
				if sourceExcludes[source.URL].Contains(module.Name) {
					continue
				}
				moduleSource := module.Source
				if step.Type == model.StepTypeArgoCD {
					moduleSource = fmt.Sprintf("k8s/%s", moduleSource)
				}
				moduleKey := fmt.Sprintf("modules/%s", moduleSource)
				if source.CurrentChecksums[moduleKey] != "" {
					moduleSources[module.Source] = source.URL
					sources[source.URL].Modules.Add(module.Name)
					found = true
					break
				}
			}
			if !found {
				return nil, nil, fmt.Errorf("module %s in step %s is not included in any Source", module.Name, step.Name)
			}
		}
	}
	return moduleSources, sources, nil
}

func (u *updater) ProcessSteps() {
	u.updateAgentCodeBuild()
	mostReleases := u.getMostReleases()
	firstRunDone := make(map[string]bool)
	for index := 0; index < mostReleases; index++ {
		updateState(u.config, u.state)
		GetChecksums(u.sources, u.github, &index)
		wg := new(model.SafeCounter)
		errChan := make(chan error, 1)
		failed := false
		retrySteps := make([]model.Step, 0)
		for _, step := range u.config.Steps {
			retry, err := u.processStep(index, step, firstRunDone, wg, errChan, u.allowParallel)
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
		if _, ok := <-errChan; ok || failed {
			common.Logger.Fatalf("One or more steps failed to apply")
		}
		u.retrySteps(index, retrySteps, firstRunDone, wg, errChan)
		for i, source := range u.sources {
			u.sources[i].PreviousChecksums = source.CurrentChecksums
		}
		err := u.putStateFile()
		if err != nil {
			state, err := yaml.Marshal(u.state)
			if err == nil {
				common.Logger.Println(state)
				common.Logger.Println("Update the state file manually to avoid reapplying steps")
			}
			common.Logger.Fatalf("Failed to put state file: %v", err)
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

func (u *updater) processStep(index int, step model.Step, firstRunDone map[string]bool, wg *model.SafeCounter, errChan chan<- error, allowParallel bool) (bool, error) {
	step, err := u.replaceConfigStepValues(step, index)
	if err != nil {
		var parameterError *model.ParameterNotFoundError
		if wg.HasCount() && errors.As(err, &parameterError) {
			common.PrintWarning(err.Error())
			common.Logger.Printf("Step %s will be retried if others succeed\n", step.Name)
			return true, nil
		}
		return false, err
	}
	stepState, err := u.getStepState(step)
	if err != nil {
		return false, err
	}
	var executePipelines bool
	var providers map[string]model.Set[string]
	if !firstRunDone[step.Name] {
		executePipelines, err = u.createStepFiles(step, stepState, index)
	} else {
		executePipelines, providers, err = u.updateStepFiles(step, stepState, index)
	}
	if err != nil {
		return false, err
	}
	err = u.applyRelease(!firstRunDone[step.Name], executePipelines, step, stepState, index, providers, wg, errChan, allowParallel)
	if err != nil {
		return false, err
	}
	firstRunDone[step.Name] = true
	return false, nil
}

func (u *updater) retrySteps(index int, retrySteps []model.Step, firstRunDone map[string]bool, wg *model.SafeCounter, errChan chan<- error) {
	for _, step := range retrySteps {
		common.Logger.Printf("Retrying step %s\n", step.Name)
		_, err := u.processStep(index, step, firstRunDone, wg, errChan, false)
		if err != nil {
			common.PrintError(err)
			common.Logger.Fatalf("Failed to apply step %s", step.Name)
		}
	}
}

func (u *updater) applyRelease(firstRun bool, executePipelines bool, step model.Step, stepState *model.StateStep, index int, providers map[string]model.Set[string], wg *model.SafeCounter, errChan chan<- error, allowParallel bool) error {
	if !executePipelines {
		return nil
	}
	err := u.putStateFile()
	if err != nil {
		return err
	}
	if !firstRun {
		if !u.hasChanged(step, providers) {
			fmt.Printf("Skipping step %s\n", step.Name)
			u.updateStepState(stepState)
			return nil
		}
		return u.executePipeline(firstRun, step, stepState, index)
	}
	if !allowParallel {
		return u.executePipeline(firstRun, step, stepState, index)
	}
	parallelExecution, err := u.appliedVersionMatchesRelease(step, stepState, index)
	if err != nil {
		return err
	}
	if !parallelExecution {
		return u.executePipeline(firstRun, step, stepState, index)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := u.executePipeline(firstRun, step, stepState, index)
		if err != nil {
			common.PrintError(err)
			errChan <- err
		}
	}()
	return nil
}

func (u *updater) hasChanged(step model.Step, providers map[string]model.Set[string]) bool {
	changed := u.getChangedProviders(providers)
	if len(changed) > 0 {
		common.Logger.Printf("Step %s providers have changed: %s\n", step.Name,
			strings.Join(changed, ", "))
		return true
	}
	changed = u.getChangedModules(step)
	if len(changed) > 0 {
		common.Logger.Printf("Step %s modules have changed: %s\n", step.Name,
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
				changed = append(changed, provider)
				continue
			}
			currentChecksum, ok := providerSource.CurrentChecksums[providerKey]
			if !ok {
				changed = append(changed, provider)
				continue
			}
			if previousChecksum != currentChecksum {
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
		source := module.Source
		moduleSource := u.getModuleSource(module.Source)
		if moduleSource.PreviousChecksums == nil || moduleSource.CurrentChecksums == nil {
			changed = append(changed, module.Name)
			continue
		}
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

func (u *updater) appliedVersionMatchesRelease(step model.Step, stepState *model.StateStep, index int) (bool, error) {
	for _, moduleState := range stepState.Modules {
		if moduleState.Type != nil && *moduleState.Type == model.ModuleTypeCustom {
			continue
		}
		if moduleState.AppliedVersion == nil {
			return false, nil
		}
		appliedVersion, err := version.NewVersion(*moduleState.AppliedVersion)
		if err != nil {
			return false, err
		}
		module := getModule(moduleState.Name, step.Modules)
		moduleSource := u.getModuleSource(module.Source)
		release := moduleSource.Releases[util.MinInt(index, len(moduleSource.Releases)-1)]
		if !appliedVersion.Equal(release) {
			return false, nil
		}
	}
	return true, nil
}

func (u *updater) executePipeline(firstRun bool, step model.Step, stepState *model.StateStep, index int) error {
	common.Logger.Printf("applying %d. release for step %s\n", index+1, step.Name)
	var err error
	if firstRun {
		err = u.createExecuteStepPipelines(step, stepState, index)
	} else {
		err = u.executeStepPipelines(step, stepState, index)
	}
	if err != nil {
		return err
	}
	common.Logger.Printf("%d. release applied successfully for step %s\n", index+1, step.Name)
	return u.putAppliedStateFile(stepState)
}

func (u *updater) updateAgentCodeBuild() {
	agent := NewAgent(u.resources)
	err := agent.UpdateProjectImage(u.config.AgentVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to update agent codebuild: %s", err)
	}
}

func (u *updater) getStepState(step model.Step) (*model.StateStep, error) {
	stepState := GetStepState(u.state, step.Name)
	if stepState == nil {
		return nil, fmt.Errorf("failed to get state for step %s", step.Name)
	}
	return stepState, nil
}

func (u *updater) createStepFiles(step model.Step, stepState *model.StateStep, index int) (bool, error) {
	switch step.Type {
	case model.StepTypeTerraform:
		execute, _, err := u.createTerraformFiles(step, stepState, index)
		return execute, err
	case model.StepTypeArgoCD:
		return u.createArgoCDFiles(step, stepState, index)
	}
	return true, nil
}

func (u *updater) updateStepFiles(step model.Step, stepState *model.StateStep, index int) (bool, map[string]model.Set[string], error) {
	switch step.Type {
	case model.StepTypeTerraform:
		return u.updateTerraformFiles(step, stepState, index)
	case model.StepTypeArgoCD:
		execute, err := u.updateArgoCDFiles(step, stepState, index)
		return execute, nil, err
	}
	return true, nil, nil
}

func (u *updater) createExecuteStepPipelines(step model.Step, stepState *model.StateStep, index int) error {
	bucket := u.resources.GetBucket()
	repoMetadata, err := bucket.GetRepoMetadata()
	if err != nil {
		return err
	}

	stepName := fmt.Sprintf("%s-%s", u.config.Prefix, step.Name)

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
	if step.VpcId == "" {
		return nil
	}
	return &model.VpcConfig{
		VpcId:            &step.VpcId,
		Subnets:          util.ToList(step.VpcSubnetIds),
		SecurityGroupIds: util.ToList(step.VpcSecurityGroupIds),
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

func (u *updater) executeStepPipelines(step model.Step, stepState *model.StateStep, index int) error {
	stepName := fmt.Sprintf("%s-%s", u.config.Prefix, step.Name)
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

func getAutoApprove(state *model.StateStep) bool {
	for _, module := range state.Modules {
		if !module.AutoApprove {
			return false
		}
	}
	return true
}

func getSourceReleases(githubClient github.Github, config model.Config, source *model.Source, state *model.State) (*version.Version, []*version.Version, error) {
	oldestVersion, err := getOldestVersion(config, source, state)
	if err != nil {
		return nil, nil, err
	}
	if oldestVersion == StableVersion || oldestVersion == source.StableVersion.Original() {
		latestRelease := source.StableVersion
		common.Logger.Printf("Latest release for %s is %s\n", source.URL, latestRelease.Original())
		return latestRelease, []*version.Version{latestRelease}, nil
	}
	oldestRelease, err := githubClient.GetReleaseByTag(source.URL, getFormattedVersionString(oldestVersion))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get oldest release %s: %w", oldestVersion, err)
	}
	common.Logger.Printf("Oldest module version for %s is %s\n", source.URL, oldestRelease.Tag)

	newestVersion, err := getNewestVersion(config, source)
	if err != nil {
		return nil, nil, err
	}
	var newestRelease *github.Release
	if newestVersion != StableVersion {
		newestRelease, err = githubClient.GetReleaseByTag(source.URL, getFormattedVersionString(newestVersion))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get newest release %s: %w", oldestVersion, err)
		}
		common.Logger.Printf("Newest module version for %s is %s\n", source.URL, oldestRelease.Tag)
	}

	releases, err := githubClient.GetReleases(source.URL, *oldestRelease, newestRelease)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get newer releases: %w", err)
	}
	return releases[len(releases)-1], releases, nil
}

func getLatestRelease(githubClient github.Github, repoURL string) *version.Version {
	latestRelease, err := githubClient.GetLatestReleaseTag(repoURL)
	if err != nil {
		common.Logger.Fatalf(err.Error())
	}
	latestSemver, err := version.NewVersion(latestRelease.Tag)
	if err != nil {
		common.Logger.Fatalf("Failed to parse latest release version %s: %s", latestRelease.Tag, err)
	}
	return latestSemver
}

func getOldestVersion(config model.Config, source *model.Source, state *model.State) (string, error) {
	oldestVersion := source.Version.Original()
	var err error
	for _, step := range config.Steps {
		for _, module := range step.Modules {
			if util.IsClientModule(module) || !source.Modules.Contains(module.Name) {
				continue
			}
			oldestVersion, err = getOlderVersion(oldestVersion, module.Version)
			if err != nil {
				return "", err
			}
		}
	}
	for _, step := range state.Steps {
		for _, module := range step.Modules {
			if (module.Type != nil && *module.Type == model.ModuleTypeCustom) || !source.Modules.Contains(module.Name) {
				continue
			}
			moduleVersion := ""
			if module.Version != "" {
				moduleVersion = module.Version
			}
			oldestVersion, err = getOlderVersion(oldestVersion, moduleVersion)
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
			if util.IsClientModule(module) || !source.Modules.Contains(module.Name) {
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

func (u *updater) createTerraformFiles(step model.Step, stepState *model.StateStep, index int) (bool, map[string]model.Set[string], error) {
	err := u.createBackendConf(fmt.Sprintf("%s-%s", u.config.Prefix, step.Name), u.resources.GetBucket())
	if err != nil {
		return false, nil, err
	}
	return u.updateTerraformFiles(step, stepState, index)
}

func (u *updater) updateTerraformFiles(step model.Step, stepState *model.StateStep, index int) (bool, map[string]model.Set[string], error) {
	changed, moduleVersions, err := u.createTerraformMain(step, stepState, index)
	if err != nil {
		return false, nil, err
	}
	hasFiles := step.Files != nil && len(step.Files) > 0
	if hasFiles {
		err = u.updateIncludedStepFiles(step)
		if err != nil {
			return false, nil, err
		}
	}
	if !changed {
		return hasFiles, nil, nil
	}
	if len(moduleVersions) == 0 {
		return false, nil, errors.New("no module versions found")
	}
	provider, providers, err := u.terraform.GetTerraformProvider(step, moduleVersions, u.resources.GetProviderType(),
		u.sources, u.moduleSources)
	if err != nil {
		return false, nil, fmt.Errorf("failed to create terraform provider: %s", err)
	}
	err = u.resources.GetBucket().PutFile(fmt.Sprintf("steps/%s-%s/provider.tf", u.config.Prefix, step.Name), provider)
	return true, providers, err
}

func (u *updater) createArgoCDFiles(step model.Step, stepState *model.StateStep, index int) (bool, error) {
	executePipeline := false
	activeModules := model.NewSet[string]()
	for _, module := range step.Modules {
		moduleVersion, changed, err := u.getModuleVersion(module, stepState, index, step.Approve)
		if err != nil {
			return false, err
		}
		if changed {
			executePipeline = true
		}
		inputBytes, err := getModuleInputBytes(module)
		if err != nil {
			return false, err
		}
		err = u.createArgoCDApp(module, step, moduleVersion, inputBytes)
		if err != nil {
			return false, err
		}
		activeModules.Add(module.Name)
	}
	err := u.removeUnusedArgoCDApps(step, activeModules)
	return executePipeline, err
}

func (u *updater) updateArgoCDFiles(step model.Step, stepState *model.StateStep, index int) (bool, error) {
	executePipeline := false
	for _, module := range step.Modules {
		moduleVersion, changed, err := u.getModuleVersion(module, stepState, index, step.Approve)
		if err != nil {
			return false, err
		}
		if !changed {
			continue
		}
		inputBytes, err := getModuleInputBytes(module)
		if err != nil {
			return false, err
		}
		executePipeline = true
		err = u.createArgoCDApp(module, step, moduleVersion, inputBytes)
		if err != nil {
			return false, err
		}
	}
	return executePipeline, nil
}

func getModuleInputBytes(module model.Module) ([]byte, error) {
	inputs := module.Inputs
	if len(inputs) == 0 {
		return []byte{}, nil
	}
	bytes, err := yaml.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal inputs: %s", err)
	}
	return bytes, nil
}

func (u *updater) createBackendConf(path string, codeCommit model.Bucket) error {
	key := fmt.Sprintf("%s/terraform.tfstate", path)
	backendConfig := u.resources.GetBackendConfigVars(key)
	bytes, err := util.CreateKeyValuePairs(backendConfig, "", "")
	if err != nil {
		return fmt.Errorf("failed to convert backend config values: %w", err)
	}
	return codeCommit.PutFile(fmt.Sprintf("steps/%s/backend.conf", path), bytes)
}

func (u *updater) putStateFile() error {
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	return u.resources.GetBucket().PutFile(stateFile, bytes)
}

func (u *updater) putAppliedStateFile(stepState *model.StateStep) error {
	u.updateStepState(stepState)
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	return u.resources.GetBucket().PutFile(stateFile, bytes)
}

func (u *updater) updateStepState(stepState *model.StateStep) {
	stepState.AppliedAt = time.Now()
	for _, module := range stepState.Modules {
		module.AppliedVersion = &module.Version
	}
}

func (u *updater) createTerraformMain(step model.Step, stepState *model.StateStep, index int) (bool, map[string]string, error) {
	file := hclwrite.NewEmptyFile()
	body := file.Body()
	changed := false
	moduleVersions := make(map[string]string)
	for _, module := range step.Modules {
		moduleVersion, moduleChanged, err := u.getModuleVersion(module, stepState, index, step.Approve)
		if err != nil {
			return false, nil, err
		}
		if moduleChanged {
			changed = true
		}
		newModule := body.AppendNewBlock("module", []string{module.Name})
		moduleBody := newModule.Body()
		if util.IsClientModule(module) {
			moduleBody.SetAttributeValue("source",
				cty.StringVal(fmt.Sprintf("%s?ref=%s", module.Source, moduleVersion)))
		} else {
			moduleSource := u.getModuleSource(module.Source)
			moduleBody.SetAttributeValue("source",
				cty.StringVal(fmt.Sprintf("git::%s.git//modules/%s?ref=%s", moduleSource.URL, module.Source, moduleVersion)))
			moduleVersions[module.Name] = moduleVersion
		}
		moduleBody.SetAttributeValue("prefix", cty.StringVal(fmt.Sprintf("%s-%s-%s", u.config.Prefix, step.Name, module.Name)))
		terraform.AddInputs(module.Inputs, moduleBody)
	}
	if changed {
		err := u.resources.GetBucket().PutFile(fmt.Sprintf("steps/%s-%s/main.tf", u.config.Prefix, step.Name), file.Bytes())
		if err != nil {
			return false, nil, err
		}
	}
	return changed, moduleVersions, nil
}

func (u *updater) createArgoCDApp(module model.Module, step model.Step, moduleVersion string, values []byte) error {
	moduleSource := u.getModuleSource(module.Source)
	appBytes, err := argocd.GetApplicationFile(u.github, module, moduleSource.URL, step.RepoUrl, moduleVersion, values,
		u.resources.GetProviderType())
	if err != nil {
		return fmt.Errorf("failed to create application file: %w", err)
	}
	return u.resources.GetBucket().PutFile(fmt.Sprintf("steps/%s-%s/%s.yaml", u.config.Prefix, step.Name, module.Name),
		appBytes)
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
	var moduleSemver *version.Version
	if moduleVersion == "" {
		moduleSemver = moduleSource.Version
	} else if moduleVersion == StableVersion {
		moduleSemver = moduleSource.NewestVersion
	} else {
		moduleSemver, err = version.NewVersion(moduleVersion)
		if err != nil {
			return "", false, fmt.Errorf("failed to parse module version %s: %s", moduleVersion, err)
		}
	}
	if index > len(moduleSource.Releases)-1 {
		return getFormattedVersion(moduleSemver), false, nil
	}
	releaseTag := moduleSource.Releases[index]
	moduleState.AutoApprove = true
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

func (u *updater) removeUnusedArgoCDApps(step model.Step, modules model.Set[string]) error {
	folder := fmt.Sprintf("steps/%s-%s", u.config.Prefix, step.Name)
	files, err := u.resources.GetBucket().ListFolderFiles(folder)
	if err != nil {
		return err
	}
	for _, file := range files {
		if !strings.HasSuffix(file, ".yaml") {
			continue
		}
		if modules.Contains(strings.TrimSuffix(file, ".yaml")) {
			continue
		}
		err = u.resources.GetBucket().DeleteFile(file)
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *updater) replaceConfigStepValues(step model.Step, index int) (model.Step, error) {
	stepYaml, err := yaml.Marshal(step)
	if err != nil {
		return step, fmt.Errorf("failed to convert step %s to yaml, error: %v", step.Name, err)
	}
	modifiedStepYaml, err := u.replaceStringValues(step, string(stepYaml), index)
	if err != nil {
		common.Logger.Printf("Failed to replace tags in step %s", step.Name)
		return step, err
	}
	var modifiedStep model.Step
	err = yaml.Unmarshal([]byte(modifiedStepYaml), &modifiedStep)
	if err != nil {
		return step, fmt.Errorf("failed to unmarshal modified step %s yaml, error: %v", step.Name, err)
	}
	if step.Files == nil {
		return modifiedStep, nil
	}
	for _, file := range step.Files {
		if !strings.HasSuffix(file.Name, ".tf") && !strings.HasSuffix(file.Name, ".yaml") &&
			!strings.HasSuffix(file.Name, ".hcl") {
			continue
		}
		newContent, err := u.replaceStringValues(step, string(file.Content), index)
		if err != nil {
			return modifiedStep, fmt.Errorf("failed to replace tags in file %s: %v", file.Name, err)
		}
		modifiedStep.Files = append(modifiedStep.Files, model.File{
			Name:    strings.TrimPrefix(file.Name, fmt.Sprintf(IncludeFormat, step.Name)+"/"),
			Content: []byte(newContent),
		})
	}
	return modifiedStep, nil
}

func (u *updater) replaceStringValues(step model.Step, content string, index int) (string, error) {
	re := regexp.MustCompile(replaceRegex)
	matches := re.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, nil
	}
	for _, match := range matches {
		if len(match) != 2 {
			return "", fmt.Errorf("failed to parse replace tag match %s", match[0])
		}
		replaceTag := match[0]
		replaceKey := strings.TrimLeft(strings.Trim(match[1], " "), ".")
		replaceType := strings.ToLower(replaceKey[:strings.Index(replaceKey, ".")])
		switch replaceType {
		case string(model.ReplaceTypeOutput):
			fallthrough
		case string(model.ReplaceTypeGCSM):
			fallthrough
		case string(model.ReplaceTypeSSM):
			parameter, err := u.getSSMParameter(u.config.Prefix, step, replaceKey)
			if err != nil {
				return "", err
			}
			content = strings.Replace(content, replaceTag, parameter, 1)
		case string(model.ReplaceTypeOutputCustom):
			fallthrough
		case string(model.ReplaceTypeGCSMCustom):
			fallthrough
		case string(model.ReplaceTypeSSMCustom):
			parameter, err := u.getSSMCustomParameter(replaceKey)
			if err != nil {
				return "", err
			}
			content = strings.Replace(content, replaceTag, parameter, 1)
		case string(model.ReplaceTypeConfig):
			configKey := replaceKey[strings.Index(replaceKey, ".")+1:]
			configValue, err := util.GetValueFromStruct(configKey, u.config)
			if err != nil {
				return "", fmt.Errorf("failed to get config value %s: %s", configKey, err)
			}
			content = strings.Replace(content, replaceTag, configValue, 1)
		case string(model.ReplaceTypeAgent):
			key := replaceKey[strings.Index(replaceKey, ".")+1:]
			agentValue, err := u.getReplacementAgentValue(key, index)
			if err != nil {
				return "", fmt.Errorf("failed to get agent value %s: %s", key, err)
			}
			content = strings.Replace(content, replaceTag, agentValue, 1)
		default:
			return "", fmt.Errorf("unknown replace type in tag %s", match[0])
		}
	}
	return content, nil
}

func (u *updater) getReplacementAgentValue(key string, index int) (string, error) {
	parts := strings.Split(key, ".")
	if parts[0] == string(model.AgentReplaceTypeVersion) {
		_, referencedStep := findStep(parts[1], u.config.Steps)
		if referencedStep == nil {
			return "", fmt.Errorf("failed to find step %s", parts[1])
		}
		stepState := GetStepState(u.state, referencedStep.Name)
		referencedModule := getModule(parts[2], referencedStep.Modules)
		if referencedModule == nil {
			return "", fmt.Errorf("failed to find module %s in step %s", parts[2], parts[1])
		}
		moduleVersion, _, err := u.getModuleVersion(*referencedModule, stepState, index, model.ApproveNever)
		return moduleVersion, err
	} else if parts[0] == string(model.AgentReplaceTypeAccountId) {
		return u.resources.(aws.Resources).AccountId, nil
	}
	return "", fmt.Errorf("unknown agent replace type %s", parts[0])
}

func (u *updater) getSSMParameter(prefix string, step model.Step, replaceKey string) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 4 {
		return "", fmt.Errorf("failed to parse ssm parameter key %s for step %s, got %d split parts instead of 4",
			replaceKey, step.Name, len(parts))
	}
	re := regexp.MustCompile(parameterIndexRegex)
	match := re.FindStringSubmatch(parts[3])
	parameterName := fmt.Sprintf("%s/%s-%s-%s/%s", ssmPrefix, prefix, parts[1], parts[2], match[1])
	return u.getSSMParameterValue(match, replaceKey, parameterName)
}

func (u *updater) getSSMCustomParameter(replaceKey string) (string, error) {
	parts := strings.Split(replaceKey, ".")
	if len(parts) != 2 {
		return "", fmt.Errorf("failed to parse ssm custom parameter key %s, got %d split parts instead of 2", replaceKey, len(parts))
	}
	re := regexp.MustCompile(parameterIndexRegex)
	match := re.FindStringSubmatch(parts[1])
	return u.getSSMParameterValue(match, replaceKey, parts[1])
}

func (u *updater) getSSMParameterValue(match []string, replaceKey string, parameterName string) (string, error) {
	parameter, err := u.resources.GetSSM().GetParameter(parameterName)
	if err != nil {
		return "", err
	}
	if match[2] == "" {
		return *parameter.Value, nil
	}
	if parameter.Type != string(ssmTypes.ParameterTypeStringList) && parameter.Type != "" {
		return "", fmt.Errorf("parameter index was given, but ssm parameter %s is not a string list", match[1])
	}
	return getSSMParameterValueFromList(match, parameter, replaceKey, match[1])
}

func (u *updater) updatePipelines(projectName string, step model.Step, bucket string) error {
	stepName := fmt.Sprintf("%s-%s", u.config.Prefix, step.Name)
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
			source := u.getModuleSource(module.Source)
			if !strings.Contains(source.URL, EntigoSource) {
				continue
			}
			release = getFormattedVersion(source.Releases[util.MinInt(index, len(source.Releases)-1)])
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

func GetChecksums(sources map[string]*model.Source, githubClient github.Github, index *int) {
	for url, source := range sources {
		if index != nil && (len(source.Releases)-1 < *index) {
			continue
		}
		release := source.StableVersion.Original()
		if index != nil {
			release = source.Releases[*index].Original()
		}
		content, err := githubClient.GetRawFileContent(source.URL, checksumsFile, release)
		if err != nil {
			var fileError github.FileNotFoundError
			if errors.As(err, &fileError) {
				return
			}
			common.Logger.Fatalf("Failed to get checksums for release %s: %v", release, err)
		}
		checksums := make(map[string]string)
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, " ", 2)
			if len(parts) != 2 {
				fmt.Printf("Invalid line: %s\n", line)
				continue
			}
			checksums[strings.TrimRight(parts[0], ":")] = parts[1]
		}
		sources[url].CurrentChecksums = checksums
	}
}

func (u *updater) updateIncludedStepFiles(step model.Step) error {
	files := model.Set[string]{}
	folder := fmt.Sprintf("steps/%s-%s", u.config.Prefix, step.Name)
	for _, file := range step.Files {
		target := fmt.Sprintf("%s/%s", folder, file.Name)
		err := u.resources.GetBucket().PutFile(target, file.Content)
		if err != nil {
			return err
		}
		files.Add(target)
	}
	folderFiles, err := u.resources.GetBucket().ListFolderFiles(folder)
	if err != nil {
		return err
	}
	for _, file := range folderFiles {
		if ReservedFiles.Contains(strings.TrimPrefix(file, folder+"/")) {
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

func getSSMParameterValueFromList(match []string, parameter *model.Parameter, replaceKey string, parameterName string) (string, error) {
	parameters := strings.Split(*parameter.Value, ",")
	start, err := strconv.Atoi(match[3])
	if err != nil {
		return "", fmt.Errorf("failed to parse start index %s of parameter %s: %s", match[3], replaceKey, err)
	}
	if start+1 > len(parameters) {
		return "", fmt.Errorf("start index %d of parameter %s is out of range", start, parameterName)
	}
	if match[5] == "" {
		return strings.Trim(parameters[start], "\""), nil
	}
	end, err := strconv.Atoi(match[5])
	if err != nil {
		return "", fmt.Errorf("failed to parse end index %s of parameter %s: %s", match[5], replaceKey, err)
	}
	if end+1 > len(parameters) {
		return "", fmt.Errorf("end index %d of parameter %s is out of range", end, parameterName)
	}
	return strings.Join(parameters[start:end+1], ","), nil
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
