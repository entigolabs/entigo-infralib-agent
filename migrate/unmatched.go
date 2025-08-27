package migrate

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"log/slog"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"gopkg.in/yaml.v3"
)

type UnmatchedFinder interface {
	Find()
}

type unmatchedFinder struct {
	ctx    context.Context
	state  stateV4
	config importConfig
}

func NewUnmatchedFinder(ctx context.Context, flags common.Migrate) (UnmatchedFinder, error) {
	state, err := getState(flags.StateFile)
	if err != nil {
		return nil, err
	}
	if state.Version != 4 {
		return nil, fmt.Errorf("unsupported state version: %d", state.Version)
	}
	config, err := getConfig(flags.ImportFile)
	if err != nil {
		return nil, err
	}
	return &unmatchedFinder{
		ctx:    ctx,
		state:  state,
		config: config,
	}, nil
}

func (u *unmatchedFinder) Find() {
	log.Println("Finding unmatched resources...")
	resourcesWithIndexes := make(map[ResourceKey][]interface{})
	for _, item := range u.config.Import {
		itemResources, err := u.processItem(item)
		if err != nil {
			slog.Error(common.PrefixError(err))
		}
		for resourceKey, indexes := range itemResources {
			if _, exists := resourcesWithIndexes[resourceKey]; !exists {
				resourcesWithIndexes[resourceKey] = indexes
			} else {
				resourcesWithIndexes[resourceKey] = append(resourcesWithIndexes[resourceKey], indexes...)
			}
		}
	}
	missingResources := u.findMissingResources(resourcesWithIndexes)
	if len(missingResources) == 0 {
		log.Println("No unmatched resources found.")
		return
	}
	log.Printf("%d unmatched resources found\n", len(missingResources))
	importYaml, err := yaml.Marshal(importConfig{Import: missingResources})
	if err == nil {
		fmt.Println(string(importYaml))
	} else {
		slog.Error("Failed to marshal unmatched resources to YAML", "error", err)
		for _, item := range missingResources {
			log.Printf("Type: %s, Name: %s, Module: %s, IndexKeys: %v\n",
				item.Type, item.Name, item.Module, formatIndexKeys(item.IndexKeys))
		}
	}
}

func (u *unmatchedFinder) processItem(item importItem) (map[ResourceKey][]interface{}, error) {
	err := processItem(&item)
	if err != nil {
		return nil, err
	}
	resources, err := u.getResources(item.Type, item.Source)
	if err != nil {
		return nil, err
	}
	indexKeys, err := getIndexKeys(item)
	if err != nil {
		return nil, err
	}
	resourceWithIndexes := make(map[ResourceKey][]interface{})
	for _, resource := range resources {
		if len(resource.Instances) == 0 {
			continue
		}
		resourceWithIndexes[ResourceKey{
			Type:   resource.Type,
			Module: resource.Module,
			Name:   resource.Name,
		}] = getResourceIndexes(indexKeys, resource)
	}
	return resourceWithIndexes, nil
}

func (u *unmatchedFinder) findMissingResources(resources map[ResourceKey][]interface{}) []importItem {
	missingResources := make([]importItem, 0)
	for _, resource := range u.state.Resources {
		if resource.Mode != "managed" {
			continue
		}
		resourceKey := ResourceKey{
			Type:   resource.Type,
			Module: resource.Module,
			Name:   resource.Name,
		}
		missingResource := u.getMissingResource(resources, resourceKey, resource)
		if missingResource != nil {
			missingResources = append(missingResources, *missingResource)
		}
	}
	return missingResources
}

func (u *unmatchedFinder) getMissingResource(resources map[ResourceKey][]interface{}, resourceKey ResourceKey, resource resourceStateV4) *importItem {
	item := &importItem{
		Type:   resource.Type,
		Name:   resource.Name,
		Module: resource.Module,
	}
	indexes, ok := resources[resourceKey]
	if !ok {
		indexes = getResourceIndexes(nil, resource)
		if len(indexes) > 0 && indexes[0] != nil {
			item.IndexKeys = getResourceIndexes(nil, resource)
		}
		return item
	}
	var missingIndexes []interface{}
	for _, instance := range resource.Instances {
		found := false
		for _, index := range indexes {
			equal, err := compareValues(instance.IndexKey, index)
			if err != nil {
				slog.Error("Error comparing index keys", "error", err, "indexKey", instance.IndexKey, "index", index)
				continue
			}
			if equal {
				found = true
				break
			}
		}
		if !found && instance.IndexKey != nil {
			missingIndexes = append(missingIndexes, instance.IndexKey)
		}
	}
	if len(missingIndexes) == 0 {
		return nil
	}
	item.IndexKeys = missingIndexes
	return item
}

func (u *unmatchedFinder) getResources(rsType string, module module) ([]resourceStateV4, error) {
	var matching []resourceStateV4
	for _, resource := range u.state.Resources {
		if resource.Mode != "managed" {
			continue
		}
		if resource.Type != rsType {
			continue
		}
		if module.Module != "" && module.Module != resource.Module {
			continue
		}
		if module.Name != "" {
			if resource.Name == module.Name {
				return append(matching, resource), nil
			}
			continue
		}
		matching = append(matching, resource)
	}
	if len(matching) == 0 {
		return nil, fmt.Errorf("resource of type '%s' module '%s' name '%s' not found in state file",
			rsType, module.Module, module.Name)
	}
	return matching, nil
}

func getResourceIndexes(indexKeys []KeyPair, resource resourceStateV4) []interface{} {
	var indexes []interface{}
	if len(indexKeys) != 0 {
		for _, index := range indexKeys {
			indexes = append(indexes, index.Key1)
		}
		return indexes
	}
	for _, instance := range resource.Instances {
		indexes = append(indexes, instance.IndexKey)
	}
	if len(indexes) == 0 {
		indexes = append(indexes, nil)
	}
	return indexes
}

func formatIndexKeys(indexKeys []interface{}) []string {
	var keys []string
	for _, indexKey := range indexKeys {
		if indexKey == nil {
			continue
		}
		switch v := indexKey.(type) {
		case string:
			keys = append(keys, v)
		case int:
			keys = append(keys, fmt.Sprintf("%d", v))
		case float64:
			keys = append(keys, fmt.Sprintf("%g", v))
		default:
			slog.Warn(fmt.Sprintf("unsupported index key type: %T, skipping", v))
		}
	}
	return keys
}
