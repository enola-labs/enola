package orphans

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// creditCppLifecycle records the use of C++ constructors and destructors.
//
// Neither is called by name: `Holder h(x)`, `Holder(x)` and a derived class's
// construction run Holder::Holder, and the end of each such object's lifetime runs
// Holder::~Holder. The graph records creating an object as an instantiates edge to
// the class, so each instantiation is credited, as a use by the instantiating
// symbol, to the constructor and destructor of the class and of every base it
// derives from (a Derived object runs Base's as well). A class nothing
// instantiates keeps both reported: its constructor and destructor never run.
//
// Classes and members are matched by their qualified name without the directory
// ("db::Holder"), the form references are matched by, because the usual split puts
// the class in include/ and the member definitions in src/.
func creditCppLifecycle(symbols []facts.Fact, refSources refIndex) {
	bases := map[string][]string{}
	type use struct {
		class, source string
		test          bool
	}
	var uses []use
	for i := range symbols {
		f := &symbols[i]
		if f.PropString(propLanguage) != "cpp" {
			continue
		}
		for _, r := range f.Relations {
			switch r.Kind {
			case facts.RelInstantiates:
				uses = append(uses, use{lastSeg(r.Target), f.Name, isTestPath(f.File)})
			case facts.RelImplements:
				own := lastSeg(f.Name)
				bases[own] = append(bases[own], lastSeg(r.Target))
			}
		}
	}
	for _, u := range uses {
		add := refAdder(refSources, u.test)
		seen := map[string]bool{}
		queue := []string{u.class}
		for len(queue) > 0 {
			class := queue[0]
			queue = queue[1:]
			if seen[class] {
				continue
			}
			seen[class] = true
			leaf := class
			if i := strings.LastIndex(class, "::"); i >= 0 {
				leaf = class[i+2:]
			}
			leaf, _, _ = strings.Cut(leaf, "<")
			add(class+"::"+leaf, u.source)
			add(class+"::~"+leaf, u.source)
			queue = append(queue, bases[class]...)
		}
	}
}
