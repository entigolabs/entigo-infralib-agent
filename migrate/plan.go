package migrate

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/common"
	"gopkg.in/yaml.v3"
)

//go:embed types.yaml
var typesYaml []byte

var replaceRegex = regexp.MustCompile(`\{\s*(.*?)\s*}`)
var indexRegex = regexp.MustCompile(`^(.*?)\[(\S+)]$`)

type Planner interface {
	Plan()
}

type planner struct {
	ctx    context.Context
	types  map[string]typeIdentification
	state  stateV4
	plan   plan
	config importConfig
}

func NewPlanner(ctx context.Context, flags common.Migrate) (Planner, error) {
	state, err := getState(flags.StateFile)
	if err != nil {
		return nil, err
	}
	if state.Version != 4 {
		return nil, fmt.Errorf("unsupported state version: %d", state.Version)
	}
	types, err := getTypes(flags.TypesFile)
	if err != nil {
		return nil, err
	}
	planFile, err := getPlan(flags.PlanFile)
	if err != nil {
		return nil, err
	}
	config, err := getConfig(flags.ImportFile)
	if err != nil {
		return nil, err
	}
	return &planner{
		ctx:    ctx,
		types:  types,
		state:  state,
		plan:   planFile,
		config: config,
	}, nil
}

func getTypes(typesFile string) (map[string]typeIdentification, error) {
	rawYaml := typesYaml
	if typesFile != "" {
		var err error
		rawYaml, err = os.ReadFile(typesFile)
		if err != nil {
			return nil, err
		}
	}
	var types typesConfig
	err := yaml.Unmarshal(rawYaml, &types)
	if err != nil {
		return nil, err
	}
	typeMap := make(map[string]typeIdentification)
	for _, typeIdentification := range types.TypeIdentifications {
		for _, typeName := range typeIdentification.Types {
			if id, found := typeMap[typeName]; found {
				return nil, fmt.Errorf("type %s identification already exists in map: %s", typeName, id)
			}
			typeMap[typeName] = typeIdentification
		}
	}
	return typeMap, nil
}

func getState(stateFile string) (stateV4, error) {
	fileBytes, err := os.ReadFile(stateFile)
	if err != nil {
		return stateV4{}, err
	}
	var state stateV4
	err = json.Unmarshal(fileBytes, &state)
	if err != nil {
		return stateV4{}, err
	}
	return state, nil
}

func getPlan(planFile string) (plan, error) {
	fileBytes, err := os.ReadFile(planFile)
	if err != nil {
		return plan{}, err
	}
	var tfPlan plan
	err = json.Unmarshal(fileBytes, &tfPlan)
	if err != nil {
		return plan{}, err
	}
	return tfPlan, nil
}

func getConfig(stateFile string) (importConfig, error) {
	if stateFile == "" {
		return importConfig{}, nil
	}
	fileBytes, err := os.ReadFile(stateFile)
	if err != nil {
		return importConfig{}, err
	}
	var config importConfig
	err = yaml.Unmarshal(fileBytes, &config)
	if err != nil {
		return importConfig{}, err
	}
	return config, nil
}

func (p *planner) Plan() {
	log.Println("Planning migration")
	var imports []string
	var removes []string
	for _, item := range p.config.Import {
		itemImports, itemRemoves, err := p.planItem(item)
		if err != nil {
			slog.Error(common.PrefixError(err))
			continue
		}
		imports = append(imports, itemImports...)
		removes = append(removes, itemRemoves...)
	}
	for _, cmd := range imports {
		fmt.Println(cmd)
	}
	fmt.Println()
	for _, cmd := range removes {
		fmt.Println(cmd)
	}
}

func (p *planner) planItem(item importItem) ([]string, []string, error) {
	err := processItem(&item)
	if err != nil {
		return nil, nil, err
	}
	identification, found := p.types[item.Type]
	if !found {
		return nil, nil, fmt.Errorf("type %s not found in typeIdentifications", item.Type)
	}
	resources, err := p.getResources(item.Type, item.Source)
	if err != nil {
		return nil, nil, err
	}
	indexKeys, err := getIndexKeys(item)
	if err != nil {
		return nil, nil, err
	}
	imports, removes := p.planResources(item, resources, identification, indexKeys)
	return imports, removes, nil
}

func (p *planner) planResources(item importItem, resources []resourceStateV4, identification typeIdentification, indexKeys []KeyPair) ([]string, []string) {
	var imports, removes []string
	for _, resource := range resources {
		if len(resource.Instances) == 0 {
			slog.Warn(fmt.Sprintf("no instances found for type '%s' resource '%s'", item.Type, resource.Name))
			continue
		}
		name := ""
		if len(resources) > 1 {
			name = resource.Name
		}
		source := getReference(item.Type, item.Source, resource.Name, resource.Module)
		name, dstModule, index, err := getDestination(item, p.plan.PlannedValues.RootModule, name)
		if err != nil {
			slog.Error(fmt.Sprintf("item type '%s' resource '%s' %s", item.Type, resource.Name, err))
			continue
		}
		dest := getReference(item.Type, item.Destination, name, dstModule)
		indexes := getPlannedIndexes(indexKeys, resource)
		rsImports, rsRemoves, err := p.planItemKeys(indexes, resource, identification, index, dest, source)
		if err != nil {
			slog.Error(fmt.Sprintf("item type '%s' resource '%s' %s", item.Type, resource.Name, err))
			continue
		}
		imports = append(imports, rsImports...)
		removes = append(removes, rsRemoves...)
	}
	return imports, removes
}

func processItem(item *importItem) error {
	if item.Name != "" {
		item.Source.Name = item.Name
		item.Destination.Name = item.Name
	}
	if item.Module != "" {
		item.Source.Module = item.Module
		item.Destination.Module = item.Module
	}
	if len(item.IndexKeys) > 0 {
		item.Source.IndexKeys = item.IndexKeys
		item.Destination.IndexKeys = item.IndexKeys
	}
	var err error
	item.Source, err = parseNameIndex(item.Source)
	if err != nil {
		return err
	}
	item.Destination, err = parseNameIndex(item.Destination)
	return err
}

func getDestination(item importItem, rootModule modulePlan, rsName string) (string, string, interface{}, error) {
	name := item.Destination.Name
	if name == "" {
		name = rsName
		item.Destination.Name = rsName
	}
	dstModule := item.Destination.Module
	if name != "" && dstModule != "" {
		return name, dstModule, nil, nil
	}
	plannedResource, names, err := getPlannedResource(item, rootModule)
	if err != nil {
		return "", "", nil, err
	}
	if plannedResource == nil {
		return "", "", nil, fmt.Errorf("resource of type '%s' module '%s' name '%s' not found in plan file",
			item.Type, dstModule, name)
	}
	if len(names) > 1 {
		return "", "", nil, fmt.Errorf("multiple plan resources of type '%s' module '%s' found: %s", item.Type,
			dstModule, strings.Join(names, ", "))
	}
	name = plannedResource.Name
	typeIndex := strings.Index(plannedResource.Address, item.Type)
	if typeIndex > 0 {
		dstModule = plannedResource.Address[0 : typeIndex-1]
	}
	return name, dstModule, plannedResource.Index, nil
}

func parseNameIndex(module module) (module, error) {
	matches := indexRegex.FindStringSubmatch(module.Name)
	if len(matches) == 3 {
		if module.IndexKey != nil || len(module.IndexKeys) > 0 {
			return module, fmt.Errorf("item name %s includes index but indexKey or indexKeys already set", module.Name)
		}
		module.Name = matches[1]
		key, err := strconv.Atoi(matches[2])
		if err == nil {
			module.IndexKey = key
		} else {
			module.IndexKey = strings.Trim(strings.Trim(matches[2], "\""), "'")
		}
		return module, nil
	}
	return module, nil
}

func (p *planner) planItemKeys(indexKeys []KeyPair, resource resourceStateV4, identification typeIdentification, index interface{}, dest, source string) ([]string, []string, error) {
	separator := "/"
	if identification.ListSeparator != "" {
		separator = identification.ListSeparator
	}
	var imports []string
	var removes []string
	for _, keys := range indexKeys {
		instance, err := getResourceInstance(resource, keys.Key1)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get instance: %s", err)
		}
		id, err := getReplacedIdentification(identification.Identification, instance, separator)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to replace identification %s: %s",
				identification.Identification, err)
		}
		key := keys.Key2
		if key == nil {
			key = index
		}
		indexed, err := addIndex(dest, key)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to add index to reference %s: %s", dest, err)
		}
		importCommand := fmt.Sprintf("terraform import \"%s\" \"%s\"", indexed, id)
		indexed, err = addIndex(source, keys.Key1)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to add index to reference %s: %s", source, err)
		}
		stateRmCommand := fmt.Sprintf("terraform state rm \"%s\"", indexed)
		imports = append(imports, importCommand)
		removes = append(removes, stateRmCommand)
	}
	return imports, removes, nil
}

func getIndexKeys(item importItem) ([]KeyPair, error) {
	if len(item.Source.IndexKeys) == 0 {
		if item.Source.IndexKey != nil || item.Destination.IndexKey != nil {
			return []KeyPair{newKeyPair(item.Source.IndexKey, item.Destination.IndexKey)}, nil
		}
		return nil, nil
	}
	if len(item.Source.IndexKeys) != len(item.Destination.IndexKeys) {
		return nil, fmt.Errorf("source and destination index keys must have the same length for type %s", item.Type)
	}
	var keys []KeyPair
	for i := 0; i < len(item.Source.IndexKeys); i++ {
		keys = append(keys, newKeyPair(item.Source.IndexKeys[i], item.Destination.IndexKeys[i]))
	}
	return keys, nil
}

func (p *planner) getResources(rsType string, module module) ([]resourceStateV4, error) {
	var matching []resourceStateV4
	var names []string
	for _, resource := range p.state.Resources {
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
		names = append(names, resource.Name)
	}
	if len(matching) == 0 {
		return nil, fmt.Errorf("resource of type '%s' module '%s' name '%s' not found in state file",
			rsType, module.Module, module.Name)
	} else if len(matching) > 1 {
		slog.Warn(fmt.Sprintf("multiple state resources of type '%s' found, no name was given: %s", rsType, strings.Join(names, ", ")))
	}
	return matching, nil
}

func getPlannedResource(item importItem, module modulePlan) (*resourcePlan, []string, error) {
	var found *resourcePlan
	var names []string
	for _, resource := range module.Resources {
		if resource.Mode != "managed" {
			continue
		}
		if resource.Type != item.Type {
			continue
		}
		if item.Destination.Module != "" && !strings.HasPrefix(resource.Address, item.Destination.Module) {
			continue
		}
		if item.Destination.Name != "" {
			if resource.Name == item.Destination.Name {
				return &resource, nil, nil
			} else {
				continue
			}
		}
		if found == nil {
			found = &resource
		}
		names = append(names, resource.Name)
	}
	for _, childModule := range module.ChildModules {
		child, childNames, err := getPlannedResource(item, childModule)
		if err != nil {
			return nil, nil, err
		}
		if child == nil {
			continue
		}
		names = append(names, childNames...)
		if found == nil {
			found = child
		}
	}
	return found, names, nil
}

func getReference(rsType string, module module, name, resourceModule string) string {
	var parts []string
	if module.Module != "" {
		parts = append(parts, module.Module)
	} else if resourceModule != "" {
		parts = append(parts, resourceModule)
	}
	parts = append(parts, rsType)
	if module.Name != "" {
		parts = append(parts, module.Name)
	} else if name != "" {
		parts = append(parts, name)
	}
	return strings.Join(parts, ".")
}

func getPlannedIndexes(indexKeys []KeyPair, resource resourceStateV4) []KeyPair {
	if len(indexKeys) != 0 {
		return indexKeys
	}
	var indexes []KeyPair
	for _, instance := range resource.Instances {
		if instance.IndexKey == nil {
			continue
		}
		indexes = append(indexes, newKeyPair(instance.IndexKey, instance.IndexKey))
	}
	if len(indexes) == 0 {
		indexes = append(indexes, newKeyPair(nil, nil))
	}
	return indexes
}

func getResourceInstance(resource resourceStateV4, key interface{}) (instanceObjectStateV4, error) {
	if key == nil {
		return resource.Instances[0], nil
	}
	if value, ok := key.(int); ok {
		if len(resource.Instances) <= value {
			return instanceObjectStateV4{}, fmt.Errorf("key index %d out of range", value)
		}
		return resource.Instances[value], nil
	}
	for _, instance := range resource.Instances {
		equal, err := compareValues(instance.IndexKey, key)
		if err != nil {
			return instanceObjectStateV4{}, err
		}
		if equal {
			return instance, nil
		}
	}
	return instanceObjectStateV4{}, fmt.Errorf("instance with key %v not found", key)
}

func compareValues(a, b interface{}) (bool, error) {
	if a == nil && b == nil {
		return true, nil
	}
	switch a := a.(type) {
	case string:
		if b, ok := b.(string); ok {
			return a == b, nil
		}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b), nil
	}
	return false, fmt.Errorf("incompatible types: %T and %T", a, b)
}

func addIndex(reference string, indexKey interface{}) (string, error) {
	index, err := getIndexKey(indexKey)
	if err != nil {
		return "", err
	}
	if index == "" {
		return reference, nil
	}
	return reference + index, nil
}

func getIndexKey(indexKey interface{}) (string, error) {
	if indexKey == nil {
		return "", nil
	}
	switch v := indexKey.(type) {
	case string:
		return fmt.Sprintf(`[\"%s\"]`, v), nil
	case int:
		return fmt.Sprintf("[%d]", v), nil
	case float64:
		return fmt.Sprintf("[%g]", v), nil
	default:
		return "", fmt.Errorf("unsupported index key type: %T", v)
	}
}

func getReplacedIdentification(identification string, instance instanceObjectStateV4, separator string) (string, error) {
	matches := replaceRegex.FindAllStringSubmatch(identification, -1)
	if len(matches) == 0 {
		return identification, nil
	}
	var values map[string]interface{}
	err := json.Unmarshal(instance.AttributesRaw, &values)
	if err != nil {
		return "", err
	}
	for _, match := range matches {
		replaceTag := match[0]
		replaceKey := match[1]
		replaceValue, err := getJsonValue(values, replaceKey, separator)
		if err != nil {
			return "", err
		}
		identification = strings.ReplaceAll(identification, replaceTag, replaceValue)
	}
	return identification, nil
}

func getJsonValue(values map[string]interface{}, key string, separator string) (string, error) {
	val, found := values[key]
	if !found {
		return "", fmt.Errorf("key %s not found", key)
	}
	switch v := val.(type) {
	case []interface{}:
		var values []string
		for _, value := range v {
			stringValue, err := getStringValue(value)
			if err != nil {
				return "", fmt.Errorf("unsupported value type for key %s: %T", key, val)
			}
			values = append(values, stringValue)
		}
		return strings.Join(values, separator), nil
	default:
		value, err := getStringValue(v)
		if err != nil {
			return "", fmt.Errorf("unsupported value type for key %s: %T", key, val)
		}
		return value, nil
	}
}

func getStringValue(v interface{}) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v), nil
	case float64, float32:
		return fmt.Sprintf("%g", v), nil
	default:
		return "", fmt.Errorf("unsupported value type: %T", v)
	}
}
