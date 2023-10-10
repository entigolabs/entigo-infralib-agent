package merge

import (
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"gopkg.in/yaml.v3"
)

func Merge(flags *common.Flags) {
	if flags.Config == "" {
		common.Logger.Fatal("Config file is required")
	}
	config := service.GetLocalConfig(flags.Config)
	baseConfig := service.GetLocalConfig(flags.BaseConfig)
	mergedConfig := service.MergeConfig(config, baseConfig)
	service.ValidateConfig(mergedConfig, nil)
	bytes, err := yaml.Marshal(mergedConfig)
	if err != nil {
		common.Logger.Fatalf("Failed to marshal config: %s", err)
	}
	fmt.Println(string(bytes))
}
