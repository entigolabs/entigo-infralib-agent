package run

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/test"
	"os"
	"testing"
)

func TestRunAWS(t *testing.T) {
	t.Parallel()
	common.ChooseLogger(string(common.DebugLogLevel))
	test.ChangeRunDir()
	prefix := os.Getenv(common.AwsPrefixEnv)
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	flags := &common.Flags{
		Config:                  "test/profile-aws.yaml",
		Prefix:                  prefix,
		SkipBucketCreationDelay: true,
		Delete: common.DeleteFlags{
			DeleteBucket: true,
		},
		Pipeline: common.Pipeline{
			Type: string(common.PipelineTypeCloud),
		},
	}
	Run(context.Background(), flags)
	deleter := service.NewDeleter(context.Background(), flags)
	deleter.Destroy()
	deleter.Delete()
}

func TestRunGCloud(t *testing.T) {
	t.Parallel()
	common.ChooseLogger(string(common.DebugLogLevel))
	test.ChangeRunDir()
	projectId := os.Getenv(common.GCloudProjectIdEnv)
	location := os.Getenv(common.GCloudLocationEnv)
	zone := os.Getenv(common.GCloudZoneEnv)
	prefix := os.Getenv(common.AwsPrefixEnv)
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	flags := &common.Flags{
		Config: "test/profile-gcloud.yaml",
		Prefix: prefix,
		GCloud: common.GCloud{
			ProjectId: projectId,
			Location:  location,
			Zone:      zone,
		},
		SkipBucketCreationDelay: true,
		Delete: common.DeleteFlags{
			DeleteBucket: true,
		},
		Pipeline: common.Pipeline{
			Type: string(common.PipelineTypeCloud),
		},
	}
	Run(context.Background(), flags)
	deleter := service.NewDeleter(context.Background(), flags)
	deleter.Destroy()
	deleter.Delete()
}
