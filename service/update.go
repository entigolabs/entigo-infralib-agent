package service

import (
	"fmt"
	ccTypes "github.com/aws/aws-sdk-go-v2/service/codecommit/types"
	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/github"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
	"strings"
	"sync"
	"time"
)

const stateFile = "state.yaml"

type Updater interface {
	ProcessSteps()
}

type updater struct {
	config        model.Config
	resources     AWSResources
	github        github.Github
	terraform     terraform.Terraform
	customCC      CodeCommit
	state         *model.State
	stableRelease *version.Version
}

func NewUpdater(flags *common.Flags) Updater {
	awsService := NewAWS(flags.AWSPrefix)
	resources := awsService.SetupAWSResources(flags.Branch)
	config := GetConfig(flags.Config, resources.CodeCommit)
	githubClient := github.NewGithub(config.Source)
	if config.BaseConfig.Profile != "" {
		config = mergeBaseConfig(config, resources.CodeCommit, githubClient)
	}
	state := getLatestState(resources.CodeCommit)
	if state == nil {
		state = createState(config)
	} else {
		updateSteps(config, state)
	}
	ValidateConfig(config, state)
	customCodeCommit := setupCustomCodeCommit(config, awsService, flags.Branch)
	return &updater{
		config:    config,
		resources: resources,
		github:    githubClient,
		terraform: terraform.NewTerraform(githubClient),
		customCC:  customCodeCommit,
		state:     state,
	}
}

func mergeBaseConfig(config model.Config, codeCommit CodeCommit, githubClient github.Github) model.Config {
	release := config.BaseConfig.Version
	if release == "" {
		release = config.Version
	}
	if release == StableVersion {
		latestRelease, err := githubClient.GetLatestReleaseTag()
		if err != nil {
			common.Logger.Fatalf("Failed to get latest release: %s", err)
		}
		release = latestRelease.Tag
	}
	rawBaseConfig, err := githubClient.GetRawFileContent(fmt.Sprintf("profiles/%s.conf", config.BaseConfig.Profile), release)
	if err != nil {
		common.Logger.Fatalf("Failed to get base config: %s", err)
	}
	var baseConfig model.Config
	err = yaml.Unmarshal(rawBaseConfig, &baseConfig)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal base config: %s", err)
	}
	config = MergeConfig(config, baseConfig)
	bytes, err := yaml.Marshal(config)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal config: %s", err)
	}
	codeCommit.PutFile("merged_config.yaml", bytes)
	return config
}

func getLatestState(codeCommit CodeCommit) *model.State {
	file := codeCommit.GetFile(stateFile)
	if file == nil {
		return nil
	}
	var state model.State
	err := yaml.Unmarshal(file, &state)
	if err != nil {
		common.Logger.Fatalf("Failed to unmarshal state file: %s", err)
	}
	return &state
}

func setupCustomCodeCommit(config model.Config, service AWS, branch string) CodeCommit {
	for _, step := range config.Steps {
		if step.Type == model.StepTypeTerraformCustom {
			return service.SetupCustomCodeCommit(branch)
		}
	}
	return nil
}

func (u *updater) ProcessSteps() {
	u.updateAgentCodeBuild()
	releases := u.getReleases()
	firstRunDone := make(map[string]bool)
	for _, release := range releases {
		terraformExecuted := false
		wg := new(sync.WaitGroup)
		for _, step := range u.config.Steps {
			stepSemVer := u.getStepSemVer(step)
			stepState := u.getStepState(step)
			var executePipelines bool
			if !firstRunDone[step.Name] {
				executePipelines = u.createStepFiles(step, stepState, release, stepSemVer)
			} else {
				executePipelines = u.updateStepFiles(step, stepState, release, stepSemVer, terraformExecuted)
				if step.Type == model.StepTypeTerraform && executePipelines {
					terraformExecuted = true
				}
			}
			u.applyRelease(!firstRunDone[step.Name], executePipelines, step, stepState, release, wg)
			firstRunDone[step.Name] = true
		}
		wg.Wait()
	}
}

func (u *updater) getStepSemVer(step model.Step) *version.Version {
	stepVersion := step.Version
	if stepVersion == "" {
		stepVersion = u.config.Version
	}
	if stepVersion == StableVersion {
		return u.stableRelease
	}
	stepSemVer, err := version.NewVersion(stepVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to parse step version %s: %s", stepVersion, err)
	}
	return stepSemVer
}

func (u *updater) applyRelease(firstRun bool, executePipelines bool, step model.Step, stepState *model.StateStep, release *version.Version, wg *sync.WaitGroup) {
	if !executePipelines {
		return
	}
	u.putPlannedStateFile()
	if firstRun && appliedVersionMatchesRelease(stepState, release) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u.executePipeline(firstRun, step, stepState, release)
		}()
	} else {
		u.executePipeline(firstRun, step, stepState, release)
	}
}

func appliedVersionMatchesRelease(stepState *model.StateStep, release *version.Version) bool {
	for _, moduleState := range stepState.Modules {
		if moduleState.AppliedVersion == nil || !moduleState.AppliedVersion.Equal(release) {
			return false
		}
	}
	return true
}

func (u *updater) executePipeline(firstRun bool, step model.Step, stepState *model.StateStep, release *version.Version) {
	common.Logger.Printf("applying version %s for step %s\n", release.Original(), step.Name)
	if firstRun {
		u.createExecuteStepPipelines(step, stepState)
	} else {
		u.executeStepPipelines(step, stepState)
	}
	u.putAppliedStateFile(stepState)
	common.Logger.Printf("release %s applied successfully for step %s\n", release.Original(), step.Name)
}

func (u *updater) updateAgentCodeBuild() {
	agent := NewAgent(u.resources)
	err := agent.UpdateProjectImage(u.config.AgentVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to update agent codebuild: %s", err)
	}
}

func (u *updater) getStepState(step model.Step) *model.StateStep {
	stepState := GetStepState(u.state, step)
	if stepState == nil {
		common.Logger.Fatalf("Failed to get state for step %s", step.Name)
	}
	return stepState
}

func (u *updater) createStepFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	switch step.Type {
	case model.StepTypeTerraform:
		return u.createTerraformFiles(step, stepState, releaseTag, stepSemver)
	case model.StepTypeArgoCD:
		return u.createArgoCDFiles(step, stepState, releaseTag, stepSemver)
	case model.StepTypeTerraformCustom:
		u.createCustomTerraformFiles(step, stepState, stepSemver)
	}
	return true
}

func (u *updater) updateStepFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version, terraformExecuted bool) bool {
	switch step.Type {
	case model.StepTypeTerraform:
		return u.updateTerraformFiles(step, stepState, releaseTag, stepSemver)
	case model.StepTypeArgoCD:
		return u.updateArgoCDFiles(step, stepState, releaseTag, stepSemver)
	case model.StepTypeTerraformCustom:
		return terraformExecuted
	}
	return true
}

func (u *updater) createExecuteStepPipelines(step model.Step, stepState *model.StateStep) {
	repoMetadata := u.getRepoMetadata(step.Type)

	stepName := fmt.Sprintf("%s-%s", u.config.Prefix, step.Name)
	projectName := fmt.Sprintf("%s-%s", stepName, step.Workspace)

	vpcConfig := u.resources.SSM.GetVpcConfig(u.config.Prefix, step.VpcPrefix, step.Workspace)
	err := u.resources.CodeBuild.CreateProject(projectName, *repoMetadata.CloneUrlHttp, stepName, step.Workspace, vpcConfig)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodeBuild project: %s", err)
	}
	autoApprove := getAutoApprove(stepState)

	switch step.Type {
	case model.StepTypeTerraform:
		u.createExecuteTerraformPipelines(projectName, stepName, step, autoApprove, "")
	case model.StepTypeArgoCD:
		//u.createExecuteArgoCDPipelines(projectName, stepName, step, autoApprove) Temporarily disabled to save time
	case model.StepTypeTerraformCustom:
		u.createExecuteTerraformPipelines(projectName, stepName, step, autoApprove, *repoMetadata.RepositoryName)
	}
}

func (u *updater) getRepoMetadata(stepType model.StepType) *ccTypes.RepositoryMetadata {
	switch stepType {
	case model.StepTypeTerraformCustom:
		return u.customCC.GetRepoMetadata()
	default:
		return u.resources.CodeCommit.GetRepoMetadata()
	}
}

func (u *updater) createExecuteTerraformPipelines(projectName string, stepName string, step model.Step, autoApprove bool, customRepo string) {
	executionId, err := u.resources.CodePipeline.CreateTerraformPipeline(projectName, projectName, stepName, step.Workspace, customRepo)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodePipeline %s: %s", projectName, err)
	}
	err = u.resources.CodePipeline.CreateTerraformDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace, customRepo)
	if err != nil {
		common.Logger.Fatalf("Failed to create destroy CodePipeline %s: %s", projectName, err)
	}
	err = u.resources.CodePipeline.WaitPipelineExecution(projectName, executionId, autoApprove, 30, step.Type)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline %s execution: %s", projectName, err)
	}
}

func (u *updater) createExecuteArgoCDPipelines(projectName string, stepName string, step model.Step, autoApprove bool) {
	executionId, err := u.resources.CodePipeline.CreateArgoCDPipeline(projectName, projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodePipeline %s: %s", projectName, err)
	}
	err = u.resources.CodePipeline.CreateArgoCDDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create destroy CodePipeline %s: %s", projectName, err)
	}
	err = u.resources.CodePipeline.WaitPipelineExecution(projectName, executionId, autoApprove, 30, step.Type)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline %s execution: %s", projectName, err)
	}
}

func (u *updater) executeStepPipelines(step model.Step, stepState *model.StateStep) {
	if step.Type == model.StepTypeArgoCD {
		return // Temporarily disabled to save time
	}
	projectName := fmt.Sprintf("%s-%s-%s", u.config.Prefix, step.Name, step.Workspace)
	vpcConfig := u.resources.SSM.GetVpcConfig(u.config.Prefix, step.VpcPrefix, step.Workspace)
	if vpcConfig != nil {
		err := u.resources.CodeBuild.UpdateProjectVpc(projectName, vpcConfig)
		if err != nil {
			common.Logger.Fatalf("Failed to update CodeBuild project %s VPC: %s", projectName, err)
		}
	}
	executionId, err := u.resources.CodePipeline.StartPipelineExecution(projectName)
	if err != nil {
		common.Logger.Fatalf("Failed to start pipeline %s execution: %s", projectName, err)
	}
	autoApprove := getAutoApprove(stepState)
	err = u.resources.CodePipeline.WaitPipelineExecution(projectName, executionId, autoApprove, 30, step.Type)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline %s execution: %s", projectName, err)
	}
}

func getAutoApprove(state *model.StateStep) bool {
	for _, module := range state.Modules {
		if !module.AutoApprove {
			return false
		}
	}
	return true
}

func (u *updater) getReleases() []*version.Version {
	oldestVersion := u.getOldestVersion()
	newestVersion := u.getNewestVersion()
	latestRelease, err := u.github.GetLatestReleaseTag()
	if err != nil {
		common.Logger.Fatalf("Failed to get latest release: %s", err)
	}
	latestSemver, err := version.NewVersion(latestRelease.Tag)
	if err != nil {
		common.Logger.Fatalf("Failed to parse latest release version %s: %s", latestRelease.Tag, err)
	}
	u.stableRelease = latestSemver
	if oldestVersion == StableVersion {
		common.Logger.Printf("Latest release is %s\n", latestRelease.Tag)
		return toVersions([]github.Release{*latestRelease})
	}
	oldestRelease, err := u.github.GetReleaseByTag(oldestVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to get oldest release %s: %s", oldestVersion, err)
	}
	var newestRelease *github.Release
	if newestVersion == StableVersion {
		newestRelease = latestRelease
	} else {
		newestRelease, err = u.github.GetReleaseByTag(newestVersion)
		if err != nil {
			common.Logger.Fatalf("Failed to get newest release %s: %s", newestVersion, err)
		}
	}
	common.Logger.Printf("Oldest module version is %s\n", oldestRelease.Tag)
	common.Logger.Printf("Newest assigned module version is %s\n", newestRelease.Tag)
	releases, err := u.github.GetNewerReleases(oldestRelease.PublishedAt, newestRelease.PublishedAt)
	if err != nil {
		common.Logger.Fatalf("Failed to get newer releases: %s", err)
	}
	return toVersions(releases)
}

func toVersions(releases []github.Release) []*version.Version {
	versions := make([]*version.Version, 0)
	for _, release := range releases {
		releaseVersion, err := version.NewVersion(release.Tag)
		if err != nil {
			common.Logger.Fatalf("Failed to parse release version %s: %s", release.Tag, err)
		}
		versions = append(versions, releaseVersion)
	}
	return versions
}

func (u *updater) getOldestVersion() string {
	oldestVersion := u.config.Version
	if oldestVersion == "" {
		oldestVersion = StableVersion
	}
	for _, step := range u.config.Steps {
		oldestVersion = getOlderVersion(oldestVersion, step.Version)
		for _, module := range step.Modules {
			oldestVersion = getOlderVersion(oldestVersion, module.Version)
		}
	}
	for _, step := range u.state.Steps {
		for _, module := range step.Modules {
			moduleVersion := ""
			if module.Version != nil {
				moduleVersion = getFormattedVersion(module.Version)
			}
			oldestVersion = getOlderVersion(oldestVersion, moduleVersion)
		}
	}
	return oldestVersion
}

func getOlderVersion(oldestVersion string, compareVersion string) string {
	if compareVersion == "" || oldestVersion != StableVersion && compareVersion == StableVersion ||
		oldestVersion == StableVersion && compareVersion == StableVersion {
		return oldestVersion
	} else if oldestVersion == StableVersion && compareVersion != StableVersion {
		return compareVersion
	}
	version1, err := version.NewVersion(oldestVersion)
	if err != nil {
		common.Logger.Fatalf("failed to parse version %s: %s", oldestVersion, err)
	}
	version2, err := version.NewVersion(compareVersion)
	if err != nil {
		common.Logger.Fatalf("failed to parse version %s: %s", compareVersion, err)
	}
	if version1.LessThan(version2) {
		return oldestVersion
	} else {
		return compareVersion
	}
}

func (u *updater) getNewestVersion() string {
	newestVersion := ""
	configVersion := u.config.Version
	for _, step := range u.config.Steps {
		stepVersion := step.Version
		if stepVersion == "" {
			stepVersion = configVersion
		}
		for _, module := range step.Modules {
			moduleVersion := module.Version
			if moduleVersion == "" {
				moduleVersion = stepVersion
			}
			if moduleVersion == StableVersion || moduleVersion == "" {
				return StableVersion
			}
			if newestVersion == "" {
				newestVersion = moduleVersion
			} else {
				newestVersion = getNewerVersion(newestVersion, moduleVersion)
			}
		}
	}
	return newestVersion
}

func getNewerVersion(newestVersion string, moduleVersion string) string {
	version1, err := version.NewVersion(newestVersion)
	if err != nil {
		common.Logger.Fatalf("failed to parse version %s: %s", newestVersion, err)
	}
	version2, err := version.NewVersion(moduleVersion)
	if err != nil {
		common.Logger.Fatalf("failed to parse version %s: %s", moduleVersion, err)
	}
	if version1.GreaterThan(version2) {
		return newestVersion
	} else {
		return moduleVersion
	}
}

func (u *updater) createTerraformFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	u.createBackendConf(fmt.Sprintf("%s-%s", u.config.Prefix, step.Name), u.resources.CodeCommit)
	return u.updateTerraformFiles(step, stepState, releaseTag, stepSemver)
}

func (u *updater) updateTerraformFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	changed, moduleVersions := u.createTerraformMain(step, stepState, releaseTag, stepSemver)
	if !changed {
		return false
	}
	provider, err := u.terraform.GetTerraformProvider(step, moduleVersions)
	if err != nil {
		common.Logger.Fatalf("Failed to create terraform provider: %s", err)
	}
	u.resources.CodeCommit.PutFile(fmt.Sprintf("%s-%s/%s/provider.tf", u.config.Prefix, step.Name, step.Workspace), provider)
	return changed
}

func (u *updater) createArgoCDFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	executePipeline := false
	var clusterOIDC string
	if step.EksPrefix != "" {
		clusterOIDC = u.resources.SSM.GetClusterOIDC(u.config.Prefix, step.EksPrefix, step.Workspace)
	}
	activeModules := model.NewSet[string]()
	for _, module := range step.Modules {
		moduleVersion, changed := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve)
		if changed {
			executePipeline = true
		}
		inputs := u.addEksInputs(module.Inputs, step.EksPrefix, clusterOIDC)
		var bytes []byte
		if len(inputs) == 0 {
			bytes = []byte{}
		} else {
			var err error
			bytes, err = yaml.Marshal(inputs)
			if err != nil {
				common.Logger.Fatalf("Failed to marshal helm values: %s", err)
			}
		}
		u.resources.CodeCommit.PutFile(fmt.Sprintf("%s-%s/%s/%s/values.yaml", u.config.Prefix, step.Name,
			step.Workspace, module.Name), bytes)
		argoRepoUrl := u.resources.SSM.GetArgoCDRepoUrl(u.config.Prefix, step.ArgoCDPrefix, step.Workspace)
		u.createArgoCDApp(module, step, argoRepoUrl, moduleVersion)
		activeModules.Add(module.Name)
	}
	u.removeUnusedArgoCDApps(step, activeModules)
	return executePipeline
}

func (u *updater) addEksInputs(inputs map[string]interface{}, eksPrefix string, clusterOIDC string) map[string]interface{} {
	if eksPrefix == "" {
		return inputs
	}
	if inputs == nil || len(inputs) == 0 {
		inputs = make(map[string]interface{})
	}
	inputs["awsAccount"] = u.resources.AccountId
	inputs["awsRegion"] = u.resources.Region
	inputs["clusterOIDC"] = clusterOIDC
	return inputs
}

func (u *updater) updateArgoCDFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	executePipeline := false
	for _, module := range step.Modules {
		moduleVersion, changed := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve)
		if !changed {
			continue
		}
		executePipeline = true
		argoRepoUrl := u.resources.SSM.GetArgoCDRepoUrl(u.config.Prefix, step.ArgoCDPrefix, step.Workspace)
		u.createArgoCDApp(module, step, argoRepoUrl, moduleVersion)
	}
	return executePipeline
}

func (u *updater) createBackendConf(path string, codeCommit CodeCommit) {
	bytes, err := util.CreateKeyValuePairs(map[string]string{
		"bucket":         u.resources.Bucket,
		"key":            fmt.Sprintf("%s/terraform.tfstate", path),
		"dynamodb_table": u.resources.DynamoDBTable,
		"encrypt":        "true",
	}, "", "")
	if err != nil {
		common.Logger.Fatalf("Failed to convert backend config values: %s", err)
	}
	codeCommit.PutFile(fmt.Sprintf("%s/backend.conf", path), bytes)
}

func (u *updater) putPlannedStateFile() {
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal state: %s", err)
	}
	u.resources.CodeCommit.PutFile(stateFile, bytes)
}

func (u *updater) putAppliedStateFile(stepState *model.StateStep) {
	stepState.AppliedAt = time.Now()
	for _, module := range stepState.Modules {
		module.AppliedVersion = module.Version
	}
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal state: %s", err)
	}
	u.resources.CodeCommit.PutFile(stateFile, bytes)
}

func (u *updater) createTerraformMain(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) (bool, map[string]string) {
	file := hclwrite.NewEmptyFile()
	body := file.Body()
	changed := false
	moduleVersions := make(map[string]string)
	for _, module := range step.Modules {
		moduleVersion, moduleChanged := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve)
		if moduleChanged {
			changed = true
		}
		newModule := body.AppendNewBlock("module", []string{module.Name})
		moduleBody := newModule.Body()
		moduleBody.SetAttributeValue("source",
			cty.StringVal(fmt.Sprintf("git::%s.git//modules/%s?ref=%s", u.config.Source, module.Source, moduleVersion)))
		moduleBody.SetAttributeValue("prefix", cty.StringVal(fmt.Sprintf("%s-%s-%s", u.config.Prefix, step.Name, module.Name)))
		terraform.AddInputs(module.Inputs, moduleBody, moduleVersion)
		moduleVersions[module.Name] = moduleVersion
	}
	if changed {
		u.resources.CodeCommit.PutFile(fmt.Sprintf("%s-%s/%s/main.tf", u.config.Prefix, step.Name, step.Workspace), file.Bytes())
	}
	return changed, moduleVersions
}

func (u *updater) createArgoCDApp(module model.Module, step model.Step, argoRepoUrl string, moduleVersion string) {
	appFilePath := fmt.Sprintf("%s-%s/%s/%s/values.yaml", u.config.Prefix, step.Name, step.Workspace, module.Name)
	appBytes, err := argocd.GetApplicationFile(module, argoRepoUrl, moduleVersion, appFilePath)
	if err != nil {
		common.Logger.Fatalf("Failed to create application file: %s", err)
	}
	u.resources.CodeCommit.PutFile(fmt.Sprintf("%s-%s/%s/app-of-apps/%s.yaml", u.config.Prefix, step.Name,
		step.Workspace, module.Name), appBytes)
}

func (u *updater) getModuleVersion(module model.Module, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version, approve model.Approve) (string, bool) {
	moduleVersion := module.Version
	moduleState := getModuleState(stepState, module)
	var moduleSemver *version.Version
	var err error
	if moduleVersion == "" {
		moduleSemver = stepSemver
	} else if moduleVersion == StableVersion {
		moduleSemver = u.stableRelease
	} else {
		moduleSemver, err = version.NewVersion(moduleVersion)
		if err != nil {
			common.Logger.Fatalf("Failed to parse module version %s: %s", moduleVersion, err)
		}
	}
	moduleState.AutoApprove = true
	if moduleState.Version == nil {
		if moduleSemver.GreaterThan(releaseTag) {
			moduleState.Version = releaseTag
			return getFormattedVersion(releaseTag), true
		} else {
			moduleState.Version = moduleSemver
			return getFormattedVersion(moduleSemver), true
		}
	}
	if moduleSemver.Equal(moduleState.Version) && moduleSemver.LessThan(releaseTag) {
		return getFormattedVersion(moduleState.Version), false
	}
	if moduleState.Version.GreaterThan(releaseTag) {
		return getFormattedVersion(moduleState.Version), false
	} else {
		moduleState.AutoApprove = getModuleAutoApprove(moduleState.Version, releaseTag, approve)
		moduleState.Version = releaseTag
		return getFormattedVersion(releaseTag), true
	}
}

func (u *updater) createCustomTerraformFiles(step model.Step, stepState *model.StateStep, semver *version.Version) {
	u.createBackendConf(fmt.Sprintf("%s-%s", u.config.Prefix, step.Name), u.customCC)
	workspacePath := fmt.Sprintf("%s-%s/%s", u.config.Prefix, step.Name, step.Workspace)
	for _, module := range step.Modules {
		moduleState := getModuleState(stepState, module)
		if step.Approve == model.ApproveNever {
			moduleState.AutoApprove = true
		} else {
			moduleState.AutoApprove = false
		}
	}
	workspaceExists := u.customCC.CheckFolderExists(workspacePath)
	if workspaceExists {
		return
	}
	providerBytes, err := u.terraform.GetEmptyTerraformProvider(getFormattedVersion(semver))
	if err != nil {
		common.Logger.Fatalf("Failed to create empty terraform provider: %s", err)
	}
	u.customCC.PutFile(fmt.Sprintf("%s/provider.tf", workspacePath), providerBytes)
	mainBytes := terraform.GetMockTerraformMain()
	u.customCC.PutFile(fmt.Sprintf("%s/main.tf", workspacePath), mainBytes)
}

func (u *updater) removeUnusedArgoCDApps(step model.Step, modules model.Set[string]) {
	files := u.resources.CodeCommit.ListFolderFiles(fmt.Sprintf("%s-%s/%s/app-of-apps", u.config.Prefix, step.Name, step.Workspace))
	for _, file := range files {
		file = strings.TrimSuffix(file, ".yaml")
		if modules.Contains(file) {
			continue
		}
		u.resources.CodeCommit.DeleteFile(fmt.Sprintf("%s-%s/%s/app-of-apps/%s.yaml", u.config.Prefix, step.Name,
			step.Workspace, file))
		u.resources.CodeCommit.DeleteFile(fmt.Sprintf("%s-%s/%s/%s/values.yaml", u.config.Prefix, step.Name,
			step.Workspace, file))
	}
}

func getFormattedVersion(version *version.Version) string {
	if version == nil {
		return ""
	}
	original := version.Original()
	if strings.HasPrefix(original, "v") {
		return original
	}
	return fmt.Sprintf("v%s", original)
}

func getModuleState(stepState *model.StateStep, module model.Module) *model.StateModule {
	moduleState := GetModuleState(stepState, module.Name)
	if moduleState == nil {
		common.Logger.Fatalf("Failed to get state for module %s", module.Name)
	}
	return moduleState
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
