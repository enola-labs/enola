// Package stimulusresolver binds the method a Stimulus data-action names to the
// symbol the controller file declares.
package stimulusresolver

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/extcoverage"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder grounds markup-declared Stimulus handlers after extraction, where the
// whole store is visible.
//
// WHY A BINDER. `data-action="click->dropdown#toggle"` is written in an ERB
// view and names a method on a TypeScript class. The Ruby pass that reads the
// view resolves the controller FILE by naming convention and stops there: the
// symbol the file declares is known only once the TypeScript extractor has run,
// which is after every Ruby file is done. The per-file pass records the handler
// names; this binder joins them to the members.
//
// WHY IT SKIPS ON AMBIGUITY. A handler grounds only when the controller file
// declares exactly one member of that name. Two matches mean the file alone
// cannot decide, and a name absent from the file is a binding the markup makes
// and the code does not answer — a real defect in the application, reported as
// an unresolved handler rather than dropped or guessed at. Nothing is derived
// from the class NAME: an identifier that resolved to no file grounds nothing,
// because a conventional class name is exactly the derivation findings 0009 and
// 0010 were filed against.
//
// It is post-link: every reference is inside one repository, like http-handler.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "stimulus-resolver" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

// handlersProp/unresolvedProp mirror the constants the Ruby extractor's Stimulus
// pass emits. Spelled here rather than imported so this package — like every
// binder — depends only on facts and plugin.
const (
	handlersProp   = "stimulus_handlers"
	unresolvedProp = "stimulus_unresolved"
	frameworkProp  = "framework"
	stimulus       = "stimulus"
)

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	members := membersByFile(store)
	if len(members) == 0 {
		return nil
	}

	var resolved, unresolved, bindingsWithMisses, bound int
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindDependency || f.PropString(frameworkProp) != stimulus {
			return
		}
		handlers := strings.Fields(f.PropString(handlersProp))
		if len(handlers) == 0 {
			return
		}
		byName := members[f.Repo+"\x00"+controllerFileOf(f)]
		var misses []string
		for _, handler := range handlers {
			target := byName[handler]
			if target == "" {
				misses = append(misses, handler)
				continue
			}
			resolved++
			if !f.HasRelation(facts.RelCalls, target) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: target})
				bound++
			}
		}
		if len(misses) > 0 {
			sort.Strings(misses)
			f.SetProp(unresolvedProp, strings.Join(misses, " "))
			unresolved += len(misses)
			bindingsWithMisses++
		}
	})

	if bound > 0 {
		log.Printf("[binder:stimulus-resolver] bound %d Stimulus handler edge(s)", bound)
	}
	if fact, ok := extcoverage.Fact(repoRoot(store), "stimulus:actions", "stimulus_handler",
		resolved, map[string]int{"unresolved_handler": unresolved}); ok {
		if bindingsWithMisses > 0 {
			fact.SetProp("bindings_with_misses", bindingsWithMisses)
		}
		store.Add(fact)
	}
	return nil
}

// controllerFileOf returns the controller file the markup pass already grounded
// the identifier on, or "" when it grounded none. The path is repo-relative on
// both sides: a fact's File is prefixed with its repo label in append mode, its
// relation targets are not, so the index strips the prefix rather than this
// adding one.
func controllerFileOf(f *facts.Fact) string {
	for _, rel := range f.Relations {
		if rel.Kind == facts.RelDependsOn {
			return rel.Target
		}
	}
	return ""
}

// membersByFile indexes, per repo and controller file, the members that file
// declares by their own (last) name. A member is a symbol carrying a receiver —
// the class it belongs to — which is what distinguishes a method on the
// controller from a free function in the same file. A name declared twice in one
// file is dropped: which one a descriptor means cannot be decided from the name.
func membersByFile(store *facts.Store) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, s := range store.ByKind(facts.KindSymbol) {
		if s.File == "" || s.PropString("receiver") == "" {
			continue
		}
		file := strings.TrimPrefix(filepath.ToSlash(s.File), s.Repo+"/")
		if !strings.HasSuffix(file, "_controller.js") && !strings.HasSuffix(file, "_controller.ts") {
			continue
		}
		key := s.Repo + "\x00" + file
		if out[key] == nil {
			out[key] = map[string]string{}
		}
		name := s.Name[strings.LastIndexByte(s.Name, '.')+1:]
		if prior, seen := out[key][name]; seen && prior != s.Name {
			out[key][name] = ""
			continue
		}
		out[key][name] = s.Name
	}
	return out
}

// repoRoot names the repository this binder ran over, for the coverage fact's
// file field. A multi-repo store reports the first label, which is the same
// convention the cross-repo coverage already uses.
func repoRoot(store *facts.Store) string {
	if labels := store.RepoLabels(); len(labels) > 0 {
		return labels[0]
	}
	return "."
}
