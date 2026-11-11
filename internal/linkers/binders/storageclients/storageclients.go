// Package storageclients links code to the storage it reaches through a client
// library rather than a query: a function calling a Redis client depends on that
// Redis.
//
// Storage the graph knows of is what a query names, a table, or what a framework
// declares, a model. A cache or a key-value store is reached through a client
// library's methods and names neither, so a function reading it depended on nothing,
// no walk could end at it, and an endpoint served from a cache looked as if it touched
// no storage.
//
// The binder reads the calls the extractors already recorded, so it serves every
// language whose extractor resolves a call to the library's type, with one table of
// client libraries rather than one detector per language. It cannot read the
// arguments of a call, so the storage is the engine per repository ("redis:shop"),
// not a key: keys are mostly built inside a wrapper from a namespace it was
// constructed with, and are not readable at the call either way.
package storageclients

import (
	"context"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// clientLib is a storage client library: the import paths it is published under, the
// name code calls its package by, and the storage engine behind it.
type clientLib struct {
	importPrefixes []string
	pkgName        string
	engine         string
	storageKind    string
}

// clientLibs are the libraries recognized. A call is one of a library's when its
// target is a method of a type of the library ("<pkg>.<Type>.<Method>"), written
// under the import path or under the package name in a file importing the library.
// A package-level function (a constructor) is not a use of the store.
var clientLibs = []clientLib{
	{importPrefixes: []string{"github.com/redis/go-redis/", "github.com/go-redis/redis"}, pkgName: "redis", engine: "redis", storageKind: "cache"},
}

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "storage-clients" }

// Pre-link: the storage nodes and edges are within one repository, and the graph is
// built after.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()

	// The libraries each file imports.
	imported := map[string][]int{} // file -> clientLibs indexes
	for _, f := range all {
		if f.Kind != facts.KindDependency {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImports {
				continue
			}
			for i, lib := range clientLibs {
				if libImport(lib, r.Target) && !contains(imported[f.File], i) {
					imported[f.File] = append(imported[f.File], i)
				}
			}
		}
	}

	type node struct{ repo, name string }
	uses := map[node][]string{} // function -> storage node names
	first := map[string]facts.Fact{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls {
				continue
			}
			i, ok := libOfCall(r.Target, imported[f.File])
			if !ok {
				continue
			}
			name := storageName(clientLibs[i].engine, f.Repo)
			k := node{f.Repo, f.Name}
			if !contains(uses[k], name) {
				uses[k] = append(uses[k], name)
			}
			if _, seen := first[name]; !seen {
				first[name] = f
			}
		}
	}
	if len(uses) == 0 {
		return nil
	}

	names := make([]string, 0, len(first))
	for name := range first {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f := first[name]
		engine, _, _ := strings.Cut(name, ":")
		lib := libByEngine(engine)
		store.Add(facts.Fact{
			Kind: facts.KindStorage, Name: name, File: f.File, Line: f.Line, Repo: f.Repo,
			Props: map[string]any{"storage_kind": lib.storageKind, "engine": lib.engine, "synthetic": "storage-clients"},
		})
	}
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol {
			return
		}
		for _, name := range uses[node{f.Repo, f.Name}] {
			if !hasRelation(f, facts.RelDependsOn, name) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: name})
			}
		}
	})
	return nil
}

// storageName is the storage node of an engine in a repository. A repository's
// services talk to their own instance, so the repository is in the name; two
// repositories sharing a node by name would be joined through it.
func storageName(engine, repo string) string {
	if repo == "" {
		return engine
	}
	return engine + ":" + repo
}

// libOfCall reports the library a call target is a method of: "<import path>.<Type>.<Method>",
// or "<package name>.<Type>.<Method>" in a file importing the library.
func libOfCall(target string, inFile []int) (int, bool) {
	dot := strings.LastIndex(target, ".")
	if dot <= 0 {
		return 0, false
	}
	owner := target[:dot]
	tdot := strings.LastIndex(owner, ".")
	if tdot <= 0 {
		return 0, false
	}
	pkg, typ := owner[:tdot], owner[tdot+1:]
	if typ == "" || typ[0] < 'A' || typ[0] > 'Z' {
		return 0, false // a package-level function, or not a type
	}
	for i, lib := range clientLibs {
		if libImport(lib, pkg) {
			return i, true
		}
	}
	for _, i := range inFile {
		if pkg == clientLibs[i].pkgName {
			return i, true
		}
	}
	return 0, false
}

func libImport(lib clientLib, path string) bool {
	for _, p := range lib.importPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func libByEngine(engine string) clientLib {
	for _, lib := range clientLibs {
		if lib.engine == engine {
			return lib
		}
	}
	return clientLib{engine: engine}
}

func hasRelation(f *facts.Fact, kind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == kind && r.Target == target {
			return true
		}
	}
	return false
}

func contains[T comparable](s []T, v T) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
