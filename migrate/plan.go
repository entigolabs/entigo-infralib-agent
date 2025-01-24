package migrate

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"gopkg.in/yaml.v3"
	"log"
	"os"
	"regexp"
	"strings"
)

//go:embed types.yaml
var typesYaml []byte

var replaceRegex = regexp.MustCompile(`\{\s*(.*?)\s*}`)

type Planner interface {
	Plan()
}

type planner struct {
	ctx    context.Context
	types  map[string]string
	state  stateV4
	config importConfig
}

func NewPlanner(ctx context.Context, flags common.Migrate) Planner {
	state := getState(flags.StateFile)
	if state.Version != 4 {
		log.Fatalf("Unsupported state version: %d", state.Version)
	}
	return &planner{
		ctx:    ctx,
		types:  getTypes(),
		state:  state,
		config: getConfig(flags.ImportFile),
	}
}

func getTypes() map[string]string {
	var types typesConfig
	err := yaml.Unmarshal(typesYaml, &types)
	if err != nil {
		log.Fatal(&common.PrefixedError{Reason: err})
	}
	typeMap := make(map[string]string)
	for _, typeIdentification := range types.TypeIdentifications {
		for _, typeName := range typeIdentification.Types {
			if id, found := typeMap[typeName]; found {
				log.Fatalf("Type %s identification already exists in map: %s", typeName, id)
			}
			typeMap[typeName] = typeIdentification.Identification
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
	for _, item := range p.config.Import {
		identification, found := p.types[item.Type]
		if !found {
			log.Fatalf("Type %s not found in typeIdentifications", item.Type)
		}
		resource := p.getResource(item.Type, item.Source.Name)
		if len(resource.Instances) == 0 {
			log.Fatalf("No instances found for type %s", item.Type)
		}
		source := getReference(item.Type, item.Source, resource.Name)
		dest := getReference(item.Type, item.Destination, item.Destination.Name)
		indexKeys := getIndexKeys(item)

		for _, keys := range indexKeys {
			instance, err := getResourceInstance(resource, keys.Key1)
			if err != nil {
				log.Fatalf("Failed to get instance for type %s: %s", item.Type, err)
			}
			id, err := getReplacedIdentification(identification, instance)
			if err != nil {
				log.Fatalf("Failed to replace identification %s for type %s: %s", identification, item.Type, err)
			}

			importCommand := fmt.Sprintf("terraform import \"%s\" %s", addIndex(dest, keys.Key2), id)
			log.Println(importCommand)
			stateRmCommand := fmt.Sprintf("terraform state rm \"%s\"", addIndex(source, keys.Key1))
			log.Println(stateRmCommand)
		}
	}
}

func getIndexKeys(item importItem) []KeyPair {
	if len(item.Source.IndexKeys) == 0 {
		return []KeyPair{newKeyPair(item.Source.IndexKey, item.Destination.IndexKey)}
	}
	if len(item.Source.IndexKeys) != len(item.Destination.IndexKeys) {
		log.Fatalf("Source and destination index keys must have the same length")
	}
	var keys []KeyPair
	for i := 0; i < len(item.Source.IndexKeys); i++ {
		keys = append(keys, newKeyPair(item.Source.IndexKeys[i], item.Destination.IndexKeys[i]))
	}
	return keys
}

func (p *planner) getResource(rsType string, name string) resourceStateV4 {
	var found *resourceStateV4
	for _, resource := range p.state.Resources {
		if resource.Type != rsType {
			continue
		}
		if name != "" && resource.Name == name {
			return resource
		}
		if name != "" {
			continue
		}
		if found != nil {
			log.Fatalf("Multiple resources of type %s found, name is required", rsType)
		}
		found = &resource
	}
	if found == nil {
		log.Fatalf("Resource of type %s not found", rsType)
	}
	return *found
}

func getReference(rsType string, module module, name string) string {
	var parts []string
	if module.Module != "" {
		parts = append(parts, module.Module)
	}
	parts = append(parts, rsType)
	if module.Name != "" {
		parts = append(parts, module.Name)
	}
	if module.Name == "" && name != "" {
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

func addIndex(reference string, indexKey interface{}) string {
	index := getIndexKey(indexKey)
	if index == "" {
		return reference
	}
	return reference + index
}

func getIndexKey(indexKey interface{}) string {
	if indexKey == nil {
		return ""
	}
	switch v := indexKey.(type) {
	case string:
		return fmt.Sprintf(`[\"%s\"]`, v)
	case int:
		return fmt.Sprintf("[%d]", v)
	default:
		log.Fatalf("Unsupported index key type: %T", v)
	}
	return ""
}

func getReplacedIdentification(identification string, instance instanceObjectStateV4) (string, error) {
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
		replaceValue, err := getJsonValue(values, replaceKey)
		if err != nil {
			return "", err
		}
		identification = strings.ReplaceAll(identification, replaceTag, replaceValue)
	}
	return identification, nil
}

func getJsonValue(values map[string]interface{}, key string) (string, error) {
	val, found := values[key]
	if !found {
		return "", fmt.Errorf("key %s not found", key)
	}
	switch v := val.(type) {
	case string:
		return fmt.Sprintf("%s", val), nil
	case int:
		return fmt.Sprintf("%d", v), nil
	default:
		return "", fmt.Errorf("unsupported value type for key %s: %T", key, v)
	}
}
