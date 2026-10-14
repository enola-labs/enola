package flows

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Interface dispatch.
//
// A call through an interface resolves to the interface's method, which has no body:
// `_sessionManager.AuthenticateNewSession(...)` in a controller names
// ISessionManager.AuthenticateNewSession, and a walk that follows only calls stops
// there. What runs is the method of the class implementing it. In an application
// wired by dependency injection that is nearly always one class, so the walk goes on
// into the implementations of an interface method, when there are few enough of them
// for the flow to stay a description of one path. Jellyfin has 1,867 calls to an
// interface with one implementation, and 8 to one with more than three.

// maxImplementations is the most implementations an interface method is followed
// into. Past it the interface is an extension point, a logger or a storage backend,
// and every implementation in the flow would describe none of them.
const maxImplementations = 3

// dispatchIndex is what the walk knows about interfaces, per repository.
type dispatchIndex struct {
	interfaces map[[2]string]bool     // (repo, name) of every interface
	implements map[[2]string][]string // (repo, interface) -> the types implementing it
}

func (a *Analysis) dispatch() *dispatchIndex {
	if a.ifaces != nil {
		return a.ifaces
	}
	ix := &dispatchIndex{interfaces: map[[2]string]bool{}, implements: map[[2]string][]string{}}
	for _, f := range a.src.FactsRef() {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if f.PropString("symbol_kind") == facts.SymbolInterface {
			ix.interfaces[[2]string{f.Repo, f.Name}] = true
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImplements {
				continue
			}
			key := [2]string{f.Repo, r.Target}
			if !containsString(ix.implements[key], f.Name) {
				ix.implements[key] = append(ix.implements[key], f.Name)
			}
		}
	}
	a.ifaces = ix
	return ix
}

// implementations returns the nodes of the methods implementing an interface method,
// or nil when the node is not one, or has none or too many.
func (a *Analysis) implementations(id int32) []int32 {
	f := a.nodes[id].fact
	if f.PropString("symbol_kind") != facts.SymbolMethod {
		return nil
	}
	dot := strings.LastIndexByte(f.Name, '.')
	if dot <= 0 {
		return nil
	}
	owner, method := f.Name[:dot], f.Name[dot+1:]
	ix := a.dispatch()
	if !ix.interfaces[[2]string{f.Repo, owner}] {
		return nil
	}
	types := ix.implements[[2]string{f.Repo, owner}]
	if len(types) == 0 || len(types) > maxImplementations {
		return nil
	}
	var out []int32
	for _, t := range types {
		if impl, ok := a.nodeFor(f.Repo, t+"."+method); ok && impl != id {
			out = append(out, impl)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
