package run

import (
	"context"
	"os"
	"testing"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/test"
)

func TestRunAWS(t *testing.T) {
	t.Parallel()
	if err := common.ChooseLogger(string(common.DebugLogLevel)); err != nil {
		t.Fatalf("failed to choose logger: %v", err)
	}
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
	if err := Run(context.Background(), flags); err != nil {
		t.Fatalf("failed to run: %v", err)
	}
	deleter, err := service.NewDeleter(context.Background(), flags)
	if err != nil {
		t.Fatalf("failed to create deleter: %v", err)
	}
	if err := deleter.Destroy(); err != nil {
		t.Fatalf("failed to destroy: %v", err)
	}
	if err := deleter.Delete(); err != nil {
		t.Fatalf("failed to delete: %v", err)
	}
}

func TestRunGCloud(t *testing.T) {
	t.Parallel()
	if err := common.ChooseLogger(string(common.DebugLogLevel)); err != nil {
		t.Fatalf("failed to choose logger: %v", err)
	}
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
	if err := Run(context.Background(), flags); err != nil {
		t.Fatalf("failed to run: %v", err)
	}
	deleter, err := service.NewDeleter(context.Background(), flags)
	if err != nil {
		t.Fatalf("failed to create deleter: %v", err)
	}
	if err := deleter.Destroy(); err != nil {
		t.Fatalf("failed to destroy: %v", err)
	}
	if err := deleter.Delete(); err != nil {
		t.Fatalf("failed to delete: %v", err)
	}
}
