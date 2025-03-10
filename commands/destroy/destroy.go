package destroy

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log"
	"log/slog"
)

func Destroy(ctx context.Context, flags *common.Flags) {
	slog.Warn(common.PrefixWarning(`Destroy pipelines will be executed in reverse config order.
This will remove the resources provisioned by the step pipelines.`))
	if !flags.Delete.SkipConfirmation {
		log.Print("Do you want to run the destroy pipelines that remove the provisioned resources? (Y/N): ")
		util.AskForConfirmation()
	}
	deleter := service.NewDeleter(ctx, flags)
	deleter.Destroy()
}
