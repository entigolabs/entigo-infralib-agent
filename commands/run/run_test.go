package run

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/test"
	"log"
	"os"
	"testing"
)

func TestRunAWS(t *testing.T) {
	t.Parallel()
	common.ChooseLogger(string(common.DebugLogLevel))
	test.ChangeRunDir()
	awsPrefix := os.Getenv(common.AwsPrefixEnv)
	flags := &common.Flags{
		Config: "test/profile-aws.yaml",
		Branch: "main",
		Prefix: awsPrefix,
	}
	Run(context.Background(), flags)
}

func TestRunGCloud(t *testing.T) {
	t.Parallel()
	common.ChooseLogger(string(common.DebugLogLevel))
	test.ChangeRunDir()
	projectId := os.Getenv(common.GCloudProjectIdEnv)
	location := os.Getenv(common.GCloudLocationEnv)
	zone := os.Getenv(common.GCloudZoneEnv)
	flags := &common.Flags{
		Config: "test/profile-gcloud.yaml",
		Prefix: os.Getenv(common.AwsPrefixEnv),
		GCloud: common.GCloud{
			ProjectId: projectId,
			Location:  location,
			Zone:      zone,
		},
		Delete: common.DeleteFlags{
			DeleteBucket: true,
		},
	}
	Run(context.Background(), flags)
	deleter := service.NewDeleter(context.Background(), flags)
	failed := deleter.Destroy()
	if failed {
		log.Fatalf("Running destroy pipelines failed")
	}
	deleter.Delete()
}
