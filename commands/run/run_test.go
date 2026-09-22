package run

import (
	"os"
	"strings"
	"testing"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/test"
)

func TestRunAWS(t *testing.T) {
	t.Parallel()
	if err := common.ChooseLogger(string(common.DebugLogLevel)); err != nil {
		t.Fatalf("failed to choose logger: %v", err)
	}
	test.ChangeRunDir()
	flags := &common.Flags{
		Config:                  "test/profile-aws.yaml",
		Prefix:                  getTestPrefix(),
		SkipBucketCreationDelay: true,
		Delete: common.DeleteFlags{
			DeleteBucket: true,
		},
		Pipeline: common.Pipeline{
			Type: string(common.PipelineTypeCloud),
		},
	}
	runTest(t, flags)
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
	flags := &common.Flags{
		Config: "test/profile-gcloud.yaml",
		Prefix: getTestPrefix(),
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
	runTest(t, flags)
}

func TestRunOracle(t *testing.T) {
	t.Parallel()
	if err := common.ChooseLogger(string(common.DebugLogLevel)); err != nil {
		t.Fatalf("failed to choose logger: %v", err)
	}
	test.ChangeRunDir()
	flags := &common.Flags{
		Config: "test/profile-oracle.yaml",
		Prefix: getTestPrefix(),
		Oracle: common.Oracle{
			Region:        os.Getenv(model.OracleRegion),
			CompartmentId: os.Getenv(common.OracleCompartmentIdEnv),
			Profile:       os.Getenv(common.OracleProfileEnv),
			ConfigFile:    os.Getenv(common.OracleConfigFileEnv),
		},
		SkipBucketCreationDelay: true,
		Delete: common.DeleteFlags{
			DeleteBucket: true,
		},
		Pipeline: common.Pipeline{
			Type: string(common.PipelineTypeCloud),
		},
	}
	runTest(t, flags)
}

func TestPrefix(t *testing.T) {
	err := os.Setenv(common.AwsPrefixEnv, "at-oracle-test")
	if err != nil {
		t.Fatal()
	}
	prefix := getTestPrefix()
	if prefix != "at-oracle" {
		t.Fatalf("expected prefix at-oracle, got %s", prefix)
	}
}

func getTestPrefix() string {
	prefix := strings.TrimSpace(os.Getenv(common.AwsPrefixEnv))
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	return strings.TrimFunc(prefix, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
}

func runTest(t *testing.T, flags *common.Flags) {
	if err := Run(t.Context(), flags); err != nil {
		t.Fatalf("failed to run: %v", err)
	}
	deleter, err := service.NewDeleter(t.Context(), flags)
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
