package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/cli"
	"github.com/entigolabs/entigo-infralib-agent/common"
)

const shutdownGrace = 20 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() {
		cli.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		slog.Warn(common.PrefixWarning("agent was terminated, waiting for shutdown"))
	}

	select {
	case <-done:
	case <-time.After(shutdownGrace):
		slog.Warn(common.PrefixWarning("shutdown grace period exceeded, exiting"))
	}
}
