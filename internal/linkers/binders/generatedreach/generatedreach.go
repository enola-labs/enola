// Package generatedreach decides whether a client call made from generated code can
// run.
//
// A client generated into the checkout carries one method per operation in the spec
// it was generated from, and an extractor that reads endpoints from code finds a call
// in each of them. Most extractors cannot tell whether the application reaches that
// method, so they leave facts.PropClientReachable unset, and an unset prop counts as
// reachable. Every operation of a generated client therefore counted as a call the
// application makes, including the ones nothing in the application uses.
//
// The binder walks back from the method that makes the call, through everything that
// calls, constructs or names it. If that walk only ever reaches generated code, or
// test code, nothing the application runs can get there, and the call is marked
// unreachable. One hand-written caller outside the tests is enough to leave the prop
// unset: being called does not make code reachable, so it is not a verdict either way.
package generatedreach

import (
	"context"
	"log"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// PropReachSource marks a route whose PropClientReachable this binder wrote, so a later
// run can withdraw it without touching a verdict an extractor wrote.
const PropReachSource = "client_reachable_source"

// reachSource is this binder's PropReachSource value.
const reachSource = "generated-callers"

// useRelations are the relations that make the source a user of the target. Structural
// relations (a module declaring a symbol, a type owning a method) are not uses: the
// module of a generated client is usually hand-written, and following it would reach
// the whole application.
var useRelations = map[string]bool{
	facts.RelCalls:        true,
	facts.RelInstantiates: true,
	facts.RelInjects:      true,
	facts.RelNames:        true,
}

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "generated-reach" }

// Pre-link: the cross-repo HTTP signal reads PropClientReachable when it decides which
// server routes a client reaches.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()

	type node struct{ repo, name string }
	users := map[node][]int{} // target -> facts using it
	// A name declared in generated code, and one declared outside it. Overloads share
	// a name, so a name can be both, and then it is not only generated.
	generated, runtime := map[node]bool{}, map[node]bool{}
	for i, f := range all {
		if f.Kind == facts.KindSymbol {
			if f.PropBool("generated") {
				generated[node{f.Repo, f.Name}] = true
			} else if !facts.IsTestPath(f.File) {
				runtime[node{f.Repo, f.Name}] = true
			}
		}
		for _, r := range f.Relations {
			if useRelations[r.Kind] {
				k := node{f.Repo, r.Target}
				users[k] = append(users[k], i)
			}
		}
	}

	// reached reports whether anything outside generated and test code uses caller,
	// directly or through other generated code.
	memo := map[node]bool{}
	reached := func(caller node) bool {
		if v, ok := memo[caller]; ok {
			return v
		}
		seen := map[node]bool{caller: true}
		queue := []node{caller}
		found := false
		for len(queue) > 0 && !found {
			n := queue[0]
			queue = queue[1:]
			for _, i := range users[n] {
				u := all[i]
				if !u.PropBool("generated") && !facts.IsTestPath(u.File) {
					found = true
					break
				}
				if u.Kind != facts.KindSymbol {
					continue
				}
				k := node{u.Repo, u.Name}
				if !seen[k] {
					seen[k] = true
					queue = append(queue, k)
				}
			}
		}
		memo[caller] = found
		return found
	}

	marked := 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.PropString(facts.PropRole) != facts.RoleClient {
			return
		}
		ours := f.PropString(PropReachSource) == reachSource
		if !ours && f.PropAny(facts.PropClientReachable) != nil {
			return // an extractor's verdict
		}
		caller := node{f.Repo, f.PropString(facts.PropCaller)}
		unreachable := f.PropBool("generated") && generated[caller] && !runtime[caller] && !reached(caller)
		switch {
		case unreachable:
			f.SetProp(facts.PropClientReachable, false)
			f.SetProp(PropReachSource, reachSource)
			marked++
		case ours:
			f.DelProp(facts.PropClientReachable)
			f.DelProp(PropReachSource)
		}
	})
	if marked > 0 {
		log.Printf("[binder:generated-reach] %d client call(s) only generated or test code reaches", marked)
	}
	return nil
}
