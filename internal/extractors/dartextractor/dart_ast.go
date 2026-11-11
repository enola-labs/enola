package dartextractor

import (
	"strings"
	"sync"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/extractors/callsite"
	"github.com/enola-labs/enola/internal/extractors/dartextractor/grammar"
	"github.com/enola-labs/enola/internal/extractors/tsutil"
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
	// valueFunctions contains callable declarations visible in this lexical scope.
	// A bare data read must never bind to an unrelated project-wide method.
	valueFunctions  map[string]string
	memberFunctions map[string]string
	fieldTypes      map[string]string
	callOwner       string
	getterNames     map[string]bool
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
	if w.partOf != "" || len(w.parts) > 0 {
		props := map[string]any{"language": "dart"}
		if w.partOf != "" {
			props["library_file"] = w.partOf
		}
		if len(w.parts) > 0 {
			props["library_parts"] = w.parts
		}
		w.out = append(w.out, facts.Fact{Kind: facts.KindFileRef, Name: w.relFile, File: w.relFile, Props: props})
	}
	w.valueFunctions = w.fileCallableValues(root)
	w.walkDeclarations(root)
	w.moduleValueReferences(root)
	w.extractFrameworkSurface(root)
	w.attributeCallers(root)

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
	if spec := firstOfKind(n, "import_specification"); spec != nil {
		if prefix := childOfKind(spec, "identifier"); prefix != nil {
			props["import_prefix"] = prefix.Utf8Text(w.src)
		}
	}
	var combinators func(*sitter.Node)
	combinators = func(node *sitter.Node) {
		if kindOf(node) == "combinator" {
			var names []string
			for _, id := range namedChildren(node) {
				if kindOf(id) == "identifier" {
					names = append(names, id.Utf8Text(w.src))
				}
			}
			if strings.HasPrefix(strings.TrimSpace(node.Utf8Text(w.src)), "hide") {
				old, _ := props["hide_names"].([]string)
				props["hide_names"] = append(old, names...)
			} else {
				if old, exists := props["show_names"]; exists {
					var intersection []string
					for _, name := range names {
						for _, prior := range old.([]string) {
							if name == prior {
								intersection = append(intersection, name)
								break
							}
						}
					}
					names = intersection
				}
				props["show_names"] = names
				if len(names) == 0 {
					props["show_none"] = true
				}
			}
			return
		}
		for _, c := range namedChildren(node) {
			combinators(c)
		}
	}
	combinators(n)

	if source == facts.DepSourceInternal {
		props["resolved_file"] = w.resolveRelativeURI(uri)
	}
	var uris []string
	var collectURIs func(*sitter.Node)
	collectURIs = func(node *sitter.Node) {
		if kindOf(node) == "uri" {
			if value := stringLiteralValue(childOfKind(node, "string_literal"), w.src); value != "" {
				uris = append(uris, value)
			}
			return
		}
		for _, c := range namedChildren(node) {
			collectURIs(c)
		}
	}
	collectURIs(n)
	if len(uris) > 1 {
		props["conditional_import"] = true
		paths := make([]string, 0, len(uris))
		for _, uri := range uris {
			paths = append(paths, w.resolveRelativeURI(uri))
		}
		props["resolved_files"] = paths
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
	if kindOf(n) == "extension_declaration" && childOfKind(n, "type_parameters", "type_arguments") == nil {
		props["extension_on"] = declaredReceiverType(n, w.src)
	}
	if parent := childOfKind(n, "superclass"); parent != nil {
		if typ := declaredReceiverType(parent, w.src); typ != "" {
			props["extends_type"] = typ
		}
	}
	mixins := childOfKind(n, "mixins")
	if mixins == nil {
		mixins = childOfKind(childOfKind(n, "superclass"), "mixins")
	}
	if names := w.inheritedTypeNames(mixins); len(names) > 0 {
		props["mixin_types"] = names
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
	members := w.primaryConstructor(n, name)
	members = append(members, w.memberDecls(body, name)...)
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
		EndLine:   tsutil.EndLine(n),
		Props:     props,
		Relations: rels,
	})
	w.setDoc(n)
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
	previous, previousFields, previousOwner := w.memberFunctions, w.fieldTypes, w.callOwner
	previousGetters := w.getterNames
	w.getterNames = map[string]bool{}
	w.memberFunctions = w.memberCallableValues(body, typeName)
	w.fieldTypes = w.memberReceiverTypes(body)
	w.callOwner = w.qualify(typeName)
	defer func() {
		w.memberFunctions, w.fieldTypes, w.callOwner = previous, previousFields, previousOwner
		w.getterNames = previousGetters
	}()
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
				Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(n), EndLine: tsutil.EndLine(n),
				Props: map[string]any{
					"language": "dart", "symbol_kind": facts.SymbolConstant,
					"enum_constant": true, "exported": !strings.HasPrefix(cname, "_"),
				},
			})
			w.setDoc(n)
			out = append(out, memberInfo{name: full, isField: true})
		}
	}
	return out
}

// primaryConstructor emits a Dart primary constructor (`class Point(final int x)`):
// the constructor itself, named as an ordinary one would be, and a field for each
// `final` or `var` parameter, since those declare the class's fields. Only public
// fields become symbols, as for fields declared in the body (see declarationMember);
// an initializing formal (`this.x`) declares nothing.
func (w *walker) primaryConstructor(n *sitter.Node, typeName string) []memberInfo {
	pc := n.ChildByFieldName("primary_constructor")
	if pc == nil {
		return nil
	}
	name := typeName
	if nm := pc.ChildByFieldName("name"); nm != nil && nm.Utf8Text(w.src) != "new" {
		name = typeName + "." + nm.Utf8Text(w.src)
	}
	full := w.qualify(typeName + "." + name)
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(pc), EndLine: tsutil.EndLine(pc),
		Props: map[string]any{
			"language": "dart", "symbol_kind": facts.SymbolMethod,
			"constructor": true, "primary_constructor": true, "exported": !strings.HasPrefix(name, "_"),
		},
		Relations: w.injectRelations(pc),
	})
	out := []memberInfo{{name: full, isCtor: true}}

	var visit func(*sitter.Node)
	visit = func(p *sitter.Node) {
		if kindOf(p) != "formal_parameter" {
			for _, c := range namedChildren(p) {
				visit(c)
			}
			return
		}
		if childOfKind(p, "final_builtin", "inferred_type") == nil {
			return
		}
		id := p.ChildByFieldName("name")
		if id == nil {
			return
		}
		fieldName := id.Utf8Text(w.src)
		fieldFull := w.qualify(typeName + "." + fieldName)
		if !strings.HasPrefix(fieldName, "_") {
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindSymbol, Name: fieldFull, File: w.relFile, Line: lineOf(p), EndLine: tsutil.EndLine(p),
				Props: map[string]any{
					"language": "dart", "symbol_kind": facts.SymbolVariable,
					"field": true, "exported": true,
				},
			})
		}
		out = append(out, memberInfo{name: fieldFull, isField: true})
	}
	if params := pc.ChildByFieldName("parameters"); params != nil {
		visit(params)
	}
	return out
}

// declarationMember handles the `declaration` node's three shapes.
func (w *walker) declarationMember(n *sitter.Node, typeName string, annos []string) []memberInfo {
	var out []memberInfo
	for _, c := range namedChildren(n) {
		switch kindOf(c) {
		case "constructor_signature", "factory_constructor_signature", "constant_constructor_signature":
			ctor := dartConstructorSignature(c, typeName, w.src)
			name := signatureName(c, w.src)
			if ctor {
				name = constructorName(c, typeName, w.src)
			}
			full := w.qualify(typeName + "." + name)
			props := map[string]any{
				"language": "dart", "symbol_kind": facts.SymbolMethod,
				"exported": !strings.HasPrefix(name, "_"),
			}
			if ctor {
				props["constructor"] = true
			} else {
				props["abstract_member"] = true
			}
			if len(annos) > 0 {
				props["annotations"] = annos
			}
			rels := w.injectRelations(c)
			if init := childOfKind(n, "initializers"); init != nil {
				bw := w.walkBody(init, full, c)
				rels = append(rels, bw.relations...)
				bw.applyTo(props)
			}
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(c), EndLine: tsutil.EndLine(c),
				Props: props, Relations: rels,
			})
			w.setDoc(n)
			out = append(out, memberInfo{name: full, isCtor: ctor, isMethod: !ctor, isAbstract: !ctor})

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
			if kindOf(c) == "getter_signature" {
				props["accessor"] = "getter"
			}
			if kindOf(c) == "setter_signature" {
				props["accessor"] = "setter"
			}
			if len(annos) > 0 {
				props["annotations"] = annos
			}
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(c), EndLine: tsutil.EndLine(c),
				Props: props,
			})
			w.setDoc(n)
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
						Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(c), EndLine: tsutil.EndLine(c),
						Props: map[string]any{
							"language": "dart", "symbol_kind": facts.SymbolVariable,
							"field": true, "exported": true,
						},
					})
					w.setDoc(n)
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
		if dartConstructorSignature(inner, typeName, w.src) {
			name = constructorName(inner, typeName, w.src)
		} else {
			name = signatureName(inner, w.src)
		}
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
		if dartConstructorSignature(inner, typeName, w.src) {
			props["constructor"] = true
		}
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
	var init *sitter.Node
	if props["constructor"] == true {
		init = childOfKind(sig, "initializers")
	}
	if body != nil || init != nil {
		bw := w.walkBody(body, full, inner, init)
		rels = append(rels, bw.relations...)
		bw.applyTo(props)
	}

	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(sig), EndLine: max(tsutil.EndLine(sig), tsutil.EndLine(body)),
		Props: props, Relations: rels,
	})
	w.setDoc(sig)
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
		bw := w.walkBody(body, full, sig)
		rels = append(rels, bw.relations...)
		bw.applyTo(props)
	}
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol, Name: full, File: w.relFile, Line: lineOf(sig), EndLine: max(tsutil.EndLine(sig), tsutil.EndLine(body)),
		Props: props, Relations: rels,
	})
	w.setDoc(sig)
}

// attributeCallers names the caller of each client call this file makes. Dart's
// grammar makes a function's signature and body two sibling nodes, so no single node
// contains a call and names its function: each function_body is paired with the
// signature before it, whose start row is the Line of the symbol the declaration
// walk emitted for it. It walks the tree only for a file that makes a client call;
// the declaration walk runs on every snapshot, cache or not, and bookkeeping there
// for every function cost a measurable share of a warm run.
func (w *walker) attributeCallers(root *sitter.Node) {
	hasCall := false
	for i := range w.out {
		if w.out[i].Kind == facts.KindRoute && w.out[i].PropAny(facts.PropRole) == facts.RoleClient {
			hasCall = true
			break
		}
	}
	if !hasCall {
		return
	}
	byLine := map[uint][]string{}
	for i := range w.out {
		if f := &w.out[i]; f.Kind == facts.KindSymbol && f.Line > 0 {
			byLine[uint(f.Line-1)] = append(byLine[uint(f.Line-1)], f.Name)
		}
	}
	var spans []callsite.Span
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		kids := namedChildren(n)
		for i, c := range kids {
			if kindOf(c) == "function_body" && i > 0 {
				sig := kids[i-1]
				if names := byLine[sig.StartPosition().Row]; len(names) == 1 {
					spans = append(spans, callsite.Span{Start: sig.StartPosition().Row, End: c.EndPosition().Row, Symbol: names[0]})
				}
			}
			visit(c)
		}
	}
	visit(root)
	callsite.AttributeSpans(spans, w.out)
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
		Kind: facts.KindSymbol, Name: w.qualify(name), File: w.relFile, Line: lineOf(n), EndLine: tsutil.EndLine(n),
		Props: props, Relations: rels,
	})
	w.setDoc(n)
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
		Kind: facts.KindSymbol, Name: w.qualify(name), File: w.relFile, Line: lineOf(n), EndLine: tsutil.EndLine(n),
		Props: map[string]any{
			"language": "dart", "symbol_kind": facts.SymbolType,
			"dart_construct": "typedef", "exported": !strings.HasPrefix(name, "_"),
		},
	})
	w.setDoc(n)
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
