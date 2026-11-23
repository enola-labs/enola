package phpextractor

import (
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// namedClass returns the class an expression names without calling it: `Foo::class`,
// a class constant or enum case `Foo::BAR`, a static property `Foo::$bar`. "" for
// anything else, and for self, static, parent and a variable scope, which name no
// class the expression itself states.
//
// A container binding, a config array and an `app(Foo::class)` lookup refer to a
// class this way and no other, so without it the class has no reference at all.
func (w *phpWalker) namedClass(node *sitter.Node) string {
	switch kindOf(node) {
	case "class_constant_access_expression", "scoped_property_access_expression":
	default:
		return ""
	}
	scope := node.NamedChild(0)
	if scope == nil {
		return ""
	}
	switch kindOf(scope) {
	case "name", "qualified_name":
	default:
		return ""
	}
	switch phpText(scope, w.src) {
	case "self", "static", "parent":
		return ""
	}
	return w.className(phpText(scope, w.src))
}

// namedClasses collects the classes named under node, in source order, each once.
// It does not enter a function or method, whose body walk records its own, and with
// skipTypes not a class-like declaration either.
func (w *phpWalker) namedClasses(node *sitter.Node, skipTypes bool, seen map[string]bool, out []string) []string {
	if node == nil {
		return out
	}
	switch kindOf(node) {
	case "method_declaration", "function_definition":
		return out
	case "class_declaration", "interface_declaration", "trait_declaration", "enum_declaration", "anonymous_class":
		if skipTypes {
			return out
		}
	}
	if class := w.namedClass(node); class != "" && !seen[class] {
		seen[class] = true
		out = append(out, class)
	}
	for i := uint(0); i < node.NamedChildCount(); i++ {
		out = w.namedClasses(node.NamedChild(i), skipTypes, seen, out)
	}
	return out
}

// nameClassesInBody records, on a class, the classes its body names outside its
// methods (a property default, a constant) and the classes its own attributes name:
// `#[ObservedBy(OrderObserver::class)]` is how a model registers its observer, and
// nothing else in the code names that class.
func (w *phpWalker) nameClassesInBody(classIdx int, nodes ...*sitter.Node) {
	seen := map[string]bool{w.out[classIdx].Name: true}
	var classes []string
	for _, node := range nodes {
		classes = w.namedClasses(node, false, seen, classes)
	}
	for _, class := range classes {
		w.out[classIdx].Relations = append(w.out[classIdx].Relations, facts.Relation{Kind: facts.RelNames, Target: class})
	}
}

// emitFileScopeNames records what a file's code outside any declaration names,
// constructs and calls: a config file returning an array of providers, an extension
// file returning `new Extender()` objects, a bootstrap script calling
// `Site::fromPaths(...)`. No symbol owns that code, so the references ride on a
// file-reference fact, which only the dead-code detector and the graph read.
//
// Only what the source states by name: a method called on an object of unknown
// type (`$app->make(...)`) is not recorded, since its bare name would bind to every
// method of that name.
func (w *phpWalker) emitFileScopeNames(root *sitter.Node) {
	seen := map[string]bool{}
	classes := w.namedClasses(root, true, map[string]bool{}, nil)
	rels := make([]facts.Relation, 0, len(classes))
	for _, class := range classes {
		rels = append(rels, facts.Relation{Kind: facts.RelNames, Target: class})
		seen[facts.RelNames+" "+class] = true
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindFileRef,
		Name:      w.relFile,
		File:      w.relFile,
		Props:     map[string]any{"language": "php"},
		Relations: rels,
	})
	idx := len(w.out) - 1
	w.fileScope = true
	w.walkForCalls(root, idx, seen)
	w.fileScope = false
	if len(w.out[idx].Relations) == 0 && w.out[idx].Props[propFileScopeCalls] == nil {
		w.out = w.out[:idx]
	}
}

// propFileScopeCalls carries the functions a file's top-level code calls from the
// per-file walk to bindFileScopeCalls; it never reaches the graph.
const propFileScopeCalls = "_file_scope_calls"

func (w *phpWalker) noteFileScopeCall(ownerIdx int, target string) {
	if target == "" || ownerIdx < 0 {
		return
	}
	calls, _ := w.out[ownerIdx].Props[propFileScopeCalls].([]string)
	for _, c := range calls {
		if c == target {
			return
		}
	}
	w.out[ownerIdx].Props[propFileScopeCalls] = append(calls, target)
}

// bindFileScopeCalls turns the function calls file-scope code makes into calls
// edges, for the functions the repository declares only. A framework helper
// (`config()`, `app()`) has no declaration here, and its bare name would bind to
// a directory module of that name. A file reference left with no relation is
// dropped.
func bindFileScopeCalls(allFacts []facts.Fact) []facts.Fact {
	declared := map[string]bool{}
	pending := false
	for i := range allFacts {
		f := &allFacts[i]
		switch {
		case f.Kind == facts.KindSymbol && f.PropString("symbol_kind") == facts.SymbolFunc:
			declared[f.Name] = true
		case f.Kind == facts.KindFileRef && f.Props[propFileScopeCalls] != nil:
			pending = true
		}
	}
	if !pending {
		return allFacts
	}
	out := allFacts[:0]
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindFileRef && f.Props != nil && f.Props[propFileScopeCalls] != nil {
			calls, _ := f.Props[propFileScopeCalls].([]string)
			delete(f.Props, propFileScopeCalls)
			for _, c := range calls {
				if declared[c] && !f.HasRelation(facts.RelCalls, c) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
				}
			}
			if len(f.Relations) == 0 {
				continue
			}
		}
		out = append(out, *f)
	}
	return out
}
