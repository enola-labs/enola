// The NgModule graph and the workspace shape — what an Angular application says
// about its own composition.
//
// An Angular application's dependency structure is not in its imports. A component
// can only render another one because some @NgModule declared the first and
// imported the module that exports the second, or — since standalone components —
// because the component's own `imports:` array names it. Those arrays are the
// composition, and none of it was an edge: 3,425 NgModules across ten repositories
// contributed their class symbol and nothing else.
//
// The second half is naming. In a workspace of many projects, a module fact keyed
// only by directory says `libs/billing/src/lib/invoices` and nothing about which
// project owns it, so every reading that groups by unit — layers, coupling,
// cross-repo — has to infer the boundary from a path. The workspace files state it,
// so it is read rather than inferred.
package tsextractor

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// angularModuleArrays are the decorator properties whose entries name other
// declarations, with the prop each is recorded under. `providers` is included
// because a provider is a real dependency of the module that registers it; `schemas`
// and `bootstrap` are not — one names a compiler mode and the other is one entry
// that `declarations` already covers.
var angularModuleArrays = map[string]string{
	"imports":      "angular_module_imports",
	"declarations": "angular_declarations",
	"exports":      "angular_module_exports",
	"providers":    "angular_providers",
}

// angularModuleEdges reads a class-level decorator's composition arrays and returns
// the edges they declare, together with the names recorded per array.
//
// A member is an identifier, a `Module.forRoot(…)` call (whose receiver is the
// module), or a provider object (`{provide: TOKEN, useClass: Impl}`) — for which
// both the token and the implementation are read, since a module that registers one
// depends on both. Anything else contributes nothing and is counted.
func angularModuleEdges(kinds *tsutil.KindTable, args *sitter.Node, ctx *extractCtx,
	imports map[string]string, local map[string]bool) ([]facts.Relation, map[string]string, angularCounts) {

	var counts angularCounts
	obj := firstObjectArg(kinds, args)
	if obj == nil {
		return nil, nil, counts
	}

	var rels []facts.Relation
	props := map[string]string{}
	seen := map[string]bool{}

	for _, key := range []string{"declarations", "imports", "exports", "providers"} {
		arr := objectPropValue(kinds, obj, ctx.src, key)
		if arr == nil || kindOf(kinds, arr) != "array" {
			continue
		}
		var named []string
		for i := range arr.ChildCount() {
			for _, name := range angularMemberNames(kinds, arr.Child(i), ctx.src) {
				if name == "" {
					continue
				}
				named = append(named, name)
				target, ok := angularResolveType(name, ctx, imports, local)
				if !ok {
					counts.miss(angularMissCause(name, nil))
					continue
				}
				if seen[target] {
					continue
				}
				seen[target] = true
				counts.resolved++
				rels = append(rels, facts.Relation{Kind: facts.RelDependsOn, Target: target})
			}
		}
		if len(named) > 0 {
			props[angularModuleArrays[key]] = strings.Join(named, ",")
		}
	}
	return rels, props, counts
}

// angularMemberNames returns the declaration names one array member states.
func angularMemberNames(kinds *tsutil.KindTable, member *sitter.Node, src []byte) []string {
	switch kindOf(kinds, member) {
	case "identifier":
		return []string{nodeText(member, src)}
	case "call_expression":
		// `RouterModule.forRoot(routes)` / `StoreModule.forFeature(…)`: the module is
		// the receiver, and it is what the importing module depends on.
		fn := member.ChildByFieldName("function")
		if fn == nil {
			return nil
		}
		if obj := fn.ChildByFieldName("object"); obj != nil && kindOf(kinds, obj) == "identifier" {
			return []string{nodeText(obj, src)}
		}
		if kindOf(kinds, fn) == "identifier" {
			return []string{nodeText(fn, src)}
		}
	case "object":
		// A provider literal names a token and an implementation, and the module
		// depends on both: one is what it registers, the other is what it registers.
		var out []string
		for _, key := range []string{"provide", "useClass", "useExisting"} {
			if v := objectPropValue(kinds, member, src, key); v != nil && kindOf(kinds, v) == "identifier" {
				out = append(out, nodeText(v, src))
			}
		}
		return out
	}
	return nil
}

// --- workspace shape ---------------------------------------------------------

// angularProjectNames maps a directory to the workspace project that owns it, read
// from the files that state it: an Nx `project.json`, or the `projects` map of an
// `angular.json`. Both are read because both are current — Nx generates the first,
// the Angular CLI the second, and a workspace routinely has one of each.
func angularProjectNames(repoPath string) map[string]string {
	out := map[string]string{}

	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != repoPath && (strings.HasPrefix(name, ".") || tsSkipDirs[name] || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		switch d.Name() {
		case "project.json":
			var p struct {
				Name string `json:"name"`
			}
			if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &p) == nil && p.Name != "" {
				if rel, err := filepath.Rel(repoPath, filepath.Dir(path)); err == nil { //factpath:host
					out[factpath.Slash(rel)] = p.Name
				}
			}
		case "angular.json":
			for dir, name := range angularWorkspaceProjects(path, repoPath) {
				out[dir] = name
			}
		}
		return nil
	})
	return out
}

// angularWorkspaceProjects reads an angular.json's projects map into directory →
// project name. A project states its own root; one whose root is the workspace root
// is skipped, since naming every directory after it says nothing.
func angularWorkspaceProjects(path, repoPath string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ws struct {
		Projects map[string]struct {
			Root       string `json:"root"`
			SourceRoot string `json:"sourceRoot"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &ws); err != nil {
		return nil
	}
	base := filepath.Dir(path)               //factpath:host — the workspace file's own directory on disk
	rel, err := filepath.Rel(repoPath, base) //factpath:host
	if err != nil {
		return nil
	}
	prefix := factpath.Slash(rel)
	out := map[string]string{}
	for name, p := range ws.Projects {
		root := p.Root
		if root == "" {
			root = p.SourceRoot
		}
		root = strings.TrimPrefix(strings.TrimSpace(root), "./")
		if root == "" || root == "." {
			continue
		}
		dir := root
		if prefix != "" && prefix != "." {
			dir = prefix + "/" + root
		}
		out[factpath.Clean(dir)] = name
	}
	return out
}

// nearestProjectName returns the workspace project owning a directory, by walking
// up from it — the same nearest-ancestor rule package names already use.
func nearestProjectName(projects map[string]string, dir string) string {
	if len(projects) == 0 {
		return ""
	}
	return nearestPackageName(projects, dir)
}
