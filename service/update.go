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
	"sync"
	"time"
)

const stateFile = "state.yaml"
const checksumsFile = "checksums.sha256"

const replaceRegex = `{{(.*?)}}`
const parameterIndexRegex = `(\w+)(\[(\d+)(-(\d+))?])?`
const ssmPrefix = "/entigo-infralib"

type Updater interface {
	ProcessSteps()
}

type updater struct {
	config                 model.Config
	patchConfig            model.Config
	provider               model.CloudProvider
	resources              model.Resources
	github                 github.Github
	terraform              terraform.Terraform
	customCC               model.CodeRepo
	state                  *model.State
	stableRelease          *version.Version
	baseConfigReleaseLimit *version.Version
	previousChecksums      map[string]string
	currentChecksums       map[string]string
}

func NewUpdater(flags *common.Flags) Updater {
	provider := GetCloudProvider(context.Background(), flags)
	resources := provider.SetupResources(flags.Branch)
	config := GetConfig(flags.Config, resources.GetCodeRepo())
	githubClient := github.NewGithub(config.Source)
	stableRelease := getLatestRelease(githubClient)
	latestState, err := getLatestState(resources.GetCodeRepo())
	if err != nil {
		common.Logger.Fatalf(fmt.Sprintf("%s", err))
	}
	return &updater{
		config:                 config,
		patchConfig:            config,
		provider:               provider,
		resources:              resources,
		github:                 githubClient,
		terraform:              terraform.NewTerraform(githubClient),
		state:                  latestState,
		stableRelease:          stableRelease,
		baseConfigReleaseLimit: getBaseConfigReleaseLimit(config, stableRelease),
	}
}

func getLatestRelease(githubClient github.Github) *version.Version {
	latestRelease, err := githubClient.GetLatestReleaseTag()
	if err != nil {
		common.Logger.Fatalf("Failed to get latest release: %s", err)
	}
	latestSemver, err := version.NewVersion(latestRelease.Tag)
	if err != nil {
		common.Logger.Fatalf("Failed to parse latest release version %s: %s", latestRelease.Tag, err)
	}
	return latestSemver
}

func getBaseConfigReleaseLimit(config model.Config, stableRelease *version.Version) *version.Version {
	releaseLimit := config.BaseConfig.Version
	if releaseLimit == "" || releaseLimit == StableVersion {
		return stableRelease
	}
	limitSemver, err := version.NewVersion(releaseLimit)
	if err != nil {
		common.Logger.Fatalf("Failed to parse base config release version %s: %s", releaseLimit, err)
	}
	return limitSemver
}

func (u *updater) mergeBaseConfig(release *version.Version) {
	if release.GreaterThan(u.baseConfigReleaseLimit) {
		return
	}
	rawBaseConfig, err := u.github.GetRawFileContent(fmt.Sprintf("profiles/%s.yaml", u.patchConfig.BaseConfig.Profile),
		release.Original())
	if err != nil {
		common.Logger.Fatalf("Failed to get base config: %s", err)
	}
	var baseConfig model.Config
	err = yaml.Unmarshal(rawBaseConfig, &baseConfig)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal base config: %s", err)
	}
	config := MergeConfig(u.patchConfig, baseConfig)
	bytes, err := yaml.Marshal(config)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal config: %s", err)
	}
	err = u.resources.GetCodeRepo().PutFile("merged_config.yaml", bytes)
	if err != nil {
		common.Logger.Fatalf("Failed to put merged config file: %s", err)
	}
	u.config = config
	u.state.BaseConfig.Version = release
}

func getLatestState(codeCommit model.CodeRepo) (*model.State, error) {
	file, err := codeCommit.GetFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get state file: %w", err)
	}
	if file == nil {
		return &model.State{}, err
	}
	var state model.State
	err = yaml.Unmarshal(file, &state)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal state file: %w", err)
	}
	return &state, nil
}

func (u *updater) setupCustomCodeRepo() error {
	if u.customCC != nil {
		return nil
	}
	for _, step := range u.config.Steps {
		if step.Type == model.StepTypeTerraformCustom {
			var err error
			u.customCC, err = u.provider.SetupCustomCodeRepo("main")
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (u *updater) ProcessSteps() {
	u.updateAgentCodeBuild()
	releases, err := u.getReleases()
	if err != nil {
		common.Logger.Fatalf("Failed to get releases: %s", err)
	}
	firstRunDone := make(map[string]bool)
	for _, release := range releases {
		if u.config.BaseConfig.Profile != "" {
			u.mergeBaseConfig(release)
			updateState(u.config, u.state)
			ValidateConfig(u.config, u.state)
		}
		if u.releaseNewerThanConfigVersions(release) {
			break
		}
		err = u.setupCustomCodeRepo()
		if err != nil {
			common.Logger.Fatalf("Failed to setup custom CodeRepo: %s", err)
		}
		u.currentChecksums, err = u.GetChecksums(release)
		if err != nil {
			common.Logger.Fatalf("Failed to get checksums for release %s: %v", release.Original(), err)
		}
		terraformExecuted := false
		wg := new(sync.WaitGroup)
		errChan := make(chan error, 1)
		failed := false
		for _, step := range u.config.Steps {
			step, err = u.replaceConfigStepValues(step, release)
			if err != nil {
				common.PrintError(err)
				failed = true
				break
			}
			stepSemVer, err := u.getStepSemVer(step)
			if err != nil {
				common.PrintError(err)
				failed = true
				break
			}
			stepState, err := u.getStepState(step)
			if err != nil {
				common.PrintError(err)
				failed = true
				break
			}
			var executePipelines bool
			var providers []string
			if !firstRunDone[step.Name] {
				executePipelines, err = u.createStepFiles(step, stepState, release, stepSemVer)
			} else {
				executePipelines, providers, err = u.updateStepFiles(step, stepState, release, stepSemVer, terraformExecuted)
				if executePipelines {
					terraformExecuted = true
				}
			}
			if err != nil {
				common.PrintError(err)
				failed = true
				break
			}
			err = u.applyRelease(!firstRunDone[step.Name], executePipelines, step, stepState, release, providers, wg, errChan)
			if err != nil {
				common.PrintError(err)
				failed = true
				break
			}
			firstRunDone[step.Name] = true
		}
		wg.Wait()
		close(errChan)
		time.Sleep(1 * time.Second)
		if _, ok := <-errChan; ok || failed {
			common.Logger.Fatalf("One or more steps failed to apply")
		}
		u.previousChecksums = u.currentChecksums
		if u.state.BaseConfig.AppliedVersion != u.state.BaseConfig.Version {
			u.state.BaseConfig.AppliedVersion = u.state.BaseConfig.Version
			err = u.putStateFile()
			if err != nil {
				common.Logger.Fatalf("Failed to put state file: %s", err)
			}
		}
	}
}

func (u *updater) releaseNewerThanConfigVersions(release *version.Version) bool {
	newestVersion, err := u.getNewestVersion()
	if err != nil {
		common.Logger.Fatalf("Failed to get newest version: %s", err)
	}
	if newestVersion == StableVersion {
		return false
	}
	newestSemVer, err := version.NewVersion(newestVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to parse newest version %s: %s", newestVersion, err)
	}
	if release.GreaterThan(newestSemVer) {
		common.Logger.Printf("Newest required module version is %s", newestVersion)
		return true
	}
	return false
}

func (u *updater) getStepSemVer(step model.Step) (*version.Version, error) {
	stepVersion := step.Version
	if stepVersion == "" {
		stepVersion = u.config.Version
	}
	if stepVersion == StableVersion {
		return u.stableRelease, nil
	}
	stepSemVer, err := version.NewVersion(stepVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to parse step version %s: %s", stepVersion, err)
	}
	return stepSemVer, nil
}

func (u *updater) applyRelease(firstRun bool, executePipelines bool, step model.Step, stepState *model.StateStep, release *version.Version, providers []string, wg *sync.WaitGroup, errChan chan<- error) error {
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
		return u.executePipeline(firstRun, step, stepState, release)
	}
	parallelExecution, err := appliedVersionMatchesRelease(stepState, release)
	if err != nil {
		return err
	}
	if !parallelExecution {
		return u.executePipeline(firstRun, step, stepState, release)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := u.executePipeline(firstRun, step, stepState, release)
		if err != nil {
			common.PrintError(err)
			errChan <- err
		}
	}()
	return nil
}

func (u *updater) hasChanged(step model.Step, providers []string) bool {
	if u.previousChecksums == nil || u.currentChecksums == nil {
		return true
	}
	if u.hasProfileChanged() {
		return true
	}
	if u.haveProvidersChanged(providers) {
		return true
	}
	return u.haveModulesChanged(step)
}

func (u *updater) hasProfileChanged() bool {
	profile := u.config.BaseConfig.Profile
	if profile == "" {
		return false
	}
	profileKey := fmt.Sprintf("profiles/%s.yaml", profile)
	previousChecksum, ok := u.previousChecksums[profileKey]
	if !ok {
		return true
	}
	currentChecksum, ok := u.currentChecksums[profileKey]
	if !ok {
		return true
	}
	return previousChecksum != currentChecksum
}

func (u *updater) haveProvidersChanged(providers []string) bool {
	if providers == nil {
		return false
	}
	for _, provider := range providers {
		providerKey := fmt.Sprintf("providers/%s.tf", provider)
		previousChecksum, ok := u.previousChecksums[providerKey]
		if !ok {
			return true
		}
		currentChecksum, ok := u.currentChecksums[providerKey]
		if !ok {
			return true
		}
		if previousChecksum != currentChecksum {
			return true
		}
	}
	return false
}

func (u *updater) haveModulesChanged(step model.Step) bool {
	if step.Modules == nil {
		return false
	}
	for _, module := range step.Modules {
		moduleKey := fmt.Sprintf("modules/%s", module.Source)
		previousChecksum, ok := u.previousChecksums[moduleKey]
		if !ok {
			return true
		}
		currentChecksum, ok := u.currentChecksums[moduleKey]
		if !ok {
			return true
		}
		if previousChecksum != currentChecksum {
			return true
		}
	}
	return false
}

func appliedVersionMatchesRelease(stepState *model.StateStep, release *version.Version) (bool, error) {
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
		if !appliedVersion.Equal(release) {
			return false, nil
		}
	}
	return true, nil
}

func (u *updater) executePipeline(firstRun bool, step model.Step, stepState *model.StateStep, release *version.Version) error {
	common.Logger.Printf("applying version %s for step %s\n", release.Original(), step.Name)
	var err error
	if firstRun {
		err = u.createExecuteStepPipelines(step, stepState, release)
	} else {
		err = u.executeStepPipelines(step, stepState, release)
	}
	if err != nil {
		return err
	}
	common.Logger.Printf("release %s applied successfully for step %s\n", release.Original(), step.Name)
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
	stepState := GetStepState(u.state, step)
	if stepState == nil {
		return nil, fmt.Errorf("failed to get state for step %s", step.Name)
	}
	return stepState, nil
}

func (u *updater) createStepFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) (bool, error) {
	switch step.Type {
	case model.StepTypeTerraform:
		execute, _, err := u.createTerraformFiles(step, stepState, releaseTag, stepSemver)
		return execute, err
	case model.StepTypeArgoCD:
		return u.createArgoCDFiles(step, stepState, releaseTag, stepSemver)
	case model.StepTypeTerraformCustom:
		return true, u.createCustomTerraformFiles(step, stepState, stepSemver)
	}
	return true, nil
}

func (u *updater) updateStepFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version, terraformExecuted bool) (bool, []string, error) {
	switch step.Type {
	case model.StepTypeTerraform:
		return u.updateTerraformFiles(step, stepState, releaseTag, stepSemver)
	case model.StepTypeArgoCD:
		execute, err := u.updateArgoCDFiles(step, stepState, releaseTag, stepSemver)
		return execute, nil, err
	case model.StepTypeTerraformCustom:
		return terraformExecuted, nil, nil
	}
	return true, nil, nil
}

func (u *updater) createExecuteStepPipelines(step model.Step, stepState *model.StateStep, release *version.Version) error {
	repoMetadata, err := u.getRepoMetadata(step.Type)
	if err != nil {
		return err
	}

	stepName := fmt.Sprintf("%s-%s", u.config.Prefix, step.Name)
	projectName := fmt.Sprintf("%s-%s", stepName, step.Workspace)

	vpcConfig := getVpcConfig(step)
	imageVersion := u.getBaseImageVersion(step, release)
	err = u.resources.GetBuilder().CreateProject(projectName, repoMetadata.URL, stepName, step, imageVersion, vpcConfig)
	if err != nil {
		return fmt.Errorf("failed to create CodeBuild project: %w", err)
	}
	autoApprove := getAutoApprove(stepState)
	return u.createExecuteTerraformPipelines(projectName, stepName, step, autoApprove, repoMetadata.Name)
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

func (u *updater) getRepoMetadata(stepType model.StepType) (*model.RepositoryMetadata, error) {
	switch stepType {
	case model.StepTypeTerraformCustom:
		return u.customCC.GetRepoMetadata()
	default:
		return u.resources.GetCodeRepo().GetRepoMetadata()
	}
}

func (u *updater) createExecuteTerraformPipelines(projectName string, stepName string, step model.Step, autoApprove bool, repo string) error {
	executionId, err := u.resources.GetPipeline().CreatePipeline(projectName, stepName, step, repo)
	if err != nil {
		return fmt.Errorf("failed to create pipeline %s: %w", projectName, err)
	}
	err = u.resources.GetPipeline().WaitPipelineExecution(projectName, projectName, executionId, autoApprove, step.Type)
	if err != nil {
		return fmt.Errorf("failed to wait for pipeline %s execution: %w", projectName, err)
	}
	return nil
}

func (u *updater) executeStepPipelines(step model.Step, stepState *model.StateStep, release *version.Version) error {
	stepName := fmt.Sprintf("%s-%s", u.config.Prefix, step.Name)
	projectName := fmt.Sprintf("%s-%s-%s", u.config.Prefix, step.Name, step.Workspace)
	vpcConfig := getVpcConfig(step)
	imageVersion := u.getBaseImageVersion(step, release)
	repoMetadata, err := u.getRepoMetadata(step.Type)
	if err != nil {
		return err
	}
	err = u.resources.GetBuilder().UpdateProject(projectName, repoMetadata.URL, step.Name, step, imageVersion, vpcConfig)
	if err != nil {
		return err
	}
	err = u.updatePipelines(projectName, step, repoMetadata.Name)
	if err != nil {
		return err
	}
	executionId, err := u.resources.GetPipeline().StartPipelineExecution(projectName, stepName, step, repoMetadata.Name)
	if err != nil {
		return fmt.Errorf("failed to start pipeline %s execution: %w", projectName, err)
	}
	autoApprove := getAutoApprove(stepState)
	return u.resources.GetPipeline().WaitPipelineExecution(projectName, projectName, executionId, autoApprove, step.Type)
}

func getAutoApprove(state *model.StateStep) bool {
	for _, module := range state.Modules {
		if !module.AutoApprove {
			return false
		}
	}
	return true
}

func (u *updater) getReleases() ([]*version.Version, error) {
	oldestVersion, err := u.getOldestVersion()
	if err != nil {
		return nil, err
	}
	latestRelease := u.stableRelease
	if oldestVersion == StableVersion {
		common.Logger.Printf("Latest release is %s\n", latestRelease.Original())
		return []*version.Version{latestRelease}, nil
	}
	oldestRelease, err := u.github.GetReleaseByTag(getFormattedVersionString(oldestVersion))
	if err != nil {
		return nil, fmt.Errorf("failed to get oldest release %s: %w", oldestVersion, err)
	}
	common.Logger.Printf("Oldest module version is %s\n", oldestRelease.Tag)
	releases, err := u.github.GetNewerReleases(oldestRelease.PublishedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to get newer releases: %w", err)
	}
	return toVersions(releases)
}

func toVersions(releases []github.Release) ([]*version.Version, error) {
	versions := make([]*version.Version, 0)
	for _, release := range releases {
		releaseVersion, err := version.NewVersion(release.Tag)
		if err != nil {
			return nil, fmt.Errorf("failed to parse release version %s: %w", release.Tag, err)
		}
		versions = append(versions, releaseVersion)
	}
	return versions, nil
}

func (u *updater) getOldestVersion() (string, error) {
	oldestVersion := u.config.Version
	if oldestVersion == "" {
		oldestVersion = StableVersion
	}
	var err error
	for _, step := range u.config.Steps {
		oldestVersion, err = getOlderVersion(oldestVersion, step.Version)
		if err != nil {
			return "", err
		}
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				continue
			}
			oldestVersion, err = getOlderVersion(oldestVersion, module.Version)
			if err != nil {
				return "", err
			}
		}
	}
	for _, step := range u.state.Steps {
		for _, module := range step.Modules {
			if module.Type != nil && *module.Type == model.ModuleTypeCustom {
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

func (u *updater) getNewestVersion() (string, error) {
	newestVersion := ""
	configVersion := u.config.Version
	for _, step := range u.config.Steps {
		stepVersion := step.Version
		if stepVersion == "" {
			stepVersion = configVersion
		}
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				continue
			}
			moduleVersion := module.Version
			if moduleVersion == "" {
				moduleVersion = stepVersion
			}
			if moduleVersion == StableVersion || moduleVersion == "" {
				return StableVersion, nil
			}
			if newestVersion == "" {
				newestVersion = moduleVersion
			} else {
				var err error
				newestVersion, err = getNewerVersion(newestVersion, moduleVersion)
				if err != nil {
					return "", err
				}
			}
		}
	}
	if newestVersion == "" {
		return u.config.Version, nil
	}
	return newestVersion, nil
}

func getNewerVersion(newestVersion string, moduleVersion string) (string, error) {
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

func (u *updater) createTerraformFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) (bool, []string, error) {
	err := u.createBackendConf(fmt.Sprintf("%s-%s", u.config.Prefix, step.Name), u.resources.GetCodeRepo())
	if err != nil {
		return false, nil, err
	}
	return u.updateTerraformFiles(step, stepState, releaseTag, stepSemver)
}

func (u *updater) updateTerraformFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) (bool, []string, error) {
	changed, moduleVersions, err := u.createTerraformMain(step, stepState, releaseTag, stepSemver)
	if err != nil {
		return false, nil, err
	}
	if !changed {
		return false, nil, nil
	}
	if len(moduleVersions) == 0 {
		// Add a version for external providers fallback
		moduleVersions["current"] = getFormattedVersion(releaseTag)
	}
	provider, providers, err := u.terraform.GetTerraformProvider(step, moduleVersions, u.resources.GetProviderType())
	if err != nil {
		return false, nil, fmt.Errorf("failed to create terraform provider: %s", err)
	}
	err = u.resources.GetCodeRepo().PutFile(fmt.Sprintf("%s-%s/%s/provider.tf", u.config.Prefix, step.Name, step.Workspace), provider)
	return changed, providers, err
}

func (u *updater) createArgoCDFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) (bool, error) {
	executePipeline := false
	activeModules := model.NewSet[string]()
	for _, module := range step.Modules {
		moduleVersion, changed, err := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve)
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

func (u *updater) updateArgoCDFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) (bool, error) {
	executePipeline := false
	for _, module := range step.Modules {
		moduleVersion, changed, err := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve)
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

func (u *updater) createBackendConf(path string, codeCommit model.CodeRepo) error {
	key := fmt.Sprintf("%s/terraform.tfstate", path)
	backendConfig := u.resources.GetBackendConfigVars(key)
	bytes, err := util.CreateKeyValuePairs(backendConfig, "", "")
	if err != nil {
		return fmt.Errorf("failed to convert backend config values: %w", err)
	}
	return codeCommit.PutFile(fmt.Sprintf("%s/backend.conf", path), bytes)
}

func (u *updater) putStateFile() error {
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	return u.resources.GetCodeRepo().PutFile(stateFile, bytes)
}

func (u *updater) putAppliedStateFile(stepState *model.StateStep) error {
	u.updateStepState(stepState)
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	return u.resources.GetCodeRepo().PutFile(stateFile, bytes)
}

func (u *updater) updateStepState(stepState *model.StateStep) {
	stepState.AppliedAt = time.Now()
	for _, module := range stepState.Modules {
		module.AppliedVersion = &module.Version
	}
}

func (u *updater) createTerraformMain(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) (bool, map[string]string, error) {
	file := hclwrite.NewEmptyFile()
	body := file.Body()
	changed := false
	moduleVersions := make(map[string]string)
	for _, module := range step.Modules {
		moduleVersion, moduleChanged, err := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve)
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
			moduleBody.SetAttributeValue("source",
				cty.StringVal(fmt.Sprintf("git::%s.git//modules/%s?ref=%s", u.config.Source, module.Source, moduleVersion)))
			moduleVersions[module.Name] = moduleVersion
		}
		moduleBody.SetAttributeValue("prefix", cty.StringVal(fmt.Sprintf("%s-%s-%s", u.config.Prefix, step.Name, module.Name)))
		terraform.AddInputs(module.Inputs, moduleBody)
	}
	if changed {
		err := u.resources.GetCodeRepo().PutFile(fmt.Sprintf("%s-%s/%s/main.tf", u.config.Prefix, step.Name, step.Workspace), file.Bytes())
		if err != nil {
			return false, nil, err
		}
	}
	return changed, moduleVersions, nil
}

func (u *updater) createArgoCDApp(module model.Module, step model.Step, moduleVersion string, values []byte) error {
	appBytes, err := argocd.GetApplicationFile(u.github, module, step.RepoUrl, moduleVersion, values,
		u.resources.GetProviderType())
	if err != nil {
		return fmt.Errorf("failed to create application file: %w", err)
	}
	return u.resources.GetCodeRepo().PutFile(fmt.Sprintf("%s-%s/%s/%s.yaml", u.config.Prefix, step.Name,
		step.Workspace, module.Name), appBytes)
}

func (u *updater) getModuleVersion(module model.Module, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version, approve model.Approve) (string, bool, error) {
	moduleVersion := module.Version
	moduleState, err := getModuleState(stepState, module)
	if err != nil {
		return "", false, err
	}
	if util.IsClientModule(module) {
		clientModuleVersion, moduleChanged := getClientModuleVersion(module, moduleState)
		return clientModuleVersion, moduleChanged, nil
	}
	var moduleSemver *version.Version
	if moduleVersion == "" {
		moduleSemver = stepSemver
	} else if moduleVersion == StableVersion {
		moduleSemver = u.stableRelease
	} else {
		moduleSemver, err = version.NewVersion(moduleVersion)
		if err != nil {
			return "", false, fmt.Errorf("failed to parse module version %s: %s", moduleVersion, err)
		}
	}
	moduleState.AutoApprove = true
	if moduleState.Version == "" {
		if moduleSemver.GreaterThan(releaseTag) {
			moduleState.Version = getFormattedVersion(releaseTag)
			return getFormattedVersion(releaseTag), true, nil
		} else {
			moduleState.Version = getFormattedVersion(moduleSemver)
			return getFormattedVersion(moduleSemver), true, nil
		}
	}
	var moduleStateSemver *version.Version
	moduleStateSemver, err = version.NewVersion(moduleState.Version)
	if err != nil {
		return "", false, fmt.Errorf("failed to parse module state version %s: %s", moduleVersion, err)
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

func getClientModuleVersion(module model.Module, moduleState *model.StateModule) (string, bool) {
	moduleState.Version = module.Version
	return module.Version, module.Version != moduleState.Version
}

func (u *updater) createCustomTerraformFiles(step model.Step, stepState *model.StateStep, semver *version.Version) error {
	err := u.createBackendConf(fmt.Sprintf("%s-%s", u.config.Prefix, step.Name), u.customCC)
	if err != nil {
		return err
	}
	workspacePath := fmt.Sprintf("%s-%s/%s", u.config.Prefix, step.Name, step.Workspace)
	for _, module := range step.Modules {
		moduleState, err := getModuleState(stepState, module)
		if err != nil {
			return err
		}
		if step.Approve == model.ApproveNever {
			moduleState.AutoApprove = true
		} else {
			moduleState.AutoApprove = false
		}
	}
	workspaceExists, err := u.customCC.CheckFolderExists(workspacePath)
	if err != nil {
		return err
	}
	if workspaceExists {
		return nil
	}
	providerBytes, err := u.terraform.GetEmptyTerraformProvider(getFormattedVersion(semver), u.resources.GetProviderType())
	if err != nil {
		return fmt.Errorf("failed to create empty terraform provider: %w", err)
	}
	err = u.customCC.PutFile(fmt.Sprintf("%s/provider.tf", workspacePath), providerBytes)
	if err != nil {
		return err
	}
	mainBytes := terraform.GetMockTerraformMain()
	return u.customCC.PutFile(fmt.Sprintf("%s/main.tf", workspacePath), mainBytes)
}

func (u *updater) removeUnusedArgoCDApps(step model.Step, modules model.Set[string]) error {
	files, err := u.resources.GetCodeRepo().ListFolderFiles(fmt.Sprintf("%s-%s/%s", u.config.Prefix, step.Name, step.Workspace))
	if err != nil {
		return err
	}
	for _, file := range files {
		file = strings.TrimSuffix(file, ".yaml")
		if modules.Contains(file) {
			continue
		}
		err = u.resources.GetCodeRepo().DeleteFile(fmt.Sprintf("%s-%s/%s/%s.yaml", u.config.Prefix, step.Name,
			step.Workspace, file))
		if err != nil {
			return err
		}
		err = u.resources.GetCodeRepo().DeleteFile(fmt.Sprintf("%s-%s/%s/%s/values.yaml", u.config.Prefix, step.Name,
			step.Workspace, file))
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *updater) replaceConfigStepValues(step model.Step, release *version.Version) (model.Step, error) {
	stepYaml, err := yaml.Marshal(step)
	if err != nil {
		return step, fmt.Errorf("failed to convert step %s to yaml, error: %s", step.Name, err)
	}
	modifiedStepYaml, err := u.replaceStepYamlValues(step, string(stepYaml), release)
	if err != nil {
		return step, fmt.Errorf("failed to replace tags in step %s, error: %s", step.Name, err)
	}
	var modifiedStep model.Step
	err = yaml.Unmarshal([]byte(modifiedStepYaml), &modifiedStep)
	if err != nil {
		return step, fmt.Errorf("failed to unmarshal modified step %s yaml, error: %s", step.Name, err)
	}
	return modifiedStep, nil
}

func (u *updater) replaceStepYamlValues(step model.Step, configYaml string, release *version.Version) (string, error) {
	re := regexp.MustCompile(replaceRegex)
	matches := re.FindAllStringSubmatch(configYaml, -1)
	if len(matches) == 0 {
		return configYaml, nil
	}
	for _, match := range matches {
		if len(match) != 2 {
			return "", fmt.Errorf("failed to parse replace tag match %s", match[0])
		}
		replaceTag := match[0]
		replaceKey := strings.TrimLeft(strings.Trim(match[1], " "), ".")
		replaceType := strings.ToLower(replaceKey[:strings.Index(replaceKey, ".")])
		switch replaceType {
		case string(model.ReplaceTypeGCSM):
			fallthrough
		case string(model.ReplaceTypeSSM):
			parameter, err := u.getSSMParameter(u.config.Prefix, step, replaceKey)
			if err != nil {
				return "", err
			}
			configYaml = strings.Replace(configYaml, replaceTag, parameter, 1)
		case string(model.ReplaceTypeGCSMCustom):
			fallthrough
		case string(model.ReplaceTypeSSMCustom):
			parameter, err := u.getSSMCustomParameter(replaceKey)
			if err != nil {
				return "", err
			}
			configYaml = strings.Replace(configYaml, replaceTag, parameter, 1)
		case string(model.ReplaceTypeConfig):
			configKey := replaceKey[strings.Index(replaceKey, ".")+1:]
			configValue, err := util.GetValueFromStruct(configKey, u.config)
			if err != nil {
				return "", fmt.Errorf("failed to get config value %s: %s", configKey, err)
			}
			configYaml = strings.Replace(configYaml, replaceTag, configValue, 1)
		case string(model.ReplaceTypeAgent):
			key := replaceKey[strings.Index(replaceKey, ".")+1:]
			agentValue, err := u.getReplacementAgentValue(step, key, release)
			if err != nil {
				return "", fmt.Errorf("failed to get agent value %s: %s", key, err)
			}
			configYaml = strings.Replace(configYaml, replaceTag, agentValue, 1)
		default:
			return "", fmt.Errorf("unknown replace type in tag %s", match[0])
		}
	}
	return configYaml, nil
}

func (u *updater) getReplacementAgentValue(step model.Step, key string, release *version.Version) (string, error) {
	parts := strings.Split(key, ".")
	if parts[0] == string(model.AgentReplaceTypeVersion) {
		_, referencedStep := findStep(model.Step{Name: parts[1], Workspace: step.Workspace}, u.config.Steps)
		if referencedStep == nil {
			return "", fmt.Errorf("failed to find step %s", parts[1])
		}
		stepState := GetStepState(u.state, *referencedStep)
		stepVersion, err := u.getStepSemVer(*referencedStep)
		if err != nil {
			return "", fmt.Errorf("failed to get step %s semver: %s", parts[1], err)
		}
		referencedModule := getModule(parts[2], referencedStep.Modules)
		if referencedModule == nil {
			return "", fmt.Errorf("failed to find module %s in step %s", parts[2], parts[1])
		}
		moduleVersion, _, err := u.getModuleVersion(*referencedModule, stepState, release, stepVersion, model.ApproveNever)
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
	parameterName := fmt.Sprintf("%s/%s-%s-%s-%s/%s", ssmPrefix, prefix, parts[1], parts[2], step.Workspace, match[1])
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
		return "", fmt.Errorf("failed to get ssm parameter %s: %s", parameterName, err)
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

func (u *updater) getBaseImageVersion(step model.Step, release *version.Version) string {
	if step.BaseImageVersion != "" {
		return step.BaseImageVersion
	}
	if u.config.BaseImageVersion != "" {
		return u.config.BaseImageVersion
	}
	return getFormattedVersion(release)
}

func (u *updater) GetChecksums(release *version.Version) (map[string]string, error) {
	content, err := u.github.GetRawFileContent(checksumsFile, release.Original())
	if err != nil {
		var fileError github.FileNotFoundError
		if errors.As(err, &fileError) {
			return nil, nil
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
			fmt.Printf("Invalid line: %s\n", line)
			continue
		}
		checksums[strings.TrimRight(parts[0], ":")] = parts[1]
	}
	return checksums, nil
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
