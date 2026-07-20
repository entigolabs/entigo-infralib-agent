package service

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/entigolabs/entigo-infralib-agent/wrapper"
)

const executeScript = "entrypoint-core.sh"

type LocalPipeline struct {
	ctx            context.Context
	prefix         string
	regionKey      string
	region         string
	project        string
	zone           string
	compartmentId  string
	bucket         string
	backendEnv     map[string]string
	enableOpenTofu bool
	pipeline       common.Pipeline
	inputLock      sync.Mutex
	manager        model.NotificationManager
	wrapper        *model.NotificationApi
	campaignId     string
	pipelineIndex  int
}

func (l *LocalPipeline) SetPipelineIndex(index int) {
	l.pipelineIndex = index
}

func NewLocalPipeline(ctx context.Context, resources model.Resources, pipeline common.Pipeline, flags *common.Flags, manager model.NotificationManager, config model.Config, campaignId string) *LocalPipeline {
	regionKey := model.AWSRegion
	project := ""
	zone := ""
	compartmentId := ""
	switch resources.GetProviderType() {
	case model.GCLOUD:
		regionKey = model.GoogleRegion
		project = flags.GCloud.ProjectId
		zone = flags.GCloud.Zone
	case model.ORACLE:
		regionKey = model.OracleRegion
		compartmentId = flags.Oracle.CompartmentId
	}
	var backendEnv map[string]string
	if provider, ok := resources.(model.BackendEnvProvider); ok {
		backendEnv = provider.GetBackendEnv()
	}
	return &LocalPipeline{
		ctx:            ctx,
		prefix:         resources.GetCloudPrefix(),
		regionKey:      regionKey,
		region:         resources.GetRegion(),
		project:        project,
		zone:           zone,
		compartmentId:  compartmentId,
		bucket:         resources.GetBucketName(),
		backendEnv:     backendEnv,
		pipeline:       pipeline,
		manager:        manager,
		enableOpenTofu: config.IsOpenTofuEnabled(),
		wrapper:        getWrapperConfig(config.Notifications),
		campaignId:     campaignId,
	}
}

func (l *LocalPipeline) executeLocalPipeline(step model.Step, autoApprove bool, sourceAuths map[string]model.SourceAuth, approve model.ManualApprove) error {
	prefixStep := fmt.Sprintf("%s-%s", l.prefix, step.Name)
	log.Printf("Starting local pipeline %s", prefixStep)
	planCommand, applyCommand := model.GetCommands(step.Type)
	output, err := l.executeWrapper(prefixStep, planCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", planCommand, prefixStep, err)
	}
	approved, err := l.getApproval(prefixStep, step, autoApprove, output, approve)
	if err != nil {
		return fmt.Errorf("failed to get approval for %s: %v", prefixStep, err)
	}
	if !approved {
		return nil
	}
	_, err = l.executeWrapper(prefixStep, applyCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", applyCommand, prefixStep, err)
	}
	return nil
}

func (l *LocalPipeline) startDestroyExecution(step model.Step, sourceAuths map[string]model.SourceAuth) error {
	prefixStep := fmt.Sprintf("%s-%s", l.prefix, step.Name)
	planCommand, applyCommand := model.GetDestroyCommands(step.Type)
	_, err := l.executeWrapper(prefixStep, planCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", planCommand, prefixStep, err)
	}
	_, err = l.executeWrapper(prefixStep, applyCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", applyCommand, prefixStep, err)
	}
	return nil
}

func (l *LocalPipeline) executeWrapper(prefixStep string, command model.ActionCommand, step model.Step, sourceAuths map[string]model.SourceAuth) ([]byte, error) {
	flags := common.Wrapper{
		Step:          step.Name,
		Command:       string(command),
		Entrypoint:    executeScript,
		PrefixStep:    prefixStep,
		PlanPath:      "/tmp/project",
		CampaignId:    l.campaignId,
		PipelineIndex: strconv.Itoa(l.pipelineIndex),
		//		Insecure:      true, // Development only
	}
	env := l.getEnv(prefixStep, command, step, sourceAuths)
	var stdoutBuf bytes.Buffer
	writers := []io.Writer{&stdoutBuf}
	if l.pipeline.PrintLogs {
		writers = append(writers, log.Writer())
	}
	file := l.getLogFileWriter(prefixStep, command)
	if file != nil {
		defer func(file *os.File) {
			_ = file.Close()
		}(file)
		writers = append(writers, file)
	}
	stdout := io.MultiWriter(writers...)
	wrap, err := wrapper.NewWrapper(l.ctx, flags, l.wrapper, env, stdout)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize wrapper: %w", err)
	}
	err = wrap.Provision() // Provision results have to be before stdoutBuf.Bytes()
	return stdoutBuf.Bytes(), err
}

func (l *LocalPipeline) getEnv(prefixStep string, command model.ActionCommand, step model.Step, sourceAuths map[string]model.SourceAuth) []string {
	env := os.Environ()
	env = append(env, fmt.Sprintf("COMMAND=%s", command), fmt.Sprintf("TF_VAR_prefix=%s", prefixStep),
		fmt.Sprintf("INFRALIB_BUCKET=%s", l.bucket), fmt.Sprintf("%s=%s", l.regionKey, l.region))
	for key, value := range l.backendEnv {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	for source, auth := range sourceAuths {
		hash := util.HashCode(source)
		env = append(env, fmt.Sprintf("%s=%s", fmt.Sprintf(model.GitSourceEnvFormat, hash), source),
			fmt.Sprintf("%s=%s", fmt.Sprintf(model.GitUsernameEnvFormat, hash), auth.Username),
			fmt.Sprintf("%s=%s", fmt.Sprintf(model.GitPasswordEnvFormat, hash), auth.Password))
	}
	if l.project != "" {
		env = append(env, fmt.Sprintf("GOOGLE_PROJECT=%s", l.project), fmt.Sprintf("GOOGLE_ZONE=%s", l.zone))
	}
	if l.compartmentId != "" {
		env = append(env, fmt.Sprintf("%s=%s", common.OracleCompartmentIdEnv, l.compartmentId))
	}
	if step.Type == model.StepTypeArgoCD {
		if step.KubernetesClusterName != "" {
			env = append(env, fmt.Sprintf("KUBERNETES_CLUSTER_NAME=%s", step.KubernetesClusterName))
		}
		if step.ArgocdNamespace == "" {
			env = append(env, "ARGOCD_NAMESPACE=argocd")
		} else {
			env = append(env, fmt.Sprintf("ARGOCD_NAMESPACE=%s", step.ArgocdNamespace))
		}
	}
	if step.Type == model.StepTypeTerraform {
		env = append(env, fmt.Sprintf("TERRAFORM_CACHE=%t", *l.pipeline.TerraformCache.Value))
		if l.enableOpenTofu {
			env = append(env, fmt.Sprintf("TF_TOOL=%s", model.TofuTfTool))
		}
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				env = append(env, fmt.Sprintf("GIT_AUTH_USERNAME_%s=%s", strings.ToUpper(module.Name), module.HttpUsername),
					fmt.Sprintf("GIT_AUTH_PASSWORD_%s=%s", strings.ToUpper(module.Name), module.HttpPassword),
					fmt.Sprintf("GIT_AUTH_SOURCE_%s=%s", strings.ToUpper(module.Name), module.Source))
			}
		}
	}
	return dedupeEnv(env)
}

// dedupeEnv collapses duplicate keys keeping the last value, so the values the
// agent appends (e.g. the provisioned AWS_ACCESS_KEY_ID for the Oracle s3 backend)
// deterministically override anything inherited from the caller's shell via
// os.Environ — otherwise a stale exported credential could shadow the real one,
// and duplicate-key resolution across bash/aws is unspecified.
func dedupeEnv(env []string) []string {
	index := make(map[string]int, len(env))
	result := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			result = append(result, kv)
			continue
		}
		key := kv[:eq]
		if i, ok := index[key]; ok {
			result[i] = kv
			continue
		}
		index[key] = len(result)
		result = append(result, kv)
	}
	return result
}

func (l *LocalPipeline) getLogFileWriter(prefix string, command model.ActionCommand) *os.File {
	if l.pipeline.LogsPath == "" {
		return nil
	}
	fileName := fmt.Sprintf("%s_%s_%s.log", command, prefix, time.Now().Format(time.RFC3339))
	fileName = strings.ReplaceAll(fileName, "-", "_")
	file, err := os.OpenFile(filepath.Join(l.pipeline.LogsPath, fileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to open log file: %v", err)))
		return nil
	}
	return file
}

func (l *LocalPipeline) getApproval(pipelineName string, step model.Step, autoApprove bool, output []byte, approve model.ManualApprove) (bool, error) {
	if output == nil {
		return false, fmt.Errorf("no output from execution")
	}
	pipeChanges, err := getPipelineChanges(pipelineName, step.Type, output)
	if err != nil {
		return false, err
	}
	if step.Approve == model.ApproveReject || approve == model.ManualApproveReject {
		return false, fmt.Errorf("stopped because step approve type is 'reject'")
	}
	if pipeChanges.NoChanges {
		log.Printf("No changes detected for %s, skipping apply", pipelineName)
		return false, nil
	}
	if util.ShouldApprovePipeline(*pipeChanges, step.Approve, autoApprove, approve) {
		log.Printf("Approved %s\n", pipelineName)
		return true, nil
	}
	return l.getManualApproval(pipelineName, step.Name, pipeChanges)
}

func getPipelineChanges(pipelineName string, stepType model.StepType, output []byte) (*model.PipelineChanges, error) {
	var logParser func(string, string) (*model.PipelineChanges, error)
	switch stepType {
	case model.StepTypeTerraform:
		logParser = terraform.ParseLogChanges
	case model.StepTypeArgoCD:
		logParser = argocd.ParseLogChanges
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		logRow := scanner.Text()
		changes, err := logParser(pipelineName, logRow)
		if err != nil {
			return nil, err
		}
		if changes != nil {
			return changes, nil
		}
	}
	return nil, fmt.Errorf("couldn't find plan output from logs for %s", pipelineName)
}

func (l *LocalPipeline) getManualApproval(pipelineName, step string, changes *model.PipelineChanges) (bool, error) {
	l.inputLock.Lock()

	var logBuffer bytes.Buffer
	originalLogOutput := log.Writer()
	log.SetOutput(&logBuffer)
	defer func() {
		log.SetOutput(originalLogOutput)
		log.Println(logBuffer.String())
		l.inputLock.Unlock()
	}()
	time.Sleep(1 * time.Second) // Wait for output to be redirected
	l.manager.ManualApproval(pipelineName, step, *changes, "")

	fmt.Printf("Pipeline %s changes: %d to change, %d to destroy. Approve changes? (yes/no)", pipelineName,
		changes.Changed, changes.Destroyed)
	err := util.AskForConfirmation()
	if err != nil {
		return false, fmt.Errorf("manual approval failed: %v", err)
	}
	return true, nil
}
