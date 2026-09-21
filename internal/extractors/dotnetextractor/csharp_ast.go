package dotnetextractor

import (
	"log"
	"strings"
	"sync"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	csharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
)

// grammarErrOnce keeps an unusable-grammar report to a single line rather than
// one per source file.
var grammarErrOnce sync.Once

// extractFileAST parses a single C# file with tree-sitter and emits architectural
// facts: declaration symbols (types, methods, properties, fields, enum members),
// using-directive dependencies, and call-graph relations (RelImplements,
// RelInstantiates, RelInjects, RelCalls).
//
// Type-reference targets are emitted as they are WRITTEN — a bare simple name, or
// a dotted name when the source qualified it — and resolveCSharpTargets binds them
// against the project-wide index once every file has been walked. A per-file
// import map cannot do this job the way Java's can: `using` opens a namespace
// rather than naming a type, so the file's directives say which namespaces are in
// scope and nothing about which one declares any particular name.
// extractFileAST is the single-value form used by tests; production code calls
// extractFileASTFull to also receive the file's ASP.NET routing evidence, which
// can only be composed once every file is in (see aspnet.go).
func extractFileAST(src []byte, relFile string) []facts.Fact {
	ff, _ := extractFileASTFull(src, relFile)
	return ff
}

func extractFileASTFull(src []byte, relFile string) ([]facts.Fact, aspnetScaffold) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(csharp.Language())); err != nil {
		// Reported once rather than per file, but reported: the only way this
		// fails is a grammar whose ABI the vendored tree-sitter runtime does not
		// accept, and the symptom — every C# file parsing to nothing — is
		// otherwise indistinguishable from a repository with no C# in it.
		grammarErrOnce.Do(func() {
			log.Printf("[dotnet-extractor] tree-sitter grammar unusable, no C# will be extracted: %v", err)
		})
		return nil, aspnetScaffold{}
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil, aspnetScaffold{}
	}
	defer tree.Close()

	w := &astWalker{
		src:      src,
		relFile:  relFile,
		dir:      factpath.Dir(relFile),
		aliases:  make(map[string]string),
		fileRefI: -1,
	}
	root := tree.RootNode()
	w.walkTopLevelChildren(root)
	w.scaffold.minimal = collectMinimalAPIRoutes(root, src, relFile, w.dir)
	// APPEND: noteRefit already added this file's Refit declarations during the
	// walk above, and assigning here would discard them.
	w.scaffold.clients = append(w.scaffold.clients, collectHTTPClientCalls(root, src, relFile, w.dir)...)
	w.scaffold.conventional, w.scaffold.conventionalSkipped = collectConventionalRoutes(root, src, relFile, w.dir)
	return w.out, w.scaffold
}

type astWalker struct {
	src     []byte
	relFile string
	dir     string

	// ns is the enclosing namespace, dotted. Both spellings feed it: a block
	// `namespace A.B { }` pushes for the duration of its body, a file-scoped
	// `namespace A.B;` sets it for the rest of the file.
	ns []string

	out []facts.Fact

	// aliases holds this file's `using X = Some.Qualified.Type;` directives, the
	// one C# using form that DOES name a type. Applied at walk time because it is
	// file-local knowledge the whole-repo resolution pass no longer has.
	aliases map[string]string

	// typeStack holds the simple names of the enclosing type declarations, so a
	// method in class Foo is named "<dir>.Foo.method" and a nested type is
	// "<dir>.Outer.Inner". methodStack is parallel and holds each enclosing type's
	// own method-name set, used to resolve same-type bare calls.
	typeStack   []string
	methodStack []map[string]bool
	// interfaceStack is parallel to typeStack: whether that type is an interface,
	// whose members are public by default and carry no access modifier.
	interfaceStack []bool
	// ctorStack is parallel to typeStack: how many constructors that type
	// declares, which decides whether their parameters read as injected.
	ctorStack []int
	// partialStack is parallel to typeStack: whether that type is `partial`, and
	// so whether a bare call this file cannot see may still be a sibling member
	// declared in another half.
	partialStack []bool

	// ownerStack[len-1] indexes into out for the symbol fact currently being
	// built; edges found while walking its body attach to it. An index rather
	// than a pointer: nested declarations append to out, which can reallocate the
	// backing array and strand a raw pointer.
	ownerStack []int

	// scaffold accumulates this file's ASP.NET routing evidence, composed into
	// route facts once every file has been walked (see aspnet.go).
	scaffold aspnetScaffold

	// fileRefI indexes the lazily created file-scope reference fact, or -1. C#
	// top-level statements (a Program.cs with no class) have no enclosing symbol,
	// and their calls would otherwise be dropped.
	fileRefI int

	// Per-body complexity state, saved and restored around the re-entrant walk
	// (a body may contain a local function or a nested type).
	metrics   *bodyMetrics
	loopDepth int
	// scalingDepth counts only loops that add a factor of n — the Big-O exponent.
	// repeatDepth counts loops whose body runs a non-constant number of times,
	// which differs for `while (true)` / `for (;;)`: they add no factor of n, but a
	// query inside one still runs many times and stays an N+1 candidate.
	scalingDepth int
	repeatDepth  int
	selfName     string
	selfShort    string
	selfParams   int
}

// bodyMetrics accumulates per-member complexity signals during the single body
// traversal — mirrors the other AST extractors.
type bodyMetrics struct {
	loopDepth          int
	loopCount          int
	decisions          int
	callsInLoop        []string
	inLoopSeen         map[string]bool
	scalingLoopDepth   int
	callsInScalingLoop []string
	inScalingSeen      map[string]bool
	recursive          bool
	ioDirect           bool
}

// ── Scope helpers ───────────────────────────────────────────────────────────

func (w *astWalker) enclosingType() string { return strings.Join(w.typeStack, ".") }

func (w *astWalker) qualify(name string) string {
	if t := w.enclosingType(); t != "" {
		return t + "." + name
	}
	return name
}

// canonicalName is the "<dir>.<QualifiedType>" fact name of a declaration. The
// namespace is deliberately not part of it: enola names facts by directory across
// every language, and a C# namespace routinely disagrees with the file system.
// The namespace travels as a prop instead, where resolution can use it.
func (w *astWalker) canonicalName(qualified string) string { return w.dir + "." + qualified }

func (w *astWalker) namespace() string { return strings.Join(w.ns, ".") }

// fqn is the "<namespace>.<QualifiedType>" name a C# reference would use.
func (w *astWalker) fqn(qualified string) string {
	if ns := w.namespace(); ns != "" {
		return ns + "." + qualified
	}
	return qualified
}

func (w *astWalker) currentMethods() map[string]bool {
	if len(w.methodStack) == 0 {
		return nil
	}
	return w.methodStack[len(w.methodStack)-1]
}

func (w *astWalker) inInterface() bool {
	return len(w.interfaceStack) > 0 && w.interfaceStack[len(w.interfaceStack)-1]
}

func (w *astWalker) pushOwner(i int) { w.ownerStack = append(w.ownerStack, i) }
func (w *astWalker) popOwner()       { w.ownerStack = w.ownerStack[:len(w.ownerStack)-1] }

func (w *astWalker) currentOwner() *facts.Fact {
	if len(w.ownerStack) == 0 {
		return nil
	}
	return &w.out[w.ownerStack[len(w.ownerStack)-1]]
}

// ownerForEdge returns the fact an edge should attach to, creating the file-scope
// reference carrier when there is no enclosing symbol (top-level statements).
func (w *astWalker) ownerForEdge() *facts.Fact {
	if o := w.currentOwner(); o != nil {
		return o
	}
	if w.fileRefI < 0 {
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindFileRef,
			Name:  w.relFile,
			File:  w.relFile,
			Line:  1,
			Props: map[string]any{"language": "csharp"},
		})
		w.fileRefI = len(w.out) - 1
	}
	return &w.out[w.fileRefI]
}

func (w *astWalker) addEdge(kind, target string) {
	if target == "" {
		return
	}
	owner := w.ownerForEdge()
	if owner == nil || owner.Name == target {
		return
	}
	for _, r := range owner.Relations {
		if r.Kind == kind && r.Target == target {
			return
		}
	}
	owner.Relations = append(owner.Relations, facts.Relation{Kind: kind, Target: target})
}

// ── Top level ───────────────────────────────────────────────────────────────

// walkTopLevelChildren dispatches the direct children of a compilation unit or of
// a namespace/preprocessor block. Preprocessor branches are descended rather than
// skipped: code inside `#if NET8_0` is real source for some configuration, and
// dropping it would make whole files look empty.
func (w *astWalker) walkTopLevelChildren(node *sitter.Node) {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkTopLevel(node.Child(i))
	}
}

func (w *astWalker) walkTopLevel(node *sitter.Node) {
	switch kindOf(node) {
	case "using_directive":
		w.handleUsing(node)
	case "namespace_declaration":
		name := w.nameText(node.ChildByFieldName("name"))
		if name != "" {
			w.ns = append(w.ns, name)
		}
		if body := node.ChildByFieldName("body"); body != nil {
			w.walkTopLevelChildren(body)
		}
		if name != "" {
			w.ns = w.ns[:len(w.ns)-1]
		}
	case "file_scoped_namespace_declaration":
		// `namespace A.B;` — no body: everything after it in the file is in scope,
		// and those declarations are siblings of this node.
		if name := w.nameText(node.ChildByFieldName("name")); name != "" {
			w.ns = append(w.ns, name)
		}
	case "class_declaration":
		w.handleTypeDecl(node, facts.SymbolClass)
	case "interface_declaration":
		w.handleTypeDecl(node, facts.SymbolInterface)
	case "struct_declaration":
		w.handleTypeDecl(node, facts.SymbolStruct)
	case "record_declaration":
		w.handleTypeDecl(node, recordSymbolKind(node, w.src))
	case "enum_declaration":
		w.handleEnum(node)
	case "delegate_declaration":
		w.handleDelegate(node)
	case "preproc_if", "preproc_else", "preproc_elif":
		w.walkTopLevelChildren(node)
	case "global_statement":
		// Top-level statements (a Program.cs with no class). No symbol to declare;
		// the calls attach to the file-scope reference fact.
		w.walkForCalls(node)
	}
}

// recordSymbolKind distinguishes `record class` (the default) from `record struct`.
// The grammar gives both the same node, with the keyword as an anonymous child.
func recordSymbolKind(node *sitter.Node, src []byte) string {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if nodeText(node.Child(i), src) == "struct" {
			return facts.SymbolStruct
		}
	}
	return facts.SymbolClass
}

func (w *astWalker) handleUsing(node *sitter.Node) {
	// Shapes: `using System.Text;`, `using static System.Math;`,
	// `using Alias = Some.Type;`, and any of them prefixed `global`.
	var isStatic, isGlobal bool
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		switch nodeText(node.Child(i), w.src) {
		case "static":
			isStatic = true
		case "global":
			isGlobal = true
		}
	}
	alias := ""
	if n := node.ChildByFieldName("name"); n != nil {
		alias = nodeText(n, w.src)
	}
	path := ""
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "qualified_name", "identifier", "generic_name", "alias_qualified_name":
			if alias != "" && nodeText(c, w.src) == alias {
				continue // the alias itself, not the target
			}
			path = nodeText(c, w.src)
		}
	}
	if path == "" {
		return
	}
	if alias != "" {
		// The one using form that names a type. Recorded so a bare reference to
		// the alias in this file resolves to what it aliases.
		w.aliases[alias] = path
	}

	props := map[string]any{
		"language": "csharp",
		"import":   path,
		"source":   "external", // refined by classifyUsing once the repo is indexed
	}
	if isStatic {
		props["static"] = true
	}
	if isGlobal {
		props["global"] = true
	}
	if alias != "" {
		props["alias"] = alias
	}
	w.out = append(w.out, facts.Fact{
		Kind:  facts.KindDependency,
		Name:  w.dir + " -> " + path,
		File:  w.relFile,
		Line:  int(node.StartPosition().Row) + 1,
		Props: props,
		Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: path},
		},
	})
}

// ── Type declarations ───────────────────────────────────────────────────────

func (w *astWalker) handleTypeDecl(node *sitter.Node, kind string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	mods := modifierSet(node, w.src)
	qualified := w.qualify(name)

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.canonicalName(qualified),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    w.exported(mods, true),
			"language":    "csharp",
			"fqn":         w.fqn(qualified),
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if ns := w.namespace(); ns != "" {
		f.SetProp("namespace", ns)
	}
	for _, m := range []string{"abstract", "sealed", "static", "partial"} {
		if mods[m] {
			f.SetProp(m, true)
		}
	}
	if kindOf(node) == "record_declaration" {
		f.SetProp("record", true)
	}
	// State and no behaviour: a DTO, a constants holder, an attribute. Read by the
	// package-metrics explainer, which spares such packages advice that only makes
	// sense for types with behaviour to abstract.
	if kind != facts.SymbolInterface && isDataHolderBody(node.ChildByFieldName("body")) {
		f.SetProp("data_holder", true)
	}

	for _, t := range w.baseTypes(node) {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: t})
	}

	w.out = append(w.out, f)
	idx := len(w.out) - 1
	w.pushOwner(idx)

	// ASP.NET routing evidence: recorded for every type, decided later. Whether
	// this one serves routes depends on its methods and on a base class that is
	// usually in another file.
	if kind == facts.SymbolClass {
		w.noteController(node, f.Name, name)
		w.noteStorage(nodeText(node, w.src), f.Name, w.baseTypes(node), int(node.StartPosition().Row)+1)
	}

	body := node.ChildByFieldName("body")
	w.typeStack = append(w.typeStack, name)
	w.methodStack = append(w.methodStack, collectMemberNames(body, w.src))
	w.interfaceStack = append(w.interfaceStack, kind == facts.SymbolInterface)
	w.ctorStack = append(w.ctorStack, countConstructors(body))
	w.partialStack = append(w.partialStack, mods["partial"])

	// A primary constructor's parameters are injected dependencies — the C# 12
	// spelling of the constructor-injection pattern Java expresses with a sole
	// constructor. For a `record` they are also its public properties.
	if params := findChildByKind(node, "parameter_list"); params != nil {
		w.injectParams(params, idx)
		if kindOf(node) == "record_declaration" {
			w.recordPositionalProperties(params)
		}
	}

	if body != nil {
		w.walkMembers(body)
	}

	w.partialStack = w.partialStack[:len(w.partialStack)-1]
	w.ctorStack = w.ctorStack[:len(w.ctorStack)-1]
	w.interfaceStack = w.interfaceStack[:len(w.interfaceStack)-1]
	w.methodStack = w.methodStack[:len(w.methodStack)-1]
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
	w.popOwner()
}

func (w *astWalker) handleEnum(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	mods := modifierSet(node, w.src)
	qualified := w.qualify(name)
	exported := w.exported(mods, true)

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.canonicalName(qualified),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolEnum,
			"exported":    exported,
			"language":    "csharp",
			"fqn":         w.fqn(qualified),
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if ns := w.namespace(); ns != "" {
		f.SetProp("namespace", ns)
	}
	w.out = append(w.out, f)

	body := node.ChildByFieldName("body")
	if body == nil {
		return
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if kindOf(c) != "enum_member_declaration" {
			continue
		}
		mn := c.ChildByFieldName("name")
		if mn == nil {
			continue
		}
		member := nodeText(mn, w.src)
		props := map[string]any{
			"symbol_kind": facts.SymbolConstant,
			"exported":    exported,
			"language":    "csharp",
			"receiver":    qualified,
		}
		if ns := w.namespace(); ns != "" {
			props["namespace"] = ns
		}
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindSymbol,
			Name:  w.canonicalName(qualified + "." + member),
			File:  w.relFile,
			Line:  int(c.StartPosition().Row) + 1,
			Props: props,
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: w.dir},
			},
		})
	}
}

func (w *astWalker) handleDelegate(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	mods := modifierSet(node, w.src)
	qualified := w.qualify(name)
	props := map[string]any{
		"symbol_kind": facts.SymbolType,
		"exported":    w.exported(mods, true),
		"language":    "csharp",
		"fqn":         w.fqn(qualified),
		"delegate":    true,
	}
	if ns := w.namespace(); ns != "" {
		props["namespace"] = ns
	}
	w.out = append(w.out, facts.Fact{
		Kind:  facts.KindSymbol,
		Name:  w.canonicalName(qualified),
		File:  w.relFile,
		Line:  int(node.StartPosition().Row) + 1,
		Props: props,
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
}

// ── Members ─────────────────────────────────────────────────────────────────

func (w *astWalker) walkMembers(body *sitter.Node) {
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		switch kindOf(c) {
		case "class_declaration":
			w.handleTypeDecl(c, facts.SymbolClass)
		case "interface_declaration":
			w.handleTypeDecl(c, facts.SymbolInterface)
		case "struct_declaration":
			w.handleTypeDecl(c, facts.SymbolStruct)
		case "record_declaration":
			w.handleTypeDecl(c, recordSymbolKind(c, w.src))
		case "enum_declaration":
			w.handleEnum(c)
		case "delegate_declaration":
			w.handleDelegate(c)
		case "method_declaration", "constructor_declaration", "destructor_declaration",
			"operator_declaration", "conversion_operator_declaration":
			w.handleMethod(c)
		case "property_declaration", "indexer_declaration":
			w.handleProperty(c)
		case "field_declaration", "event_field_declaration":
			w.handleField(c)
		case "preproc_if", "preproc_else", "preproc_elif":
			w.walkMembers(c)
		default:
			// Static constructors' bodies, event_declaration accessors, and
			// anything else that can hold a call site.
			w.walkForCalls(c)
		}
	}
}

func (w *astWalker) handleMethod(node *sitter.Node) {
	name := w.memberName(node)
	if name == "" {
		return
	}
	mods := modifierSet(node, w.src)
	qualified := w.qualify(name)

	props := map[string]any{
		"symbol_kind": facts.SymbolMethod,
		"exported":    w.exported(mods, false),
		"language":    "csharp",
	}
	if ns := w.namespace(); ns != "" {
		props["namespace"] = ns
	}
	if t := w.enclosingType(); t != "" {
		props["receiver"] = t
	}
	for _, m := range []string{"static", "abstract", "virtual", "override", "async", "partial"} {
		if mods[m] {
			props[m] = true
		}
	}
	// An extension method is a static method whose first parameter carries `this`.
	// The receiver type is recorded but NOT turned into an edge: binding `y.Foo()`
	// to it needs the receiver's static type, which this extractor does not track.
	if mods["static"] {
		if recv, ok := extensionReceiver(node, w.src); ok {
			props["extension_method"] = true
			props["extends_type"] = recv
		}
	}

	w.out = append(w.out, facts.Fact{
		Kind:  facts.KindSymbol,
		Name:  w.canonicalName(qualified),
		File:  w.relFile,
		Line:  int(node.StartPosition().Row) + 1,
		Props: props,
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
	idx := len(w.out) - 1
	w.pushOwner(idx)

	if kindOf(node) == "method_declaration" && len(w.typeStack) > 0 {
		w.noteAction(node, w.canonicalName(w.enclosingType()), w.out[idx].Name, name)
		// A Refit attribute on an interface method declares an OUTBOUND request the
		// same way [HttpGet] declares an inbound one.
		w.noteRefit(node, int(node.StartPosition().Row)+1)
	}

	// A constructor is where dependency injection lands in C#. Injecting from
	// every constructor rather than only a sole one (Java's rule) would turn a
	// value type's convenience overloads into dependencies, so the same
	// restriction applies: one constructor, or none of them.
	if kindOf(node) == "constructor_declaration" && w.soleConstructor() {
		if params := node.ChildByFieldName("parameters"); params != nil {
			w.injectParams(params, w.ownerStack[len(w.ownerStack)-2])
		}
	}

	w.walkBodyWithMetrics(node, idx, name, paramCount(node))
	w.popOwner()
}

// handleProperty emits a property or indexer. Non-exported members are skipped as
// symbols — a private backing property is implementation detail, and on a corpus
// this size emitting every one of them would swamp the symbol set — but their
// accessor bodies are still walked so the call edges inside them survive,
// attributed to the enclosing type.
func (w *astWalker) handleProperty(node *sitter.Node) {
	mods := modifierSet(node, w.src)
	exported := w.exported(mods, false)
	name := w.memberName(node)
	if kindOf(node) == "indexer_declaration" {
		name = "this[]"
	}
	if !exported || name == "" {
		w.walkForCalls(node)
		return
	}
	qualified := w.qualify(name)
	props := map[string]any{
		"symbol_kind": facts.SymbolVariable,
		"exported":    true,
		"language":    "csharp",
		"property":    true,
	}
	if ns := w.namespace(); ns != "" {
		props["namespace"] = ns
	}
	if t := w.enclosingType(); t != "" {
		props["receiver"] = t
	}
	for _, m := range []string{"static", "abstract", "virtual", "override", "required"} {
		if mods[m] {
			props[m] = true
		}
	}
	w.out = append(w.out, facts.Fact{
		Kind:  facts.KindSymbol,
		Name:  w.canonicalName(qualified),
		File:  w.relFile,
		Line:  int(node.StartPosition().Row) + 1,
		Props: props,
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
	idx := len(w.out) - 1
	w.pushOwner(idx)
	w.walkBodyWithMetrics(node, idx, name, 0)
	w.popOwner()
}

// handleField emits public and protected fields and events. Private fields are
// deliberately not symbols: they are a type's internal state, and emitting them
// would multiply the symbol count without adding an architectural node anyone
// traverses. Their initializers are still walked for constructor calls.
func (w *astWalker) handleField(node *sitter.Node) {
	mods := modifierSet(node, w.src)
	if !w.exported(mods, false) {
		w.walkForCalls(node)
		return
	}
	kind := facts.SymbolVariable
	if mods["const"] || mods["readonly"] {
		kind = facts.SymbolConstant
	}
	decl := findChildByKind(node, "variable_declaration")
	if decl == nil {
		w.walkForCalls(node)
		return
	}
	for i := uint(0); i < uint(decl.ChildCount()); i++ {
		c := decl.Child(i)
		if kindOf(c) != "variable_declarator" {
			continue
		}
		nameNode := c.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nodeText(nameNode, w.src)
		props := map[string]any{
			"symbol_kind": kind,
			"exported":    true,
			"language":    "csharp",
		}
		if ns := w.namespace(); ns != "" {
			props["namespace"] = ns
		}
		if t := w.enclosingType(); t != "" {
			props["receiver"] = t
		}
		if mods["static"] {
			props["static"] = true
		}
		if kindOf(node) == "event_field_declaration" {
			props["event"] = true
		}
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindSymbol,
			Name:  w.canonicalName(w.qualify(name)),
			File:  w.relFile,
			Line:  int(c.StartPosition().Row) + 1,
			Props: props,
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: w.dir},
			},
		})
	}
	w.walkForCalls(node)
}

// recordPositionalProperties emits a positional record's parameters as the public
// properties the compiler generates from them. Only records: a class or struct
// primary constructor's parameters are constructor state, not members.
func (w *astWalker) recordPositionalProperties(params *sitter.Node) {
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		p := params.Child(i)
		if kindOf(p) != "parameter" {
			continue
		}
		nameNode := p.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nodeText(nameNode, w.src)
		props := map[string]any{
			"symbol_kind": facts.SymbolVariable,
			"exported":    true,
			"language":    "csharp",
			"property":    true,
			"positional":  true,
		}
		if ns := w.namespace(); ns != "" {
			props["namespace"] = ns
		}
		if t := w.enclosingType(); t != "" {
			props["receiver"] = t
		}
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindSymbol,
			Name:  w.canonicalName(w.qualify(name)),
			File:  w.relFile,
			Line:  int(p.StartPosition().Row) + 1,
			Props: props,
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: w.dir},
			},
		})
	}
}

// injectParams adds a RelInjects edge from the fact at ownerIdx to each parameter
// type. Primitives and framework scalars resolve to nothing and are dropped.
func (w *astWalker) injectParams(params *sitter.Node, ownerIdx int) {
	if ownerIdx < 0 || ownerIdx >= len(w.out) {
		return
	}
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		p := params.Child(i)
		if kindOf(p) != "parameter" {
			continue
		}
		t := w.targetForType(p.ChildByFieldName("type"))
		if t == "" {
			continue
		}
		owner := &w.out[ownerIdx]
		if owner.Name == t {
			continue
		}
		dup := false
		for _, r := range owner.Relations {
			if r.Kind == facts.RelInjects && r.Target == t {
				dup = true
				break
			}
		}
		if !dup {
			owner.Relations = append(owner.Relations, facts.Relation{Kind: facts.RelInjects, Target: t})
		}
	}
}

// soleConstructor reports whether the enclosing type declares exactly one
// constructor, making its parameters unambiguously injected dependencies.
func (w *astWalker) soleConstructor() bool {
	return len(w.ctorStack) > 0 && w.ctorStack[len(w.ctorStack)-1] == 1
}

// ── Body walk and metrics ───────────────────────────────────────────────────

// walkBodyWithMetrics walks a member's body once, accumulating call edges and the
// complexity metrics together, then writes the metrics onto the fact at idx. The
// walk is re-entrant (a body may hold a local function or a nested type), so the
// outer state is saved and restored around it.
func (w *astWalker) walkBodyWithMetrics(node *sitter.Node, idx int, shortName string, params int) {
	savedMetrics, savedDepth := w.metrics, w.loopDepth
	savedScaling, savedRepeat := w.scalingDepth, w.repeatDepth
	savedName, savedShort, savedParams := w.selfName, w.selfShort, w.selfParams

	w.metrics = &bodyMetrics{}
	w.loopDepth, w.scalingDepth, w.repeatDepth = 0, 0, 0
	w.selfName = w.out[idx].Name
	w.selfShort = shortName
	w.selfParams = params

	// Both spellings of a body: a block, and an expression-bodied `=> expr`.
	// A property's accessors carry theirs individually.
	if body := node.ChildByFieldName("body"); body != nil {
		w.walkForCalls(body)
	}
	if arrow := findChildByKind(node, "arrow_expression_clause"); arrow != nil {
		w.walkForCalls(arrow)
	}
	if accessors := node.ChildByFieldName("accessors"); accessors != nil {
		w.walkForCalls(accessors)
	}
	if init := node.ChildByFieldName("value"); init != nil {
		w.walkForCalls(init)
	}

	m := w.metrics
	props := w.out[idx].Props
	props["cyclomatic"] = 1 + m.decisions
	if m.loopDepth > 0 {
		props["loop_depth"] = m.loopDepth
		props["scaling_loop_depth"] = m.scalingLoopDepth
	}
	if m.loopCount > 0 {
		props["loop_count"] = m.loopCount
	}
	if len(m.callsInLoop) > 0 {
		props["calls_in_loop"] = m.callsInLoop
		if m.callsInScalingLoop == nil {
			m.callsInScalingLoop = []string{}
		}
		props["calls_in_scaling_loop"] = m.callsInScalingLoop
	}
	if m.recursive {
		props["recursive_self"] = true
	}
	if m.ioDirect {
		props["io_direct"] = true
	}

	w.metrics, w.loopDepth = savedMetrics, savedDepth
	w.scalingDepth, w.repeatDepth = savedScaling, savedRepeat
	w.selfName, w.selfShort, w.selfParams = savedName, savedShort, savedParams
}

func (w *astWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	kind := kindOf(node)

	// A lambda is a deferred scope: its body runs when the delegate is invoked,
	// not once per iteration of the loops it was created inside. An iterator's own
	// lambda is handled at the invocation below, which walks it at depth+1.
	if w.metrics != nil && (kind == "lambda_expression" || kind == "anonymous_method_expression") {
		saved, savedScaling, savedRepeat := w.loopDepth, w.scalingDepth, w.repeatDepth
		w.loopDepth, w.scalingDepth, w.repeatDepth = 0, 0, 0
		w.walkChildren(node)
		w.loopDepth, w.scalingDepth, w.repeatDepth = saved, savedScaling, savedRepeat
		return
	}

	if w.metrics != nil {
		switch kind {
		case "if_statement", "conditional_expression", "catch_clause",
			"switch_section", "switch_expression_arm", "when_clause":
			w.metrics.decisions++
		case "binary_expression":
			// `&&`, `||` and `??` each add a path. `?.` is not counted, matching
			// the other extractors' treatment of optional chaining.
			switch operatorText(node, w.src) {
			case "&&", "||", "??":
				w.metrics.decisions++
			}
		}
	}

	switch kind {
	case "class_declaration":
		w.handleTypeDecl(node, facts.SymbolClass)
		return
	case "interface_declaration":
		w.handleTypeDecl(node, facts.SymbolInterface)
		return
	case "struct_declaration":
		w.handleTypeDecl(node, facts.SymbolStruct)
		return
	case "record_declaration":
		w.handleTypeDecl(node, recordSymbolKind(node, w.src))
		return
	case "for_statement", "foreach_statement", "while_statement", "do_statement":
		class := syntacticLoopClass(node, w.src)
		// The parts evaluated in the ENCLOSING scope are walked outside the loop.
		// A `foreach (x in items.Where(p))` enumerates its iterable once — the
		// lambda runs per element of `items`, which is the same n the loop itself
		// runs, not n per iteration. Walking it inside made that idiom, which is
		// everywhere in C#, report O(n²) for O(n) work. A `for` initializer runs
		// once for the same reason; its condition and update genuinely repeat, so
		// they stay inside.
		var outer []*sitter.Node
		switch kindOf(node) {
		case "foreach_statement":
			outer = append(outer, node.ChildByFieldName("right"))
		case "for_statement":
			outer = append(outer, node.ChildByFieldName("initializer"))
		}
		for _, n := range outer {
			w.walkForCalls(n)
		}
		w.enterLoop(class)
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			if c := node.Child(i); !containsAny(outer, c) {
				w.walkForCalls(c)
			}
		}
		w.exitLoop(class)
		return
	case "object_creation_expression":
		typeNode := node.ChildByFieldName("type")
		if t := w.targetForType(typeNode); t != "" {
			w.addEdge(facts.RelInstantiates, t)
		}
		if w.metrics != nil && ioConstructedTypes[simpleTypeName(typeFullName(typeNode, w.src))] {
			w.metrics.ioDirect = true
		}
	case "member_access_expression":
		// A qualified reference that is NOT a call: an enum member
		// (`VideoRange.HDR`), a static field, a constant. Only a PascalCase
		// receiver is considered, which is C#'s type-naming convention — a
		// camelCase receiver is a value whose type is not tracked. The invocation
		// case below reaches the same helper for `Type.Method()`, and addEdge
		// dedupes, so a call site is not counted twice.
		if recv := node.ChildByFieldName("expression"); recv != nil {
			text := nodeText(recv, w.src)
			if isTypeNameShaped(recv, text) {
				if nameNode := node.ChildByFieldName("name"); nameNode != nil {
					w.addQualifiedRef(text, simpleTypeName(nodeText(nameNode, w.src)))
				}
			}
		}
	case "invocation_expression":
		w.handleInvocation(node)
		// A container registration is the only place a DI-wired implementation is
		// named, so without this it has no inbound edge and reads as dead.
		w.noteDIRegistration(node)
		if w.metrics != nil {
			if lam := iteratorLambda(node, w.src); lam != nil {
				bounded := iteratorReceiverBounded(node, w.src)
				class := loopScaling
				if bounded {
					class = loopConstant
				}
				// The receiver and the non-lambda arguments are evaluated once;
				// only the lambda body runs per element.
				w.walkChildrenExcept(node, lam)
				w.enterLoop(class)
				if b := lam.ChildByFieldName("body"); b != nil {
					w.walkForCalls(b)
				}
				w.exitLoop(class)
				return
			}
		}
	}

	w.walkChildren(node)
}

func (w *astWalker) walkChildren(node *sitter.Node) {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkForCalls(node.Child(i))
	}
}

// walkChildrenExcept walks every child except the subtree rooted at skip.
func (w *astWalker) walkChildrenExcept(node, skip *sitter.Node) {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch {
		case sameNode(c, skip):
			// skipped
		case contains(c, skip):
			w.walkChildrenExcept(c, skip)
		default:
			w.walkForCalls(c)
		}
	}
}

func (w *astWalker) enterLoop(class loopClass) {
	if w.metrics != nil {
		w.metrics.loopCount++
		w.metrics.decisions++
		if w.loopDepth+1 > w.metrics.loopDepth {
			w.metrics.loopDepth = w.loopDepth + 1
		}
		if class.scales() && w.scalingDepth+1 > w.metrics.scalingLoopDepth {
			w.metrics.scalingLoopDepth = w.scalingDepth + 1
		}
	}
	w.loopDepth++
	if class.scales() {
		w.scalingDepth++
	}
	if class.repeats() {
		w.repeatDepth++
	}
}

func (w *astWalker) exitLoop(class loopClass) {
	w.loopDepth--
	if class.scales() {
		w.scalingDepth--
	}
	if class.repeats() {
		w.repeatDepth--
	}
}

// handleInvocation resolves a call site.
//
// Resolution is deliberately narrow: a bare `Foo()` or `this.Foo()` binds to the
// enclosing type's own member, and `Type.Method()` binds when Type resolves to a
// declared type (settled in resolveCSharpTargets). A call on any other receiver
// records its name for the in-loop metrics but draws no edge, because the
// receiver's static type is not tracked and guessing it on a corpus where
// `Execute`, `Handle` and `Dispose` each name hundreds of unrelated methods would
// manufacture edges between unrelated subsystems.
func (w *astWalker) handleInvocation(node *sitter.Node) {
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return
	}
	args := argCount(node)

	switch kindOf(fn) {
	case "identifier", "generic_name":
		name := simpleTypeName(nodeText(fn, w.src))
		if ioMethods[name] {
			w.markIO()
		}
		if target, ok := w.resolveOwnMember(name); ok {
			w.addEdge(facts.RelCalls, target)
			w.recordCallMetrics(target, name, args)
		}
	case "member_access_expression":
		nameNode := fn.ChildByFieldName("name")
		if nameNode == nil {
			return
		}
		name := simpleTypeName(nodeText(nameNode, w.src))
		recvNode := fn.ChildByFieldName("expression")
		recv := ""
		if recvNode != nil {
			recv = nodeText(recvNode, w.src)
		}
		if ioMethods[name] || ioStaticTypes[recv] {
			w.markIO()
		}
		switch {
		case recv == "this" || recv == "base":
			if target, ok := w.resolveOwnMember(name); ok {
				w.addEdge(facts.RelCalls, target)
				w.recordCallMetrics(target, name, args)
			}
		case recvNode != nil && isTypeNameShaped(recvNode, recv):
			// `Type.Method(...)` — a static call. Emitted as "<Type>.<Method>" and
			// bound only if Type resolves to a declared type; an unresolved one is
			// left as written and matches nothing.
			w.addQualifiedRef(recv, name)
			w.recordCallMetrics(recv+"."+name, name, args)
		default:
			// A call on some other receiver — a field, a local, a parameter, an
			// interface-typed dependency. The receiver's static type is not
			// tracked, so the method name is emitted BARE and bound in
			// resolveCSharpTargets against the project's own method index.
			//
			// Without it the C# call graph is same-type-only, and a method reached
			// through an interface — how a DI-wired .NET application calls almost
			// everything — has no inbound edge at all. On jellyfin that read as
			// 1,667 dead methods, including BOTH halves of every implicit interface
			// implementation: the interface member and the class method serving it.
			w.addEdge(facts.RelCalls, name)
			w.recordInLoop(recv, name)
		}
	}
}

// resolveOwnMember binds a bare `Foo()` or `this.Foo()` call to the enclosing
// type's own member.
//
// The member set is collected from the type body this file declares, which is the
// whole story for an ordinary type and only half of it for a `partial` one: a
// method in Widget.Core.cs calling one declared in Widget.Extra.cs finds nothing,
// because the two halves are different parse trees. So inside a partial type the
// name is offered speculatively — the target is the type's own member namespace,
// where it either exists in some other half or does not exist at all, and
// dropPartialGuesses removes the ones that turned out not to.
//
// That is safe in a way a general short-name guess would not be: the candidate is
// scoped to one type rather than to the repository, so the worst case is an edge
// to a base-class or extension method that is dropped for naming no fact, never an
// edge to an unrelated type that happens to share a method name.
func (w *astWalker) resolveOwnMember(name string) (string, bool) {
	if len(w.typeStack) == 0 {
		return "", false
	}
	if w.currentMethods()[name] {
		return w.canonicalName(w.qualify(name)), true
	}
	if len(w.partialStack) > 0 && w.partialStack[len(w.partialStack)-1] {
		return w.canonicalName(w.qualify(name)), true
	}
	return "", false
}

// addQualifiedRef records a `Type.Member` reference — an enum member, a static
// field, a constant, or a static call.
//
// Only the qualified form is emitted here. The edge to the TYPE itself is added by
// resolveCSharpTargets, and only once the receiver has provably resolved to a
// declared type: emitting a bare `VideoRange` here would be indistinguishable from
// the bare method name a member call produces, and the resolver would have to
// guess which it was — binding `foo.Order()` to a class named Order.
func (w *astWalker) addQualifiedRef(recv, member string) {
	w.addEdge(facts.RelCalls, aliasOr(w.aliases, recv)+"."+member)
}

func (w *astWalker) markIO() {
	if w.metrics != nil {
		w.metrics.ioDirect = true
	}
}

// recordCallMetrics flags direct recursion and records in-loop call targets.
// Recursion needs the argument count to match the enclosing member's parameter
// count, so a call to a same-named overload is not read as self-recursion.
func (w *astWalker) recordCallMetrics(target, short string, args int) {
	if w.metrics == nil {
		return
	}
	if target == w.selfName && args == w.selfParams {
		w.metrics.recursive = true
	}
	if w.loopDepth > 0 && !cheapMethods[short] {
		w.noteInLoop(target)
	}
}

func (w *astWalker) recordInLoop(recv, name string) {
	if w.metrics == nil || w.loopDepth == 0 || cheapMethods[name] {
		return
	}
	t := name
	if recv != "" {
		t = recv + "." + name
	}
	w.noteInLoop(t)
}

func (w *astWalker) noteInLoop(target string) {
	m := w.metrics
	if m.inLoopSeen == nil {
		m.inLoopSeen = map[string]bool{}
	}
	if !m.inLoopSeen[target] {
		m.inLoopSeen[target] = true
		m.callsInLoop = append(m.callsInLoop, target)
	}
	// Gated on repeatDepth, not scalingDepth: only a genuinely constant loop
	// excludes its calls. A `while (true)` polling loop is discounted from the
	// Big-O exponent but its body still runs many times, so a query inside it is
	// exactly the N+1 this list exists to surface.
	if w.repeatDepth == 0 {
		return
	}
	if m.inScalingSeen == nil {
		m.inScalingSeen = map[string]bool{}
	}
	if !m.inScalingSeen[target] {
		m.inScalingSeen[target] = true
		m.callsInScalingLoop = append(m.callsInScalingLoop, target)
	}
}
