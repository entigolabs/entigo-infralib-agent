package updater

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(flags *common.Flags) {
	awsConfig := service.NewAWSConfig()
	stsService := service.NewSTS(awsConfig)

	accountID := stsService.GetAccountID()

	steps := service.NewSteps(awsConfig, accountID, flags)

	steps.CreateStepsFiles()
	steps.CreateStepsPipelines()
	// TODO update entigo-infralib-state.yaml with applied release tag after successful run
}
