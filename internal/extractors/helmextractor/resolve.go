package helmextractor

import (
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// resolveDependencies decides which charts each chart loads: every chart in its
// charts/ directory (Helm loads them whether or not Chart.yaml lists them), each
// file:// dependency, and each remote dependency the repository itself holds, which
// is the chart of that name when exactly one chart has it and its version satisfies
// the constraint. A chart monorepo publishes to the registry its charts depend on,
// so bitnami/nginx's `common` from oci://registry-1.docker.io/bitnamicharts is the
// bitnami/common beside it. Anything else is left to its registry.
func resolveDependencies(charts map[string]*chart, dirs []string) {
	byName := map[string][]*chart{}
	for _, d := range dirs {
		c := charts[d]
		byName[c.meta.Name] = append(byName[c.meta.Name], c)
	}
	for _, d := range dirs {
		c := charts[d]
		seen := map[*chart]bool{}
		add := func(t *chart) {
			if t != nil && t != c && !seen[t] {
				seen[t] = true
				c.deps = append(c.deps, t)
			}
		}
		subcharts := map[string]*chart{}
		for _, sd := range dirs {
			if factpath.Dir(sd) == factpath.Join(c.dir, "charts") {
				add(charts[sd])
				subcharts[charts[sd].meta.Name] = charts[sd]
			}
		}
		for _, dp := range c.meta.Dependencies {
			if dp.Name == "" {
				continue
			}
			if path, ok := strings.CutPrefix(dp.Repository, "file://"); ok {
				add(charts[factpath.Clean(factpath.Join(c.dir, path))])
				continue
			}
			if sub := subcharts[dp.Name]; sub != nil {
				continue // vendored: already loaded
			}
			if cands := byName[dp.Name]; len(cands) == 1 && cands[0] != c && satisfies(cands[0].meta.Version, dp.Version) {
				add(cands[0])
				c.byName = append(c.byName, dp.Name)
				continue
			}
			c.remote = append(c.remote, dp.Name)
		}
		sort.Strings(c.byName)
		sort.Strings(c.remote)
	}
}

// closure lists the charts c loads, transitively, in a stable order.
func closure(c *chart) []*chart {
	var out []*chart
	seen := map[*chart]bool{c: true}
	queue := append([]*chart{}, c.deps...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		out = append(out, next)
		queue = append(queue, next.deps...)
	}
	return out
}

// lookup resolves a named template from chart c: its own definition, else the one
// definition among the charts it loads.
func lookup(c *chart, loaded []*chart, name string) *define {
	if d := c.defines[name]; d != nil {
		return d
	}
	var found *define
	for _, l := range loaded {
		if d := l.defines[name]; d != nil {
			if found != nil {
				return nil
			}
			found = d
		}
	}
	return found
}

// nameObjects names each resource by its literal metadata.name, else by its file's
// path under templates/ without the extension. Two resources landing on one name
// (several documents of one kind in one file) keep the first, and the rest take
// their line.
func nameObjects(c *chart) {
	taken := map[string]bool{}
	for _, o := range c.objects {
		name := o.name
		if name == "" {
			name = strings.TrimSuffix(strings.TrimPrefix(o.file, prefix(c.dir)), factpath.Ext(o.file))
			name = strings.TrimPrefix(strings.TrimPrefix(name, "templates/"), "crds/")
		}
		sym := qualify(c.dir, o.kind+"."+name)
		if taken[sym] {
			sym += "@" + strconv.Itoa(o.line)
		}
		taken[sym] = true
		o.sym = sym
	}
	for _, d := range c.defines {
		d.sym = qualify(c.dir, "template."+d.name)
	}
}

func emit(charts map[string]*chart, dirs []string) []facts.Fact {
	for _, d := range dirs {
		nameObjects(charts[d])
	}
	var out []facts.Fact
	for _, d := range dirs {
		c := charts[d]
		loaded := closure(c)
		out = append(out, facts.Fact{
			Kind:  facts.KindModule,
			Name:  c.dir,
			File:  c.dir,
			Props: map[string]any{"language": "helm"},
		})

		cf := symbol(c.sym, c.file, 1, c.dir, "chart")
		chartType := c.meta.Type
		if chartType == "" {
			chartType = "application"
		}
		cf.SetProp("chart_type", chartType)
		if c.meta.Version != "" {
			cf.SetProp("version", c.meta.Version)
		}
		if v, ok := c.meta.AppVersion.(string); ok && v != "" {
			cf.SetProp("app_version", v)
		}
		if len(c.remote) > 0 {
			cf.SetProp("remote_dependencies", c.remote)
		}
		if len(c.byName) > 0 {
			cf.SetProp("dependencies_by_name", c.byName)
		}
		targets := map[string]bool{}
		for _, dep := range c.deps {
			targets[dep.sym] = true
		}
		for _, o := range c.objects {
			targets[o.sym] = true
		}
		addRelations(&cf, facts.RelDependsOn, targets)
		out = append(out, cf)

		names := make([]string, 0, len(c.defines))
		for n := range c.defines {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			def := c.defines[n]
			f := symbol(def.sym, def.file, def.line, c.dir, "named_template")
			f.SetProp("symbol_kind", "function")
			callsFrom(&f, c, loaded, def.calls, def.sym)
			out = append(out, f)
		}
		for _, o := range c.objects {
			f := symbol(o.sym, o.file, o.line, c.dir, o.helmKind)
			f.SetProp("k8s_kind", o.kind)
			if o.apiVersion != "" {
				f.SetProp("api_version", o.apiVersion)
			}
			callsFrom(&f, c, loaded, o.calls, o.sym)
			out = append(out, f)
		}
	}
	return out
}

func symbol(name, file string, line int, dir, helmKind string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol,
		Name: name,
		File: file,
		Line: line,
		Props: map[string]any{
			"language":    "helm",
			"symbol_kind": "type",
			"helm_kind":   helmKind,
			"exported":    true,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
	}
}

// callsFrom draws a calls edge to each named template an include or template action
// names, and counts the names no loaded chart defines.
func callsFrom(f *facts.Fact, c *chart, loaded []*chart, calls []string, self string) {
	targets := map[string]bool{}
	unresolved := map[string]bool{}
	for _, name := range calls {
		if d := lookup(c, loaded, name); d != nil {
			if d.sym != self {
				targets[d.sym] = true
			}
		} else {
			unresolved[name] = true
		}
	}
	addRelations(f, facts.RelCalls, targets)
	if len(unresolved) > 0 {
		f.SetProp("unresolved_includes", len(unresolved))
	}
}

func addRelations(f *facts.Fact, kind string, targets map[string]bool) {
	sorted := make([]string, 0, len(targets))
	for t := range targets {
		sorted = append(sorted, t)
	}
	sort.Strings(sorted)
	for _, t := range sorted {
		f.Relations = append(f.Relations, facts.Relation{Kind: kind, Target: t})
	}
}
