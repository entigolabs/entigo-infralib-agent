package run

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/test"
	"os"
	"testing"
)

func TestRunAWS(t *testing.T) {
	t.Parallel()
	test.ChangeRunDir()
	awsPrefix := os.Getenv(common.AwsPrefixEnv)
	flags := &common.Flags{
		Config:    "test/profile-aws.yaml",
		Branch:    "main",
		AWSPrefix: awsPrefix,
	}
	Run(flags)
}

func TestRunGCloud(t *testing.T) {
	t.Parallel()
	test.ChangeRunDir()
	projectId := os.Getenv(common.GCloudProjectIdEnv)
	location := os.Getenv(common.GCloudLocationEnv)
	zone := os.Getenv(common.GCloudZoneEnv)
	flags := &common.Flags{
		Config:    "test/profile-gcloud.yaml",
		AWSPrefix: os.Getenv(common.AwsPrefixEnv),
		GCloud: common.GCloud{
			ProjectId: projectId,
			Location:  location,
			Zone:      zone,
		},
	}
	Run(flags)
	deleter := service.NewDeleter(flags)
	deleter.Destroy()
	deleter.Delete()
}
