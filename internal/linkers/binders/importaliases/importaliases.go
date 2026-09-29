// Package importaliases resolves the names a repository uses for another loaded
// repository's symbols and packages to the facts that declare them.
package importaliases

import (
	"context"
	"log"
	"path"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder builds the store's target aliases (facts.Store.SetTargetAliases).
//
// A consumer names a provider's symbol as its own source spells it. Go writes the
// import path: golf calls "github.com/acme/auth.AuthService.Login" and imports
// "github.com/acme/auth/adapters", while the auth repository declares
// "..AuthService.Login" and the module "adapters". Nothing ties the two, so the
// relation has had no target_id and a reader of facts.jsonl could not follow it.
//
// The binder does not rename the target. The consumer's spelling is unique by
// construction; the provider's local name is not ("..AuthService" can equally be
// declared by the consumer), and name resolution prefers the consumer's own facts,
// so a renamed target could resolve to the wrong one. The alias leaves the name as
// written and records which fact it means, and the writer resolves target_id
// through it.
//
// An alias is recorded only when the provider declares exactly one fact under the
// local name (of the kind the relation expects), since a wrong target_id is worse
// than a missing one.
//
// Post-link, since it reads every loaded repository, and it replaces the table on
// every run so an append sees the current set of repositories.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "import-aliases" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

// DerivesIndexOnly declares the binder a plugin.IndexBinder: it sets the store's
// target aliases and changes no fact, so a restored store gets them back.
func (b *Binder) DerivesIndexOnly() {}

// goModule is one loaded repository's Go module path.
type goModule struct {
	path string
	repo string
}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()
	aliases := map[string]facts.FactKey{}
	nGo := goAliases(store, all, aliases)
	nTS := tsAliases(store, all, aliases)
	nFQN := fqnAliases(store, all, aliases)
	if len(aliases) == 0 {
		store.SetTargetAliases(nil)
		return nil
	}
	store.SetTargetAliases(aliases)
	log.Printf("[binder:import-aliases] resolved %d Go, %d TypeScript and %d qualified-type target(s) to their declaring facts", nGo, nTS, nFQN)
	return nil
}

// fqnAliases adds the aliases for targets written as a type's fully qualified name,
// the way Java, Scala and .NET reference a type from another package
// (`org.acme.lib.Client`), or that name followed by a member
// (`org.acme.lib.Client.connect`). Those extractors name their facts by directory
// (`lib/src/main/java/org/acme/lib.Client`) and record the qualified name as the
// type's fqn prop, which is what the target is matched against. It returns how many
// it added.
//
// It applies within one repository as much as across two, since a reference its
// extractor could not resolve locally is written the same way. A qualified name two
// types claim resolves to nothing.
func fqnAliases(store *facts.Store, all []facts.Fact, aliases map[string]facts.FactKey) int {
	types := map[string]facts.FactKey{}
	ambiguous := map[string]bool{}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		fqn := f.PropString("fqn")
		if fqn == "" || ambiguous[fqn] {
			continue
		}
		k := facts.FactKey{Repo: f.Repo, Kind: f.Kind, Name: f.Name, File: f.File}
		if prev, seen := types[fqn]; seen && prev != k {
			delete(types, fqn)
			ambiguous[fqn] = true
			continue
		}
		types[fqn] = k
	}
	if len(types) == 0 {
		return 0
	}

	n := 0
	tried := map[string]bool{}
	for _, f := range all {
		for _, rel := range f.Relations {
			target := rel.Target
			if rel.Kind == facts.RelDeclares || tried[target] || !strings.Contains(target, ".") {
				continue
			}
			if _, done := aliases[target]; done {
				continue
			}
			tried[target] = true
			key, ok := fqnTarget(store, types, target)
			if !ok || len(store.ByName(target)) > 0 {
				continue
			}
			aliases[target] = key
			n++
		}
	}
	return n
}

// fqnTarget resolves a qualified-name target: the type whose fqn it is, or the
// member of the longest fqn it starts with, when that type's repository declares a
// fact named exactly "<type fact name>.<rest>".
func fqnTarget(store *facts.Store, types map[string]facts.FactKey, target string) (facts.FactKey, bool) {
	if k, ok := types[target]; ok {
		return k, true
	}
	for i := strings.LastIndexByte(target, '.'); i > 0; i = strings.LastIndexByte(target[:i], '.') {
		typ, ok := types[target[:i]]
		if !ok {
			continue
		}
		return uniqueFact(store.ByName(typ.Name+target[i:]), typ.Repo, false)
	}
	return facts.FactKey{}, false
}

// goAliases adds the aliases for Go import-path targets under another loaded
// repository's module path, and returns how many it added.
func goAliases(store *facts.Store, all []facts.Fact, aliases map[string]facts.FactKey) int {
	// A Go alias always crosses repositories: a repository's own Go targets are
	// already written relative to its module root.
	if len(store.RepoLabels()) < 2 {
		return 0
	}
	var modules []goModule
	for _, f := range all {
		if f.Kind != facts.KindModule || f.Name != "." || f.Repo == "" {
			continue
		}
		if mp := f.PropString("modulePath"); mp != "" {
			modules = append(modules, goModule{path: mp, repo: f.Repo})
		}
	}
	if len(modules) == 0 {
		return 0
	}
	// Longest first, so a module nested under another's path ("github.com/acme/auth/
	// tools" beside "github.com/acme/auth") claims its own targets.
	sort.Slice(modules, func(i, j int) bool {
		if len(modules[i].path) != len(modules[j].path) {
			return len(modules[i].path) > len(modules[j].path)
		}
		return modules[i].repo < modules[j].repo
	})

	// Only targets under a loaded module path are looked up, so the name lookups
	// below run for the handful of cross-repo references and not for every target.
	n := 0
	tried := map[string]bool{}
	for _, f := range all {
		for _, rel := range f.Relations {
			target := rel.Target
			if rel.Kind == facts.RelDeclares || tried[target] {
				continue
			}
			repo, local, ok := goLocalName(target, modules)
			if !ok || repo == f.Repo {
				continue
			}
			tried[target] = true
			if len(store.ByName(target)) > 0 {
				continue // a fact carries the name itself; name resolution answers it
			}
			wantModule := rel.Kind == facts.RelImports
			if key, ok := uniqueFact(store.ByName(local), repo, wantModule); ok {
				aliases[target] = key
				n++
			}
		}
	}
	return n
}

// goLocalName maps a Go import-path target to the repository whose module path
// prefixes it and the name that repository's facts use: the package directory
// relative to the module root, with the root package named ".".
//
//	"github.com/acme/auth"                  -> ".", the root package
//	"github.com/acme/auth/adapters"         -> "adapters"
//	"github.com/acme/auth.AuthService"      -> "..AuthService"
//	"github.com/acme/auth/adapters.H.Login" -> "adapters.H.Login"
func goLocalName(target string, modules []goModule) (repo, local string, ok bool) {
	for _, m := range modules {
		if !strings.HasPrefix(target, m.path) {
			continue
		}
		rest := target[len(m.path):]
		switch {
		case rest == "":
			return m.repo, ".", true
		case rest[0] == '/':
			return m.repo, rest[1:], true
		case rest[0] == '.':
			return m.repo, "." + rest, true
		}
		// A longer path sharing the prefix ("github.com/acme/authz"): not this module.
	}
	return "", "", false
}

// uniqueFact returns the identity of the one fact among named that repo declares
// with the wanted kind: a module for an import, anything but a module otherwise.
// Several facts of that kind under distinct identities is no answer.
func uniqueFact(named []facts.Fact, repo string, wantModule bool) (facts.FactKey, bool) {
	var key facts.FactKey
	found := false
	for _, f := range named {
		if f.Repo != repo || (f.Kind == facts.KindModule) != wantModule {
			continue
		}
		k := facts.FactKey{Repo: f.Repo, Kind: f.Kind, Name: f.Name, File: f.File}
		if found && k != key {
			return facts.FactKey{}, false
		}
		key, found = k, true
	}
	return key, found
}

// tsPackage is one npm package a loaded repository declares: the repository and the
// module directories whose package.json names it.
type tsPackage struct {
	repo string
	dirs []string
}

// tsAliases adds the aliases for TypeScript package targets, "<package>.<export>"
// and a bare "<package>" import, where a loaded repository declares the package,
// and returns how many it added.
//
// The export resolves to the one symbol named "<dir>.<export>" across the package's
// module directories. That skips the package's entry file, which usually names
// build output ("main": "dist/index.js") that is not in the source tree, and the
// barrel re-exports behind it; two such symbols under distinct identities resolve
// to nothing. A package import resolves to the package's root module, the one of its
// modules every other is under, and to nothing when there is no single root.
//
// Unlike a Go alias this may stay inside one repository: a monorepo importing its own
// workspace package by name is the same reference as one repository importing
// another's.
func tsAliases(store *facts.Store, all []facts.Fact, aliases map[string]facts.FactKey) int {
	packages := map[string]*tsPackage{}
	ambiguous := map[string]bool{}
	for _, f := range all {
		if f.Kind != facts.KindModule || f.Repo == "" {
			continue
		}
		name := f.PropString("package_name")
		if name == "" || ambiguous[name] {
			continue
		}
		p := packages[name]
		if p == nil {
			packages[name] = &tsPackage{repo: f.Repo, dirs: []string{f.Name}}
			continue
		}
		if p.repo != f.Repo {
			// Two repositories declaring one package name (a fork, a vendored copy):
			// nothing says which one a consumer imports.
			delete(packages, name)
			ambiguous[name] = true
			continue
		}
		p.dirs = append(p.dirs, f.Name)
	}
	if len(packages) == 0 {
		return 0
	}

	n := 0
	tried := map[string]bool{}
	for _, f := range all {
		for _, rel := range f.Relations {
			target := rel.Target
			if rel.Kind == facts.RelDeclares || tried[target] {
				continue
			}
			pkg, sub, rest, ok := tsPackageTarget(target, packages)
			if !ok {
				continue
			}
			tried[target] = true
			if _, done := aliases[target]; done || len(store.ByName(target)) > 0 {
				continue
			}
			p := packages[pkg]
			if rest == "" {
				if root, ok := packageRoot(p.dirs); ok {
					if key, ok := uniqueFact(store.ByName(root), p.repo, true); ok {
						aliases[target] = key
						n++
					}
				}
				continue
			}
			if key, ok := tsExport(store, p, sub, rest); ok {
				aliases[target] = key
				n++
			}
		}
	}
	return n
}

// tsExport resolves a package's export to the one symbol declaring it. A subpath
// import names a file under the package root, so the export is looked up in that
// file's directory first; a package-wide lookup follows when the subpath does not
// settle it (an entry point re-exporting from elsewhere).
func tsExport(store *facts.Store, p *tsPackage, sub, rest string) (facts.FactKey, bool) {
	if sub != "" {
		if root, ok := packageRoot(p.dirs); ok {
			file := sub
			if root != "." {
				file = root + "/" + sub
			}
			var named []facts.Fact
			for _, dir := range []string{file, path.Dir(file)} {
				named = append(named, store.ByName(dir+"."+rest)...)
			}
			if key, ok := uniqueFact(named, p.repo, false); ok {
				return key, true
			}
		}
	}
	var named []facts.Fact
	for _, dir := range p.dirs {
		named = append(named, store.ByName(dir+"."+rest)...)
	}
	return uniqueFact(named, p.repo, false)
}

// tsPackageTarget splits a target into the declared package it names, the subpath
// after the package name, and the symbol path: "@acme/sdk.Client.get" -> ("@acme/sdk",
// "", "Client.get"), "@acme/sdk/http/transport.Transport" -> ("@acme/sdk",
// "http/transport", "Transport"), and the bare package "@acme/sdk" -> ("@acme/sdk",
// "", ""). The longest declared package name wins, so "@acme/sdk-extra" is never
// read as "@acme/sdk".
func tsPackageTarget(target string, packages map[string]*tsPackage) (pkg, sub, rest string, ok bool) {
	for i := len(target); i > 0; i-- {
		if i < len(target) && target[i] != '.' && target[i] != '/' {
			continue
		}
		if _, known := packages[target[:i]]; !known {
			continue
		}
		pkg, tail := target[:i], target[i:]
		switch {
		case tail == "":
			return pkg, "", "", true
		case tail[0] == '.':
			return pkg, "", tail[1:], true
		default: // "/subpath" then ".Export…"
			dot := strings.IndexByte(tail, '.')
			if dot < 0 {
				return "", "", "", false // a subpath import with no symbol: no single module to name
			}
			return pkg, tail[1:dot], tail[dot+1:], true
		}
	}
	return "", "", "", false
}

// packageRoot returns the one directory among dirs that every other is under.
func packageRoot(dirs []string) (string, bool) {
	root := ""
	for _, d := range dirs {
		if root == "" || len(d) < len(root) {
			root = d
		}
	}
	for _, d := range dirs {
		if d != root && !strings.HasPrefix(d, root+"/") && root != "." {
			return "", false
		}
	}
	return root, root != ""
}
