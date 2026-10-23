package tsextractor

import (
	"io/fs"
	"os"
	"path/filepath"
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
// It gives a verdict only inside an application whose every use is in code this
// extractor parses: a Next.js app, or a React app with an entry file, at the root or
// as a workspace package (reachAppDirs). Vue, Svelte, Angular and Ember use components
// and handlers from templates, where a method called only from a template would read
// as unreached, and a library's calls are made by whoever consumes it, so those carry
// no verdict.

// fileReach is what one file contributes: the names each of its symbols uses, and the
// names its top-level code uses.
type fileReach struct {
	file    string
	symbols []string
	uses    map[string]map[string]bool // symbol → names used in its body
	top     map[string]bool
	// The module edges that carry no name a use could match: a default import read
	// under a local name of its own, a default re-exported from another file, and a
	// module loaded by import(). Each names the file it reaches.
	defaults       []string            // the symbols this file default-exports
	defaultAliases map[string][]string // local name → files whose default it binds
	reexports      []string            // files whose default this file re-exports
	dynamic        map[string][]string // symbol ("" for top level) → files it imports dynamically
	imports        []string            // every file this one imports, of any kind
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

// collectReach reads one parsed file. result must hold the file's symbol facts, and
// resolve maps an import specifier to the repository file it names.
func collectReach(kinds *tsutil.KindTable, root *sitter.Node, src []byte, relFile string, result []facts.Fact, resolve func(string) (string, bool)) *fileReach {
	g := callsite.Grammar{
		KindOf:    func(n *sitter.Node) string { return kindOf(kinds, n) },
		Functions: tsFunctionKinds,
		Wrappers:  tsDeclarationWrappers,
	}
	owner := map[uint]string{}
	for _, s := range callsite.Spans(g, root, result) {
		owner[s.StartByte] = s.Symbol
	}
	r := &fileReach{file: relFile, uses: map[string]map[string]bool{}, top: map[string]bool{},
		defaultAliases: map[string][]string{}, dynamic: map[string][]string{}}
	for _, f := range result {
		if f.Kind == facts.KindSymbol && f.File == relFile {
			r.symbols = append(r.symbols, f.Name)
			if f.PropAny(propDefaultExport) == true {
				r.defaults = append(r.defaults, f.Name)
			}
		}
	}
	importsOf(kinds, root, src, r, resolve)
	var walk func(n *sitter.Node, current string)
	walk = func(n *sitter.Node, current string) {
		k := kindOf(kinds, n)
		if tsFunctionKinds[k] {
			if sym, ok := owner[n.StartByte()]; ok {
				current = sym
			}
		}
		if k == "call_expression" {
			if fn := n.ChildByFieldName("function"); fn != nil && kindOf(kinds, fn) == "import" {
				if spec := importSpecifier(kinds, n, src); spec != "" {
					if file, ok := resolve(spec); ok {
						r.dynamic[current] = append(r.dynamic[current], file)
						r.imports = append(r.imports, file)
					}
				}
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

// importsOf records a file's static imports and re-exports: every file it imports, the
// local names default imports bind, and the files whose default it re-exports.
func importsOf(kinds *tsutil.KindTable, root *sitter.Node, src []byte, r *fileReach, resolve func(string) (string, bool)) {
	for i := range root.ChildCount() {
		stmt := root.Child(i)
		k := kindOf(kinds, stmt)
		if k != "import_statement" && k != "export_statement" {
			continue
		}
		source := stmt.ChildByFieldName("source")
		if source == nil {
			source = findChildByKind(kinds, stmt, "string")
		}
		if source == nil || (k == "export_statement" && !strings.Contains(nodeText(stmt, src), " from")) {
			continue
		}
		file, ok := resolve(strings.Trim(nodeText(source, src), `"'`))
		if !ok {
			continue
		}
		r.imports = append(r.imports, file)
		if k == "import_statement" {
			if clause := findChildByKind(kinds, stmt, "import_clause"); clause != nil {
				if def := findChildByKind(kinds, clause, "identifier"); def != nil {
					local := nodeText(def, src)
					r.defaultAliases[local] = append(r.defaultAliases[local], file)
				}
			}
			continue
		}
		// export { default } from '…', export { default as Name } from '…'
		if clause := findChildByKind(kinds, stmt, "export_clause"); clause != nil {
			for j := range clause.NamedChildCount() {
				spec := clause.NamedChild(j)
				name, alias := spec.ChildByFieldName("name"), spec.ChildByFieldName("alias")
				if name == nil || nodeText(name, src) != "default" {
					continue
				}
				if alias != nil {
					r.defaultAliases[nodeText(alias, src)] = append(r.defaultAliases[nodeText(alias, src)], file)
				} else {
					r.reexports = append(r.reexports, file)
				}
			}
		}
	}
}

// importSpecifier is an import() call's specifier when it is written out: a string
// literal, or a template literal with nothing substituted.
func importSpecifier(kinds *tsutil.KindTable, call *sitter.Node, src []byte) string {
	args := callArguments(kinds, call)
	if len(args) == 0 {
		return ""
	}
	switch kindOf(kinds, args[0]) {
	case "string":
		return strings.Trim(nodeText(args[0], src), `"'`)
	case "template_string":
		t := nodeText(args[0], src)
		if !strings.Contains(t, "${") {
			return strings.Trim(t, "`")
		}
	}
	return ""
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

// reachAppDirs lists the application packages the pass gives a verdict in: the root
// when it is a Next.js app, each workspace package that is one, and each package with
// a React dependency, no Vue, Svelte or Angular one, and an entry file. A package
// without an entry is a library, whose calls are made by whoever consumes it.
func reachAppDirs(repoPath string, fw tsFrameworks) []string {
	var out []string
	if fw.isNextJS && !fw.isVue && !fw.isNuxt && !fw.isSvelteKit && !fw.isAngular && !fw.isEmber {
		out = append(out, "")
	}
	out = append(out, fw.nextDirs...)
	seen := map[string]bool{}
	for _, d := range out {
		seen[d] = true
	}
	_ = filepath.WalkDir(repoPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != repoPath && (strings.HasPrefix(name, ".") || tsSkipDirs[name] || name == "testdata" || isExampleDir(name)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "package.json" {
			return nil
		}
		dir := filepath.Dir(p) //factpath:host
		rel, err := filepath.Rel(repoPath, dir)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if seen[rel] || !hasPkgDependency(dir, "react") {
			return nil
		}
		for _, other := range []string{"vue", "svelte", "@angular/core", "ember-source"} {
			if hasPkgDependency(dir, other) {
				return nil
			}
		}
		for _, e := range reactEntryFiles {
			if _, err := os.Stat(filepath.Join(dir, e)); err == nil {
				seen[rel] = true
				out = append(out, rel)
				break
			}
		}
		return nil
	})
	return out
}

// inAppDir reports the app package a file belongs to, if any.
func inAppDir(file string, dirs []string) (string, bool) {
	best, found := "", false
	for _, d := range dirs {
		if (d == "" || file == d || strings.HasPrefix(file, d+"/")) && (!found || len(d) > len(best)) {
			best, found = d, true
		}
	}
	return best, found
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
			if _, ok := inAppDir(f.File, fw.appDirs); ok {
				roots[f.File] = true
			}
		}
	}
	for _, d := range fw.appDirs {
		prefix := ""
		if d != "" {
			prefix = d + "/"
		}
		for _, e := range append(append([]string{}, reactEntryFiles...), "middleware.ts", "src/middleware.ts", "instrumentation.ts", "src/instrumentation.ts") {
			if knownFiles[prefix+e] {
				roots[prefix+e] = true
			}
		}
	}
	if len(roots) == 0 {
		return
	}

	byName := map[string][]string{} // short name → symbols
	symbolFile := map[string]*fileReach{}
	byFile := map[string]*fileReach{}
	aliases := map[string][]string{} // a default import's local name → the files it binds
	importedBy := map[string]int{}
	for _, r := range files {
		if r == nil {
			continue
		}
		byFile[r.file] = r
		for _, s := range r.symbols {
			symbolFile[s] = r
			byName[shortName(s)] = append(byName[shortName(s)], s)
		}
		for name, targets := range r.defaultAliases {
			aliases[name] = append(aliases[name], targets...)
		}
		for _, t := range r.imports {
			if t != r.file {
				importedBy[t]++
			}
		}
	}
	// A file of an application that nothing imports runs on its own if it runs at all:
	// a script, a worker, a function deployed apart. Taken as an entry, it can only
	// make more calls count as reached, which is the safe side.
	for _, r := range files {
		if r == nil || importedBy[r.file] > 0 {
			continue
		}
		if _, ok := inAppDir(r.file, fw.appDirs); ok {
			roots[r.file] = true
		}
	}

	reached := map[string]bool{}
	fileRun := map[string]bool{}
	var queue []string
	usedNames := map[string]bool{}
	reach := func(s string) {
		if !reached[s] {
			reached[s] = true
			queue = append(queue, s)
		}
	}
	var runFile func(r *fileReach)
	// reachDefault reaches what a file default-exports, and runs it. A default import
	// or re-export does use the default, so a file whose default this pass did not
	// mark has every symbol reached instead (orAll). An import() may be for a named
	// export, `const { apiClient } = await import(...)`, which the name already
	// reaches, so it takes the default only.
	reachDefault := func(file string, orAll bool) {
		r := byFile[file]
		if r == nil {
			return
		}
		runFile(r)
		targets := r.defaults
		if len(targets) == 0 && orAll {
			targets = r.symbols
		}
		for _, s := range targets {
			reach(s)
		}
	}
	use := func(names map[string]bool) {
		for n := range names {
			if usedNames[n] {
				continue
			}
			usedNames[n] = true
			for _, s := range byName[n] {
				reach(s)
			}
			for _, f := range aliases[n] {
				reachDefault(f, true)
			}
		}
	}
	runFile = func(r *fileReach) {
		if r == nil || fileRun[r.file] {
			return
		}
		fileRun[r.file] = true
		use(r.top)
		for _, f := range r.reexports {
			reachDefault(f, true)
		}
		for _, f := range r.dynamic[""] {
			reachDefault(f, false)
		}
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
			for _, f := range r.dynamic[s] {
				reachDefault(f, false)
			}
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
		if _, ok := inAppDir(f.File, fw.appDirs); !ok {
			continue // library code: its calls are made by whoever consumes it
		}
		ok := fileRun[f.File]
		if caller := f.PropString(facts.PropCaller); caller != "" {
			ok = reached[caller]
		}
		f.SetProp(facts.PropClientReachable, ok)
	}
}

// reachApplies reports a repository with an application whose every use is in code
// this extractor parses.
func reachApplies(fw tsFrameworks) bool {
	return len(fw.appDirs) > 0
}

// shortName is the name a symbol is used by: the last segment of its fact name.
func shortName(symbol string) string {
	if i := strings.LastIndexByte(symbol, '.'); i >= 0 {
		return symbol[i+1:]
	}
	return symbol
}
