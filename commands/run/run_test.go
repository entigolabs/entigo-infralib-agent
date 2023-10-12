package run

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"os"
	"testing"
)

func TestRun(t *testing.T) {
	awsPrefix := os.Getenv(common.AwsPrefixEnv)
	if awsPrefix == "" {
		awsPrefix = "entigo-infralib-test"
	}
	flags := &common.Flags{
		Config:    "../../test/profile.yaml",
		Branch:    "main",
		AWSPrefix: awsPrefix,
	}
	Run(flags)
}
