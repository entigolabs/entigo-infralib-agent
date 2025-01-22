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
	"strings"
)

//go:embed types.yaml
var typesYaml []byte

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
		// TODO Index keys

		resource := p.getResource(item.Type, item.Source.Name)
		source := getReference(item.Type, item.Source, resource.Name)
		dest := getReference(item.Type, item.Destination, "")

		importCommand := fmt.Sprintf("terraform import \"%s\" %s", dest, identification)
		log.Println(importCommand)
		stateRmCommand := fmt.Sprintf("terraform state rm \"%s\"", source)
		log.Println(stateRmCommand)
	}
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
