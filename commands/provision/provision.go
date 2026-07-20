package provision

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/wrapper"
	"gopkg.in/yaml.v3"
)

// Run executes the Infralib Tool entrypoint.sh. When a wrapper config is
// supplied via the WRAPPER_CONFIG env var (resolved from secret manager by the
// pipeline), raw log lines and the plan summary are forwarded to the portal
// backend over gRPC; without a config, the wrapper is transparent.
func Run(ctx context.Context, flags *common.Flags) error {
	config, err := parseConfig(flags.Wrapper.Config)
	if err != nil {
		return err
	}
	if config != nil {
		// Oracle cloud runs deliver the campaign correlation out-of-band (the
		// container env is immutable and reused across executions).
		wrapper.ApplyOracleRunContext(ctx, &flags.Wrapper)
	}
	wrap, err := wrapper.NewWrapper(ctx, flags.Wrapper, config, os.Environ(), os.Stdout)
	if err != nil {
		return fmt.Errorf("failed to initialize wrapper: %w", err)
	}
	return wrap.Provision()
}

func parseConfig(raw string) (*model.NotificationApi, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var config model.NotificationApi
	if err := yaml.Unmarshal([]byte(raw), &config); err != nil {
		return nil, fmt.Errorf("failed to parse wrapper config: %w", err)
	}
	return &config, nil
}
