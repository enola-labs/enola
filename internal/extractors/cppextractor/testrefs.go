package cppextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// testRefsFrom folds the usage edges of the facts declared in test files into one
// KindTestRef fact per test file.
func testRefsFrom(all []facts.Fact, cppTests []string, isTest map[string]bool) []facts.Fact {
	// Names the test files define: an edge to one is the test calling itself. A
	// bodiless prototype is kept: C tests declare the production function they
	// call (`int checksum(int);`), and the call then resolves to that prototype,
	// whose name still says which function the test reaches. The prototype's name
	// is the test directory's, though, which no production symbol carries, so the
	// target is written without it; and a prototype of a function a test file
	// defines is the test's own.
	testDeclared := make(map[string]bool)
	testDefinedBare := make(map[string]bool)
	testPrototype := make(map[string]string)
	for _, f := range all {
		if f.Kind != facts.KindSymbol || !isTest[factpath.Slash(f.File)] {
			continue
		}
		bare := withoutDir(f.Name, factpath.Dir(f.File))
		if isBodilessFunc(f) {
			testPrototype[f.Name] = bare
			continue
		}
		testDeclared[f.Name] = true
		testDefinedBare[bare] = true
	}
	for name, bare := range testPrototype {
		if testDefinedBare[bare] {
			delete(testPrototype, name)
			testDeclared[name] = true
		}
	}
	// A test helper library is often declared in a production header (KUnit
	// helpers under include/) and defined in the test tree. A call to such an
	// external definition is a use of the header's declaration, which is all the
	// graph keeps of it, so its target is written without the test directory. A
	// static helper is the file's own and stays dropped.
	prodPrototype := make(map[string]bool)
	for _, f := range all {
		if f.Kind == facts.KindSymbol && !isTest[factpath.Slash(f.File)] && isBodilessFunc(f) {
			prodPrototype[afterDir(f.Name)] = true
		}
	}
	testExternal := make(map[string]string)
	for _, f := range all {
		if f.Kind == facts.KindSymbol && testDeclared[f.Name] && f.PropBool("exported") && !f.PropBool("static") {
			if bare := afterDir(f.Name); prodPrototype[bare] {
				testExternal[f.Name] = bare
			}
		}
	}
	bases := cppBases(all)
	// A module fact is a directory; its file-scope references belong to the
	// directory's first test file, and only when every file there is a test.
	firstTestInDir := make(map[string]string)
	for _, relFile := range cppTests {
		dir := factpath.Dir(relFile)
		if cur, ok := firstTestInDir[dir]; !ok || relFile < cur {
			firstTestInDir[dir] = relFile
		}
	}
	prodDir := make(map[string]bool)
	for _, f := range all {
		if f.Kind == facts.KindSymbol && !isTest[factpath.Slash(f.File)] {
			prodDir[factpath.Dir(f.File)] = true
		}
	}

	targets := make(map[string]map[string]bool) // test file -> targets
	add := func(file string, cpp bool, rels []facts.Relation) {
		for _, r := range rels {
			if !isUsageRel(r.Kind) || r.Target == "" {
				continue
			}
			target := r.Target
			if bare, ok := testExternal[target]; ok {
				target = bare
			} else if testDeclared[target] {
				continue
			}
			if bare, ok := testPrototype[target]; ok {
				target = bare
			}
			if targets[file] == nil {
				targets[file] = make(map[string]bool)
			}
			targets[file][target] = true
			if r.Kind == facts.RelInstantiates && cpp {
				for _, m := range lifecycleMembers(target, bases) {
					targets[file][m] = true
				}
			}
		}
	}
	for _, f := range all {
		switch f.Kind {
		case facts.KindSymbol:
			if file := factpath.Slash(f.File); isTest[file] {
				add(file, f.PropString("language") == langCpp, f.Relations)
			}
		case facts.KindModule:
			if first, ok := firstTestInDir[f.Name]; ok && !prodDir[f.Name] {
				add(first, f.PropString("language") == langCpp, f.Relations)
			}
		}
	}

	out := make([]facts.Fact, 0, len(targets))
	for _, relFile := range cppTests {
		set := targets[factpath.Slash(relFile)]
		if len(set) == 0 {
			continue
		}
		names := make([]string, 0, len(set))
		for t := range set {
			names = append(names, t)
		}
		// Sorted: the set comes out of a map, and facts.jsonl is hashed into the
		// snapshot id.
		sort.Strings(names)
		rels := make([]facts.Relation, 0, len(names))
		for _, t := range names {
			rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: t})
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindTestRef,
			Name:      relFile,
			File:      relFile,
			Line:      1,
			Props:     map[string]any{"language": languageOfTest(relFile)},
			Relations: rels,
		})
	}
	return out
}

// cppBases maps a C++ class, named without its directory ("db::Holder"), to the
// bases it names, as orphans.creditCppLifecycle reads them.
func cppBases(all []facts.Fact) map[string][]string {
	bases := make(map[string][]string)
	for _, f := range all {
		if f.Kind != facts.KindSymbol || f.PropString("language") != langCpp {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelImplements {
				own := afterDir(f.Name)
				bases[own] = append(bases[own], afterDir(r.Target))
			}
		}
	}
	return bases
}

// lifecycleMembers names the constructor and destructor that creating an object
// of class runs, and those of every base it derives from, in the form the orphan
// detector matches them by ("db::Holder::Holder", "db::Holder::~Holder"). It
// mirrors orphans.creditCppLifecycle, which reads instantiates edges a test ref
// does not carry.
func lifecycleMembers(class string, bases map[string][]string) []string {
	var out []string
	seen := make(map[string]bool)
	queue := []string{afterDir(class)}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		leaf := c
		if i := strings.LastIndex(c, "::"); i >= 0 {
			leaf = c[i+2:]
		}
		leaf, _, _ = strings.Cut(leaf, "<")
		out = append(out, c+"::"+leaf, c+"::~"+leaf)
		queue = append(queue, bases[c]...)
	}
	return out
}

// afterDir drops the "<dir>." of a canonical name: the text after its last '.'.
func afterDir(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// isUsageRel reports the edge kinds that make their target used: a call, a
// construction, a base class, a function named as a value. Includes and
// declarations say nothing about a symbol.
func isUsageRel(kind string) bool {
	switch kind {
	case facts.RelCalls, facts.RelInstantiates, facts.RelImplements, facts.RelNames:
		return true
	}
	return false
}

// isBodilessFunc reports a function or method prototype: declared, never defined
// in the file.
func isBodilessFunc(f facts.Fact) bool {
	switch f.PropString("symbol_kind") {
	case facts.SymbolFunc, facts.SymbolMethod:
		hasBody, _ := f.PropAny("has_body").(bool)
		return !hasBody
	}
	return false
}

// withoutDir drops a canonical name's "<dir>." prefix: lib/tests.checksum is
// checksum.
func withoutDir(name, dir string) string {
	if rest, ok := strings.CutPrefix(name, dir+"."); ok && rest != "" {
		return rest
	}
	return name
}

func languageOfTest(relFile string) string {
	if isCFile(relFile) {
		return langC
	}
	return langCpp
}
