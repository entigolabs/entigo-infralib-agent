package service

import (
	"fmt"
	"log"

	"github.com/entigolabs/entigo-infralib-agent/aws"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

func GetEncryptionKey(providerType model.ProviderType, prefix, configFlag string, bucket model.Bucket) (string, error) {
	if providerType != model.AWS {
		return "", nil // TODO Remove when GCP encryption is implemented
	}
	exists, err := bucket.BucketExists()
	if err != nil {
		return "", fmt.Errorf("failed to check bucket existence: %s", err)
	}
	if !exists {
		return "", nil
	}
	config, err := GetBaseConfig(prefix, configFlag, bucket)
	if err != nil {
		return "", err
	}
	moduleName, outputs, err := GetEncryptionOutputs(config, prefix, bucket)
	if err != nil {
		return "", fmt.Errorf("failed to get encryption outputs: %s", err)
	}
	if len(outputs) == 0 {
		return "", nil
	}
	keyId, err := aws.GetConfigEncryptionKey(moduleName, outputs)
	if err != nil {
		return "", fmt.Errorf("failed to get encryption key: %s", err)
	}
	return keyId, nil
}

func GetEncryptionOutputs(config model.Config, prefix string, bucket model.Bucket) (string, map[string]model.TFOutput, error) {
	step, module := getModuleByType(config, "kms")
	if step == nil || module == nil {
		return "", nil, nil
	}
	log.Printf("Processing encryption based on %s module %s\n", step.Name, module.Name)
	outputs, err := getModuleOutputs(*step, prefix, bucket)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get outputs for %s: %v", step.Name, err)
	}
	return module.Name, outputs, nil
}
