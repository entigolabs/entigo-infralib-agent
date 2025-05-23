package destroy

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log/slog"
)

func Destroy(ctx context.Context, flags *common.Flags) error {
	slog.Warn(common.PrefixWarning(`Destroy pipelines will be executed in reverse config order.
This will remove the resources provisioned by the step pipelines.`))
	if !flags.Delete.SkipConfirmation {
		fmt.Print("Do you want to run the destroy pipelines that remove the provisioned resources? (Y/N): ")
		err := util.AskForConfirmation()
		if err != nil {
			return err
		}
	}
	deleter, err := service.NewDeleter(ctx, flags)
	if err != nil {
		return err
	}
	deleter.Destroy()
	return nil
}
