package main

import (
	"github.com/entigolabs/entigo-infralib-agent/cli"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	time.Local = time.UTC
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		cli.Run()
		close(terminated)
	}()

	sig := <-terminated
	if sig != nil {
		slog.Warn(common.PrefixWarning("agent was terminated, exiting"))
	}
}
