package rubyextractor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// Route-to-handler binding.
//
// The route walker reads one route file and names each handler from the text of the
// declaration. Whether that name is a symbol is a question about the whole repository,
// and three things decide it that no route file states:
//
//   - An engine that calls isolate_namespace serves every controller from inside that
//     namespace. Solidus draws `namespace :api do resources :orders end` on
//     Spree::Core::Engine, and the controller is Spree::Api::OrdersController. Named
//     without the namespace, 669 of its handled_by relations pointed at classes that do
//     not exist.
//   - An action a controller inherits is declared on an ancestor. The route is served
//     by that method, and naming the subclass names nothing.
//   - A name the application declares as an acronym keeps its capitals. Mastodon's
//     `module: :activitypub` is served from ActivityPub, and camelizing the segment
//     gives Activitypub. The inflection table is configuration, but its effect is
//     visible: exactly one declared class has that name in another casing.
//   - A singular resource is served by the pluralized controller, and the pluralization
//     is ActiveSupport's. The walker cannot check a guess, so it makes none. Here the
//     guess can be checked, and it is kept only when the controller it names exists.
//
// A relation that survives this pass names a declared symbol. One that does not is
// removed: a handler that is not in the graph reads as a bound route to every consumer
// that counts relations, and resolves to nothing for every consumer that follows them.

// singularCandidateProp carries a singular resource's pluralized controller from the
// walker to bindRouteHandlers, which removes it from every fact.
const singularCandidateProp = "_singular_handler"

// engineNamespaces maps each engine class to the namespace it isolates, read from the
// engine.rb files of the repository. An engine that does not call isolate_namespace is
// absent: its controllers are resolved from the top level, as the walker already does.
func engineNamespaces(repoPath string, files []string) map[string]string {
	out := map[string]string{}
	sorted := append([]string{}, files...)
	sort.Strings(sorted)
	for _, relFile := range sorted {
		if filepath.Base(relFile) != "engine.rb" || isNonLiveRoutePath(filepath.ToSlash(relFile)) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		for class, ns := range isolatedNamespaces(src) {
			if _, seen := out[class]; !seen {
				out[class] = ns
			}
		}
	}
	return out
}

// isolatedNamespaces returns, for every class in src that calls isolate_namespace, the
// class's qualified name and the namespace it names.
func isolatedNamespaces(src []byte) map[string]string {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	out := map[string]string{}
	var walk func(n *sitter.Node, nesting []string, class string)
	walk = func(n *sitter.Node, nesting []string, class string) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "module", "class":
			if name := rubyText(n.ChildByFieldName("name"), src); name != "" {
				nesting = append(append([]string{}, nesting...), name)
				if kindOf(n) == "class" {
					class = strings.Join(nesting, "::")
				}
			}
		case "call":
			if class != "" && rubyText(n.ChildByFieldName("method"), src) == "isolate_namespace" {
				if args := n.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
					arg := args.NamedChild(0)
					switch kindOf(arg) {
					case "constant", "scope_resolution":
						out[class] = normalizeConstant(rubyText(arg, src))
					}
				}
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), nesting, class)
		}
	}
	walk(tree.RootNode(), nil, "")
	return out
}

// drawReceiver returns the constant a `X.routes.draw do` call is made on, which for an
// engine's route file is the engine class. `Rails.application.routes.draw` names no
// constant in that position and yields "".
func drawReceiver(call *sitter.Node, src []byte) string {
	routes := call.ChildByFieldName("receiver")
	if routes == nil || kindOf(routes) != "call" || rubyText(routes.ChildByFieldName("method"), src) != "routes" {
		return ""
	}
	owner := routes.ChildByFieldName("receiver")
	if owner == nil {
		return ""
	}
	switch kindOf(owner) {
	case "constant", "scope_resolution":
		return normalizeConstant(rubyText(owner, src))
	}
	return ""
}

// handlerIndex is what the repository declares that a route can be served by.
type handlerIndex struct {
	// declared holds every Ruby symbol name.
	declared map[string]bool
	// superclass maps a class to its superclass as written, which may be relative to
	// the class's own namespace.
	superclass map[string]string
	// folded maps a lower-cased class name to the class declared under it, and to ""
	// when two classes differ only in casing.
	folded map[string]string
	// below maps a class to the classes declared beneath it, built on first use.
	below map[string][]string
}

func buildHandlerIndex(allFacts []facts.Fact) *handlerIndex {
	ix := &handlerIndex{declared: map[string]bool{}, superclass: map[string]string{}, folded: map[string]string{}}
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		ix.declared[f.Name] = true
		if sk, _ := f.PropAny("symbol_kind").(string); sk == facts.SymbolClass {
			key := strings.ToLower(f.Name)
			if prior, seen := ix.folded[key]; seen && prior != f.Name {
				ix.folded[key] = ""
			} else {
				ix.folded[key] = f.Name
			}
		}
		if super, _ := f.PropAny("superclass").(string); super != "" {
			if _, seen := ix.superclass[f.Name]; !seen {
				ix.superclass[f.Name] = super
			}
		}
	}
	return ix
}

// maxAncestors bounds the walk up a controller's superclasses. Real chains are three or
// four long; the bound is what stops a class that names itself as its own ancestor.
const maxAncestors = 12

// resolve returns the declared symbol serving target ("Api::PostsController#index"), or
// "". The action is looked for on the controller and then on each ancestor the
// repository declares. A target with no "#" is a class, and is declared or is not.
func (ix *handlerIndex) resolve(target string) string {
	class, action, isAction := strings.Cut(target, "#")
	if !isAction {
		if ix.declared[target] {
			return target
		}
		return ""
	}
	if !ix.declared[class] {
		if class = ix.folded[strings.ToLower(class)]; class == "" {
			return ""
		}
	}
	return ix.member(class, "#", action)
}

// member returns the method name declares on class or on the nearest ancestor that
// declares it, as "Class#name" for an instance method (sep "#") or "Class.name" for
// a singleton one (sep "."), or "".
func (ix *handlerIndex) member(class, sep, name string) string {
	for range maxAncestors {
		if ix.declared[class+sep+name] {
			return class + sep + name
		}
		super, ok := ix.superclass[class]
		if !ok {
			return ""
		}
		next := ix.lexical(super, class)
		if next == "" || next == class {
			return ""
		}
		class = next
	}
	return ""
}

// lexical resolves a constant written inside from's body the way Ruby looks it up:
// in from's namespace, then in each enclosing one, then at the top level.
func (ix *handlerIndex) lexical(ref, from string) string {
	if strings.HasPrefix(ref, "::") {
		ref = normalizeConstant(ref)
		if ix.declared[ref] {
			return ref
		}
		return ""
	}
	scope := strings.Split(from, "::")
	for i := len(scope) - 1; i >= 0; i-- {
		if c := strings.Join(append(append([]string{}, scope[:i]...), ref), "::"); ix.declared[c] {
			return c
		}
	}
	return ""
}

// bindRouteHandlers settles the handled_by relation of every Rails route in routeFacts
// against the symbols in allFacts. namespaceOf gives the isolated namespace a route
// file's routes are drawn in, when its engine has one.
func bindRouteHandlers(routeFacts, allFacts []facts.Fact, namespaceOf map[string]string) {
	ix := buildHandlerIndex(allFacts)
	for i := range routeFacts {
		f := &routeFacts[i]
		if f.Kind != facts.KindRoute {
			continue
		}
		candidate, _ := f.PropAny(singularCandidateProp).(string)
		delete(f.Props, singularCandidateProp)

		at := -1
		for j := range f.Relations {
			if f.Relations[j].Kind == facts.RelHandledBy {
				at = j
				break
			}
		}
		target := ""
		switch {
		case at >= 0:
			target = f.Relations[at].Target
		case candidate != "":
			target = controllerSymbol(candidate)
		}
		if target == "" {
			continue
		}
		resolved := ""
		// A mount names a class by its full name, which no namespace is added to.
		if ns := namespaceOf[f.File]; ns != "" && strings.Contains(target, "#") {
			resolved = ix.resolve(ns + "::" + target)
		}
		if resolved == "" {
			resolved = ix.resolve(target)
		}
		switch {
		case resolved == "" && at >= 0:
			f.Relations = append(f.Relations[:at], f.Relations[at+1:]...)
		case resolved == "":
		case at >= 0:
			f.Relations[at].Target = resolved
		default:
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: resolved})
			f.Props["handler"] = candidate
		}
	}
}
