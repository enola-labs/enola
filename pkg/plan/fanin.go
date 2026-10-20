package plan

import "github.com/enola-labs/enola/internal/facts"

// FanIn is what a blast radius reads its fan-in from: for each name, the facts
// whose calls, depends_on, implements or imports relation targets it.
//
// It reads the relations as written, which is what ReverseLookup answers on a
// store with no graph, and a contract store never has one. ReverseLookup gets
// there by scanning every fact, and the blast radius asked it four times for
// each member of a target: at 2.3M facts that was 99% of a plan. The graph has
// a reverse index of its own, but it adds edges no relation holds (a module to
// what its dependency facts import) and retargets calls made through a Go
// module path, so its fan-in is not the scan's and a report read from it would
// move.
type FanIn struct {
	facts []facts.Fact
	// byTarget holds the one fact targeting a name as its index, or, when
	// several do, ^i for more[i]. Most names are targeted by one fact, so this
	// is the shape the store's own name index takes.
	byTarget map[string]int32
	more     [][]int32
}

// IndexFanIn reads store's relations once. The store must not change while the
// index is read, which holds for the contract store a plan is given.
func IndexFanIn(store *facts.Store) *FanIn {
	all := store.FactsRef()
	x := &FanIn{facts: all, byTarget: map[string]int32{}}
	for i, f := range all {
		for _, rel := range f.Relations {
			if isBlastVia(rel.Kind) {
				x.add(rel.Target, int32(i))
			}
		}
	}
	return x
}

// add lists facts in store order, so a fact that relates to one name twice, or
// by two vias, is the last one listed and is listed once, as the scan
// returned it once.
func (x *FanIn) add(target string, idx int32) {
	v, seen := x.byTarget[target]
	switch {
	case !seen:
		x.byTarget[target] = idx
	case v >= 0:
		if v != idx {
			x.more = append(x.more, []int32{v, idx})
			x.byTarget[target] = ^int32(len(x.more) - 1)
		}
	default:
		if list := x.more[^v]; list[len(list)-1] != idx {
			x.more[^v] = append(list, idx)
		}
	}
}

// dependents is the names of the facts relating to any of members, other than
// the members themselves.
func (x *FanIn) dependents(members map[string]bool) map[string]bool {
	in := map[string]bool{}
	note := func(idx int32) {
		if name := x.facts[idx].Name; !members[name] {
			in[name] = true
		}
	}
	for name := range members {
		v, ok := x.byTarget[name]
		switch {
		case !ok:
		case v >= 0:
			note(v)
		default:
			for _, idx := range x.more[^v] {
				note(idx)
			}
		}
	}
	return in
}

func isBlastVia(kind string) bool {
	for _, via := range blastVias {
		if kind == via {
			return true
		}
	}
	return false
}
