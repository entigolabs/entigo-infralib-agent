package migrate

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"gopkg.in/yaml.v3"
	"log"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
)

//go:embed types.yaml
var typesYaml []byte

var replaceRegex = regexp.MustCompile(`\{\s*(.*?)\s*}`)
var indexRegex = regexp.MustCompile(`^(.*?)\[(\d+)]$`)

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

func NewPlanner(ctx context.Context, flags common.Migrate) Planner {
	state := getState(flags.StateFile)
	if state.Version != 4 {
		log.Fatalf("Unsupported state version: %d", state.Version)
	}
	return &planner{
		ctx:    ctx,
		types:  getTypes(flags.TypesFile),
		state:  state,
		plan:   getPlan(flags.PlanFile),
		config: getConfig(flags.ImportFile),
	}
}

func getTypes(typesFile string) map[string]typeIdentification {
	rawYaml := typesYaml
	if typesFile != "" {
		var err error
		rawYaml, err = os.ReadFile(typesFile)
		if err != nil {
			log.Fatal(&common.PrefixedError{Reason: err})
		}
	}
	var types typesConfig
	err := yaml.Unmarshal(rawYaml, &types)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	typeMap := make(map[string]typeIdentification)
	for _, typeIdentification := range types.TypeIdentifications {
		for _, typeName := range typeIdentification.Types {
			if id, found := typeMap[typeName]; found {
				log.Fatalf("Type %s identification already exists in map: %s", typeName, id)
			}
			typeMap[typeName] = typeIdentification
		}
	}
	return typeMap
}

func getState(stateFile string) stateV4 {
	fileBytes, err := os.ReadFile(stateFile)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	var state stateV4
	err = json.Unmarshal(fileBytes, &state)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	return state
}

func getPlan(planFile string) plan {
	fileBytes, err := os.ReadFile(planFile)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	var tfPlan plan
	err = json.Unmarshal(fileBytes, &tfPlan)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	return tfPlan
}

func getConfig(stateFile string) importConfig {
	fileBytes, err := os.ReadFile(stateFile)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	var config importConfig
	err = yaml.Unmarshal(fileBytes, &config)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	return config
}

func (p *planner) Plan() {
	log.Println("Planning migration")
	var imports []string
	var removes []string
	for _, item := range p.config.Import {
		itemImports, itemRemoves, err := p.planItem(item)
		if err != nil {
			slog.Error(err.Error())
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
	if item.Name != "" {
		item.Source.Name = item.Name
		item.Destination.Name = item.Name
	}
	var err error
	item.Source, err = parseNameIndex(item.Source)
	if err != nil {
		return nil, nil, err
	}
	item.Destination, err = parseNameIndex(item.Destination)
	if err != nil {
		return nil, nil, err
	}
	identification, found := p.types[item.Type]
	if !found {
		return nil, nil, fmt.Errorf("type %s not found in typeIdentifications", item.Type)
	}
	resource, err := p.getResource(item.Type, item.Source)
	if err != nil {
		return nil, nil, err
	}
	if len(resource.Instances) == 0 {
		return nil, nil, fmt.Errorf("no instances found for type %s", item.Type)
	}
	indexKeys, err := getIndexKeys(item)
	if err != nil {
		return nil, nil, err
	}
	source := getReference(item.Type, item.Source, resource.Name, resource.Module)
	var index interface{}
	var plannedResource *resourcePlan
	name := item.Destination.Name
	dstModule := item.Destination.Module
	if name == "" || dstModule == "" {
		plannedResource, err = getPlannedResource(item, p.plan.PlannedValues.RootModule)
		if err != nil {
			return nil, nil, err
		}
		if plannedResource == nil {
			return nil, nil, fmt.Errorf("resource of type '%s' module '%s' name '%s' not found in plan file",
				item.Type, dstModule, name)
		}
		name = plannedResource.Name
		typeIndex := strings.Index(plannedResource.Address, item.Type)
		if typeIndex > 0 {
			dstModule = plannedResource.Address[0 : typeIndex-1]
		}
		index = plannedResource.Index
	}
	dest := getReference(item.Type, item.Destination, name, dstModule)
	imports, removes, err := p.planItemKeys(indexKeys, resource, identification, index, dest, source)
	if err != nil {
		return nil, nil, fmt.Errorf("item type '%s' %s", item.Type, err)
	}
	return imports, removes, nil
}

func parseNameIndex(module module) (module, error) {
	matches := indexRegex.FindStringSubmatch(module.Name)
	if len(matches) == 3 {
		if module.IndexKey != nil || len(module.IndexKeys) > 0 {
			return module, fmt.Errorf("item name %s includes index but indexKey or indexKeys already set", module.Name)
		}
		module.Name = matches[1]
		key, err := strconv.Atoi(matches[2])
		if err != nil {
			return module, fmt.Errorf("item %s failed to parse index key %s: %v", module.Name, matches[2], err)
		}
		module.IndexKey = key
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
		return []KeyPair{newKeyPair(item.Source.IndexKey, item.Destination.IndexKey)}, nil
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

func (p *planner) getResource(rsType string, module module) (resourceStateV4, error) {
	var found *resourceStateV4
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
				return resource, nil
			}
			continue
		}
		if found != nil {
			return resourceStateV4{}, fmt.Errorf("multiple state resources of type %s found, name is required", rsType)
		}
		found = &resource
	}
	if found == nil {
		return resourceStateV4{}, fmt.Errorf("resource of type '%s' module '%s' name '%s' not found in state file",
			rsType, module.Module, module.Name)
	}
	return *found, nil
}

func getPlannedResource(item importItem, module modulePlan) (*resourcePlan, error) {
	var found *resourcePlan
	for _, resource := range module.Resources {
		if resource.Mode != "managed" {
			continue
		}
		if resource.Type != item.Type {
			continue
		}
		if item.Type == "aws_db_subnet_group" {
			fmt.Println("here")
		}
		if item.Destination.Module != "" && !strings.HasPrefix(resource.Address, item.Destination.Module) {
			continue
		}
		if item.Destination.Name != "" {
			if resource.Name == item.Destination.Name {
				return &resource, nil
			} else {
				continue
			}
		}
		if found != nil {
			return nil, fmt.Errorf("multiple plan resources of type %s found, name is required", item.Type)
		}
		found = &resource
	}
	for _, childModule := range module.ChildModules {
		child, err := getPlannedResource(item, childModule)
		if err != nil {
			return nil, err
		}
		if child == nil {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple plan resources of type %s found, name is required", item.Type)
		}
		found = child
	}
	return found, nil
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
