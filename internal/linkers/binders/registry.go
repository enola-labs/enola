// Package binders holds the registry for plugin.Binder implementations — the passes
// that resolve references across an assembled fact set, once every extractor has run
// and everything is in one store.
//
// Each binder lives in its own subpackage so a new one is an added package and a
// registration line, not an edit to the engine. Before this existed the four of them
// were methods on *Engine, called in a fixed five-call sequence, with the ordering
// constraints between them recorded nowhere: a technology-specific pass could only be
// added by editing the engine, and the one ordering dependency that actually mattered
// looked exactly like the three that did not.
package binders

import "github.com/enola-labs/enola/pkg/plugin"

// Binder is the plugin interface implemented by everything registered here.
type Binder = plugin.Binder

// Registry holds registered binders, in registration order.
type Registry struct {
	binders []Binder
}

// NewRegistry creates an empty binder registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a binder to the registry.
func (r *Registry) Register(b Binder) {
	r.binders = append(r.binders, b)
}

// Get returns the binder with the given name, or nil if not registered.
func (r *Registry) Get(name string) Binder {
	for _, b := range r.binders {
		if b.Name() == name {
			return b
		}
	}
	return nil
}

// All returns every registered binder, in registration order.
func (r *Registry) All() []Binder {
	return r.binders
}

// Stage returns the registered binders declaring the given stage, in the order they
// run.
//
// A binder that reads what a stage-mate writes declares it (plugin.OrderedBinder) and
// runs after every binder of that name; the rest keep registration order, which then
// carries no meaning: only the declared order changes what a stage writes, so the
// registration block can be reordered without changing a snapshot. A name outside the
// stage is ignored, and a cycle, which no binder should declare, runs what remains in
// registration order rather than dropping a binder.
func (r *Registry) Stage(s plugin.BindStage) []Binder {
	var in []Binder
	pending := map[string]int{} // name -> binders of that name not yet placed
	for _, b := range r.binders {
		if b.Stage() == s {
			in = append(in, b)
			pending[b.Name()]++
		}
	}
	ready := func(b Binder) bool {
		ob, ok := b.(plugin.OrderedBinder)
		if !ok {
			return true
		}
		for _, name := range ob.RunsAfter() {
			if name != b.Name() && pending[name] > 0 {
				return false
			}
		}
		return true
	}
	out := make([]Binder, 0, len(in))
	placed := make([]bool, len(in))
	for len(out) < len(in) {
		next := -1
		for i, b := range in {
			if !placed[i] && ready(b) {
				next = i
				break
			}
		}
		if next < 0 { // a cycle: the first remaining binder breaks it
			for i := range in {
				if !placed[i] {
					next = i
					break
				}
			}
		}
		placed[next] = true
		pending[in[next].Name()]--
		out = append(out, in[next])
	}
	return out
}
