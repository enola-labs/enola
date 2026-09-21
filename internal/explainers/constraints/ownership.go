package constraints

import (
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
	"strings"
)

// What a component owns is declared, never inferred, and the two declaring
// places are read through exactly one statement of precedence —
// intent.OwnershipPrecedence. Nothing in this file decides an order; it reads
// one, so a later change cannot invert the precedence here while the validator
// still states the other one.
//
// Ownership widens what a rule may WALK, never what the component contains.
// Membership stays what the selectors chose: cap counts the same set, require
// reads the same facts, and `constraints lint` prints the same numbers. What
// changes is that an edge sourced by one of a member's METHODS, or landing on
// one, now resolves onto the component — and the verdict says so in the owned
// basis rather than claiming an exactness it does not have.
//
// Methods, and nothing else. The whole of what this file reads is the graph's
// has_method edges, so a constant, a nested class or an attr_accessor variable
// written in a member's body is not owned and an edge landing on one lands
// outside the component.

// methodIndex answers which facts are a member's methods. It reads the graph's
// synthesized has_method edges rather than re-deriving ownership from the shape
// of a name, so there is one definition of what makes a fact a member's and
// every surface that asks gets the same answer. Ruby names an instance method
// `Owner#method` and a class's calls ride those facts, which is the whole
// reason the question exists. Facts a member merely lexically encloses carry no
// has_method edge and are therefore not here.
type methodIndex struct {
	store *facts.Store
	graph *facts.Graph
	owned map[string][]facts.Fact
}

func newMethodIndex(store *facts.Store) *methodIndex {
	return &methodIndex{store: store, owned: map[string][]facts.Fact{}}
}

// methodsOfOwner lists one owner's methods, sorted for the same reason every
// membership is: a walk over them must be a function of the graph and never of
// store order. The graph is built on first use — a snapshot whose declaration
// owns nothing never pays for it.
func (mi *methodIndex) methodsOfOwner(owner string) []facts.Fact {
	if owned, cached := mi.owned[owner]; cached {
		return owned
	}
	if mi.graph == nil {
		mi.graph = facts.NewGraph(mi.store.FactsRef())
	}
	var owned []facts.Fact
	for _, edge := range mi.graph.ForwardEdges(owner) {
		if edge.RelKind != facts.RelHasMethod {
			continue
		}
		owned = append(owned, mi.store.LookupByExactName(edge.Target)...)
	}
	sortFactsByNameThenFile(owned)
	mi.owned[owner] = owned
	return owned
}

// ownedFacts is one component's owned set: the method facts, and which member
// declares each. The second half is what lets a target-side resolution name the
// member a verdict is about rather than the method fact the edge spelled.
type ownedFacts struct {
	facts   []facts.Fact
	ownerOf map[string]string
}

// resolver answers how one end of a measured edge resolves onto a component.
// Both questions it can be asked — did this fact MAKE the edge, did this edge
// LAND on the component — take a rule, because a rule may override what a
// component owns for its own reach; and a caller asks exactly one of them per
// role, because a role resolves through one end of the edge and never both.
type resolver struct {
	components  map[string]component
	members     map[string]map[string]bool
	memberFacts map[string][]facts.Fact
	// carried is the ownership-free source set: the component's own members plus
	// the dependency carriers of its files. Ownership adds to it per rule.
	carried map[string][]facts.Fact
	ground  *grounding
	methods *methodIndex
	owned   map[string]*ownedFacts
	// memberFiles indexes, per component and member name, the files the
	// member was measured in, built on first use by a relation carrying a
	// target file.
	memberFiles map[string]map[string]map[string]bool
}

func newResolver(store *facts.Store, components map[string]component, members map[string]map[string]bool, memberFacts, carried map[string][]facts.Fact, ground *grounding) *resolver {
	return &resolver{
		components:  components,
		members:     members,
		memberFacts: memberFacts,
		carried:     carried,
		ground:      ground,
		methods:     newMethodIndex(store),
		owned:       map[string]*ownedFacts{},
	}
}

// ownsMethods applies the precedence to one (rule, component) pair. It is the
// only place either declaration is read.
func (rs *resolver) ownsMethods(r rule, name string) bool {
	owns, _ := intent.OwnershipPrecedence(rs.components[name].owns, r.owns[name])
	return owns == intent.OwnsMethods
}

// ownedMethodsOf indexes a component's members' methods, once per component and
// independently of any rule: which facts are a member's methods is a fact about
// the graph, while whether they COUNT is the declaration's answer and is asked
// per rule.
func (rs *resolver) ownedMethodsOf(name string) *ownedFacts {
	if owned, cached := rs.owned[name]; cached {
		return owned
	}
	owned := &ownedFacts{ownerOf: map[string]string{}}
	for _, m := range rs.memberFacts[name] {
		for _, f := range rs.methods.methodsOfOwner(m.Name) {
			if rs.members[name][f.Name] {
				continue
			}
			if _, seen := owned.ownerOf[f.Name]; seen {
				continue
			}
			owned.ownerOf[f.Name] = m.Name
			owned.facts = append(owned.facts, f)
		}
	}
	sortFactsByNameThenFile(owned.facts)
	rs.owned[name] = owned
	return owned
}

// sources lists every fact a rule may walk edges FROM for one component: its
// members, the dependency carriers of its files, and — where the declaration
// says so — its members' methods.
func (rs *resolver) sources(r rule, name string) []facts.Fact {
	if !rs.ownsMethods(r, name) {
		return rs.carried[name]
	}
	owned := rs.ownedMethodsOf(name)
	out := make([]facts.Fact, 0, len(rs.carried[name])+len(owned.facts))
	out = append(out, rs.carried[name]...)
	out = append(out, owned.facts...)
	sortFactsByNameThenFile(out)
	return out
}

// source reports how the fact that made an edge resolves onto the component,
// and whether it resolves at all. Exact first, then owned, then the file join a
// dependency carrier rides — strictly in that order, so a fact that is a member
// is never reported as anything weaker.
func (rs *resolver) source(r rule, name string, f facts.Fact) (basis, bool) {
	if rs.members[name][f.Name] {
		return exactBasis, true
	}
	if rs.ownsMethods(r, name) {
		if _, owned := rs.ownedMethodsOf(name).ownerOf[f.Name]; owned {
			return ownedBasis, true
		}
	}
	c, declared := rs.components[name]
	if declared && f.Kind == facts.KindDependency && carrierFor(f, c) {
		return groundedBasis, true
	}
	return exactBasis, false
}

// sourceIn is the source question asked of several components at once — the
// resolution the reverse-walking forms (protect, private) share as "is this
// edge's source inside any component the rule allows".
func (rs *resolver) sourceIn(r rule, names []string, f facts.Fact) bool {
	for _, name := range names {
		if _, resolved := rs.source(r, name, f); resolved {
			return true
		}
	}
	return false
}

// memberOfSource names the member a walked source fact IS or is a method of. A
// dependency carrier belongs to no member — the file join proves the file made
// the edge, never which fact in it did — so it yields none, and a per-member
// form reads its edges through the file instead.
func (rs *resolver) memberOfSource(r rule, name string, f facts.Fact) (string, bool) {
	if rs.members[name][f.Name] {
		return f.Name, true
	}
	if rs.ownsMethods(r, name) {
		if owner, owned := rs.ownedMethodsOf(name).ownerOf[f.Name]; owned {
			return owner, true
		}
	}
	return "", false
}

// target reports how an edge's target resolves onto the component. Exact name
// first, then a member's method, then the measured file a path-shaped target
// grounds onto — the same order, and for the same reason.
func (rs *resolver) target(r rule, name string, rel facts.Relation, from facts.Fact) (basis, bool) {
	if rs.members[name][rel.Target] && rs.memberAtCarriedFile(name, rel, from) {
		return exactBasis, true
	}
	if rs.ownsMethods(r, name) {
		if _, owned := rs.ownedMethodsOf(name).ownerOf[rel.Target]; owned {
			return ownedBasis, true
		}
	}
	if rs.ground.inComponent(rel, from, name, rs.components) {
		return groundedBasis, true
	}
	return exactBasis, false
}

// memberAtCarriedFile keeps an exact-name membership honest when the producer
// said which file the target is defined in. A name reopened across files is a
// member of every component holding a reopening, so without the carried file a
// read of `Foo::VERSION` landed on whichever component reopened `Foo`; with it,
// the target is a member here only if this component measured that name in
// that file. A relation carrying no file resolves by name as before.
func (rs *resolver) memberAtCarriedFile(name string, rel facts.Relation, from facts.Fact) bool {
	file, _ := from.PropAny(facts.PropTargetFile).(string)
	if file == "" {
		return true
	}
	if rs.memberFiles == nil {
		rs.memberFiles = map[string]map[string]map[string]bool{}
	}
	files, indexed := rs.memberFiles[name]
	if !indexed {
		files = map[string]map[string]bool{}
		for _, m := range rs.memberFacts[name] {
			if m.File == "" {
				continue
			}
			if files[m.Name] == nil {
				files[m.Name] = map[string]bool{}
			}
			files[m.Name][m.File] = true
			if m.Repo != "" {
				files[m.Name][strings.TrimPrefix(m.File, m.Repo+"/")] = true
			}
		}
		rs.memberFiles[name] = files
	}
	at := files[rel.Target]
	return at[file] || (from.Repo != "" && at[from.Repo+"/"+file])
}

// memberBehind names the member a resolved target is a verdict about: the
// target itself when it is a member, the declaring member when the target is a
// method the declaration owns. A grounded target names no member and yields
// none — groundedMembers answers that question, through the file.
func (rs *resolver) memberBehind(r rule, name, target string) (string, bool) {
	if rs.members[name][target] {
		return target, true
	}
	if rs.ownsMethods(r, name) {
		if owner, owned := rs.ownedMethodsOf(name).ownerOf[target]; owned {
			return owner, true
		}
	}
	return "", false
}
