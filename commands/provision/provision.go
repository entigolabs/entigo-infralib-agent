package provision

import (
	"context"
	"fmt"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/wrapper"
)

// Run executes the Infralib Tool entrypoint.sh, piping its stdout through the
// humanize hook. When a wrapper config is supplied via the WRAPPER_CONFIG env
// var (resolved from secret manager by the pipeline), raw log lines are also
// forwarded to the portal backend over gRPC.
func Run(ctx context.Context, flags *common.Flags) error {
	wrap, err := wrapper.NewWrapper(ctx, flags.Wrapper)
	if err != nil {
		return fmt.Errorf("failed to initialize wrapper: %w", err)
	}
	return wrap.Provision()
}
