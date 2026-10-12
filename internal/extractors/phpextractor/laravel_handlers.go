package phpextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// laravelControllerNamespace is where Laravel looks for a controller a route names
// by string ('OrderController@show').
const laravelControllerNamespace = `App\Http\Controllers`

// maxHandlerAncestors bounds the walk up a controller's parents and traits for an
// inherited handler method.
const maxHandlerAncestors = 8

// bindLaravelHandlers resolves each Laravel route's handler to the controller class
// that declares it, in place: the handler prop becomes `Namespace\Class::method`, and
// the route gains a handled_by edge to the method symbol.
//
// A routes file names its controller by the short name its `use` statement imports,
// or by a string relative to the controller namespace, and the controller lives in
// another file — so the join needs every file's symbols and cannot happen in the
// route walk. A class that does not resolve to exactly one declared class keeps the
// handler as written and gets no edge: a wrong handled_by feeds impact_analysis and
// find_path, and a missing one only leaves them short.
//
// The edge targets the method's declaration, which for an inherited action is on a
// parent class or a trait, while the handler prop keeps the class the route names.
func bindLaravelHandlers(allFacts []facts.Fact) {
	ix := newLaravelClassIndex(allFacts)
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindRoute || f.PropString("framework") != "laravel" {
			continue
		}
		handler := f.PropString("handler")
		sep := strings.LastIndex(handler, "::")
		if sep <= 0 {
			continue
		}
		class := ix.controller(handler[:sep])
		if class == "" {
			continue
		}
		method := handler[sep+2:]
		f.Props["handler"] = class + "::" + method
		if target := ix.declaringMethod(class, method); target != "" && !f.HasRelation(facts.RelHandledBy, target) {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		}
	}
}

// laravelClassIndex is the declared classes and methods of one repository.
type laravelClassIndex struct {
	// parents is each class's extends/implements/trait references, as written.
	parents map[string][]string
	// controllers are the classes a route can name: every class that is not a trait.
	controllers map[string]bool
	// byShort lists the controllers sharing a last namespace segment.
	byShort map[string][]string
	methods map[string]bool
}

func newLaravelClassIndex(allFacts []facts.Fact) *laravelClassIndex {
	ix := &laravelClassIndex{
		parents:     map[string][]string{},
		controllers: map[string]bool{},
		byShort:     map[string][]string{},
		methods:     map[string]bool{},
	}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch f.PropString("symbol_kind") {
		case facts.SymbolMethod:
			ix.methods[f.Name] = true
		case facts.SymbolClass:
			for _, rel := range f.Relations {
				if rel.Kind == facts.RelImplements {
					ix.parents[f.Name] = append(ix.parents[f.Name], rel.Target)
				}
			}
			if _, seen := ix.parents[f.Name]; !seen {
				ix.parents[f.Name] = nil
			}
			if f.PropBool("trait") || ix.controllers[f.Name] {
				continue
			}
			ix.controllers[f.Name] = true
			short := lastNsSegment(f.Name)
			ix.byShort[short] = append(ix.byShort[short], f.Name)
		}
	}
	return ix
}

// controller resolves a class reference from a routes file to a declared class: the
// name as written, then under Laravel's controller namespace, then the one declared
// class the reference is a namespace suffix of. "" when none or several answer.
func (ix *laravelClassIndex) controller(ref string) string {
	ref = strings.Trim(ref, `\`)
	if ix.controllers[ref] {
		return ref
	}
	if fqn := laravelControllerNamespace + `\` + ref; ix.controllers[fqn] {
		return fqn
	}
	found := ""
	for _, fqn := range ix.byShort[lastNsSegment(ref)] {
		if fqn != ref && !strings.HasSuffix(fqn, `\`+ref) {
			continue
		}
		if found != "" {
			return ""
		}
		found = fqn
	}
	return found
}

// declaringMethod returns the symbol declaring a class's method: on the class, else
// on the nearest parent class or trait that declares it. "" when none in this
// repository does, which is every action a framework base class provides.
func (ix *laravelClassIndex) declaringMethod(class, method string) string {
	seen := map[string]bool{class: true}
	frontier := []string{class}
	for depth := 0; depth <= maxHandlerAncestors && len(frontier) > 0; depth++ {
		var next []string
		for _, c := range frontier {
			if name := c + "::" + method; ix.methods[name] {
				return name
			}
			for _, ref := range ix.parents[c] {
				if parent := ix.parent(c, ref); parent != "" && !seen[parent] {
					seen[parent] = true
					next = append(next, parent)
				}
			}
		}
		frontier = next
	}
	return ""
}

// parent resolves a class's parent reference. The symbol walk expands imported
// names and leaves a same-namespace name bare, so the child's namespace is tried
// second.
func (ix *laravelClassIndex) parent(child, ref string) string {
	if _, ok := ix.parents[ref]; ok {
		return ref
	}
	if i := strings.LastIndex(child, `\`); i >= 0 {
		if fqn := child[:i+1] + ref; ix.hasClass(fqn) {
			return fqn
		}
	}
	return ""
}

func (ix *laravelClassIndex) hasClass(name string) bool {
	_, ok := ix.parents[name]
	return ok
}
