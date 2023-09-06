package service

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"gopkg.in/yaml.v3"
	"strings"
	"time"
)

const stateFile = "state.yaml"

type Updater interface {
	ProcessSteps()
}

type updater struct {
	config        model.Config
	resources     AWSResources
	state         *model.State
	stableRelease *version.Version
}

func NewUpdater(flags *common.Flags) Updater {
	resources := SetupAWSResources(flags.AWSPrefix, flags.Branch)
	config := GetConfig(flags.Config, resources.CodeCommit)
	state := getLatestState(resources.CodeCommit)
	if state == nil {
		state = createState(config)
	} else {
		UpdateSteps(config, state)
	}
	validateConfig(config, state)
	return &updater{
		config:    config,
		resources: resources,
		state:     state,
	}
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

func createState(config model.Config) *model.State {
	steps := make([]*model.StateStep, 0)
	for _, step := range config.Steps {
		modules := make([]*model.StateModule, 0)
		for _, module := range step.Modules {
			modules = append(modules, &model.StateModule{
				Name: module.Name,
			})
		}
		steps = append(steps, &model.StateStep{
			Name:      step.Name,
			Workspace: step.Workspace,
			Modules:   modules,
		})
	}
	return &model.State{
		Steps: steps,
	}
}

func (u *updater) ProcessSteps() {
	u.updateAgentCodeBuild()
	releases := u.getReleases()

	firstRun := true
	configVersion := u.config.Version
	if configVersion == "" {
		configVersion = StableVersion
	}
	for _, release := range releases {
		releaseSemVer, err := version.NewVersion(release.Tag)
		if err != nil {
			common.Logger.Fatalf("Failed to parse release version %s: %s", release.Tag, err)
		}
		for _, step := range u.config.Steps {
			stepVersion := step.Version
			if stepVersion == "" {
				stepVersion = configVersion
			}
			var stepSemVer *version.Version
			if stepVersion == StableVersion {
				stepSemVer = u.stableRelease
			} else {
				stepSemVer, err = version.NewVersion(stepVersion)
				if err != nil {
					common.Logger.Fatalf("Failed to parse step version %s: %s", stepVersion, err)
				}
			}
			stepState := u.getStepState(step)
			if firstRun {
				executePipelines := u.createStepFiles(step, stepState, releaseSemVer, stepSemVer)
				if executePipelines {
					common.Logger.Printf("applying version %s for step %s\n", release.Tag, step.Name)
					u.createExecuteStepPipelines(step, stepState)
					stepState.AppliedAt = time.Now()
					u.putStateFile()
				}
			} else {
				executePipelines := u.updateStepFiles(step, stepState, releaseSemVer, stepSemVer)
				if executePipelines {
					common.Logger.Printf("applying version %s for step %s\n", release.Tag, step.Name)
					u.executeStepPipelines(step, stepState)
					stepState.AppliedAt = time.Now()
					u.putStateFile()
					common.Logger.Printf("release %s applied successfully for step %s\n", release.Tag)
				}
			}
		}
		if firstRun {
			firstRun = false
		}
	}
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
	executePipelines := false
	switch step.Type {
	case model.StepTypeTerraform:
		changed := u.createTerraformFiles(step, stepState, releaseTag, stepSemver)
		if changed {
			executePipelines = true
		}
	case model.StepTypeArgoCD:
		changed := u.createArgoCDFiles(step, stepState, releaseTag, stepSemver)
		if changed {
			executePipelines = true
		}
	}
	return executePipelines
}

func (u *updater) updateStepFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	executePipeline := false
	switch step.Type {
	case model.StepTypeTerraform:
		changed := u.createTerraformMain(step, stepState, releaseTag, stepSemver)
		if changed {
			executePipeline = true
		}
	case model.StepTypeArgoCD:
		for _, module := range step.Modules {
			_, changed := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve) // Update state version and auto approve
			if changed {
				executePipeline = true
			}
		}
	}
	return executePipeline
}

func (u *updater) createExecuteStepPipelines(step model.Step, stepState *model.StateStep) {
	repoMetadata := u.resources.CodeCommit.GetRepoMetadata()

	stepName := fmt.Sprintf("%s-%s", u.config.Prefix, step.Name)
	projectName := fmt.Sprintf("%s-%s", stepName, step.Workspace)

	vpcConfig := u.resources.SSM.GetVpcConfig(step.VpcPrefix, step.Workspace)
	err := u.resources.CodeBuild.CreateProject(projectName, *repoMetadata.CloneUrlHttp, stepName, step.Workspace, vpcConfig)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodeBuild project: %s", err)
	}
	autoApprove := getAutoApprove(stepState)

	switch step.Type {
	case model.StepTypeTerraform:
		u.createExecuteTerraformPipelines(projectName, stepName, step, autoApprove)
	case model.StepTypeArgoCD:
		u.createExecuteArgoCDPipelines(projectName, stepName, step, autoApprove)
	}
}

func (u *updater) createExecuteTerraformPipelines(projectName string, stepName string, step model.Step, autoApprove bool) {
	executionId, err := u.resources.CodePipeline.CreateTerraformPipeline(projectName, projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodePipeline: %s", err)
	}
	err = u.resources.CodePipeline.CreateTerraformDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create destroy CodePipeline: %s", err)
	}
	err = u.resources.CodePipeline.WaitPipelineExecution(projectName, executionId, autoApprove, 30, step.Type)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
	}
}

func (u *updater) createExecuteArgoCDPipelines(projectName string, stepName string, step model.Step, autoApprove bool) {
	executionId, err := u.resources.CodePipeline.CreateArgoCDPipeline(projectName, projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create CodePipeline: %s", err)
	}
	err = u.resources.CodePipeline.CreateArgoCDDestroyPipeline(fmt.Sprintf("%s-destroy", projectName), projectName, stepName, step.Workspace)
	if err != nil {
		common.Logger.Fatalf("Failed to create destroy CodePipeline: %s", err)
	}
	err = u.resources.CodePipeline.WaitPipelineExecution(projectName, executionId, autoApprove, 30, step.Type)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
	}
}

func (u *updater) executeStepPipelines(step model.Step, stepState *model.StateStep) {
	projectName := fmt.Sprintf("%s-%s-%s", u.config.Prefix, step.Name, step.Workspace)
	executionId, err := u.resources.CodePipeline.StartPipelineExecution(projectName)
	if err != nil {
		common.Logger.Fatalf("Failed to start pipeline execution: %s", err)
	}
	autoApprove := getAutoApprove(stepState)
	err = u.resources.CodePipeline.WaitPipelineExecution(projectName, executionId, autoApprove, 30, step.Type)
	if err != nil {
		common.Logger.Fatalf("Failed to wait for pipeline execution: %s", err)
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

func (u *updater) getReleases() []Release {
	githubClient := NewGithub(u.config.Source)
	oldestVersion := u.getOldestVersion()
	newestVersion := u.getNewestVersion()
	latestRelease, err := githubClient.GetLatestReleaseTag()
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
		return []Release{*latestRelease}
	}
	oldestRelease, err := githubClient.GetReleaseByTag(oldestVersion)
	if err != nil {
		common.Logger.Fatalf("Failed to get oldest release %s: %s", oldestVersion, err)
	}
	var newestRelease *Release
	if newestVersion == StableVersion {
		newestRelease = latestRelease
	} else {
		newestRelease, err = githubClient.GetReleaseByTag(newestVersion)
		if err != nil {
			common.Logger.Fatalf("Failed to get newest release %s: %s", newestVersion, err)
		}
	}
	common.Logger.Printf("Oldest module version is %s\n", oldestRelease.Tag)
	common.Logger.Printf("Newest assigned module version is %s\n", newestRelease.Tag)
	releases, err := githubClient.GetNewerReleases(oldestRelease.PublishedAt, newestRelease.PublishedAt)
	if err != nil {
		common.Logger.Fatalf("Failed to get newer releases: %s", err)
	}
	return releases
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
	provider, err := terraform.GetTerraformProvider(step)
	if err != nil {
		common.Logger.Fatalf("Failed to create terraform provider: %s", err)
	}
	u.resources.CodeCommit.PutFile(fmt.Sprintf("%s-%s/%s/provider.tf", u.config.Prefix, step.Name, step.Workspace), provider)
	u.createBackendConf(fmt.Sprintf("%s-%s", u.config.Prefix, step.Name))
	return u.createTerraformMain(step, stepState, releaseTag, stepSemver)
}

func (u *updater) createArgoCDFiles(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	executePipeline := false
	for _, module := range step.Modules {
		_, changed := u.getModuleVersion(module, stepState, releaseTag, stepSemver, step.Approve) // Update state version and auto approve
		if changed {
			executePipeline = true
		}
		inputs := module.Inputs
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
		u.resources.CodeCommit.PutFile(fmt.Sprintf("%s-%s/%s/%s/%s/values.yaml", u.config.Prefix, step.Name,
			step.Workspace, module.Name, module.Source), bytes)
	}
	return executePipeline
}

func (u *updater) createBackendConf(path string) {
	bytes, err := util.CreateKeyValuePairs(map[string]string{
		"Bucket":         u.resources.Bucket,
		"key":            fmt.Sprintf("%s/terraform.tfstate", path),
		"dynamodb_table": u.resources.DynamoDBTable,
		"encrypt":        "true",
	}, "", "")
	if err != nil {
		common.Logger.Fatalf("Failed to convert backend config values: %s", err)
	}
	u.resources.CodeCommit.PutFile(fmt.Sprintf("%s/backend.conf", path), bytes)
}

func (u *updater) putStateFile() {
	bytes, err := yaml.Marshal(u.state)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal state: %s", err)
	}
	u.resources.CodeCommit.PutFile(stateFile, bytes)
}

func (u *updater) createTerraformMain(step model.Step, stepState *model.StateStep, releaseTag *version.Version, stepSemver *version.Version) bool {
	file := hclwrite.NewEmptyFile()
	body := file.Body()
	changed := false
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
	}
	if changed {
		u.resources.CodeCommit.PutFile(fmt.Sprintf("%s-%s/%s/main.tf", u.config.Prefix, step.Name, step.Workspace), file.Bytes())
	}
	return changed
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
		return releaseSegments[0] > moduleSegments[0]
	}
	if approve == model.ApproveMinor {
		return releaseSegments[0] > moduleSegments[0] || releaseSegments[1] > moduleSegments[1]
	}
	return false
}
