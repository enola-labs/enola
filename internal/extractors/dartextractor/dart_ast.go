package dartextractor

import (
	"strings"
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/enola-labs/enola/internal/extractors/dartextractor/grammar"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// parserPool reuses tree-sitter parsers across files. Creating one per file is a cgo
// allocation plus a language install; the pool makes a large repo measurably cheaper
// without making extraction stateful (each Parse call is independent).
var parserPool = sync.Pool{
	New: func() any {
		p := sitter.NewParser()
		if err := p.SetLanguage(sitter.NewLanguage(grammar.Language())); err != nil {
			// TestGrammarSmoke exists precisely so this is impossible in a built
			// binary; returning nil here would turn it into a nil-deref far from the
			// cause, so the parser is returned unconfigured and the caller skips.
			return nil
		}
		return p
	},
}

// walker holds one file's extraction state.
type walker struct {
	src     []byte
	relFile string
	dir     string
	pkg     *pubPackage
	pkgs    *packageIndex

	// importURIs is every URI this file imports, which is the gate every framework
	// pass keys on. See the package doc: Dart imports are mandatory, so this is a
	// language-guaranteed answer to "can this file possibly be using X".
	importURIs []string

	out       []facts.Fact
	typeNames []string
	partOf    string
	parts     []string
	// stringConsts maps `<Type>.<CONST>` to its string literal, so a route declared as
	// `path: HomeScreen.routeName` resolves instead of being dropped.
	stringConsts map[string]string
}

// extractFile parses one Dart file and returns its facts.
//
// inheritedImports is non-nil only for a `part` file, which declares no imports of its
// own — it shares the host library's. See reextractParts.
func extractFile(src []byte, relFile string, pkgs *packageIndex, inheritedImports []string) fileResult {
	if len(src) == 0 {
		return fileResult{}
	}
	pp, _ := parserPool.Get().(*sitter.Parser)
	if pp == nil {
		return fileResult{}
	}
	defer parserPool.Put(pp)

	tree := pp.Parse(src, nil)
	if tree == nil {
		return fileResult{}
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil {
		return fileResult{}
	}

	dir := factpath.Dir(relFile)
	w := &walker{
		src:        src,
		relFile:    relFile,
		dir:        dir,
		pkg:        pkgs.ownerOf(relFile),
		pkgs:       pkgs,
		importURIs: append([]string(nil), inheritedImports...),
	}

	// Directives first: every later pass is gated on what this file imports, so the
	// import set has to be complete before any declaration is looked at.
	w.walkDirectives(root)
	w.walkDeclarations(root)
	w.extractFrameworkSurface(root)

	return fileResult{
		facts:        w.out,
		partOf:       w.partOf,
		parts:        w.parts,
		typeNames:    w.typeNames,
		importURIs:   w.importURIs,
		stringConsts: w.stringConsts,
	}
}

// ---------------------------------------------------------------------------
// Directives
// ---------------------------------------------------------------------------

// walkDirectives handles import/export/part/part-of and emits dependency facts.
func (w *walker) walkDirectives(root *sitter.Node) {
	for _, n := range namedChildren(root) {
		switch kindOf(n) {
		case "import_or_export":
			for _, c := range namedChildren(n) {
				switch kindOf(c) {
				case "library_import":
					w.importDirective(c, false)
				case "library_export":
					w.importDirective(c, true)
				}
			}
		case "part_directive":
			if uri := w.uriOf(n); uri != "" {
				w.parts = append(w.parts, w.resolveRelativeURI(uri))
			}
		case "part_of_directive":
			// `part of 'host.dart'` names the file; the older `part of my.library`
			// names a library and cannot be resolved to a path without a library
			// index, so it is left unresolved rather than guessed.
			if uri := w.uriOf(n); uri != "" {
				w.partOf = w.resolveRelativeURI(uri)
			}
		}
	}
}

// importDirective emits one dependency fact for an import or export.
func (w *walker) importDirective(n *sitter.Node, isExport bool) {
	uri := w.uriOf(n)
	if uri == "" {
		return
	}
	w.importURIs = append(w.importURIs, uri)

	target, source := w.classifyImport(uri)
	if target == "" {
		return
	}
	props := map[string]any{
		"language":       "dart",
		facts.PropSource: source,
		"uri":            uri,
	}
	if isExport {
		props["reexport"] = true
	}
	// `<importer> -> <imported>` is the shared dependency-fact naming convention, and
	// it is a contract rather than a style: the package-metrics explainer
	// recovers the IMPORTING side by splitting the name on " -> ". Naming a dependency
	// by its target alone (as this did) makes every Dart edge unrecoverable there, so
	// Ce came out 0 for every package, average instability 0.00, and the
	// most-depended-upon package of a real app was a generated platform directory with
	// Ca=3. Nothing failed; the metrics were simply computed over an empty edge set.
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindDependency,
		Name:      w.dir + " -> " + target,
		File:      w.relFile,
		Line:      lineOf(n),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}},
	})
}

// classifyImport maps an import URI to a dependency target and its source class.
//
// Dart has exactly three URI schemes and they map cleanly onto enola's three classes,
// which is unusually tidy: `dart:` is the SDK, a `package:` URI naming a package this
// repo declares is internal, any other `package:` is a third-party dependency, and a
// relative URI is always internal.
func (w *walker) classifyImport(uri string) (target, source string) {
	switch {
	case strings.HasPrefix(uri, "dart:"):
		return uri, facts.DepSourceStdlib

	case strings.HasPrefix(uri, "package:"):
		if resolved, internal := w.pkgs.resolvePackageURI(uri); internal {
			return factpath.Dir(resolved), facts.DepSourceInternal
		}
		// Third party: the dependency is the PACKAGE, not the file inside it. Two
		// imports of different files from one package are one dependency, and
		// reporting them separately would inflate every Flutter app's dependency
		// count by the number of widgets it imports.
		rest := strings.TrimPrefix(uri, "package:")
		name, _, _ := strings.Cut(rest, "/")
		return name, facts.DepSourceExternal

	case strings.Contains(uri, ":"):
		// An unrecognised scheme (dart-ext:, http:) — not a module edge.
		return "", ""

	default:
		return factpath.Dir(w.resolveRelativeURI(uri)), facts.DepSourceInternal
	}
}

// resolveRelativeURI resolves a relative import against this file's directory.
func (w *walker) resolveRelativeURI(uri string) string {
	if strings.Contains(uri, ":") {
		if resolved, ok := w.pkgs.resolvePackageURI(uri); ok {
			return resolved
		}
		return uri
	}
	return factpath.Clean(factpath.Join(w.dir, uri))
}

// uriOf pulls the string literal out of a directive, unquoted.
func (w *walker) uriOf(n *sitter.Node) string {
	var found string
	var visit func(*sitter.Node)
	visit = func(node *sitter.Node) {
		if found != "" {
			return
		}
		switch kindOf(node) {
		case "configurable_uri", "uri", "string_literal":
			txt := strings.TrimSpace(node.Utf8Text(w.src))
			txt = strings.Trim(txt, `"'`)
			if txt != "" && !strings.Contains(txt, "\n") {
				found = txt
				return
			}
		}
		for _, c := range namedChildren(node) {
			visit(c)
		}
	}
	for _, c := range namedChildren(n) {
		visit(c)
	}
	return found
}

// ---------------------------------------------------------------------------
// Declarations
// ---------------------------------------------------------------------------

// walkDeclarations emits symbol facts for every top-level declaration.
//
// The grammar puts a signature and its body in SIBLING nodes rather than nesting the
// body inside the signature, both at file scope and inside a class body. So the walk is
// an indexed scan that pairs each signature with the body that follows it, rather than
// a recursive descent — and the absence of a following body is meaningful: it is how an
// abstract member is spelled.
func (w *walker) walkDeclarations(root *sitter.Node) {
	kids := namedChildren(root)
	for i := 0; i < len(kids); i++ {
		n := kids[i]
		switch kindOf(n) {
		case "class_definition":
			w.classDecl(n, "class")
		case "mixin_declaration":
			w.classDecl(n, "mixin")
		case "extension_declaration", "extension_type_declaration":
			w.classDecl(n, "extension")
		case "enum_declaration":
			w.enumDecl(n)
		case "type_alias":
			w.typeAliasDecl(n)
		case "function_signature":
			body := nextBody(kids, i)
			w.functionDecl(n, body, w.dir, "", annotationsBefore(kids, i, w.src))
		}
	}
}

// classDecl emits a type symbol and everything it declares.
func (w *walker) classDecl(n *sitter.Node, construct string) {
	name := identifierChild(n, w.src)
	if name == "" {
		return
	}
	qualified := w.qualify(name)
	w.typeNames = append(w.typeNames, name)

	props := map[string]any{
		"language":       "dart",
		"symbol_kind":    symbolKindFor(construct),
		"dart_construct": construct,
		"exported":       !strings.HasPrefix(name, "_"),
	}
	if w.pkg != nil {
		props["pub_package"] = w.pkg.Name
	}
	annos := annotationNames(n, w.src)
	if len(annos) > 0 {
		props["annotations"] = annos
	}

	var rels []facts.Relation
	// `extends`, `with` and `implements` are all one relation here, as they are in
	// every other extractor: enola's model has a single `implements` edge and does not
	// distinguish inheritance from conformance.
	supers := supertypeNames(n, w.src)
	for _, super := range supers {
		rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: super})
	}
	if w.isFlutterFile() {
		if role := flutterRole(supers); role != "" {
			props["flutter_role"] = role
			props["framework"] = "flutter"
		}
	}

	body := childOfKind(n, "class_body", "extension_body", "enum_body")
	members := w.memberDecls(body, name)
	for _, m := range members {
		rels = append(rels, facts.Relation{Kind: facts.RelDeclares, Target: m.name})
	}

	// `abstract` is authoritative for package-metrics abstractness, and Dart needs it
	// computed rather than read off the keyword.
	//
	// Two Dart facts pull in opposite directions. EVERY class implicitly defines an
	// interface others may `implement`, so "is implementable" says nothing. And a
	// mixin routinely carries its whole implementation, exactly as a Scala trait does,
	// so the construct keyword does not decide either. What does decide it is whether
	// the type declares behaviour: a class marked abstract/sealed is one, and a mixin
	// or class whose members are all abstract is one regardless of how it was spelled.
	props["abstract"] = w.isAbstract(n, construct, members)

	// data_holder spares a package the "extract interfaces" advice. Dart
	// has no dedicated record construct in common use — the ubiquitous shape is a
	// plain class of final fields plus a const constructor, frequently generated by
	// freezed — so it is emitted when the members say so.
	if isDataHolder(members) {
		props["data_holder"] = true
	}

	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      qualified,
		File:      w.relFile,
		Line:      lineOf(n),
		Props:     props,
		Relations: rels,
	})
}

// memberInfo describes one declared member, for the abstractness and data-holder rules.
type memberInfo struct {
	name       string
	isMethod   bool
	hasBody    bool
	isField    bool
	isCtor     bool
	isAbstract bool
}

// memberDecls emits symbols for a type's members and returns what it found.
func (w *walker) memberDecls(body *sitter.Node, typeName string) []memberInfo {
	if body == nil {
		return nil
	}
	var out []memberInfo
	kids := namedChildren(body)
	for i := 0; i < len(kids); i++ {
		n := kids[i]
		switch kindOf(n) {
		case "method_signature":
			// A concrete member: signature here, body in the next sibling.
			bodyNode := nextBody(kids, i)
			if m, ok := w.methodDecl(n, bodyNode, typeName, annotationsBefore(kids, i, w.src)); ok {
				out = append(out, m)
			}
		case "declaration":
			// `declaration` carries three unrelated things: a field, a constructor,
			// and — the one that matters for abstractness — an ABSTRACT method, which
			// is a bare function_signature with no body anywhere.
			out = append(out, w.declarationMember(n, typeName, annotationsBefore(kids, i, w.src))...)
		case "enum_constant":
			cname := identifierChild(n, w.src)
			if cname == "" {
				cname = strings.TrimSpace(n.Utf8Text(w.src))
			}
			full := w.qualify(typeName + "." + cname)
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(n),
				Props: map[string]any{
					"language": "dart", "symbol_kind": facts.SymbolConstant,
					"enum_constant": true, "exported": !strings.HasPrefix(cname, "_"),
				},
			})
			out = append(out, memberInfo{name: full, isField: true})
		}
	}
	return out
}

// declarationMember handles the `declaration` node's three shapes.
func (w *walker) declarationMember(n *sitter.Node, typeName string, annos []string) []memberInfo {
	var out []memberInfo
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "constructor_signature", "factory_constructor_signature", "constant_constructor_signature":
			name := constructorName(c, typeName, w.src)
			full := w.qualify(typeName + "." + name)
			props := map[string]any{
				"language": "dart", "symbol_kind": facts.SymbolMethod,
				"constructor": true, "exported": !strings.HasPrefix(name, "_"),
			}
			if len(annos) > 0 {
				props["annotations"] = annos
			}
			rels := w.injectRelations(c)
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(c),
				Props: props, Relations: rels,
			})
			out = append(out, memberInfo{name: full, isCtor: true})

		case "function_signature", "getter_signature", "setter_signature":
			// No body sibling inside a `declaration` means this is abstract.
			name := signatureName(c, w.src)
			if name == "" {
				continue
			}
			full := w.qualify(typeName + "." + name)
			props := map[string]any{
				"language": "dart", "symbol_kind": facts.SymbolMethod,
				"abstract_member": true, "exported": !strings.HasPrefix(name, "_"),
			}
			if len(annos) > 0 {
				props["annotations"] = annos
			}
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(c),
				Props: props,
			})
			out = append(out, memberInfo{name: full, isMethod: true, isAbstract: true})

		case "initialized_identifier_list", "static_final_declaration_list":
			// `static const routeName = '/workspace/start'` is the dominant way a
			// Flutter screen declares its own path, and the router then refers to it
			// as `WorkspaceStartScreen.routeName` rather than repeating the literal.
			// Recording the value here is what lets the route pass resolve that
			// reference instead of dropping the route.
			w.recordStringConstants(c, typeName)
			for _, fieldName := range identifierNames(c, w.src) {
				full := w.qualify(typeName + "." + fieldName)
				// Only PUBLIC fields become symbols, for the reason C# gives: private
				// state is implementation detail, and on a large codebase emitting it
				// multiplies the symbol count without adding a node anyone traverses.
				// Dart makes this especially stark — the `_name` convention means the
				// private half is syntactically obvious and very common.
				if !strings.HasPrefix(fieldName, "_") {
					w.out = append(w.out, facts.Fact{
						Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(c),
						Props: map[string]any{
							"language": "dart", "symbol_kind": facts.SymbolVariable,
							"field": true, "exported": true,
						},
					})
				}
				out = append(out, memberInfo{name: full, isField: true})
			}
		}
	}
	return out
}

// methodDecl emits a concrete method/getter/setter and walks its body.
func (w *walker) methodDecl(sig, body *sitter.Node, typeName string, annos []string) (memberInfo, bool) {
	inner := firstOfKind(sig, "function_signature", "getter_signature", "setter_signature",
		"factory_constructor_signature", "constructor_signature", "operator_signature")
	if inner == nil {
		inner = sig
	}
	var name string
	switch kindOf(inner) {
	case "factory_constructor_signature", "constructor_signature":
		name = constructorName(inner, typeName, w.src)
	default:
		name = signatureName(inner, w.src)
	}
	if name == "" {
		return memberInfo{}, false
	}
	full := w.qualify(typeName + "." + name)

	props := map[string]any{
		"language": "dart", "symbol_kind": facts.SymbolMethod,
		"exported": !strings.HasPrefix(name, "_"),
	}
	switch kindOf(inner) {
	case "getter_signature":
		props["accessor"] = "getter"
	case "setter_signature":
		props["accessor"] = "setter"
	case "factory_constructor_signature", "constructor_signature":
		props["constructor"] = true
	}
	if hasToken(sig, "static") {
		props["static"] = true
	}
	if isAsync(body) {
		props["async"] = true
	}
	if len(annos) > 0 {
		props["annotations"] = annos
		for _, a := range annos {
			if a == "override" {
				props["override"] = true
			}
		}
	}

	rels := w.injectRelations(inner)
	if body != nil {
		bw := w.walkBody(body, full)
		rels = append(rels, bw.relations...)
		bw.applyTo(props)
	}

	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(sig),
		Props: props, Relations: rels,
	})
	return memberInfo{name: full, isMethod: true, hasBody: body != nil}, true
}

// functionDecl emits a top-level function.
func (w *walker) functionDecl(sig, body *sitter.Node, dir, _ string, annos []string) {
	name := signatureName(sig, w.src)
	if name == "" {
		return
	}
	full := w.qualify(name)
	props := map[string]any{
		"language": "dart", "symbol_kind": facts.SymbolFunc,
		"exported": !strings.HasPrefix(name, "_"),
	}
	if isAsync(body) {
		props["async"] = true
	}
	if len(annos) > 0 {
		props["annotations"] = annos
	}
	var rels []facts.Relation
	if body != nil {
		bw := w.walkBody(body, full)
		rels = append(rels, bw.relations...)
		bw.applyTo(props)
	}
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(sig),
		Props: props, Relations: rels,
	})
}

// enumDecl emits an enum and its constants.
//
// A Dart 3 enum is a full type: it may declare fields, methods and constructors, and
// implement interfaces. So it goes through the same member walk as a class rather than
// being treated as a bare constant list.
func (w *walker) enumDecl(n *sitter.Node) {
	name := identifierChild(n, w.src)
	if name == "" {
		return
	}
	w.typeNames = append(w.typeNames, name)
	props := map[string]any{
		"language": "dart", "symbol_kind": facts.SymbolEnum,
		"dart_construct": "enum", "exported": !strings.HasPrefix(name, "_"),
	}
	if w.pkg != nil {
		props["pub_package"] = w.pkg.Name
	}
	var rels []facts.Relation
	for _, super := range supertypeNames(n, w.src) {
		rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: super})
	}
	for _, m := range w.memberDecls(childOfKind(n, "enum_body"), name) {
		rels = append(rels, facts.Relation{Kind: facts.RelDeclares, Target: m.name})
	}
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol, Name: w.qualify(name), File: w.relFile, Line: lineOf(n),
		Props: props, Relations: rels,
	})
}

// typeAliasDecl emits a typedef.
func (w *walker) typeAliasDecl(n *sitter.Node) {
	name := ""
	for _, c := range namedChildren(n) {
		if kindOf(c) == "type_identifier" {
			name = c.Utf8Text(w.src)
			break
		}
	}
	if name == "" {
		return
	}
	w.typeNames = append(w.typeNames, name)
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol, Name: w.qualify(name), File: w.relFile, Line: lineOf(n),
		Props: map[string]any{
			"language": "dart", "symbol_kind": facts.SymbolType,
			"dart_construct": "typedef", "exported": !strings.HasPrefix(name, "_"),
		},
	})
}

// injectRelations turns constructor parameters whose type this repo declares into
// `injects` edges.
//
// Dart has no annotation-driven DI in the language, but constructor injection is the
// dominant idiom across every DI package in use (get_it, injectable, provider,
// riverpod) and, more importantly, across code using none of them: a Flutter widget or
// repository takes its collaborators as constructor parameters. Only parameters with an
// explicit declared type are considered, and the type is left as a bare name for the
// resolution pass to bind — an unresolvable one draws no edge.
func (w *walker) injectRelations(sig *sitter.Node) []facts.Relation {
	params := firstOfKind(sig, "formal_parameter_list")
	if params == nil {
		return nil
	}
	seen := map[string]bool{}
	var rels []facts.Relation
	var visit func(*sitter.Node)
	visit = func(n *sitter.Node) {
		if kindOf(n) == "formal_parameter" {
			for _, c := range namedChildren(n) {
				if kindOf(c) != "type_identifier" {
					continue
				}
				t := c.Utf8Text(w.src)
				// A capitalised, non-builtin type name is a collaborator; String and
				// int are not dependencies in any useful sense.
				if t == "" || !isUpper(t[0]) || isBuiltinType(t) || seen[t] {
					continue
				}
				seen[t] = true
				rels = append(rels, facts.Relation{Kind: facts.RelInjects, Target: t})
			}
			return
		}
		for _, c := range namedChildren(n) {
			visit(c)
		}
	}
	visit(params)
	return rels
}

// isAbstract decides the authoritative `abstract` prop. See classDecl for why Dart
// cannot read it off the keyword alone.
func (w *walker) isAbstract(n *sitter.Node, construct string, members []memberInfo) bool {
	if childOfKind(n, "abstract") != nil || childOfKind(n, "sealed") != nil {
		return true
	}
	if construct == "extension" {
		return false
	}
	// A mixin, or a class the keyword did not mark: abstract iff it declares methods
	// and every one of them is abstract. A type with no methods at all is data, not an
	// abstraction, so it does not qualify.
	methods, abstracts := 0, 0
	for _, m := range members {
		if !m.isMethod {
			continue
		}
		methods++
		if m.isAbstract {
			abstracts++
		}
	}
	return methods > 0 && methods == abstracts
}

// isDataHolder reports a type that declares state and no behaviour.
func isDataHolder(members []memberInfo) bool {
	fields, methods := 0, 0
	for _, m := range members {
		switch {
		case m.isField:
			fields++
		case m.isMethod:
			methods++
		}
	}
	return fields > 0 && methods == 0
}

// recordStringConstants captures `static const NAME = 'literal'` members as
// `<Type>.<NAME> -> literal`, for the route pass to dereference.
//
// Only string literals are kept, and only under a named type, which is exactly the
// shape a route reference takes. A computed or interpolated value records nothing, so a
// reference to one stays unresolved rather than resolving to a half-built string.
func (w *walker) recordStringConstants(list *sitter.Node, typeName string) {
	for _, d := range namedChildren(list) {
		if kindOf(d) != "static_final_declaration" && kindOf(d) != "initialized_identifier" {
			continue
		}
		name := identifierChild(d, w.src)
		if name == "" {
			continue
		}
		lit := childOfKind(d, "string_literal")
		if lit == nil {
			continue
		}
		value := stringLiteralValue(lit, w.src)
		if value == "" {
			continue
		}
		if w.stringConsts == nil {
			w.stringConsts = map[string]string{}
		}
		w.stringConsts[typeName+"."+name] = value
	}
}

// qualify builds enola's `<dir>.<name>` symbol name.
func (w *walker) qualify(name string) string {
	if w.dir == "" || w.dir == "." {
		return name
	}
	return w.dir + "." + name
}

// symbolKindFor maps a Dart construct onto enola's fixed symbol-kind vocabulary.
//
// A mixin is `interface` for the same reason a Scala trait is: it is the language's
// mixin/conformance construct, and adding a Dart-specific value would widen a shared
// vocabulary to say something `dart_construct` already says precisely.
func symbolKindFor(construct string) string {
	switch construct {
	case "mixin":
		return facts.SymbolInterface
	case "extension":
		return facts.SymbolType
	default:
		return facts.SymbolClass
	}
}
