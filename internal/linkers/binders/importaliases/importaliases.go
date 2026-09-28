// Package importaliases resolves the names a repository uses for another loaded
// repository's symbols and packages to the facts that declare them.
package importaliases

import (
	"context"
	"log"
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

// goModule is one loaded repository's Go module path.
type goModule struct {
	path string
	repo string
}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// An alias always crosses repositories: a repository's own Go targets are
	// already written relative to its module root.
	if len(store.RepoLabels()) < 2 {
		store.SetTargetAliases(nil)
		return nil
	}
	all := store.FactsRef()

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
		store.SetTargetAliases(nil)
		return nil
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
	aliases := map[string]facts.FactKey{}
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
			}
		}
	}

	store.SetTargetAliases(aliases)
	if len(aliases) > 0 {
		log.Printf("[binder:import-aliases] resolved %d cross-repo Go target(s) to their declaring facts", len(aliases))
	}
	return nil
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
