package delete

import (
	"bufio"
	"context"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"os"
	"strings"
)

func Delete(ctx context.Context, flags *common.Flags) {
	common.PrintWarning(`Execute destroy pipelines in reverse config order before running this command.
This command will remove all pipelines and resources created by terraform will otherwise remain.`)
	if !flags.Delete.SkipConfirmation {
		askForConfirmation()
	}
	deleter := service.NewDeleter(ctx, flags)
	deleter.Delete()
}

func askForConfirmation() {
	common.Logger.Print("Do you want to delete the resources that the agent created? (Y/N): ")
	for {
		reader := bufio.NewReader(os.Stdin)
		response, err := reader.ReadString('\n')
		if err != nil {
			common.Logger.Fatalf("Failed to read input: %v", err)
		}
		response = strings.ToLower(strings.TrimSpace(response))
		if response == "y" || response == "yes" {
			return
		} else if response == "n" || response == "no" {
			common.Logger.Fatalf("Operation cancelled.")
		} else {
			common.PrintWarning("Invalid input. Please enter Y or N.")
		}
	}
}
