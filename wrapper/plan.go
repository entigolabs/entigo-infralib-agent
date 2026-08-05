package wrapper

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/entigolabs/entigo-infralib-agent/gen/wrapper/v1alpha1"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

func readPlanSummary(planPath string) (*v1alpha1.PlanSummary, error) {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return nil, fmt.Errorf("read plan %s: %w", planPath, err)
	}
	var p model.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("unmarshal plan %s: %w", planPath, err)
	}
	return buildPlanSummary(p), nil
}

func buildPlanSummary(p model.Plan) *v1alpha1.PlanSummary {
	var root *v1alpha1.ModuleChanges
	modules := map[string]*v1alpha1.ModuleChanges{}
	getRoot := func() *v1alpha1.ModuleChanges {
		if root == nil {
			root = &v1alpha1.ModuleChanges{}
		}
		return root
	}
	getModule := func(name string) *v1alpha1.ModuleChanges {
		m, ok := modules[name]
		if !ok {
			m = &v1alpha1.ModuleChanges{}
			modules[name] = m
		}
		return m
	}
	bucketFor := func(moduleAddress string) *v1alpha1.ModuleChanges {
		name, isRoot := resolveModule(moduleAddress)
		if isRoot {
			return getRoot()
		}
		return getModule(name)
	}

	for _, rc := range p.ResourceChanges {
		bucket := classifyResourceAction(rc.Change.Actions)
		isMove := rc.PreviousAddress != "" && rc.PreviousAddress != rc.Address
		isImport := rc.Change.Importing != nil
		if bucket == "" && !isMove && !isImport {
			continue
		}
		m := bucketFor(rc.ModuleAddress)
		if isMove {
			m.Moved = append(m.Moved, &v1alpha1.ResourceMove{From: rc.PreviousAddress, To: rc.Address})
		}
		// Importing dominates the action bucket so a "create + import" lands in
		// Imported only — UI shouldn't list the same address under both.
		if isImport {
			m.Imported = append(m.Imported, rc.Address)
			continue
		}
		switch bucket {
		case "added":
			m.Added = append(m.Added, rc.Address)
		case "changed":
			m.Changed = append(m.Changed, rc.Address)
		case "destroyed":
			m.Destroyed = append(m.Destroyed, rc.Address)
		case "replaced":
			m.Replaced = append(m.Replaced, rc.Address)
		case "forgotten":
			m.Forgotten = append(m.Forgotten, rc.Address)
		}
	}

	for name, oc := range p.OutputChanges {
		action := classifyOutputAction(oc.Actions)
		if action == "" {
			continue
		}
		modName, rest := splitOutputName(name)
		var target *v1alpha1.ModuleChanges
		if modName == "" {
			target = getRoot()
		} else {
			target = getModule(modName)
		}
		if target.Outputs == nil {
			target.Outputs = &v1alpha1.OutputChanges{}
		}
		switch action {
		case "added":
			target.Outputs.Added = append(target.Outputs.Added, rest)
		case "changed":
			target.Outputs.Changed = append(target.Outputs.Changed, rest)
		case "destroyed":
			target.Outputs.Destroyed = append(target.Outputs.Destroyed, rest)
		}
	}

	if root != nil {
		sortModuleChanges(root)
	}
	for _, m := range modules {
		sortModuleChanges(m)
	}

	return &v1alpha1.PlanSummary{Root: root, Modules: modules}
}

func sortModuleChanges(m *v1alpha1.ModuleChanges) {
	sort.Strings(m.Added)
	sort.Strings(m.Changed)
	sort.Strings(m.Destroyed)
	sort.Strings(m.Replaced)
	sort.Strings(m.Imported)
	sort.Strings(m.Forgotten)
	sort.Slice(m.Moved, func(i, j int) bool { return m.Moved[i].To < m.Moved[j].To })
	if m.Outputs != nil {
		sort.Strings(m.Outputs.Added)
		sort.Strings(m.Outputs.Changed)
		sort.Strings(m.Outputs.Destroyed)
	}
}

// resolveModule classifies a resource's module_address. isRoot is true when
// the resource sits directly in the working dir (no module address); otherwise
// name is the first-level module's name. Submodules at any depth roll up to
// their first-level ancestor — the full address is preserved on the resource
// itself, so submodule identity is not lost.
func resolveModule(moduleAddress string) (name string, isRoot bool) {
	if moduleAddress == "" {
		return "", true
	}
	s := strings.TrimPrefix(moduleAddress, "module.")
	if i := strings.Index(s, "."); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "["); i >= 0 {
		s = s[:i]
	}
	return s, false
}

// splitOutputName interprets the `<module>__<rest>` convention. Returns
// ("", name) when the name has no `__` separator or an empty prefix.
func splitOutputName(name string) (module, rest string) {
	i := strings.Index(name, "__")
	if i <= 0 {
		return "", name
	}
	return name[:i], name[i+2:]
}

// Returns "" for no-op and read (data source refresh), which we drop.
func classifyResourceAction(actions []string) string {
	switch len(actions) {
	case 1:
		switch actions[0] {
		case "create":
			return "added"
		case "update":
			return "changed"
		case "delete":
			return "destroyed"
		case "forget":
			return "forgotten"
		}
	case 2:
		a, b := actions[0], actions[1]
		if (a == "delete" && b == "create") || (a == "create" && b == "delete") {
			return "replaced"
		}
	}
	return ""
}

// Outputs don't have replace/import/forget semantics — they're either gained,
// changed, or lost.
func classifyOutputAction(actions []string) string {
	if len(actions) != 1 {
		return ""
	}
	switch actions[0] {
	case "create":
		return "added"
	case "update":
		return "changed"
	case "delete":
		return "destroyed"
	}
	return ""
}
