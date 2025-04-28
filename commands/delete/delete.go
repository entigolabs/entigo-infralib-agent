package delete

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log/slog"
)

func Delete(ctx context.Context, flags *common.Flags) error {
	slog.Warn(common.PrefixWarning(`Execute destroy pipelines in reverse config order before running this command.
This command will remove all pipelines and resources created by terraform will otherwise remain.`))
	if !flags.Delete.SkipConfirmation {
		fmt.Print("Do you want to delete the resources that the agent created? (Y/N): ")
		err := util.AskForConfirmation()
		if err != nil {
			return err
		}
	}
	deleter, err := service.NewDeleter(ctx, flags)
	if err != nil {
		return err
	}
	return deleter.Delete()
}
