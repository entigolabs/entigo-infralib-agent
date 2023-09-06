package run

import (
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
)

func Run(flags *common.Flags) {
	updater := service.NewUpdater(flags)
	updater.ProcessSteps()
}
