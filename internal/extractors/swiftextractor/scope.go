package swiftextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Which declarations a reference can bind to.
//
// The post-passes bind bare names (a type, a member call on an unknown receiver, a
// typed receiver's method) through repo-wide indexes keyed by simple name. A test
// target depends on the code it tests and never the other way round: SPM and Xcode
// do not let a production target import a test target. So a production file's
// reference binds only to production declarations. Without the split, a test's own
// `enum BoundaryGenerator`, a test helper's unique `stamp()` or a test's
// `extension FileManager` captured production references, and impact analysis said
// production depended on its tests. A test file still sees every declaration, as
// before.

// isSwiftTestTarget reports whether a file sits under a test target's directory:
// a directory whose name ends in Tests (SPM's Tests/, Xcode's <Target>Tests/ and
// <Target>UITests/). It is the rule of the default ignore glob
// `**/*Tests/**/*.swift`, so under the defaults no indexed file is a test file and
// nothing changes. A file name is not consulted: `ABTest.swift` is production.
func isSwiftTestTarget(relFile string) bool {
	segs := strings.Split(relFile, "/")
	for _, d := range segs[:len(segs)-1] {
		if strings.HasSuffix(d, "Tests") {
			return true
		}
	}
	return false
}

// swiftNameIndex is the simple-name lookup the post-passes bind through.
type swiftNameIndex struct {
	types     map[string]string   // simple type name -> module identity, the last declaration's
	ambiguous map[string]bool     // simple type name -> declared in more than one module
	methods   map[string][]string // method short name -> qualified dir.Type.method names
	funcs     map[string][]string // top-level function short name -> qualified dir.func names
	// owners maps the simple name of every declaration (a type, a constant, a
	// member) to the simple names of what it is declared in: its type, or for a
	// top-level declaration its module. concrete holds the simple names of the
	// classes, structs, enums and actors the repository declares.
	owners   map[string][]string
	concrete map[string]bool
}

func newSwiftNameIndex() swiftNameIndex {
	return swiftNameIndex{
		types:     make(map[string]string),
		ambiguous: make(map[string]bool),
		methods:   make(map[string][]string),
		funcs:     make(map[string][]string),
		owners:    make(map[string][]string),
		concrete:  make(map[string]bool),
	}
}

// add indexes one declaration fact of a file in module dir.
func (ix *swiftNameIndex) add(fact *facts.Fact, dir string) {
	short := lastDotComponent(fact.Name)
	ix.owners[short] = append(ix.owners[short], lastDotComponent(ownerOf(fact.Name)))
	sk, _ := fact.PropAny("symbol_kind").(string)
	if sk == facts.SymbolStruct || sk == facts.SymbolClass {
		ix.concrete[lastDotComponent(fact.Name)] = true
	}
	switch sk {
	case facts.SymbolStruct, facts.SymbolClass, facts.SymbolInterface:
		if simpleName := short; simpleName != "" {
			// A bare type name is not unique across modules: Swift namespaces
			// nested types by their enclosing type/module, so short names like
			// Event/State/Style/Coordinator recur in many targets. Track which
			// names are defined in >1 module so the cross-module reference pass
			// can refuse to fabricate an edge from an ambiguous name (which
			// would otherwise resolve to one arbitrary owning module and create
			// a false, often cyclic, dependency).
			if prev, ok := ix.types[simpleName]; ok && prev != dir {
				ix.ambiguous[simpleName] = true
			}
			ix.types[simpleName] = dir
		}
	case facts.SymbolMethod:
		// Index methods by short name so the resolveMethodCalls post-pass can
		// bind tentative member-call edges (self?.x(), coordinator?.y()) to a
		// concrete method, or drop them when no project method matches.
		if short != "" {
			ix.methods[short] = append(ix.methods[short], fact.Name)
		}
	case facts.SymbolFunc:
		if short == "" {
			break
		}
		// Operator overloads (func +, func <-, …) go in methods so
		// custom-operator usage edges bind.
		if isOperatorToken(short) {
			ix.methods[short] = append(ix.methods[short], fact.Name)
		}
		// All top-level functions also go in funcs: the fallback used by
		// resolveMethodCalls when a member call's receiver type failed to parse
		// and its methods were flattened to top-level functions. Kept separate
		// from methods so a genuine method call is only rescued by a top-level
		// function when no real method of that name exists.
		ix.funcs[short] = append(ix.funcs[short], fact.Name)
	}
}

// swiftIndexes holds the index over every declaration and the one over production
// declarations alone.
type swiftIndexes struct {
	all, prod swiftNameIndex
}

func newSwiftIndexes() *swiftIndexes {
	return &swiftIndexes{all: newSwiftNameIndex(), prod: newSwiftNameIndex()}
}

// add indexes a file's symbol facts.
func (s *swiftIndexes) add(relFile, dir string, fileFacts []facts.Fact) {
	test := isSwiftTestTarget(relFile)
	for i := range fileFacts {
		if fileFacts[i].Kind != facts.KindSymbol {
			continue
		}
		s.all.add(&fileFacts[i], dir)
		if !test {
			s.prod.add(&fileFacts[i], dir)
		}
	}
}

// forFile returns the index a reference made in file binds through.
func (s *swiftIndexes) forFile(file string) *swiftNameIndex {
	if isSwiftTestTarget(file) {
		return &s.all
	}
	return &s.prod
}

// swiftHierarchies is the type hierarchy over every declaration and over production
// declarations alone, built on first use.
type swiftHierarchies struct {
	facts     []facts.Fact
	all, prod *swiftHierarchy
}

// forFile returns the hierarchy a call made in file resolves through.
func (s *swiftHierarchies) forFile(file string) *swiftHierarchy {
	if isSwiftTestTarget(file) {
		if s.all == nil {
			s.all = newSwiftHierarchy(s.facts, false)
		}
		return s.all
	}
	if s.prod == nil {
		s.prod = newSwiftHierarchy(s.facts, true)
	}
	return s.prod
}

// reachesRepo reports whether a call chain rooted at root can hand on a value of the
// repository's: the repository declares root (a type, or a constant such as a global
// `let AF = Session.default`), or a member the chain passes through is one it
// declares outside its classes and structs (`SecPolicy.af`, a property a protocol
// extension of the repository adds to every conforming type).
func (ix *swiftNameIndex) reachesRepo(root string, via []string) bool {
	if len(ix.owners[root]) > 0 {
		return true
	}
	for _, m := range via {
		for _, o := range ix.owners[m] {
			if !ix.concrete[o] {
				return true
			}
		}
	}
	return false
}

// openCandidates returns the methods named method that a value the repository did
// not make can have: those not owned by a class or struct of the repository, an
// extension of a framework type or protocol's included. With none, the top-level
// functions of that name (a module-qualified call, `SharedModels.score(word)`).
func (ix *swiftNameIndex) openCandidates(method string) []string {
	var open []string
	for _, c := range ix.methods[method] {
		if !ix.concrete[lastDotComponent(ownerOf(c))] {
			open = append(open, c)
		}
	}
	if len(open) == 0 {
		return ix.funcs[method]
	}
	return open
}
