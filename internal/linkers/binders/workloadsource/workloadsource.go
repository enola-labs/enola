// Package workloadsource joins a Kubernetes workload to the code it runs.
package workloadsource

import (
	"context"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder draws a module dependency from the directory holding a workload's manifest
// to every code module in the directory its image is built from.
//
// The kubernetes extractor knows which directory an image is built from (skaffold.yaml
// says so) but not what the code there is called: a Go package is named by its import
// path, a Python package by its dotted path, and each extractor decides. Only once all
// of them have run does the store hold both halves, so the join is a binder. The edge
// sits at the module layer, beside the dependency facts an extractor writes for an
// import, because what a manifest depends on is a program, not one of its functions:
// change microservices-demo's src/checkoutservice and impact analysis reaches the
// kubernetes-manifests directory that deploys it.
//
// A module joins when it is in the build directory or below it, in the workload's own
// repository, and is neither a manifest nor a chart directory itself. A directory
// with no module (code in a language nothing reads) joins nothing.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

// Name identifies the binder.
func (b *Binder) Name() string { return "workload-source" }

// Stage is post-link: it resolves within one repository and reads no linker output.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

// SourceLabel is the `source` prop of the dependency facts this binder writes.
const SourceLabel = "build_context"

// Bind adds the dependency facts.
func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// Binders run again on every append, over the whole union: what this one derived
	// last time goes first, or each append would add every edge again.
	store.RemoveWhere(func(f facts.Fact) bool {
		return f.Kind == facts.KindDependency && f.PropString("source") == SourceLabel
	})
	type codeModule struct{ name, dir string }
	byRepo := map[string][]codeModule{}
	declaring := map[string]string{} // symbol name + repo -> the module declaring it
	var workloads []facts.Fact
	for _, f := range store.All() {
		switch f.Kind {
		case facts.KindModule:
			lang := f.PropString("language")
			if lang == "kubernetes" || lang == "helm" {
				continue
			}
			byRepo[f.Repo] = append(byRepo[f.Repo], codeModule{name: f.Name, dir: repoRelative(f)})
		case facts.KindSymbol:
			if f.PropString("language") != "kubernetes" {
				continue
			}
			if stringList(f.PropAny(facts.PropBuildContexts)) != nil {
				workloads = append(workloads, f)
			}
			for _, r := range f.Relations {
				if r.Kind == facts.RelDeclares {
					declaring[f.Repo+"\x00"+f.Name] = r.Target
				}
			}
		}
	}
	if len(workloads) == 0 {
		return nil
	}
	type edge struct{ repo, from, to, file string }
	seen := map[edge]bool{}
	var edges []edge
	for _, w := range workloads {
		from := declaring[w.Repo+"\x00"+w.Name]
		if from == "" {
			continue
		}
		for _, ctx := range stringList(w.PropAny(facts.PropBuildContexts)) {
			for _, m := range byRepo[w.Repo] {
				if ctx != "." && m.dir != ctx && !strings.HasPrefix(m.dir, ctx+"/") {
					continue
				}
				e := edge{w.Repo, from, m.name, w.File}
				if !seen[e] {
					seen[e] = true
					edges = append(edges, e)
				}
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.repo != b.repo {
			return a.repo < b.repo
		}
		if a.from != b.from {
			return a.from < b.from
		}
		return a.to < b.to
	})
	// One fact per pair of modules, on the first manifest that states it.
	added := map[[3]string]bool{}
	var out []facts.Fact
	for _, e := range edges {
		key := [3]string{e.repo, e.from, e.to}
		if added[key] {
			continue
		}
		added[key] = true
		out = append(out, facts.Fact{
			Kind: facts.KindDependency,
			Name: e.from + " -> " + e.to,
			File: e.file,
			Repo: e.repo,
			Props: map[string]any{
				"language": "kubernetes",
				"source":   SourceLabel,
			},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: e.to}},
		})
	}
	store.Add(out...)
	return nil
}

// repoRelative is a module's directory relative to its repository: a multi-repo
// snapshot prefixes File with the repository label and says so.
func repoRelative(f facts.Fact) string {
	dir := f.File
	if f.RepoPrefixed {
		dir = strings.TrimPrefix(dir, f.Repo+"/")
	}
	return dir
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
