package updater

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(flags *common.Flags) {
	config := service.GetConfig(flags.Config)

	awsConfig := service.NewAWSConfig()
	stsService := service.NewSTS(awsConfig)

	accountID := stsService.GetAccountID()

	steps := service.NewSteps(config, awsConfig, accountID, flags)

	steps.CreateStepsFiles()
	steps.CreateStepsPipelines()
}
