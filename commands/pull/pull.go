package pull

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"gopkg.in/yaml.v3"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func Run(ctx context.Context, flags *common.Flags) {
	provider := service.GetCloudProvider(ctx, flags)
	resources := provider.GetResources()
	conf := service.GetRemoteConfig(nil, resources.GetCloudPrefix(), resources.GetBucket())
	basePath := ""
	if flags.Config != "" {
		basePath = filepath.Dir(flags.Config) + "/"
	}
	existingFiles := getExistingFiles(flags.Config, conf, basePath)
	if len(existingFiles) != 0 {
		if !flags.Force {
			log.Fatalf("Files already exist in config folder. Use force to overwrite. Files: %s", strings.Join(existingFiles, ", "))
		} else {
			log.Printf("Force flag set. Overwriting existing files: %s", strings.Join(existingFiles, ", "))
		}
	}
	if flags.Force {
		removeConfigFolder(basePath)
	}
	err := writeConfigFiles(conf, flags.Config, basePath)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Config files written successfully")
}

func getExistingFiles(config string, conf model.Config, basePath string) []string {
	files := make([]string, 0)
	if config != "" {
		files = append(files, config)
	} else if util.FileExists(basePath, service.ConfigFile) {
		files = append(files, service.ConfigFile)
	}
	if conf.Steps == nil {
		return files
	}
	for _, step := range conf.Steps {
		files = append(files, getModuleFiles(step, basePath)...)
		if step.Files == nil {
			continue
		}
		for _, file := range step.Files {
			if util.FileExists(basePath, file.Name) {
				files = append(files, file.Name)
			}
		}
	}
	return files
}

func getModuleFiles(step model.Step, basePath string) []string {
	files := make([]string, 0)
	if step.Modules == nil {
		return files
	}
	for _, module := range step.Modules {
		if module.InputsFile == "" {
			continue
		}
		if util.FileExists(basePath, module.InputsFile) {
			files = append(files, module.InputsFile)
		}
	}
	return files
}

func removeConfigFolder(basePath string) {
	fullPath := filepath.Join(basePath, "config")
	err := os.RemoveAll(fullPath)
	if err != nil {
		log.Fatalf("Failed to delete folder %s: %v", fullPath, err)
	}
	slog.Debug(fmt.Sprintf("Deleted folder %s", fullPath))
}

func writeConfigFiles(conf model.Config, config string, basePath string) error {
	bytes, err := yaml.Marshal(conf)
	if err != nil {
		log.Fatalf("Failed to marshal config: %s", err)
	}
	if config == "" {
		config = filepath.Join(basePath, service.ConfigFile)
	}
	err = writeFile(config, bytes)
	if err != nil {
		return err
	}
	if conf.Steps == nil {
		return nil
	}
	for _, step := range conf.Steps {
		err = writeModuleFiles(step, basePath)
		if err != nil {
			return err
		}
		if step.Files == nil {
			continue
		}
		for _, file := range step.Files {
			err = writeFile(filepath.Join(basePath, file.Name), file.Content)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func writeModuleFiles(step model.Step, basePath string) error {
	if step.Modules == nil {
		return nil
	}
	for _, module := range step.Modules {
		if module.InputsFile == "" {
			continue
		}
		bytes, err := yaml.Marshal(module.Inputs)
		if err != nil {
			log.Fatalf("Failed to marshal module inputs: %s", err)
		}
		err = writeFile(basePath+module.InputsFile, bytes)
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(file string, content []byte) error {
	path := filepath.Dir(file)
	err := os.MkdirAll(path, 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %v", path, err)
	}
	err = os.WriteFile(file, content, 0644)
	if err != nil {
		return fmt.Errorf("failed to write file %s: %v", file, err)
	}
	slog.Debug(fmt.Sprintf("Wrote file %s", file))
	return nil
}
