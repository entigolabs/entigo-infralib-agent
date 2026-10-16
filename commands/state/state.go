package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/service"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"gopkg.in/yaml.v3"
)

func Run(ctx context.Context, flags *common.Flags) error {
	bucket, err := getBucket(ctx, flags)
	if err != nil {
		return err
	}
	switch {
	case flags.State.Pull:
		return pull(bucket, flags.State.File, flags.Force)
	case flags.State.Push:
		return push(bucket, flags.State.File, flags.Delete.SkipConfirmation)
	default:
		state, err := service.GetLatestState(bucket)
		if err != nil {
			return err
		}
		return printState(os.Stdout, state)
	}
}

func getBucket(ctx context.Context, flags *common.Flags) (model.Bucket, error) {
	provider, err := service.GetResourceProvider(ctx, flags)
	if err != nil {
		return nil, err
	}
	prefix, err := service.GetProviderPrefix(flags)
	if err != nil {
		return nil, err
	}
	bucket, err := provider.GetBucket(strings.ToLower(prefix))
	if err != nil {
		return nil, err
	}
	exists, err := bucket.BucketExists()
	if err != nil {
		return nil, fmt.Errorf("failed to check bucket: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("bucket for prefix %s not found", prefix)
	}
	return bucket, nil
}

func pull(bucket model.Bucket, file string, force bool) error {
	if util.FileExists("", file) {
		if !force {
			return fmt.Errorf("file %s already exists. Use force to overwrite", file)
		}
		log.Printf("Force flag set. Overwriting existing file: %s", file)
	}
	content, err := bucket.GetFile(service.StateFile)
	if err != nil {
		return fmt.Errorf("failed to get state file: %w", err)
	}
	if content == nil {
		return fmt.Errorf("state file %s not found in bucket", service.StateFile)
	}
	if err = os.WriteFile(file, content, 0644); err != nil {
		return fmt.Errorf("failed to write file %s: %w", file, err)
	}
	log.Printf("State file written to %s", file)
	return nil
}

func push(bucket model.Bucket, file string, skipConfirmation bool) error {
	content, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("failed to read file %s: %w", file, err)
	}
	state, err := parseState(content)
	if err != nil {
		return fmt.Errorf("invalid state file %s: %w", file, err)
	}
	if err = printState(os.Stdout, state); err != nil {
		return err
	}
	slog.Warn(common.PrefixWarning(`A misconfigured state file can break agent run and update executions.
Make sure the module versions and sources match what has actually been provisioned.`))
	if !skipConfirmation {
		fmt.Print("Do you want to push the state file to the bucket? (Y/N): ")
		if err = util.AskForConfirmation(); err != nil {
			return err
		}
	}
	if err = bucket.PutFile(service.StateFile, content); err != nil {
		return fmt.Errorf("failed to put state file: %w", err)
	}
	log.Printf("State file %s pushed to the bucket", file)
	return nil
}

func parseState(content []byte) (*model.State, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var state model.State
	if err := decoder.Decode(&state); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("file is empty")
		}
		return nil, err
	}
	return &state, validateState(&state)
}

func validateState(state *model.State) error {
	steps := model.NewSet[string]()
	for i, step := range state.Steps {
		if step == nil || step.Name == "" {
			return fmt.Errorf("step at index %d has no name", i)
		}
		if steps.Contains(step.Name) {
			return fmt.Errorf("duplicate step %s", step.Name)
		}
		steps.Add(step.Name)
		modules := model.NewSet[string]()
		for j, module := range step.Modules {
			if module == nil || module.Name == "" {
				return fmt.Errorf("module at index %d in step %s has no name", j, step.Name)
			}
			if modules.Contains(module.Name) {
				return fmt.Errorf("duplicate module %s in step %s", module.Name, step.Name)
			}
			modules.Add(module.Name)
			if module.Type != nil && *module.Type != model.ModuleTypeCustom {
				return fmt.Errorf("module %s in step %s has unknown type %s", module.Name, step.Name, *module.Type)
			}
		}
	}
	return nil
}

func printState(w io.Writer, state *model.State) error {
	if len(state.Steps) == 0 {
		_, err := fmt.Fprintln(w, "No applied steps found")
		return err
	}
	return printGroups(w, groupState(state))
}
