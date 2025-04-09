package service

import (
	"encoding/json"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/aws"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"log"
	"log/slog"
)

func SetupEncryption(config model.Config, provider model.CloudProvider, resources model.Resources) error {
	if resources.GetProviderType() != model.AWS {
		return nil // TODO Remove when GCP encryption is implemented
	}
	moduleName, outputs, err := GetEncryptionOutputs(config, resources.GetCloudPrefix(), resources.GetBucket())
	if err != nil {
		return fmt.Errorf("failed to get outputs for %s: %v", moduleName, err)
	}
	if outputs == nil {
		return nil
	}
	return provider.AddEncryption(moduleName, outputs)
}

func GetEncryptionKey(providerType model.ProviderType, prefix, configFlag string, bucket model.Bucket) string {
	if providerType != model.AWS {
		return "" // TODO Remove when GCP encryption is implemented
	}
	exists, err := bucket.BucketExists()
	if err != nil {
		log.Fatalf("Failed to check bucket existence: %s", err)
	}
	if !exists {
		return ""
	}
	config := GetBaseConfig(prefix, configFlag, bucket)
	moduleName, outputs, err := GetEncryptionOutputs(config, prefix, bucket)
	if err != nil {
		log.Fatalf("Failed to get encryption outputs: %s", err)
	}
	if len(outputs) == 0 {
		return ""
	}
	keyId, err := aws.GetConfigEncryptionKey(moduleName, outputs)
	if err != nil {
		log.Fatalf("Failed to get encryption key: %s", err)
	}
	return keyId
}

func GetEncryptionOutputs(config model.Config, prefix string, bucket model.Bucket) (string, map[string]model.TFOutput, error) {
	step, module := getEncryptionModule(config)
	if step == nil || module == nil {
		return "", nil, nil
	}
	log.Printf("Processing encryption based on %s module %s\n", step.Name, module.Name)
	outputs, err := getModuleOutputs(*step, prefix, bucket)
	if err != nil {
		log.Fatalf("Failed to get outputs for %s: %v", step.Name, err)
	}
	return module.Name, outputs, nil
}

func getEncryptionModule(config model.Config) (*model.Step, *model.Module) {
	for _, step := range config.Steps {
		for _, module := range step.Modules {
			moduleType := getModuleType(module)
			if moduleType != "kms" {
				continue
			}
			return &step, &module
		}
	}
	return nil, nil
}

func getModuleOutputs(step model.Step, prefix string, bucket model.Bucket) (map[string]model.TFOutput, error) {
	filePath := fmt.Sprintf("%s-%s/%s", prefix, step.Name, terraformOutput)
	file, err := bucket.GetFile(filePath)
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]model.TFOutput)
	if file == nil {
		slog.Debug(fmt.Sprintf("terraform output file %s not found", filePath))
		return outputs, nil
	}
	err = json.Unmarshal(file, &outputs)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal terraform tfOutput file %s: %s", filePath, err)
	}
	return outputs, nil
}
