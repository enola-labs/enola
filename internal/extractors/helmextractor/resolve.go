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
		vals := newValueIndex(c)
		chartType := c.meta.Type
		if chartType == "" {
			chartType = "application"
		}
		module := facts.Fact{
			Kind:  facts.KindModule,
			Name:  c.dir,
			File:  c.dir,
			Props: map[string]any{"language": "helm"},
		}
		if chartType == "library" {
			// A library chart exists to be loaded by other charts: its named templates
			// are written for callers outside this repository as much as inside it.
			module.SetProp(facts.PropPublished, true)
		}
		out = append(out, module)

		cf := symbol(c.sym, c.file, 1, c.dir, "chart")
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

		for _, v := range vals.keys {
			f := symbol(v.sym, prefix(c.dir)+"values.yaml", v.line, c.dir, "value")
			f.SetProp("symbol_kind", facts.SymbolVariable)
			f.SetProp("exported", false)
			f.SetProp("value_path", v.dotted)
			out = append(out, f)
		}

		names := make([]string, 0, len(c.defines))
		for n := range c.defines {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			def := c.defines[n]
			f := symbol(def.sym, def.file, def.line, c.dir, "named_template")
			f.SetProp("symbol_kind", facts.SymbolFunc)
			refsFrom(&f, c, loaded, vals, def.refs, def.sym)
			out = append(out, f)
		}
		for _, o := range c.objects {
			f := symbol(o.sym, o.file, o.line, c.dir, o.helmKind)
			f.SetProp("k8s_kind", o.kind)
			if o.apiVersion != "" {
				f.SetProp("api_version", o.apiVersion)
			}
			refsFrom(&f, c, loaded, vals, o.refs, o.sym)
			out = append(out, f)
		}
		files := make([]string, 0, len(c.fileRefs))
		for f := range c.fileRefs {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, file := range files {
			// Reference-only, like a test_ref: it marks what NOTES.txt or a file with no
			// literal kind uses, and is consumed by dead-code analysis alone.
			f := facts.Fact{
				Kind:  facts.KindFileRef,
				Name:  file,
				File:  file,
				Line:  1,
				Props: map[string]any{"language": "helm"},
			}
			refsFrom(&f, c, loaded, vals, *c.fileRefs[file], "")
			if len(f.Relations) > 0 {
				out = append(out, f)
			}
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

// refsFrom draws a calls edge to each named template a carrier includes and a names
// edge to each value it reads, and counts the include names no loaded chart defines.
//
// A named template another chart defines reads the values of whichever chart
// includes it: bitnami's common.names.fullname reads `.Values.fullnameOverride` of
// the chart rendering it. Its reads are therefore credited to the carrier here that
// reaches it, through any number of that chart's own templates, and resolved against
// this chart's values. Crediting them to the define instead would draw an edge from
// the library into every chart using it, and a cycle with each.
func refsFrom(f *facts.Fact, c *chart, loaded []*chart, vals *valueIndex, r refs, self string) {
	calls := map[string]bool{}
	reads := map[string]bool{}
	unresolved := map[string]bool{}
	for _, name := range r.calls {
		d := lookup(c, loaded, name)
		if d == nil {
			unresolved[name] = true
			continue
		}
		if d.sym != self {
			calls[d.sym] = true
		}
	}
	for _, p := range r.reads {
		vals.resolve(p, reads)
	}
	for _, p := range r.named {
		vals.resolveDeclared(p, reads)
	}
	for _, d := range foreignReach(c, loaded, r.calls) {
		for _, p := range d.reads {
			vals.resolve(p, reads)
		}
		for _, p := range d.named {
			vals.resolveDeclared(p, reads)
		}
	}
	addRelations(f, facts.RelCalls, calls)
	addRelations(f, facts.RelNames, reads)
	if len(unresolved) > 0 {
		f.SetProp("unresolved_includes", len(unresolved))
	}
}

// foreignReach lists the named templates of other charts that calls reach, directly
// or through other foreign templates. A template of c itself stops the walk: it is a
// carrier of its own, with its own edges.
func foreignReach(c *chart, loaded []*chart, calls []string) []*define {
	var out []*define
	seen := map[*define]bool{}
	queue := append([]string{}, calls...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		d := lookup(c, loaded, name)
		if d == nil || seen[d] || c.defines[name] == d {
			continue
		}
		seen[d] = true
		out = append(out, d)
		queue = append(queue, d.calls...)
	}
	return out
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
