package wrapper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

const tfPlan = "steps/%s/%s-plan.json"

const disconnectTimeout = 10 * time.Second

type Wrapper struct {
	ctx           context.Context
	config        *model.Wrapper
	client        BackendClient
	command       model.ActionCommand
	campaignId    string
	pipelineIndex int32
	prefixStep    string
	planPath      string
	step          string
	stepType      model.StepType
	entrypoint    string
	env           []string
	stdout        io.Writer
}

func NewWrapper(ctx context.Context, flags common.Wrapper, config *model.Wrapper, env []string, stdout io.Writer) (*Wrapper, error) {
	command := model.ActionCommand(flags.Command)
	stepType := getStepType(command)
	if stepType == "" {
		return nil, fmt.Errorf("unknown command: %s", flags.Command)
	}
	campaignId := flags.CampaignId
	if campaignId == model.CampaignSentinelNone {
		campaignId = ""
	}
	pipelineIndex := parsePipelineIndex(flags.PipelineIndex)
	// Provisioning must not depend on the backend — fall back to transparent
	// mode on any init failure.
	client, err := getBackendClient(config, campaignId)
	if err != nil {
		slog.Error("wrapper backend init failed, running entrypoint without log forwarding", "err", err)
		client = nil
	}
	return &Wrapper{
		ctx:           ctx,
		config:        config,
		client:        client,
		command:       command,
		campaignId:    campaignId,
		pipelineIndex: pipelineIndex,
		prefixStep:    flags.PrefixStep,
		step:          flags.Step,
		entrypoint:    flags.Entrypoint,
		planPath:      flags.PlanPath,
		env:           env,
		stdout:        stdout,
		stepType:      stepType,
	}, nil
}

// Empty or the CampaignSentinelNone sentinel returns 0.
func parsePipelineIndex(raw string) int32 {
	if raw == "" || raw == model.CampaignSentinelNone {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		slog.Warn("wrapper PIPELINE_INDEX is not a valid integer, treating as unset", "value", raw, "err", err)
		return 0
	}
	return int32(v)
}

func getBackendClient(config *model.Wrapper, campaignId string) (BackendClient, error) {
	if config == nil || config.Api == nil {
		return nil, nil
	}
	if campaignId == "" {
		slog.Warn("wrapper config supplied but CAMPAIGN_ID is empty, running transparently")
		return nil, nil
	}
	return newBackendClient(config.Api)
}

func (w *Wrapper) Provision() error {
	w.connectBackend()

	exitCode, runErr := w.runEntrypoint()
	if w.client == nil {
		return runErr
	}

	if w.command == model.PlanCommand && exitCode == 0 && w.client != nil {
		w.sendPlan()
	}
	// w.ctx has no deadline of its own — always cap Disconnect so a hung
	// backend can't block Provision indefinitely.
	base := w.ctx
	if base.Err() != nil {
		base = context.Background()
	}
	disconnectCtx, cancel := context.WithTimeout(base, disconnectTimeout)
	defer cancel()
	if err := w.client.Disconnect(disconnectCtx, exitCode, runErr); err != nil {
		slog.Warn("wrapper backend Disconnect failed", "err", err)
	}
	return runErr
}

func (w *Wrapper) connectBackend() {
	if w.client == nil {
		return
	}
	err := w.client.Connect(HandshakeData{
		CampaignId:    w.campaignId,
		Step:          w.step,
		Command:       protoCommand(w.command),
		StepType:      protoStepType(w.stepType),
		PipelineIndex: w.pipelineIndex,
	})
	if err != nil {
		slog.Error("wrapper backend connection failed, running entrypoint without log forwarding", "err", err)
		w.client = nil
	}
}

func (w *Wrapper) runEntrypoint() (int, error) {
	cmd := exec.CommandContext(w.ctx, w.entrypoint)
	cmd.Env = w.env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, fmt.Errorf("failed to create stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("failed to start %s: %v", w.entrypoint, err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.streamStdout(stdout)
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return exitCode, waitErr
}

func (w *Wrapper) streamStdout(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		_, _ = fmt.Fprintln(w.stdout, line)
		if w.client != nil {
			if err := w.client.SendLog(line); err != nil {
				slog.Warn("wrapper backend SendLog failed", "err", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Error("wrapper stdout scanner failed", "err", err)
	}
}

func (w *Wrapper) sendPlan() {
	planFile := path.Join(w.getPlanPath(), fmt.Sprintf(tfPlan, w.prefixStep, w.prefixStep))
	summary, err := readPlanSummary(planFile)
	if err != nil {
		slog.Warn("wrapper plan summary unavailable", "err", err)
		return
	}
	if err := w.client.SendPlan(summary); err != nil {
		slog.Warn("wrapper backend SendPlan failed", "err", err)
	}
}

func (w *Wrapper) getPlanPath() string {
	if w.planPath != "" {
		return w.planPath
	}
	if os.Getenv(model.GoogleRegion) != "" {
		return "/project"
	}
	if os.Getenv(model.AWSRegion) != "" {
		return os.Getenv("CODEBUILD_SRC_DIR")
	}
	return ""
}
