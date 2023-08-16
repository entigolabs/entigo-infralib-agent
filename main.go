package main

import (
	"errors"
	"github.com/entigolabs/entigo-infralib-agent/cli"
	"github.com/entigolabs/entigo-infralib-agent/common"
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
		common.Logger.Println(&common.Warning{Reason: errors.New("agent was terminated, exiting")})
	}
}
