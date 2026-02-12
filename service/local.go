package service

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/argocd"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/terraform"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const executeScript = "entrypoint.sh"

type LocalPipeline struct {
	prefix       string
	regionKey    string
	region       string
	bucket       string
	pipeline     common.Pipeline
	inputLock    sync.Mutex
	manager      model.NotificationManager
	providerType model.ProviderType
	azureFlags   common.Azure
	gcloudFlags  common.GCloud
}

func NewLocalPipeline(resources model.Resources, pipeline common.Pipeline, gcloudFlags common.GCloud, azureFlags common.Azure, manager model.NotificationManager) *LocalPipeline {
	regionKey := "AWS_REGION"
	switch resources.GetProviderType() {
	case model.GCLOUD:
		regionKey = "GOOGLE_REGION"
	case model.AZURE:
		regionKey = "AZURE_REGION"
	}
	return &LocalPipeline{
		prefix:       resources.GetCloudPrefix(),
		regionKey:    regionKey,
		region:       resources.GetRegion(),
		bucket:       resources.GetBucketName(),
		pipeline:     pipeline,
		manager:      manager,
		providerType: resources.GetProviderType(),
		azureFlags:   azureFlags,
		gcloudFlags:  gcloudFlags,
	}
}

func (l *LocalPipeline) executeLocalPipeline(step model.Step, autoApprove bool, sourceAuths map[string]model.SourceAuth, approve model.ManualApprove) error {
	prefix := fmt.Sprintf("%s-%s", l.prefix, step.Name)
	log.Printf("Starting local pipeline %s", prefix)
	planCommand, applyCommand := model.GetCommands(step.Type)
	output, err := l.executeLocalCommand(prefix, planCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", planCommand, prefix, err)
	}
	approved, err := l.getApproval(prefix, step, autoApprove, output, approve)
	if err != nil {
		return fmt.Errorf("failed to get approval for %s: %v", prefix, err)
	}
	if !approved {
		return nil
	}
	_, err = l.executeLocalCommand(prefix, applyCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", applyCommand, prefix, err)
	}
	return nil
}

func (l *LocalPipeline) startDestroyExecution(step model.Step, sourceAuths map[string]model.SourceAuth) error {
	prefix := fmt.Sprintf("%s-%s", l.prefix, step.Name)
	planCommand, applyCommand := model.GetDestroyCommands(step.Type)
	_, err := l.executeLocalCommand(prefix, planCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", planCommand, prefix, err)
	}
	_, err = l.executeLocalCommand(prefix, applyCommand, step, sourceAuths)
	if err != nil {
		return fmt.Errorf("failed to execute %s for %s: %v", applyCommand, prefix, err)
	}
	return nil
}

func (l *LocalPipeline) executeLocalCommand(prefix string, command model.ActionCommand, step model.Step, sourceAuths map[string]model.SourceAuth) ([]byte, error) {
	cmd := exec.Command(executeScript)
	cmd.Env = l.getEnv(prefix, command, step, sourceAuths)
	var stdoutBuf bytes.Buffer
	writers := []io.Writer{&stdoutBuf}
	if l.pipeline.PrintLogs {
		writers = append(writers, log.Writer())
	}
	file := l.getLogFileWriter(prefix, command)
	if file != nil {
		defer func(file *os.File) {
			_ = file.Close()
		}(file)
		writers = append(writers, file)
	}
	stdout := io.MultiWriter(writers...)
	cmd.Stdout = stdout
	cmd.Stderr = log.Writer()
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	return stdoutBuf.Bytes(), err
}

func (l *LocalPipeline) getEnv(prefix string, command model.ActionCommand, step model.Step, sourceAuths map[string]model.SourceAuth) []string {
	env := os.Environ()
	env = append(env, fmt.Sprintf("COMMAND=%s", command), fmt.Sprintf("TF_VAR_prefix=%s", prefix),
		fmt.Sprintf("INFRALIB_BUCKET=%s", l.bucket), fmt.Sprintf("%s=%s", l.regionKey, l.region))
	for source, auth := range sourceAuths {
		hash := util.HashCode(source)
		env = append(env, fmt.Sprintf("%s=%s", fmt.Sprintf(model.GitSourceEnvFormat, hash), source),
			fmt.Sprintf("%s=%s", fmt.Sprintf(model.GitUsernameEnvFormat, hash), auth.Username),
			fmt.Sprintf("%s=%s", fmt.Sprintf(model.GitPasswordEnvFormat, hash), auth.Password))
	}
	switch l.providerType {
	case model.GCLOUD:
		env = append(env, fmt.Sprintf("GOOGLE_PROJECT=%s", l.gcloudFlags.ProjectId),
			fmt.Sprintf("GOOGLE_ZONE=%s", l.gcloudFlags.Zone))
	case model.AZURE:
		env = append(env, fmt.Sprintf("AZURE_SUBSCRIPTION_ID=%s", l.azureFlags.SubscriptionId),
			fmt.Sprintf("AZURE_RESOURCE_GROUP=%s", l.azureFlags.ResourceGroup),
			fmt.Sprintf("%s=%s", common.DevOpsOrgEnv, l.azureFlags.DevOpsOrg),
			fmt.Sprintf("%s=%s", common.DevOpsProjectEnv, l.azureFlags.DevOpsProject))
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
		for _, module := range step.Modules {
			if util.IsClientModule(module) {
				env = append(env, fmt.Sprintf("GIT_AUTH_USERNAME_%s=%s", strings.ToUpper(module.Name), module.HttpUsername),
					fmt.Sprintf("GIT_AUTH_PASSWORD_%s=%s", strings.ToUpper(module.Name), module.HttpPassword),
					fmt.Sprintf("GIT_AUTH_SOURCE_%s=%s", strings.ToUpper(module.Name), module.Source))
			}
		}
	}
	return env
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
	return l.getManualApproval(pipelineName, pipeChanges)
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

func (l *LocalPipeline) getManualApproval(pipelineName string, changes *model.PipelineChanges) (bool, error) {
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
	l.manager.ManualApproval(pipelineName, *changes, "")

	fmt.Printf("Pipeline %s changes: %d to change, %d to destroy. Approve changes? (yes/no)", pipelineName,
		changes.Changed, changes.Destroyed)
	err := util.AskForConfirmation()
	if err != nil {
		return false, fmt.Errorf("manual approval failed: %v", err)
	}
	return true, nil
}
