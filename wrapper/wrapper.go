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
	"strings"
	"sync"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"gopkg.in/yaml.v3"
)

const disconnectTimeout = 10 * time.Second

type Wrapper struct {
	ctx        context.Context
	config     *model.Wrapper
	client     BackendClient
	command    string
	step       string
	entrypoint string
}

func NewWrapper(ctx context.Context, flags common.Wrapper) (*Wrapper, error) {
	config, err := parseConfig(flags.Config)
	if err != nil {
		return nil, err
	}
	client, err := getBackendClient(ctx, config)
	if err != nil {
		return nil, err
	}
	return &Wrapper{
		ctx:        ctx,
		config:     config,
		client:     client,
		command:    flags.Command,
		step:       flags.Step,
		entrypoint: flags.Entrypoint,
	}, nil
}

func getBackendClient(ctx context.Context, config *model.Wrapper) (BackendClient, error) {
	if config == nil || config.Api == nil {
		return nil, nil
	}
	return newBackendClient(ctx, config.Api)
}

func parseConfig(raw string) (*model.Wrapper, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var config model.Wrapper
	if err := yaml.Unmarshal([]byte(raw), &config); err != nil {
		return nil, fmt.Errorf("failed to parse wrapper config: %w", err)
	}
	return &config, nil
}

func (w *Wrapper) Provision() error {
	w.connectBackend()

	exitCode, runErr := w.runEntrypoint()

	if w.client != nil {
		disconnectCtx := w.ctx
		if w.ctx.Err() != nil {
			var cancel context.CancelFunc
			disconnectCtx, cancel = context.WithTimeout(context.Background(), disconnectTimeout)
			defer cancel()
		}
		if err := w.client.Disconnect(disconnectCtx, exitCode, runErr); err != nil {
			slog.Warn("wrapper backend Disconnect failed", "err", err)
		}
	}
	return runErr
}

func (w *Wrapper) connectBackend() {
	if w.client == nil {
		return
	}
	err := w.client.Connect(HandshakeData{
		CampaignId: w.config.CampaignId,
		Step:       w.step,
		Command:    w.command,
	})
	if err != nil {
		slog.Error("wrapper backend connection failed, running entrypoint without log forwarding", "err", err)
		w.client = nil
	}
}

func (w *Wrapper) runEntrypoint() (int, error) {
	cmd := exec.CommandContext(w.ctx, w.entrypoint)
	cmd.Env = os.Environ()

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
		fmt.Println(w.humanize(line))
		if w.client != nil {
			if err := w.client.SendLog(line); err != nil {
				slog.Warn("wrapper backend SendLog failed", "err", err)
			}
		}
	}
}

func (w *Wrapper) humanize(line string) string {
	return line
}
