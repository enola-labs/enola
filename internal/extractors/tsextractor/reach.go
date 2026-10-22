package tsextractor

import (
	"path"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/callsite"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/facts"
)

// Which client calls the application can run.
//
// A frontend keeps client code no page reaches: an API method whose only caller is a
// facade method nothing calls, a method an interface declares and no one invokes.
// A request written there is in the source and is never sent, yet as a client route
// it made the endpoint it names look called. This pass marks each hand-written client
// route with PropClientReachable: whether its caller is reached from the
// application's entry points.
//
// Reachability is read by name, because the calls that matter are the ones a call
// graph cannot bind: `const api = useApi(); api.getItem(id)` reaches the client
// through a React context. A function or method is reached when its name is used in
// code already reached; everything an entry file declares is reached; and a file's
// top-level code runs once anything in it is reached. A name two symbols share
// reaches both, so the pass errs toward reached, which is the safe direction for a
// verdict that may drive an endpoint's removal.
//
// It runs only where every use is in code this extractor parses: a Next.js or React
// application. Vue, Svelte, Angular and Ember use components and handlers from
// templates, and a method called only from a template would read as unreached, so
// there the routes carry no verdict.

// fileReach is what one file contributes: the names each of its symbols uses, and the
// names its top-level code uses.
type fileReach struct {
	file    string
	symbols []string
	uses    map[string]map[string]bool // symbol → names used in its body
	top     map[string]bool
}

// declaredNameOf maps a declaration's kind to the fields that name what it declares
// rather than use: their identifiers are not uses.
var declaredNameOf = map[string][]string{
	"function_declaration":           {"name"},
	"generator_function_declaration": {"name"},
	"class_declaration":              {"name"},
	"abstract_class_declaration":     {"name"},
	"interface_declaration":          {"name"},
	"type_alias_declaration":         {"name"},
	"method_definition":              {"name"},
	"method_signature":               {"name"},
	"abstract_method_signature":      {"name"},
	"public_field_definition":        {"name"},
	"property_signature":             {"name"},
	"variable_declarator":            {"name"},
	"pair":                           {"key"},
}

// collectReach reads one parsed file. result must hold the file's symbol facts.
func collectReach(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, result []facts.Fact) *fileReach {
	g := callsite.Grammar{
		KindOf:    func(n *sitter.Node) string { return kindOf(kinds, n) },
		Functions: tsFunctionKinds,
		Wrappers:  tsDeclarationWrappers,
	}
	owner := map[uint]string{}
	for _, s := range callsite.Spans(g, root, result) {
		owner[s.StartByte] = s.Symbol
	}
	r := &fileReach{file: relFile, uses: map[string]map[string]bool{}, top: map[string]bool{}}
	for _, f := range result {
		if f.Kind == facts.KindSymbol && f.File == relFile {
			r.symbols = append(r.symbols, f.Name)
		}
	}
	var walk func(n *sitter.Node, current string)
	walk = func(n *sitter.Node, current string) {
		k := kindOf(kinds, n)
		if tsFunctionKinds[k] {
			if sym, ok := owner[n.StartByte()]; ok {
				current = sym
			}
		}
		switch k {
		case "identifier", "property_identifier", "shorthand_property_identifier",
			"shorthand_property_identifier_pattern", "type_identifier":
			if !isDeclaredName(kinds, n) {
				name := nodeText(n, src)
				if current == "" {
					r.top[name] = true
				} else {
					if r.uses[current] == nil {
						r.uses[current] = map[string]bool{}
					}
					r.uses[current][name] = true
				}
			}
		}
		for i := range n.ChildCount() {
			walk(n.Child(i), current)
		}
	}
	walk(root, "")
	return r
}

// isDeclaredName reports an identifier that names what its parent declares.
func isDeclaredName(kinds *tsutil.KindTable, n *sitter.Node) bool {
	p := n.Parent()
	if p == nil {
		return false
	}
	for _, field := range declaredNameOf[kindOf(kinds, p)] {
		if c := p.ChildByFieldName(field); c != nil && c.StartByte() == n.StartByte() && c.EndByte() == n.EndByte() {
			return true
		}
	}
	return false
}

// reactEntryFiles are where a React application without a file router starts.
var reactEntryFiles = []string{
	"src/main.tsx", "src/main.ts", "src/main.jsx", "src/main.js",
	"src/index.tsx", "src/index.ts", "src/index.jsx", "src/index.js",
	"index.tsx", "index.ts", "index.jsx", "index.js", "App.tsx", "App.jsx",
}

// markClientReachability sets PropClientReachable on the hand-written client routes in
// ff, from what each file uses. It does nothing outside a Next.js or React application,
// or when no entry file is found.
func markClientReachability(ff []facts.Fact, files []*fileReach, fw tsFrameworks, knownFiles map[string]bool) {
	if !reachApplies(fw) {
		return
	}
	roots := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString("framework") == "nextjs" && f.PropAny(facts.PropRole) != facts.RoleClient {
			roots[f.File] = true
		}
	}
	for file := range knownFiles {
		base := path.Base(file)
		stem := strings.TrimSuffix(base, path.Ext(base))
		if (stem == "middleware" || stem == "instrumentation") && fw.isNextJS && !strings.Contains(strings.TrimPrefix(file, "src/"), "/") {
			roots[file] = true
		}
	}
	if fw.isReact {
		for _, e := range reactEntryFiles {
			if knownFiles[e] {
				roots[e] = true
			}
		}
	}
	if len(roots) == 0 {
		return
	}

	byName := map[string][]string{} // short name → symbols
	symbolFile := map[string]*fileReach{}
	byFile := map[string]*fileReach{}
	for _, r := range files {
		if r == nil {
			continue
		}
		byFile[r.file] = r
		for _, s := range r.symbols {
			symbolFile[s] = r
			byName[shortName(s)] = append(byName[shortName(s)], s)
		}
	}

	reached := map[string]bool{}
	fileRun := map[string]bool{}
	var queue []string
	usedNames := map[string]bool{}
	use := func(names map[string]bool) {
		for n := range names {
			if usedNames[n] {
				continue
			}
			usedNames[n] = true
			for _, s := range byName[n] {
				if !reached[s] {
					reached[s] = true
					queue = append(queue, s)
				}
			}
		}
	}
	runFile := func(r *fileReach) {
		if r == nil || fileRun[r.file] {
			return
		}
		fileRun[r.file] = true
		use(r.top)
	}
	for file := range roots {
		r := byFile[file]
		if r == nil {
			continue
		}
		runFile(r)
		for _, s := range r.symbols {
			if !reached[s] {
				reached[s] = true
				queue = append(queue, s)
			}
		}
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		r := symbolFile[s]
		runFile(r)
		if r != nil {
			use(r.uses[s])
		}
	}

	for i := range ff {
		f := &ff[i]
		if f.Kind != facts.KindRoute || f.PropAny(facts.PropRole) != facts.RoleClient ||
			!facts.HandWrittenClientSources[f.PropString(facts.PropSource)] || facts.IsTestPath(f.File) {
			continue
		}
		if byFile[f.File] == nil {
			continue // a file this pass did not read: no verdict
		}
		ok := fileRun[f.File]
		if caller := f.PropString(facts.PropCaller); caller != "" {
			ok = reached[caller]
		}
		f.SetProp(facts.PropClientReachable, ok)
	}
}

// reachApplies reports a repository where every use is in code this extractor parses.
func reachApplies(fw tsFrameworks) bool {
	return (fw.isNextJS || fw.isReact) && !fw.isVue && !fw.isNuxt && !fw.isSvelteKit && !fw.isAngular && !fw.isEmber
}

// shortName is the name a symbol is used by: the last segment of its fact name.
func shortName(symbol string) string {
	if i := strings.LastIndexByte(symbol, '.'); i >= 0 {
		return symbol[i+1:]
	}
	return symbol
}
