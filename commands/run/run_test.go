package run

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"testing"
)

func TestRun(t *testing.T) {
	flags := &common.Flags{
		Config:    "test/profile.yaml",
		Branch:    "main",
		AWSPrefix: "entigo-infralib-test",
	}
	Run(flags)
	// TODO Test run command
}
