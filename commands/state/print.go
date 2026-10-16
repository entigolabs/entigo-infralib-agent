package state

import (
	"io"
	"slices"
	"strings"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/hashicorp/go-version"
	"gopkg.in/yaml.v3"
)

const unknownSource = "unknown"

type stepModules struct {
	step    *model.StateStep
	modules []*model.StateModule
}

type versionGroup struct {
	version string
	steps   []*stepModules
}

type sourceGroup struct {
	source   string
	versions []*versionGroup
}

type groupedState struct {
	sources    []*sourceGroup
	custom     []*stepModules
	notApplied []*stepModules
}

func groupState(state *model.State) groupedState {
	var grouped groupedState
	for _, step := range state.Steps {
		for _, module := range step.Modules {
			switch {
			case module.AppliedVersion == nil:
				grouped.notApplied = addModule(grouped.notApplied, step, module)
			case module.Type != nil && *module.Type == model.ModuleTypeCustom:
				grouped.custom = addModule(grouped.custom, step, module)
			default:
				vg := getSourceGroup(&grouped.sources, module.Source).getVersionGroup(*module.AppliedVersion)
				vg.steps = addModule(vg.steps, step, module)
			}
		}
	}
	for _, source := range grouped.sources {
		slices.SortStableFunc(source.versions, func(a, b *versionGroup) int {
			return compareVersions(b.version, a.version)
		})
	}
	return grouped
}

func getSourceGroup(sources *[]*sourceGroup, url string) *sourceGroup {
	if url == "" {
		url = unknownSource
	}
	for _, source := range *sources {
		if source.source == url {
			return source
		}
	}
	source := &sourceGroup{source: url}
	*sources = append(*sources, source)
	return source
}

func (g *sourceGroup) getVersionGroup(version string) *versionGroup {
	for _, vg := range g.versions {
		if vg.version == version {
			return vg
		}
	}
	vg := &versionGroup{version: version}
	g.versions = append(g.versions, vg)
	return vg
}

func addModule(steps []*stepModules, step *model.StateStep, module *model.StateModule) []*stepModules {
	for _, sm := range steps {
		if sm.step == step {
			sm.modules = append(sm.modules, module)
			return steps
		}
	}
	return append(steps, &stepModules{step: step, modules: []*model.StateModule{module}})
}

func compareVersions(a, b string) int {
	va, errA := version.NewVersion(a)
	vb, errB := version.NewVersion(b)
	switch {
	case errA == nil && errB == nil:
		return va.Compare(vb)
	case errA == nil:
		return 1
	case errB == nil:
		return -1
	default:
		return strings.Compare(b, a)
	}
}

func printGroups(w io.Writer, grouped groupedState) error {
	root := mappingNode()
	if len(grouped.sources) > 0 {
		sources := mappingNode()
		for _, source := range grouped.sources {
			versions := mappingNode()
			for _, vg := range source.versions {
				addPair(versions, vg.version, stepsNode(vg.steps, addSourceModules))
			}
			addPair(sources, source.source, versions)
		}
		addPair(root, "sources", sources)
	}
	if len(grouped.custom) > 0 {
		addPair(root, "custom", stepsNode(grouped.custom, func(step *yaml.Node, modules []*model.StateModule) {
			addPair(step, "modules", moduleVersionsNode(modules, func(m *model.StateModule) string {
				return *m.AppliedVersion
			}))
		}))
	}
	if len(grouped.notApplied) > 0 {
		addPair(root, "not_applied", stepsNode(grouped.notApplied, func(step *yaml.Node, modules []*model.StateModule) {
			addPair(step, "modules", moduleVersionsNode(modules, func(m *model.StateModule) string {
				return m.Version
			}))
		}))
	}
	encoder := yaml.NewEncoder(w)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return err
	}
	return encoder.Close()
}

func stepsNode(steps []*stepModules, addModules func(*yaml.Node, []*model.StateModule)) *yaml.Node {
	node := mappingNode()
	for _, sm := range steps {
		step := mappingNode()
		if !sm.step.AppliedAt.IsZero() {
			addPair(step, "applied_at", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp",
				Value: sm.step.AppliedAt.UTC().Format(time.RFC3339)})
		}
		addModules(step, sm.modules)
		addPair(node, sm.step.Name, step)
	}
	return node
}

func addSourceModules(step *yaml.Node, modules []*model.StateModule) {
	names := &yaml.Node{Kind: yaml.SequenceNode}
	var planned []*model.StateModule
	for _, module := range modules {
		names.Content = append(names.Content, stringNode(module.Name))
		if module.Version != "" && module.Version != *module.AppliedVersion {
			planned = append(planned, module)
		}
	}
	addPair(step, "modules", names)
	if len(planned) > 0 {
		addPair(step, "planned", moduleVersionsNode(planned, func(m *model.StateModule) string {
			return m.Version
		}))
	}
}

func moduleVersionsNode(modules []*model.StateModule, version func(*model.StateModule) string) *yaml.Node {
	node := mappingNode()
	for _, module := range modules {
		addPair(node, module.Name, stringNode(version(module)))
	}
	return node
}

func mappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode}
}

func stringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func addPair(node *yaml.Node, key string, value *yaml.Node) {
	node.Content = append(node.Content, stringNode(key), value)
}
