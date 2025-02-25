package main

import (
	"github.com/entigolabs/entigo-infralib-agent/cli"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	terminated := make(chan os.Signal, 1)
	signal.Notify(terminated, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		cli.Run()
		close(terminated)
	}()

	sig := <-terminated
	if sig != nil {
		common.PrintWarning("agent was terminated, exiting")
	}
}
