package agent

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(flags *common.Flags) {
	awsConfig := service.NewAWSConfig()
	stsService := service.NewSTS(awsConfig)

	accountID := stsService.GetAccountID()

	updater := service.NewUpdater(awsConfig, accountID, flags)
	updater.ProcessSteps()
}
