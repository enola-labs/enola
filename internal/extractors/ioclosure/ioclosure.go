// Package ioclosure turns an extractor's direct-I/O marks into the transitive fact.
//
// An extractor's walker sets io_direct on a function whose body calls a network,
// file or database primitive. What a consumer asks is wider: does calling this
// function reach I/O at all. Propagate answers that over the call graph and writes
// performs_io. It was written once per extractor, seven times in the same words,
// and none of the copies followed a call through an interface.
package ioclosure

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

const (
	propIODirect   = "io_direct"
	propPerformsIO = "performs_io"
	propAbstract   = "abstract"
	// propCallsOnce lists the callees a function reaches only from inside a
	// run-once guard (Go's sync.Once). The call happens once per process, so what
	// it does is not what the function does each time it is called.
	propCallsOnce = "calls_once"
)

// Propagate sets performs_io on every symbol in all that is io_direct or reaches
// an io_direct symbol over calls edges between symbols in all.
//
// A call on a method that has no body of its own continues to the methods that
// supply one: a method of an interface, or an abstract method, performs I/O when
// an implementer's method of that name does. Without that the closure stops at the
// first injected dependency, one hop short of the I/O it was looking for.
//
// A concrete method is NOT continued into its overrides. `Base.save` with a body
// says what a call resolved to it does, and a subclass that overrides it with a
// query does not change that; such a call is as often the subclass's own
// `super.save()`.
//
// Names key the result, as they key the graph: facts that share a name share the
// answer.
func Propagate(all []facts.Fact) {
	PropagateWith(all, Options{})
}

// Options are what a language adds to Propagate.
type Options struct {
	// Declared maps an interface to the types the source states implement it, for
	// a language whose facts do not say. It is read beside the implements edges.
	Declared map[string][]string
	// Opaque names the functions the closure does not pass through: they neither
	// inherit I/O from what they call nor hand theirs to their callers. It is for
	// code whose I/O is a side channel and not what a caller asked for: a logger
	// writes a file, and a function is not doing file I/O per iteration for
	// logging in its loop. An opaque function that is io_direct is still marked.
	Opaque func(name string) bool
}

// PropagateWith is Propagate with a language's additions.
func PropagateWith(all []facts.Fact, opts Options) {
	declared := opts.Declared
	exists := make(map[string]bool)
	bodiless := make(map[string]bool) // an interface, or an abstract method
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		exists[f.Name] = true
		if kind, _ := f.PropAny("symbol_kind").(string); kind == facts.SymbolInterface {
			bodiless[f.Name] = true
		}
		if b, _ := f.PropAny(propAbstract).(bool); b {
			bodiless[f.Name] = true
		}
	}

	io := make(map[string]bool)
	callers := make(map[string][]string) // callee → the names whose answer depends on it
	implementers := make(map[string][]string)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if b, _ := f.PropAny(propIODirect).(bool); b {
			io[f.Name] = true
		}
		seen := make(map[string]bool)
		if once, ok := f.PropAny(propCallsOnce).([]string); ok {
			for _, c := range once {
				seen[c] = true // never followed
			}
		}
		for _, r := range f.Relations {
			switch r.Kind {
			case facts.RelCalls:
				if r.Target == f.Name || seen[r.Target] || !exists[r.Target] {
					continue
				}
				seen[r.Target] = true
				callers[r.Target] = append(callers[r.Target], f.Name)
			case facts.RelImplements:
				if exists[r.Target] {
					implementers[r.Target] = append(implementers[r.Target], f.Name)
				}
			}
		}
	}

	for iface, impls := range declared {
		for _, impl := range impls {
			if exists[iface] && exists[impl] {
				implementers[iface] = append(implementers[iface], impl)
			}
		}
	}

	// A bodiless method depends on each implementer's method of the same name, the
	// way a caller depends on its callee.
	if len(implementers) > 0 {
		for name := range exists {
			owner, sep, member := splitMember(name)
			if owner == "" || len(implementers[owner]) == 0 || (!bodiless[owner] && !bodiless[name]) {
				continue
			}
			for _, impl := range transitive(owner, implementers) {
				if m := impl + sep + member; exists[m] && m != name {
					callers[m] = append(callers[m], name)
				}
			}
		}
	}

	// Backwards from the direct I/O: each name is visited once.
	work := make([]string, 0, len(io))
	for name := range io {
		work = append(work, name)
	}
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		if opts.Opaque != nil && opts.Opaque(name) {
			continue
		}
		for _, c := range callers[name] {
			if !io[c] && (opts.Opaque == nil || !opts.Opaque(c)) {
				io[c] = true
				work = append(work, c)
			}
		}
	}

	for i := range all {
		f := &all[i]
		if f.Kind == facts.KindSymbol && io[f.Name] {
			f.SetProp(propPerformsIO, true)
		}
	}
}

// splitMember splits a method's name into its owning type, the separator the
// language joins them with, and the member: `pkg.Type.Save` and `Ns\Type::save`.
// A name with no separator has no owner.
func splitMember(name string) (owner, sep, member string) {
	if i := strings.LastIndex(name, "::"); i > 0 {
		return name[:i], "::", name[i+2:]
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i], ".", name[i+1:]
	}
	return "", "", name
}

// transitive returns every type that implements root, directly or through a type
// that does: an interface extended by another interface is implemented by that
// one's implementers too.
func transitive(root string, implementers map[string][]string) []string {
	var out []string
	seen := map[string]bool{root: true}
	queue := implementers[root]
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		queue = append(queue, implementers[t]...)
	}
	return out
}
