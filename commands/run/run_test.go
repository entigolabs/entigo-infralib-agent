package run

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/test"
	"os"
	"testing"
)

func TestRun(t *testing.T) {
	test.ChangeRunDir()
	awsPrefix := os.Getenv(common.AwsPrefixEnv)
	if awsPrefix == "" {
		awsPrefix = "entigo-infralib-test"
	}
	fmt.Println(os.Getwd())
	flags := &common.Flags{
		Config:    "test/profile.yaml",
		Branch:    "main",
		AWSPrefix: awsPrefix,
	}
	Run(flags)
}
