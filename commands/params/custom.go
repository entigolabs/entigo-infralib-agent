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

func Custom(ctx context.Context, flags *common.Flags, command common.Command) error {
	provider, err := service.GetResourceProvider(ctx, flags)
	if err != nil {
		return err
	}
	ssm, err := getSSM(provider, flags)
	if err != nil {
		return err
	}
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
	return err
}

func getSSM(provider model.ResourceProvider, flags *common.Flags) (model.SSM, error) {
	ssm, err := provider.GetSSM()
	if err != nil {
		return nil, err
	}
	if flags.Prefix == "" && flags.Config == "" {
		return ssm, nil
	}
	prefix, err := service.GetProviderPrefix(flags)
	if err != nil {
		return nil, err
	}
	bucket, err := provider.GetBucket(prefix)
	if err != nil {
		return nil, err
	}
	keyId, err := service.GetEncryptionKey(provider.GetProviderType(), prefix, flags.Config, bucket)
	if err != nil {
		return nil, err
	}
	if keyId == "" {
		return ssm, nil
	}
	ssm.AddEncryptionKeyId(keyId)
	return ssm, nil
}

func addParam(ssm model.SSM, params common.Params) error {
	exists, err := ssm.ParameterExists(params.Key)
	if err != nil {
		return err
	}
	if exists && !params.Overwrite {
		fmt.Printf("Parameter '%s' already exists. Overwrite the value? (Y/N):", params.Key)
		err = util.AskForConfirmation()
		if err != nil {
			return err
		}
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
