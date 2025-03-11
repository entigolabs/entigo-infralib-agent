package params

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"log"
)

func Custom(ctx context.Context, flags *common.Flags, command common.Command) {
	provider := service.GetResourceProvider(ctx, flags)
	ssm := provider.GetSSM()
	var err error
	switch command {
	case common.AddCustomCommand:
		err = addParam(ssm, flags.Params)
	case common.DeleteCustomCommand:
		err = deleteParam(ssm, flags.Params)
	case common.GetCustomCommand:
		err = getParam(ssm, flags.Params)
	case common.ListCustomCommand:
		err = listParams(ssm)
	}
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
}

func addParam(ssm model.SSM, params common.Params) error {
	exists, err := ssm.ParameterExists(params.Key)
	if err != nil {
		return err
	}
	if exists && !params.Overwrite {
		fmt.Printf("Parameter '%s' already exists. Overwrite the value? (Y/N):", params.Key)
		util.AskForConfirmation()
	}
	err = ssm.PutParameter(params.Key, params.Value)
	if err == nil {
		fmt.Printf("To use the parameter in your configuration use {{ .%s.%s }}\n", model.ReplaceTypeOutputCustom,
			params.Key)
	}
	return err
}

func deleteParam(ssm model.SSM, params common.Params) error {
	err := ssm.DeleteParameter(params.Key)
	if err == nil {
		log.Printf("Successfully deleted %s", params.Key)
	}
	return err
}

func getParam(ssm model.SSM, params common.Params) error {
	param, err := ssm.GetParameter(params.Key)
	if err != nil {
		return err
	}
	if param.Value == nil {
		fmt.Println("Param has empty value")
	} else {
		fmt.Printf("Value: %s\n", *param.Value)
	}
	fmt.Printf("To use the parameter in your configuration use {{ .%s.%s }}\n", model.ReplaceTypeOutputCustom,
		params.Key)
	return nil
}

func listParams(ssm model.SSM) error {
	keys, err := ssm.ListParameters()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("No custom parameters found")
	}
	for _, key := range keys {
		fmt.Println(key)
	}
	return nil
}
