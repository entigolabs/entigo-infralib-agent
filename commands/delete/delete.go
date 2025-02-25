package delete

import (
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log"
)

func Delete(ctx context.Context, flags *common.Flags) {
	common.PrintWarning(`Execute destroy pipelines in reverse config order before running this command.
This command will remove all pipelines and resources created by terraform will otherwise remain.`)
	if !flags.Delete.SkipConfirmation {
		log.Print("Do you want to delete the resources that the agent created? (Y/N): ")
		util.AskForConfirmation()
	}
	deleter := service.NewDeleter(ctx, flags)
	deleter.Delete()
}
