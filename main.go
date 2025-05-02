package main

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/cli"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		cli.Run(ctx)
		close(terminated)
	}()

	sig := <-terminated
	if sig != nil {
		slog.Warn(common.PrefixWarning("agent was terminated, exiting"))
	}
}
