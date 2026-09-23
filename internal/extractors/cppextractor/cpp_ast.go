package cppextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
)

// extractFileAST parses a C++ translation unit with tree-sitter and emits
// architectural facts (parity with the Swift/Kotlin extractors).
//
// Symbols are named "<dir>.<ns1::ns2::...::Class::member>": enola's "<dir>."
// module convention on the outside, native C++ "::" scope separators inside. The
// scheme is what makes an out-of-line definition "Foo::bar" (parsed from a
// qualified_identifier) produce a byte-identical name to its in-class declaration,
// so the header/source dedup pass in Extract collapses them automatically.
//
// Call-graph / inheritance / instantiation edge targets are emitted as bare names
// here (e.g. "Msg::Error", "GEntity") and canonicalised to "<dir>.<...>" by the
// post-pass in Extract, which holds the project-wide type index.
//
// lang selects the tree-sitter grammar ("c" -> tree-sitter-c, otherwise
// tree-sitter-cpp) and is emitted as the per-fact "language" prop. The two
// grammars share node-type names for every kind the walker reads; the C++-only
// kinds (class_specifier, namespace_definition, template_declaration,
// qualified_identifier, alias_declaration) simply never appear in C trees.
func extractFileAST(src []byte, relFile, lang string, macros macroTable) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	grammar := cpp.Language()
	if lang == langC {
		grammar = c.Language()
	}
	if err := parser.SetLanguage(sitter.NewLanguage(grammar)); err != nil {
		return nil
	}
	kinds := kindsFor(lang)
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	w := &astWalker{
		src:            src,
		kinds:          kinds,
		relFile:        relFile,
		dir:            factpath.Dir(relFile),
		lang:           lang,
		fileMethods:    buildFileMethodIndex(kinds, tree.RootNode(), src),
		moduleOwnerIdx: -1,
		macros:         macros,
	}
	w.walkDeclList(tree.RootNode())
	// Recover callbacks from macro-opened struct initializers (machine_desc), which
	// tree-sitter can't parse and scatters as `.field = fn` assignment debris.
	w.salvageMacroStructAssigns(tree.RootNode())
	return w.out
}

// buildFileMethodIndex collects, per simple class name, the set of method names
// declared or defined anywhere in this file (inline prototypes/definitions and
// out-of-line "Class::method" definitions). It lets an out-of-line method body
// resolve calls to sibling methods of the same class to their canonical names.
func buildFileMethodIndex(kinds *tsutil.KindTable, root *sitter.Node, src []byte) map[string]map[string]bool {
	idx := make(map[string]map[string]bool)
	add := func(owner, method string) {
		if owner == "" || method == "" {
			return
		}
		if idx[owner] == nil {
			idx[owner] = make(map[string]bool)
		}
		idx[owner][method] = true
	}
	var visit func(n *sitter.Node)
	visit = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch kindOf(kinds, n) {
		case "class_specifier", "struct_specifier", "union_specifier":
			if nn := n.ChildByFieldName("name"); nn != nil {
				owner := simpleTypeName(nn, src)
				for m := range collectMethodNames(kinds, n.ChildByFieldName("body"), src) {
					add(owner, m)
				}
			}
		case "function_definition":
			if fd := findFunctionDeclarator(kinds, n.ChildByFieldName("declarator")); fd != nil {
				if nameNode := fd.ChildByFieldName("declarator"); nameNode != nil && kindOf(kinds, nameNode) == "qualified_identifier" {
					scopes, leaf := splitQualified(kinds, nameNode, src)
					if len(scopes) > 0 {
						add(scopes[len(scopes)-1], leaf)
					}
				}
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			visit(n.Child(i))
		}
	}
	visit(root)
	return idx
}

type astWalker struct {
	src     []byte
	relFile string
	dir     string
	// lang is "c" or "cpp" — emitted as the per-fact "language" prop and used to
	// apply C-only semantics (e.g. static => file-private).
	lang string
	// kinds names node types for the grammar `lang` selected. It travels with the
	// walker because the two grammars do not share symbol ids; see kinds.go.
	kinds *tsutil.KindTable

	out []facts.Fact

	// nsStack holds the enclosing namespace components (e.g. ["gmsh","fs"]).
	nsStack []string
	// typeStack holds the enclosing class/struct/union names. methodStack is
	// parallel and holds the set of method names declared directly in each
	// enclosing type, used to resolve same-type bare/this calls.
	typeStack   []string
	methodStack []map[string]bool

	// ownerStack holds INDICES into out (not pointers) of the symbol fact whose
	// body is currently being walked. Indices stay valid across append/realloc,
	// which pointers would not. -1 means no owner.
	ownerStack []int

	// fileMethods maps a simple class name to the set of its method names declared
	// anywhere in this file, so out-of-line method bodies can resolve sibling calls.
	fileMethods map[string]map[string]bool

	// moduleOwnerIdx is the index in out of a lazily-emitted KindModule fact that
	// hosts file-scope macro-call references (module_init(foo), EXPORT_SYMBOL(foo)).
	// -1 until first needed. Extract folds its relations into the canonical per-dir
	// module fact.
	moduleOwnerIdx int

	// macros is the project-wide #define table, used to expand file-scope macro
	// invocations (CONFIGFS_ATTR, DEVICE_ATTR_RO, …) and recover the token-pasted
	// callbacks they reference.
	macros macroTable

	// Per-function complexity state, set up by handleFunctionDefinition around
	// walkForCalls and saved/restored across the re-entrant body walk. metrics is
	// nil outside a function body walk. loopDepth is the current loop nesting depth;
	// selfName/selfShort are the enclosing function's full and leaf names (for
	// direct-recursion detection).
	metrics   *cppBodyMetrics
	loopDepth int
	// scalingDepth counts only input-scaling (unbounded) loops — the Big-O exponent.
	// repeatDepth counts loops that run a non-constant number of times; it differs from
	// scalingDepth only for `for (;;)` / `while (true)`, which add no factor of n but
	// whose body still runs many times, so a query inside stays an N+1 candidate.
	scalingDepth int
	repeatDepth  int
	selfName     string
	selfShort    string
	// receiverTypes maps explicit parameter/local names in the current function
	// body to their simple project type. Empty values mark conflicting declarations.
	receiverTypes map[string]string
}

// cppBodyMetrics accumulates per-function complexity signals during the single
// walkForCalls body traversal — mirrors the other extractors.
type cppBodyMetrics struct {
	loopDepth          int             // max loop nesting depth
	loopCount          int             // number of loop constructs (syntactic + STL-algorithm lambdas)
	decisions          int             // decision points (cyclomatic = 1 + decisions)
	callsInLoop        []string        // distinct call targets invoked at loop depth >= 1
	inLoopSeen         map[string]bool // dedup set for callsInLoop
	scalingLoopDepth   int             // max nesting counting only unbounded (input-scaling) loops
	callsInScalingLoop []string        // distinct targets invoked inside a repeating loop (N+1 candidates)
	inScalingSeen      map[string]bool // dedup set for callsInScalingLoop
	recursive          bool            // body directly calls the enclosing function
	ioDirect           bool            // body makes a direct file/socket I/O call
}

// cppStlIterators are <algorithm>/<numeric> functions whose lambda/functor argument
// runs once per element — i.e. a loop. A lambda passed to a function NOT in this set
// is a deferred scope and not treated as a loop.
var cppStlIterators = map[string]bool{
	"for_each": true, "transform": true, "accumulate": true, "reduce": true,
	"count_if": true, "find_if": true, "find_if_not": true, "copy_if": true,
	"remove_if": true, "remove_copy_if": true, "replace_if": true, "erase_if": true,
	"sort": true, "stable_sort": true, "partial_sort": true, "nth_element": true,
	"generate": true, "generate_n": true, "transform_reduce": true, "transform_exclusive_scan": true,
	"all_of": true, "any_of": true, "none_of": true, "partition": true, "stable_partition": true,
	"min_element": true, "max_element": true, "sort_heap": true, "inner_product": true,
}

// recordCallMetrics notes a resolved call target against the current function's
// complexity metrics: flags direct recursion and records calls made inside loops.
func (w *astWalker) recordCallMetrics(target string) {
	if w.metrics == nil || target == "" {
		return
	}
	if target == w.selfName || target == w.selfShort {
		w.metrics.recursive = true
	}
	w.recordInLoop(target)
}

// recordInLoop adds a target to calls_in_loop (deduped) when inside a loop, without
// the recursion check — used for raw instance-method names.
func (w *astWalker) recordInLoop(target string) {
	if w.metrics == nil || target == "" || w.loopDepth == 0 {
		return
	}
	if w.metrics.inLoopSeen == nil {
		w.metrics.inLoopSeen = make(map[string]bool)
	}
	if !w.metrics.inLoopSeen[target] {
		w.metrics.inLoopSeen[target] = true
		w.metrics.callsInLoop = append(w.metrics.callsInLoop, target)
	}
	// A call inside a loop that repeats a non-constant number of times is an N+1
	// candidate. A constant loop (literal-bounded for, range-for over a braced list)
	// excludes its calls; `for (;;)` / `while (true)` repeats, so its calls stay
	// candidates even though its depth is discounted from the Big-O exponent.
	if w.repeatDepth > 0 {
		if w.metrics.inScalingSeen == nil {
			w.metrics.inScalingSeen = make(map[string]bool)
		}
		if !w.metrics.inScalingSeen[target] {
			w.metrics.inScalingSeen[target] = true
			w.metrics.callsInScalingLoop = append(w.metrics.callsInScalingLoop, target)
		}
	}
}

// cppLoopClass classifies a loop by two independent properties: whether it adds a
// factor of n to Big-O (scales) and whether its body runs a non-constant number of
// times (repeats). A constant-count loop — `for (int i = 0; i < 3; i++)`, a range-for
// over a braced init-list — does neither. An infinite `for (;;)` / `while (true)` does
// not scale but still repeats. Collapsing the two deletes true positives (cache.go v99).
type cppLoopClass int

const (
	cppLoopScaling cppLoopClass = iota
	cppLoopConstant
	cppLoopInfinite
)

func (c cppLoopClass) scales() bool  { return c == cppLoopScaling }
func (c cppLoopClass) repeats() bool { return c != cppLoopConstant }

// cppSyntacticLoopClass classifies a for / range-for / while / do-while statement.
func cppSyntacticLoopClass(kinds *tsutil.KindTable, node *sitter.Node, src []byte) cppLoopClass {
	switch kindOf(kinds, node) {
	case "for_statement":
		cond := node.ChildByFieldName("condition")
		if cond == nil {
			return cppLoopInfinite // for (;;)
		}
		if cppConstantForCondition(kinds, cond) {
			return cppLoopConstant
		}
	case "for_range_loop":
		// for (T x : {a, b, c}) — a braced init-list iterates a fixed count. A variable
		// range (for (x : xs)) scales.
		if r := node.ChildByFieldName("right"); r != nil && kindOf(kinds, r) == "initializer_list" {
			return cppLoopConstant
		}
	case "while_statement", "do_statement":
		if cppIsTrueCondition(node.ChildByFieldName("condition"), src) {
			return cppLoopInfinite
		}
	}
	return cppLoopScaling
}

// cppConstantForCondition reports whether a three-clause for's condition compares the
// loop variable against a numeric literal (`i < 3`), so the loop runs a statically fixed
// number of times. A data-derived bound (`i < n`, `i < v.size()`) or a compound
// condition is conservatively treated as scaling — no genuine O(n) finding is deleted.
func cppConstantForCondition(kinds *tsutil.KindTable, cond *sitter.Node) bool {
	if cond == nil || kindOf(kinds, cond) != "binary_expression" {
		return false
	}
	hasCmp, hasLiteral := false, false
	for i := uint(0); i < cond.ChildCount(); i++ {
		switch kindOf(kinds, cond.Child(i)) {
		case "<", "<=", ">", ">=", "!=":
			hasCmp = true
		case "number_literal":
			hasLiteral = true
		}
	}
	return hasCmp && hasLiteral
}

// cppIsTrueCondition reports whether a loop condition is the literal `true` (or C's
// `1`) — `while (true)`, `do … while (1)`. A named constant is not matched. The
// condition arrives wrapped (condition_clause / parenthesized_expression), so strip
// the outer parens and compare text.
func cppIsTrueCondition(cond *sitter.Node, src []byte) bool {
	if cond == nil {
		return false
	}
	t := strings.TrimSpace(nodeText(cond, src))
	for strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	return t == "true" || t == "1"
}

// cppCheapMethods are obviously-cheap STL container / iterator / accessor methods.
// A call to one of these on an unknown receiver inside a loop is NOT recorded in
// calls_in_loop, keeping the metric focused on potential per-iteration work (the
// analyzer's keyword gate is the real precision filter, but suppressing these at the
// source avoids false positives like a JSON `db.begin()`/`db.end()` matching the
// generic `db.` I/O prefix).
var cppCheapMethods = map[string]bool{
	"begin": true, "end": true, "cbegin": true, "cend": true,
	"rbegin": true, "rend": true, "size": true, "length": true,
	"empty": true, "clear": true, "data": true, "c_str": true,
	"at": true, "front": true, "back": true, "top": true,
	"push_back": true, "emplace_back": true, "pop_back": true,
	"push": true, "pop": true, "emplace": true, "reserve": true,
	"first": true, "second": true, "count": true, "capacity": true,
	"str": true, "get": true, "set": true, "key": true, "value": true,
}

// cppIODirect are distinctive C/C++ primitives that perform a direct file or
// socket DATA transfer — the seed for io_direct/performs_io. Deliberately narrow:
// only unambiguous data-I/O free functions. Console/logging primitives
// (printf/fprintf/fputs) are excluded (they would mark every Msg::-style logger as
// I/O and turn ordinary logging-in-a-loop into false N+1s), as are the ambiguous
// socket verbs (bind = std::bind, connect = Qt signal, send/recv/accept). C++
// <fstream> stream I/O (ifs.read(), operator>>) is not detectable here — member
// calls on non-`this` receivers and std::-qualified names are dropped upstream.
var cppIODirect = map[string]bool{
	"fopen": true, "freopen": true, "fread": true, "fwrite": true,
	"socket": true, "recvfrom": true, "sendto": true,
}

// cppBooleanOp reports whether a binary_expression's operator is a short-circuit
// boolean operator (&& / ||), which adds a cyclomatic decision point.
func cppBooleanOp(kinds *tsutil.KindTable, node *sitter.Node) bool {
	for i := uint(0); i < node.ChildCount(); i++ {
		switch kindOf(kinds, node.Child(i)) {
		case "&&", "||":
			return true
		}
	}
	return false
}

// cppByteContains reports whether inner's byte span is nested within outer's.
func cppByteContains(outer, inner *sitter.Node) bool {
	return inner.StartByte() >= outer.StartByte() && inner.EndByte() <= outer.EndByte()
}

func (w *astWalker) pushType(name string, methods map[string]bool) {
	w.typeStack = append(w.typeStack, name)
	w.methodStack = append(w.methodStack, methods)
}

func (w *astWalker) popType() {
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
	w.methodStack = w.methodStack[:len(w.methodStack)-1]
}

func (w *astWalker) currentMethods() map[string]bool {
	if len(w.methodStack) == 0 {
		return nil
	}
	return w.methodStack[len(w.methodStack)-1]
}

// scopePrefix returns the "ns::...::Class::" prefix for the current position, or
// "" at file scope.
func (w *astWalker) scopePrefix() string {
	n := len(w.nsStack) + len(w.typeStack)
	if n == 0 {
		return ""
	}
	parts := make([]string, 0, n)
	parts = append(parts, w.nsStack...)
	parts = append(parts, w.typeStack...)
	return strings.Join(parts, "::") + "::"
}

func (w *astWalker) qualify(name string) string { return w.scopePrefix() + name }

// factName returns the canonical "<dir>.<scope>::<name>" fact name.
func (w *astWalker) factName(name string) string { return w.dir + "." + w.qualify(name) }

// enclosingTypeReceiver returns the innermost enclosing class name (the receiver
// for a method), or "" when at namespace/file scope.
func (w *astWalker) enclosingTypeReceiver() string {
	if len(w.typeStack) == 0 {
		return ""
	}
	return w.typeStack[len(w.typeStack)-1]
}

func (w *astWalker) pushOwner(i int) { w.ownerStack = append(w.ownerStack, i) }
func (w *astWalker) popOwner()       { w.ownerStack = w.ownerStack[:len(w.ownerStack)-1] }
func (w *astWalker) currentOwner() int {
	if len(w.ownerStack) == 0 {
		return -1
	}
	return w.ownerStack[len(w.ownerStack)-1]
}

// addOwnerEdge appends a relation to the current owner fact, if any, avoiding
// duplicate relations on the same fact.
func (w *astWalker) addOwnerEdge(kind, target string) {
	idx := w.currentOwner()
	if idx < 0 || target == "" {
		return
	}
	for _, r := range w.out[idx].Relations {
		if r.Kind == kind && r.Target == target {
			return
		}
	}
	w.out[idx].Relations = append(w.out[idx].Relations, facts.Relation{Kind: kind, Target: target})
}

// walkDeclList dispatches every child of a translation_unit / declaration_list /
// preproc guard body to walkDecl.
func (w *astWalker) walkDeclList(node *sitter.Node) {
	if node == nil {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkDecl(node.Child(i))
	}
}

// walkDecl handles a single top-level / namespace-level / preproc-guarded
// declaration node.
func (w *astWalker) walkDecl(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(w.kinds, node) {
	case "preproc_include":
		w.handleInclude(node)
	case "namespace_definition":
		w.handleNamespace(node)
	case "class_specifier":
		w.handleClassLike(node, facts.SymbolClass)
	case "struct_specifier":
		w.handleClassLike(node, facts.SymbolStruct)
	case "union_specifier":
		w.handleClassLike(node, facts.SymbolStruct)
	case "enum_specifier":
		w.handleEnum(node)
	case "function_definition":
		w.handleFunctionDefinition(node)
	case "template_declaration":
		w.handleTemplate(node)
	case "declaration":
		w.handleDeclaration(node)
	case "type_definition":
		w.handleTypedef(node)
	case "alias_declaration":
		w.handleUsing(node)
	case "linkage_specification":
		// extern "C" { ... } — recurse into the wrapped body.
		w.walkDeclList(findChildByKind(w.kinds, node, "declaration_list"))
	case "expression_statement":
		// A bare `call_expression;` at file/namespace scope is illegal C/C++ — it is
		// always a registration macro the preprocessor would expand (module_init(foo),
		// fs_initcall(foo), EXPORT_SYMBOL(foo), DEVICE_ATTR(name, mode, show, store)).
		// Macros are not expanded, so the function-name arguments would otherwise be
		// invisible and the referenced entry points mis-reported as dead.
		w.handleFileScopeMacroCall(node)
	case "preproc_def", "preproc_function_def":
		// A function called inside a #define body (e.g. `#define ____cmpxchg(...) (
		// size == 2 ? ____cmpxchg_u16(p,o,n) : ...)`) is invisible to the AST — the
		// replacement list is opaque preproc_arg text — so the called function looks
		// dead. Scan that text for call-position identifiers and record them.
		w.handleMacroBodyCalls(node)
	case "compound_statement":
		// A bare `{ ... }` at file/namespace scope is illegal C/C++ — like the bare
		// call_expression above, it is always the detached body of a function defined
		// by a name-carrying macro: SYSCALL_DEFINE2(name, ...) { ... },
		// COMPAT_SYSCALL_DEFINE*, and kin. tree-sitter renders the macro head as a
		// separate (errored) call_expression and leaves this body loose, so its calls
		// have no owning function_definition and would be dropped — making a static
		// helper reached only from a syscall handler look dead. Credit the body's
		// calls to the module owner.
		w.handleDetachedBody(node)
	case "preproc_if", "preproc_ifdef", "preproc_else", "preproc_elif",
		"preproc_elifdef":
		// Descend through #if/#ifdef guards: declarations inside them are direct
		// children (after the condition/name field). gmsh wraps large amounts of
		// code in #if defined(HAVE_*); headers wrap everything in #ifndef guards.
		w.walkGuardChildren(node)
	}
}

// walkGuardChildren walks the declaration children of a preproc guard, skipping
// the field children (the condition/name and the nested alternative branch, which
// is itself a preproc_* node handled via walkDecl).
func (w *astWalker) walkGuardChildren(node *sitter.Node) {
	for i := uint(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		switch fieldName := node.FieldNameForChild(uint32(i)); fieldName {
		case "condition", "name":
			continue
		}
		w.walkDecl(child)
	}
}

func (w *astWalker) handleNamespace(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	var pushed int
	if nameNode != nil {
		// namespace_identifier (simple) or nested_namespace_specifier (C++17 A::B).
		for _, comp := range strings.Split(nodeText(nameNode, w.src), "::") {
			comp = strings.TrimSpace(comp)
			if comp == "" {
				continue
			}
			w.nsStack = append(w.nsStack, comp)
			pushed++
		}
	}
	// nameNode == nil ⇒ anonymous namespace: push nothing.
	w.walkDeclList(node.ChildByFieldName("body"))
	w.nsStack = w.nsStack[:len(w.nsStack)-pushed]
}

func (w *astWalker) handleClassLike(node *sitter.Node, kind string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return // anonymous struct/union (e.g. inside a typedef) — skip
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}
	body := node.ChildByFieldName("body")
	if body == nil {
		return // forward declaration (class Foo;) — emit nothing
	}

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.factName(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    true, // C++ has no file-private types; visibility is via access specifiers
			"language":    w.lang,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	if kindOf(w.kinds, node) == "union_specifier" {
		f.SetProp("union", true)
	}
	for _, base := range baseClassNames(w.kinds, node, w.src) {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: base})
	}

	w.out = append(w.out, f)
	ownerIdx := len(w.out) - 1

	w.pushOwner(ownerIdx)
	w.pushType(name, collectMethodNames(w.kinds, body, w.src))
	w.walkClassBody(body)
	w.popType()
	w.popOwner()
}

// walkClassBody iterates the direct members of a field_declaration_list.
func (w *astWalker) walkClassBody(body *sitter.Node) {
	for i := uint(0); i < body.ChildCount(); i++ {
		c := body.Child(i)
		switch kindOf(w.kinds, c) {
		case "field_declaration":
			w.handleFieldDeclaration(c)
		case "function_definition":
			w.handleFunctionDefinition(c) // inline method definition
		case "class_specifier":
			w.handleClassLike(c, facts.SymbolClass)
		case "struct_specifier":
			w.handleClassLike(c, facts.SymbolStruct)
		case "union_specifier":
			w.handleClassLike(c, facts.SymbolStruct)
		case "enum_specifier":
			w.handleEnum(c)
		case "template_declaration":
			w.handleTemplate(c)
		case "type_definition":
			w.handleTypedef(c)
		case "alias_declaration":
			w.handleUsing(c)
		case "friend_declaration":
			// friend functions/classes: walk the inner decl so e.g. friend
			// operator definitions still contribute symbols, but don't crash.
			for j := uint(0); j < c.ChildCount(); j++ {
				w.walkDecl(c.Child(j))
			}
		}
	}
}

func (w *astWalker) handleEnum(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return // anonymous enum — skip
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.factName(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolEnum,
			"exported":    true,
			"language":    w.lang,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	if isScopedEnum(node, w.src) {
		f.SetProp("scoped", true)
	}
	w.out = append(w.out, f)
}

// handleFunctionDefinition handles a function_definition, which may be a free
// function, an inline method (inside a class body), or an OUT-OF-LINE method /
// namespaced-function definition (declarator is a qualified_identifier).
func (w *astWalker) handleFunctionDefinition(node *sitter.Node) {
	fdecl := findFunctionDeclarator(w.kinds, node.ChildByFieldName("declarator"))
	if fdecl == nil {
		// A function_definition with no function_declarator is a name-carrying macro
		// that tree-sitter parsed cleanly (rather than as an errored expression): a
		// zero-argument SYSCALL_DEFINE0(rt_sigreturn) { … } renders as a
		// function_definition whose declarator is a parenthesized_declarator around the
		// bare syscall name, so there is no function_declarator to name a symbol from.
		// We still credit the body's calls (a helper reached only from that handler
		// would otherwise look dead) — the same crediting handleDetachedBody applies to
		// the errored SYSCALL_DEFINE1-6 form parsed as a loose top-level block.
		if body := node.ChildByFieldName("body"); body != nil {
			w.handleDetachedBody(body)
		}
		return
	}
	nameNode := fdecl.ChildByFieldName("declarator")
	if nameNode == nil {
		return
	}

	var symbolName, receiver, shortName string
	var outOfLineScopes []string // scope chain to push for an out-of-line def
	switch kindOf(w.kinds, nameNode) {
	case "qualified_identifier":
		// Out-of-line definition: "A::B::method". Combine any enclosing namespace
		// with the qualified scope so the name matches the in-class declaration.
		scopes, leaf := splitQualified(w.kinds, nameNode, w.src)
		if leaf == "" {
			return
		}
		comps := make([]string, 0, len(w.nsStack)+len(scopes)+1)
		comps = append(comps, w.nsStack...)
		comps = append(comps, scopes...)
		comps = append(comps, leaf)
		symbolName = w.dir + "." + strings.Join(comps, "::")
		shortName = leaf
		if len(scopes) > 0 {
			receiver = scopes[len(scopes)-1] // immediate scope owning the definition
			outOfLineScopes = scopes
		}
	default:
		leaf := declaratorLeafName(w.kinds, nameNode, w.src)
		if leaf == "" {
			return
		}
		symbolName = w.factName(leaf)
		shortName = leaf
		receiver = w.enclosingTypeReceiver()
	}

	sk := facts.SymbolFunc
	if receiver != "" {
		sk = facts.SymbolMethod
	}
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: symbolName,
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": sk,
			"exported":    true,
			"language":    w.lang,
			"has_body":    true,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	if receiver != "" {
		f.SetProp("receiver", receiver)
	}
	w.applyFuncQualifiers(&f, node, fdecl)

	w.out = append(w.out, f)
	idx := len(w.out) - 1

	w.pushOwner(idx)
	// For an out-of-line def, push the full scope chain so the body's qualified
	// context (and thus sibling/this call resolution) matches the in-class names.
	// Only the innermost scope (the class) carries the file-wide method set.
	for i, comp := range outOfLineScopes {
		var methods map[string]bool
		if i == len(outOfLineScopes)-1 {
			methods = w.fileMethods[comp]
		}
		w.pushType(comp, methods)
	}
	// Set up per-function complexity tracking. walkForCalls can recurse into nested
	// scopes (lambdas), so save and restore the outer state. Props are written via
	// the stable index (the pointer may be invalidated if the body walk grows w.out).
	savedMetrics, savedDepth := w.metrics, w.loopDepth
	savedScaling, savedRepeat := w.scalingDepth, w.repeatDepth
	savedName, savedShort, savedReceiverTypes := w.selfName, w.selfShort, w.receiverTypes
	w.metrics = &cppBodyMetrics{}
	w.loopDepth = 0
	w.scalingDepth = 0
	w.repeatDepth = 0
	w.selfName = symbolName
	w.selfShort = shortName
	if body := node.ChildByFieldName("body"); body != nil {
		w.receiverTypes = collectExplicitReceiverTypes(w.kinds, w.lang, fdecl, body, w.src)
		w.walkForCalls(body)
	}
	m := w.metrics
	props := w.out[idx].Props
	props["cyclomatic"] = 1 + m.decisions
	if m.loopDepth > 0 {
		props["loop_depth"] = m.loopDepth
		// Emit the scaling depth (bounded loops discounted) alongside — even when 0 — so
		// the consumer distinguishes "all loops bounded" from "signal absent".
		props["scaling_loop_depth"] = m.scalingLoopDepth
	}
	if m.loopCount > 0 {
		props["loop_count"] = m.loopCount
	}
	if len(m.callsInLoop) > 0 {
		props["calls_in_loop"] = m.callsInLoop
		// Emit the N+1 subset alongside — even when EMPTY — so the consumer distinguishes
		// "no call repeats" from "signal absent" and does not fall back to calls_in_loop.
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
	w.selfName, w.selfShort = savedName, savedShort
	w.receiverTypes = savedReceiverTypes
	for range outOfLineScopes {
		w.popType()
	}
	w.popOwner()
}

// handleFieldDeclaration handles a member of a class body: either a member
// function declaration (prototype) or one-or-more data members.
func (w *astWalker) handleFieldDeclaration(node *sitter.Node) {
	if fdecl := findFunctionDeclarator(w.kinds, node.ChildByFieldName("declarator")); fdecl != nil {
		// Member function prototype (no body) — e.g. "virtual void clear();".
		nameNode := fdecl.ChildByFieldName("declarator")
		leaf := declaratorLeafName(w.kinds, nameNode, w.src)
		if leaf == "" {
			return
		}
		f := facts.Fact{
			Kind: facts.KindSymbol,
			Name: w.factName(leaf),
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"symbol_kind": facts.SymbolMethod,
				"exported":    true,
				"language":    w.lang,
				"receiver":    w.enclosingTypeReceiver(),
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
		}
		w.applyFuncQualifiers(&f, node, fdecl)
		w.out = append(w.out, f)
		return
	}

	// Data member(s): "Foo *a, b;" → one SymbolVariable per declared name.
	constMember := strings.Contains(declTypePrefix(node, w.src), "const")
	for i := uint(0); i < node.ChildCount(); i++ {
		if node.FieldNameForChild(uint32(i)) != "declarator" {
			continue
		}
		leaf := declaratorLeafName(w.kinds, node.Child(i), w.src)
		if leaf == "" {
			continue
		}
		kind := facts.SymbolVariable
		if constMember {
			kind = facts.SymbolConstant
		}
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: w.factName(leaf),
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"symbol_kind": kind,
				"exported":    true,
				"language":    w.lang,
				"receiver":    w.enclosingTypeReceiver(),
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
		})
	}
}

// handleTemplate unwraps a template_declaration to its inner class/function/etc.
// declaration and marks every fact the inner decl produced as templated.
func (w *astWalker) handleTemplate(node *sitter.Node) {
	var inner *sitter.Node
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if kindOf(w.kinds, c) == "template_parameter_list" || !c.IsNamed() {
			continue
		}
		inner = c
	}
	if inner == nil {
		return
	}
	before := len(w.out)
	w.walkDecl(inner)
	for i := before; i < len(w.out); i++ {
		if w.out[i].Props != nil {
			w.out[i].SetProp("templated", true)
		}
	}
}

// handleDeclaration handles a translation-unit / namespace-level declaration:
// free-function prototypes (which dedup with their out-of-line definitions) and
// class/struct/enum specifiers used as a declaration's type.
func (w *astWalker) handleDeclaration(node *sitter.Node) {
	// `class Foo {...} bar;` / `struct X {...};` — the specifier is the type field.
	if t := node.ChildByFieldName("type"); t != nil {
		switch kindOf(w.kinds, t) {
		case "class_specifier":
			w.handleClassLike(t, facts.SymbolClass)
		case "struct_specifier":
			w.handleClassLike(t, facts.SymbolStruct)
		case "union_specifier":
			w.handleClassLike(t, facts.SymbolStruct)
		case "enum_specifier":
			w.handleEnum(t)
		case "macro_type_specifier":
			// `static DEFINE_SIMPLE_DEV_PM_OPS(name, suspend, resume);` — a qualifier
			// (static/const) makes tree-sitter parse a registration macro as a
			// declaration whose "type" is a macro_type_specifier, with the macro args
			// as type_identifier nodes. Record the function-name args as uses so the
			// PM/driver callbacks are not mis-reported as dead (the bare
			// expression_statement form is handled by handleFileScopeMacroCall).
			w.handleMacroTypeSpecifier(t)
		case "type_identifier":
			// `static DEVICE_ATTR_RO(name);` — the single-arg form parses as a plain
			// declaration whose type is the macro name (a type_identifier) and whose
			// declarator wraps the arg (parenthesized_declarator). When the "type" is
			// actually a known function-like macro, expand it to recover its pasted
			// callbacks (name##_show / _store), then stop (it's not a real prototype).
			if w.expandMacroDeclaration(t, node.ChildByFieldName("declarator")) {
				return
			}
		}
	}
	// A file-scope object declaration with an initializer (e.g. a kernel ops table
	// `static const struct file_operations f = { .read = foo };` or a function
	// pointer `int (*fp)(void) = bar;`) is a variable definition, not a function
	// prototype — emit the variable and record any function-pointer references in
	// its initializer so the pointed-to functions are not mis-reported as dead.
	if w.handleVarInitializers(node) {
		return
	}
	// Free-function prototype: declarator is (or wraps) a function_declarator whose
	// own declarator is a plain identifier or a qualified_identifier.
	fdecl := findFunctionDeclarator(w.kinds, node.ChildByFieldName("declarator"))
	if fdecl == nil {
		return
	}
	nameNode := fdecl.ChildByFieldName("declarator")
	if nameNode == nil {
		return
	}
	var symbolName, receiver string
	switch kindOf(w.kinds, nameNode) {
	case "qualified_identifier":
		scopes, leaf := splitQualified(w.kinds, nameNode, w.src)
		if leaf == "" {
			return
		}
		comps := append(append([]string{}, w.nsStack...), scopes...)
		comps = append(comps, leaf)
		symbolName = w.dir + "." + strings.Join(comps, "::")
		if n := len(comps) - 1; n > 0 {
			receiver = comps[n-1]
		}
	default:
		leaf := declaratorLeafName(w.kinds, nameNode, w.src)
		if leaf == "" {
			return
		}
		symbolName = w.factName(leaf)
		receiver = w.enclosingTypeReceiver()
	}
	sk := facts.SymbolFunc
	if receiver != "" {
		sk = facts.SymbolMethod
	}
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: symbolName,
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": sk,
			"exported":    true,
			"language":    w.lang,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	if receiver != "" {
		f.SetProp("receiver", receiver)
	}
	w.applyFuncQualifiers(&f, node, fdecl)
	w.out = append(w.out, f)
}

// handleVarInitializers emits a SymbolVariable/SymbolConstant for each file-scope
// object declarator that has an initializer, and walks that initializer for
// function-pointer references. It returns true when the declaration is an
// initialized object definition, so the caller skips the function-prototype path
// (a function is never initialized). This is what surfaces kernel-style ops
// tables — `static const struct file_operations f = { .read = proc_reg_read }` —
// so the pointed-to functions get an inbound edge and aren't reported as dead.
func (w *astWalker) handleVarInitializers(node *sitter.Node) bool {
	constVar := strings.Contains(declTypePrefix(node, w.src), "const")
	handled := false
	for i := uint(0); i < node.ChildCount(); i++ {
		if node.FieldNameForChild(uint32(i)) != "declarator" {
			continue
		}
		decl := node.Child(i)
		if kindOf(w.kinds, decl) != "init_declarator" {
			continue
		}
		value := decl.ChildByFieldName("value")
		if value == nil {
			continue
		}
		handled = true
		leaf := declaratorLeafName(w.kinds, decl.ChildByFieldName("declarator"), w.src)
		if leaf == "" {
			continue
		}
		kind := facts.SymbolVariable
		if constVar {
			kind = facts.SymbolConstant
		}
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: w.factName(leaf),
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"symbol_kind": kind,
				"exported":    true,
				"language":    w.lang,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
		})
		w.pushOwner(len(w.out) - 1)
		w.walkInitializerRefs(value)
		w.popOwner()
	}
	return handled
}

// walkInitializerRefs scans a variable's initializer subtree for identifiers that
// name a function used as a pointer (struct `.field = func`, `&func`, a bare func
// in a positional/array initializer, or a nested initializer_list) and attaches a
// provisional func-pointer edge to the current owner. The `.field` designators,
// literals, and macro-shaped constants are skipped (see emitFuncPtrRef).
func (w *astWalker) walkInitializerRefs(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(w.kinds, node) {
	case "identifier":
		w.emitFuncPtrRef(nodeText(node, w.src))
		return
	case "pointer_expression": // &func / *func
		w.walkInitializerRefs(node.ChildByFieldName("argument"))
		return
	case "initializer_pair":
		// Recurse only into the value — never the ".field" / "[i]" designator.
		w.walkInitializerRefs(node.ChildByFieldName("value"))
		return
	case "field_designator", "subscript_designator", "field_identifier",
		"number_literal", "char_literal", "string_literal", "concatenated_string":
		return // not a function reference
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkInitializerRefs(node.Child(i))
	}
}

// moduleOwner lazily emits the directory's module fact and returns its index in
// out, giving file-scope references (registration-macro arguments) an owner to
// hang edges on — addOwnerEdge drops edges when there is no owner, and file-scope
// code has no enclosing symbol. Extract folds this fact's relations into the
// canonical per-dir module fact.
func (w *astWalker) moduleOwner() int {
	if w.moduleOwnerIdx >= 0 {
		return w.moduleOwnerIdx
	}
	w.out = append(w.out, facts.Fact{
		Kind:  facts.KindModule,
		Name:  w.dir,
		File:  w.dir,
		Props: map[string]any{"language": w.lang},
	})
	w.moduleOwnerIdx = len(w.out) - 1
	return w.moduleOwnerIdx
}

// handleMacroTypeSpecifier records the function-name arguments of a registration
// macro that a leading qualifier turned into a declaration (e.g.
// `static DEFINE_SIMPLE_DEV_PM_OPS(name, suspend, resume);`). tree-sitter renders
// the macro args as type_identifier nodes under the macro_type_specifier; the
// macro name itself is a plain identifier (the `.name` field), so collecting
// type_identifier leaves yields just the args. resolveFuncPtrRefs keeps only the
// ones that are real functions (the suspend/resume callbacks), dropping the ops
// table name and other non-functions.
func (w *astWalker) handleMacroTypeSpecifier(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	w.pushOwner(w.moduleOwner())
	var args []string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if nameNode != nil && n.StartByte() == nameNode.StartByte() && n.EndByte() == nameNode.EndByte() {
			return // the macro name itself, not an argument
		}
		// Args land as type_identifier or (across a line break, under an ERROR node)
		// plain identifier. Non-function args are dropped by funcNames.
		if k := kindOf(w.kinds, n); k == "type_identifier" || k == "identifier" {
			txt := nodeText(n, w.src)
			w.emitFuncPtrRef(txt)
			args = append(args, txt)
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(node)
	// Also expand the macro itself (e.g. a PM-ops macro that wires .suspend = fn),
	// in case its callbacks are pasted rather than passed through directly.
	if nameNode != nil {
		w.emitExpandedMacroRefs(nodeText(nameNode, w.src), args)
	}
	w.popOwner()
}

// expandMacroDeclaration handles `static DEVICE_ATTR_RO(name);` — a declaration
// whose type_identifier is actually a known function-like macro and whose
// declarator is the macro-call argument list (a parenthesized or function
// declarator, not a plain variable name). It expands the macro to record the
// callbacks it wires. Returns true when handled, so the caller skips the normal
// prototype/variable path.
func (w *astWalker) expandMacroDeclaration(typ, declarator *sitter.Node) bool {
	if typ == nil || declarator == nil {
		return false
	}
	def, ok := w.macros[nodeText(typ, w.src)]
	if !ok || def.params == nil { // must be a known function-like macro
		return false
	}
	if k := kindOf(w.kinds, declarator); k != "parenthesized_declarator" && k != "function_declarator" {
		return false // a plain `static MacroTyped var;` is not a macro invocation
	}
	var args []string
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if k := kindOf(w.kinds, n); k == "identifier" || k == "type_identifier" {
			args = append(args, nodeText(n, w.src))
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(declarator)
	w.pushOwner(w.moduleOwner())
	w.emitExpandedMacroRefs(nodeText(typ, w.src), args)
	w.popOwner()
	return true
}

// salvageMacroStructAssigns recovers function-pointer references from a
// macro-opened struct initializer — `DT_MACHINE_START(...) .init_machine = fn, ...
// MACHINE_END`. The hidden opening brace makes tree-sitter fail to parse the block;
// depending on the surrounding code it renders the `.field = fn` lines as a chain
// of `assignment_expression`s with a `field_expression` left side, scattered under
// an ERROR node, a bare top-level expression, or a neighbouring declaration. Rather
// than chase every recovery shape, walk the whole tree (skipping function bodies,
// whose assignments walkForCalls already handles) and capture the RHS of every
// `<field> = fn` assignment on the module owner. resolveFuncPtrRefs keeps only the
// real functions, so this is generic (no hard-coded macro names) and safe.
func (w *astWalker) salvageMacroStructAssigns(root *sitter.Node) {
	var assigns []*sitter.Node
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil || kindOf(w.kinds, n) == "function_definition" {
			return
		}
		if kindOf(w.kinds, n) == "assignment_expression" {
			if l := n.ChildByFieldName("left"); l != nil && kindOf(w.kinds, l) == "field_expression" {
				assigns = append(assigns, n)
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	if len(assigns) == 0 {
		return
	}
	w.pushOwner(w.moduleOwner())
	for _, a := range assigns {
		w.emitAssignFuncPtrRef(a)
	}
	w.popOwner()
}

// handleMacroBodyCalls scans a #define replacement list (preproc_def /
// preproc_function_def) for identifiers used as functions — in call position
// (`IDENT(`) or value position (`= IDENT` / `.field = IDENT` / `= &IDENT`) — and
// records each as a provisional func-pointer reference on the module owner.
// tree-sitter keeps the replacement list as one opaque preproc_arg token, so both
// calls inside a macro body (the size-dispatched `____cmpxchg_u16(...)` in an arch
// cmpxchg header) and ops tables defined via a macro (`#define F7188X_GPIO_BANK(..)
// { .set = f7188x_gpio_set, ... }`) are otherwise invisible. resolveFuncPtrRefs
// keeps only the names that are real functions, so macro params, constants, field
// names, and other macros add no edge.
func (w *astWalker) handleMacroBodyCalls(node *sitter.Node) {
	val := node.ChildByFieldName("value")
	if val == nil || kindOf(w.kinds, val) != "preproc_arg" {
		return
	}
	body := w.src[val.StartByte():val.EndByte()]
	w.pushOwner(w.moduleOwner())
	for _, name := range macroFuncRefIdents(body) {
		w.emitFuncPtrRef(name)
	}
	w.popOwner()
}

// handleDetachedBody credits the calls inside a bare top-level compound_statement
// — the detached body of a function defined by a name-carrying macro that
// tree-sitter cannot attach to a function_definition (SYSCALL_DEFINE2(name, ...)
// { ... }, COMPAT_SYSCALL_DEFINE*, …). We deliberately do NOT emit a symbol for the
// macro-defined handler: a syscall handler is dispatched through the syscall table,
// never called by C name, so a symbol for it would itself be a false dead-code
// orphan. We only credit the body's calls, so a static helper reached only from the
// handler is no longer mis-reported as dead. Same mechanism as handleMacroBodyCalls
// (scan the text for function-position identifiers, hang them off the module owner);
// resolveFuncPtrRefs keeps only the names that are real functions, so keywords,
// macros, and locals add no edge. -> cache.go v116
func (w *astWalker) handleDetachedBody(node *sitter.Node) {
	body := w.src[node.StartByte():node.EndByte()]
	w.pushOwner(w.moduleOwner())
	for _, name := range macroFuncRefIdents(body) {
		w.emitFuncPtrRef(name)
	}
	w.popOwner()
}

// macroFuncRefIdents returns the identifiers in src that are used as a function:
// in call position (immediately followed, modulo whitespace, by '(') or in value
// position (immediately preceded by a plain `=`, optionally through one `&`). The
// latter catches function pointers assigned in designated initializers inside a
// macro body (`.set = fn`). Comparison/compound operators (`==`, `!=`, `+=`, …)
// are not treated as assignment.
func macroFuncRefIdents(src []byte) []string {
	var out []string
	i := 0
	n := len(src)
	for i < n {
		if isIdentStart(src[i]) {
			start := i
			for i < n && isIdentPart(src[i]) {
				i++
			}
			if followedByCallParen(src, i) || precededByAssign(src, start) {
				out = append(out, string(src[start:i]))
			}
			continue
		}
		i++
	}
	return out
}

func isMacroSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\\'
}

// followedByCallParen reports whether the first non-space byte at or after pos is
// '(' — i.e. the identifier ending at pos is in call position.
func followedByCallParen(src []byte, pos int) bool {
	j := pos
	for j < len(src) && isMacroSpace(src[j]) {
		j++
	}
	return j < len(src) && src[j] == '('
}

// precededByAssign reports whether the identifier starting at pos is the value of
// a plain `=` assignment — `= ident`, `.field = ident`, or `= &ident` — and not the
// operand of a comparison/compound operator (`==`, `!=`, `<=`, `>=`, `+=`, …).
func precededByAssign(src []byte, pos int) bool {
	k := pos - 1
	for k >= 0 && isMacroSpace(src[k]) {
		k--
	}
	if k >= 0 && src[k] == '&' { // address-of: = &fn
		k--
		for k >= 0 && isMacroSpace(src[k]) {
			k--
		}
	}
	if k < 0 || src[k] != '=' {
		return false
	}
	if k-1 >= 0 {
		switch src[k-1] { // reject ==, !=, <=, >=, +=, -=, *=, /=, %=, &=, |=, ^=, ~=
		case '=', '!', '<', '>', '+', '-', '*', '/', '%', '&', '|', '^', '~':
			return false
		}
	}
	return true
}

func isIdentStart(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9')
}

// handleFileScopeMacroCall records provisional func-pointer references for the
// bare identifier / &identifier arguments of a file-scope macro invocation, hung
// off the module owner. resolveFuncPtrRefs later keeps only the arguments whose
// short name is a real function, so non-function arguments (a DEVICE_ATTR name, a
// mode literal) add no edge.
func (w *astWalker) handleFileScopeMacroCall(node *sitter.Node) {
	call := findChildByKind(w.kinds, node, "call_expression")
	if call == nil {
		return
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return
	}
	w.pushOwner(w.moduleOwner())
	w.emitArgRefs(args)
	if fn := call.ChildByFieldName("function"); fn != nil && kindOf(w.kinds, fn) == "identifier" {
		w.emitExpandedMacroRefs(nodeText(fn, w.src), argTexts(args, w.src))
	}
	w.popOwner()
}

// argTexts returns the source text of each named (top-level) argument of an
// argument_list — used to feed a macro invocation's args to the expander.
func argTexts(args *sitter.Node, src []byte) []string {
	var out []string
	for i := uint(0); i < args.ChildCount(); i++ {
		if a := args.Child(i); a.IsNamed() {
			out = append(out, nodeText(a, src))
		}
	}
	return out
}

// emitExpandedMacroRefs expands the macro invocation name(argTexts...) using the
// project #define table and records the function references in its expansion —
// recovering token-pasted callbacks like CONFIGFS_ATTR(pfx, name) -> .show =
// pfx##name##_show. No-op when the macro is unknown or object-like. Must be called
// with an owner pushed; resolveFuncPtrRefs drops any non-function name.
func (w *astWalker) emitExpandedMacroRefs(name string, args []string) {
	if w.macros == nil {
		return
	}
	toks := expandCall(name, args, w.macros)
	if toks == nil {
		return
	}
	// Scan every identifier in the expanded text: a referenced function may appear
	// in any position (a designated-init value, a call, or a call argument like
	// single_open(file, name_show, ...) from DEFINE_SHOW_ATTRIBUTE). funcNames drops
	// the non-functions.
	for _, ref := range allIdents([]byte(tokensText(toks))) {
		w.emitFuncPtrRef(ref)
	}
}

// emitArgRefs scans the DIRECT arguments of a call for bare function names used as
// function pointers — `register(foo)`, `request_irq(irq, &handler, ...)` — and
// attaches a provisional func-pointer edge for each. Only top-level `identifier`
// and `&identifier` arguments are considered; complex argument expressions
// (`x->field`, `a + b`, nested calls) are left to the normal call walk, so we do
// not over-emit on ordinary data arguments.
func (w *astWalker) emitArgRefs(args *sitter.Node) {
	if args == nil {
		return
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		a := args.Child(i)
		if !a.IsNamed() {
			continue
		}
		switch kindOf(w.kinds, a) {
		case "identifier":
			w.emitFuncPtrRef(nodeText(a, w.src))
		case "pointer_expression": // &func
			if inner := a.ChildByFieldName("argument"); inner != nil && kindOf(w.kinds, inner) == "identifier" {
				w.emitFuncPtrRef(nodeText(inner, w.src))
			}
		}
	}
}

// emitFuncPtrRef attaches a PROVISIONAL func-pointer reference (relFuncPtrCandidate)
// for an identifier used as a function pointer — in an initializer value or a call
// argument. It suppresses builtins (via resolveCall) and UPPER_SNAKE macro/constant
// names. The edge is only kept (and rewritten to RelCalls) by resolveFuncPtrRefs in
// Extract when the target's short name is an actual function in the snapshot, so
// ordinary data identifiers never produce a spurious reference.
func (w *astWalker) emitFuncPtrRef(name string) {
	if name == "" || isAllCapsConst(name) {
		return
	}
	if target := w.resolveCall(name); target != "" {
		w.addOwnerEdge(relFuncPtrCandidate, target)
	}
}

// isAllCapsConst reports whether s is an UPPER_SNAKE macro/constant name, which by
// C/C++ convention is never a function used as a pointer.
func isAllCapsConst(s string) bool {
	hasLetter := false
	for _, r := range s {
		switch {
		case unicode.IsUpper(r):
			hasLetter = true
		case unicode.IsDigit(r) || r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

func (w *astWalker) handleTypedef(node *sitter.Node) {
	leaf := declaratorLeafName(w.kinds, node.ChildByFieldName("declarator"), w.src)
	if leaf == "" {
		return
	}
	w.emitTypeAlias(leaf, node)
}

func (w *astWalker) handleUsing(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}
	w.emitTypeAlias(name, node)
}

func (w *astWalker) emitTypeAlias(name string, node *sitter.Node) {
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.factName(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolType,
			"exported":    true,
			"language":    w.lang,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

func (w *astWalker) handleInclude(node *sitter.Node) {
	pathNode := node.ChildByFieldName("path")
	if pathNode == nil {
		return
	}
	style := ""
	switch kindOf(w.kinds, pathNode) {
	case "string_literal":
		style = "quoted"
	case "system_lib_string":
		// ClickHouse and many CMake projects use <Project/header.h> for their own
		// headers. Emit it provisionally; the project-wide header index later keeps
		// repository-owned paths and drops actual system headers such as <vector>.
		style = "angle"
	default:
		return
	}
	inc := strings.Trim(nodeText(pathNode, w.src), `"<>`)
	if inc == "" {
		return
	}
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindDependency,
		Name: w.dir + " -> " + inc,
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"language":      w.lang,
			"include":       inc,
			"include_style": style,
			"source":        "internal",
		},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: inc}},
	})
}

// applyFuncQualifiers tags a function/method fact with static/virtual/const flags
// read from its signature text.
func (w *astWalker) applyFuncQualifiers(f *facts.Fact, node, fdecl *sitter.Node) {
	header := w.signatureText(node)
	if strings.Contains(header, "static") {
		f.SetProp("static", true)
		// In C a `static` function has internal linkage — it is file-private, not
		// externally visible. (C++ keeps exported=true: file scope there is rare and
		// visibility is expressed via access specifiers / anonymous namespaces.)
		if w.lang == langC {
			f.SetProp("exported", false)
		}
	}
	if strings.Contains(header, "virtual") {
		f.SetProp("virtual", true)
	}
	// Trailing const: a type_qualifier child after the parameter_list.
	if fdecl != nil {
		params := fdecl.ChildByFieldName("parameters")
		for i := uint(0); i < fdecl.ChildCount(); i++ {
			c := fdecl.Child(i)
			if kindOf(w.kinds, c) == "type_qualifier" && params != nil && c.StartByte() >= params.EndByte() {
				if nodeText(c, w.src) == "const" {
					f.SetProp("const", true)
				}
			}
		}
	}
}

// signatureText returns the source from the declaration start up to the body
// (or the whole node when there is no body).
func (w *astWalker) signatureText(node *sitter.Node) string {
	end := node.EndByte()
	if body := node.ChildByFieldName("body"); body != nil {
		end = body.StartByte()
	}
	return string(w.src[node.StartByte():end])
}

// --- call graph ---

// walkForCalls recursively scans a function body for call_expression nodes and
// attaches edges to the current owner (mirrors the Swift walker):
//   - Type(...)        -> RelInstantiates (constructor)
//   - bare lower call  -> RelCalls (resolved via resolveCall)
//   - Ns::fn / T::m    -> RelCalls to "scope::name" (canonicalised later)
//   - this->m()        -> RelCalls to the enclosing type's method
func (w *astWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	kind := kindOf(w.kinds, node)

	// A lambda is a deferred scope: its body runs when invoked, NOT per-iteration of
	// the enclosing loops — so reset the loop depth for its subtree (e.g. a callback
	// defined inside a loop). An STL iterator's OWN lambda is handled in the
	// call_expression branch (its body walks at +1).
	if w.metrics != nil && kind == "lambda_expression" {
		saved, savedScaling, savedRepeat := w.loopDepth, w.scalingDepth, w.repeatDepth
		w.loopDepth, w.scalingDepth, w.repeatDepth = 0, 0, 0
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i))
		}
		w.loopDepth, w.scalingDepth, w.repeatDepth = saved, savedScaling, savedRepeat
		return
	}

	// Complexity decision points: the single body walk doubles as the cyclomatic pass.
	if w.metrics != nil {
		switch kind {
		case "if_statement", "conditional_expression", "case_statement", "catch_clause":
			w.metrics.decisions++
		case "binary_expression":
			if cppBooleanOp(w.kinds, node) {
				w.metrics.decisions++
			}
		}
	}

	switch kind {
	case "for_statement", "while_statement", "do_statement", "for_range_loop":
		// Syntactic loops: everything in the body runs per iteration. A constant-count
		// loop raises loop_depth but not scaling_loop_depth (the Big-O exponent); an
		// infinite loop is discounted from the exponent but still repeats.
		class := cppSyntacticLoopClass(w.kinds, node, w.src)
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
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i))
		}
		w.loopDepth--
		if class.scales() {
			w.scalingDepth--
		}
		if class.repeats() {
			w.repeatDepth--
		}
		return
	case "call_expression":
		w.handleCall(node)
		// An STL algorithm with a lambda (std::for_each(b, e, [&]{ … })) is a loop:
		// its lambda body runs per element, but the receiver/other args run once.
		if w.metrics != nil {
			if lambda := w.cppStlLambda(node); lambda != nil {
				// An STL algorithm iterates its [begin, end) container, so it scales with
				// the input — count it toward scaling_loop_depth too.
				w.metrics.loopCount++
				w.metrics.decisions++
				if w.loopDepth+1 > w.metrics.loopDepth {
					w.metrics.loopDepth = w.loopDepth + 1
				}
				if w.scalingDepth+1 > w.metrics.scalingLoopDepth {
					w.metrics.scalingLoopDepth = w.scalingDepth + 1
				}
				for i := uint(0); i < node.ChildCount(); i++ {
					if c := node.Child(i); cppByteContains(c, lambda) {
						w.walkCppLambdaSubtree(c, lambda)
					} else {
						w.walkForCalls(c)
					}
				}
				return
			}
		}
	case "assignment_expression":
		// `obj->cb = func;` / `obj.cb = func;` / `p = &func;` — wiring a function
		// into a struct callback field (kernel gpio_chip/irq_chip/pmu_ops probes) or
		// a function-pointer variable. The RHS function is a use; without this it
		// looks dead. Non-function RHS is dropped by resolveFuncPtrRefs.
		w.emitAssignFuncPtrRef(node)
	case "compound_literal_expression":
		// An in-body designated initializer — `cfg = (struct regmap_config){ .lock =
		// fn, ... };` — wires callbacks like a file-scope ops table but inside a
		// function. Reuse the initializer walk (handles .field = fn, &fn, nesting);
		// the enclosing function is the owner.
		w.walkInitializerRefs(node)
	}

	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkForCalls(node.Child(i))
	}
}

// emitAssignFuncPtrRef records a provisional func-pointer reference for the RHS of
// a plain `=` assignment whose value is a bare function name or `&func` — the
// callback-wiring pattern `obj->field = func`. Compound assignments (`+=`) and
// complex RHS expressions are left alone; the funcNames filter in
// resolveFuncPtrRefs drops any RHS that is not a real function.
func (w *astWalker) emitAssignFuncPtrRef(node *sitter.Node) {
	if op := node.ChildByFieldName("operator"); op == nil || nodeText(op, w.src) != "=" {
		return
	}
	rhs := node.ChildByFieldName("right")
	if rhs == nil {
		return
	}
	switch kindOf(w.kinds, rhs) {
	case "identifier":
		w.emitFuncPtrRef(nodeText(rhs, w.src))
	case "pointer_expression": // &func
		if inner := rhs.ChildByFieldName("argument"); inner != nil && kindOf(w.kinds, inner) == "identifier" {
			w.emitFuncPtrRef(nodeText(inner, w.src))
		}
	}
}

// handleCall emits the call-graph edge for a call_expression and feeds the current
// function's complexity metrics (recursion + calls_in_loop).
func (w *astWalker) handleCall(node *sitter.Node) {
	// Functions passed as arguments (callbacks) are references too:
	// register(foo), request_irq(irq, &handler, ...). Resolved against the
	// project function index in Extract so non-function args are dropped.
	w.emitArgRefs(node.ChildByFieldName("arguments"))

	callee := node.ChildByFieldName("function")
	name, kind, root := calleeInfo(w.kinds, callee, w.src)
	// io_direct: a direct file/socket data-transfer primitive called as a free or
	// namespaced function (fopen/fread/… , not an obj->method()). Kept deliberately
	// narrow — console/logging primitives (fprintf/fputs) and the ambiguous socket
	// verbs (bind/connect/send) are excluded to avoid mass false positives.
	if kind != calleeField && w.metrics != nil && cppIODirect[name] {
		w.metrics.ioDirect = true
	}
	switch {
	case name == "":
		// unresolved (call through pointer, etc.)
	case kind == calleeQualified:
		if !systemNamespaces[root] {
			target := root + "::" + name
			w.addOwnerEdge(facts.RelCalls, target)
			w.recordCallMetrics(target)
		}
	case kind == calleePlain:
		if isTypeName(name) {
			if w.lang == langC {
				// C has no constructors: a capitalized callee is either a function
				// (e.g. an ALL-CAPS `static inline` like NE_PTR/STNIC_READ) or a
				// value-macro (ARRAY_SIZE, BIT). Emit a provisional func-pointer edge
				// so resolveFuncPtrRefs keeps it only when the name is a real
				// function; value-macros are dropped. (C++ keeps instantiation.)
				if target := w.resolveCall(name); target != "" {
					w.addOwnerEdge(relFuncPtrCandidate, target)
				}
			} else if !cppBuiltinTypes[name] {
				w.addOwnerEdge(facts.RelInstantiates, name)
			}
		} else if target := w.resolveCall(name); target != "" {
			w.addOwnerEdge(facts.RelCalls, target)
			w.recordCallMetrics(target)
		}
	case kind == calleeField && root == "this":
		if w.currentMethods()[name] {
			target := w.factName(name)
			w.addOwnerEdge(facts.RelCalls, target)
			w.recordCallMetrics(target)
		}
	case kind == calleeField:
		// Resolve only a simple receiver with one explicit, unambiguous parameter or
		// local type. Project-wide canonicalization later verifies that the type and
		// method actually exist; auto, templates, fields and expressions stay unknown.
		if typ := w.receiverTypes[root]; typ != "" {
			target := typ + "::" + name
			w.addOwnerEdge(relReceiverCallCandidate, target)
			w.recordCallMetrics(target)
			break
		}
		// Unknown receiver calls still feed the in-loop metric. Skip obviously cheap
		// container/iterator methods to keep calls_in_loop focused.
		if !cppCheapMethods[name] {
			tgt := name
			if root != "" {
				tgt = root + "." + name
			}
			w.recordInLoop(tgt)
		}
	}
}

// collectExplicitReceiverTypes finds parameter and local declarations whose type
// is a simple project class/struct name. It intentionally ignores auto, builtins,
// template wrappers (a unique_ptr<T> receiver is not a T), nested lambdas and any
// variable declared with conflicting types in the same function.
func collectExplicitReceiverTypes(kinds *tsutil.KindTable, lang string, fdecl, body *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	addDeclaration := func(node *sitter.Node) {
		typeNode := node.ChildByFieldName("type")
		typ := simpleTypeName(typeNode, src)
		if typ == "" || !isTypeName(typ) || (typeNode != nil && strings.Contains(nodeText(typeNode, src), "<")) {
			return
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			if node.FieldNameForChild(uint32(i)) != "declarator" {
				continue
			}
			name := declaratorLeafName(kinds, node.Child(i), src)
			if name == "" {
				continue
			}
			if prior, exists := out[name]; !exists {
				out[name] = typ
			} else if prior != typ {
				out[name] = ""
			}
		}
	}
	if params := fdecl.ChildByFieldName("parameters"); params != nil {
		for i := uint(0); i < params.ChildCount(); i++ {
			if p := params.Child(i); kindOf(kinds, p) == "parameter_declaration" {
				addDeclaration(p)
			}
		}
	}
	// The declarations are found with a tree-sitter QUERY rather than by walking the
	// body in Go. The walk visited every node in the function to find the few that
	// are declarations, and go-tree-sitter allocates a Go object for each one it
	// hands back — `Node.Child` and `TreeCursor.Node` both return `&Node{...}`, so
	// there is no traversal idiom that avoids it. Matching runs in C and only the
	// matches cross the boundary: on a 92 KB source file that is 845 allocations
	// instead of 32,150, and 3.2x faster.
	for _, decl := range queryDeclarations(lang, body) {
		if declaredInsideLambda(kinds, decl, body) {
			continue // a lambda has its own scope; the walk this replaced skipped them
		}
		addDeclaration(decl)
	}
	return out
}

// declaredInsideLambda reports whether node sits under a lambda between it and
// stop. The query cannot express "not under a lambda", so the exclusion the walk
// made by refusing to descend is made here instead — over matches rather than over
// every node, which is why it stays cheap.
func declaredInsideLambda(kinds *tsutil.KindTable, node, stop *sitter.Node) bool {
	for p := node.Parent(); p != nil && p.Id() != stop.Id(); p = p.Parent() {
		if kindOf(kinds, p) == "lambda_expression" {
			return true
		}
	}
	return false
}

// cppStlLambda returns the lambda argument of an STL-iterator call
// (std::for_each(b, e, [&]{…})), or nil if the call is not an iterator with a lambda.
func (w *astWalker) cppStlLambda(call *sitter.Node) *sitter.Node {
	name, _, _ := calleeInfo(w.kinds, call.ChildByFieldName("function"), w.src)
	if name == "" || !cppStlIterators[name] {
		return nil
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i := uint(0); i < args.ChildCount(); i++ {
		if c := args.Child(i); kindOf(w.kinds, c) == "lambda_expression" {
			return c
		}
	}
	return nil
}

// walkCppLambdaSubtree descends to an STL iterator's lambda and walks its BODY at +1
// (it runs per element), while walking everything else (other args) at the current
// depth. Kind-checked so an ancestor with the same byte span isn't mistaken for the
// lambda.
func (w *astWalker) walkCppLambdaSubtree(node, lambda *sitter.Node) {
	if node == nil {
		return
	}
	if kindOf(w.kinds, node) == "lambda_expression" && node.StartByte() == lambda.StartByte() && node.EndByte() == lambda.EndByte() {
		// The STL algorithm invokes this lambda per element and scales with the
		// container, so bump the scaling and repeat depths alongside loop_depth.
		w.loopDepth++
		w.scalingDepth++
		w.repeatDepth++
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i))
		}
		w.loopDepth--
		w.scalingDepth--
		w.repeatDepth--
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if c := node.Child(i); cppByteContains(c, lambda) {
			w.walkCppLambdaSubtree(c, lambda)
		} else {
			w.walkForCalls(c)
		}
	}
}

// resolveCall maps a bare (unqualified) call name to a canonical symbol fact name:
// a same-type method, a suppressed builtin, or a same-scope free function.
func (w *astWalker) resolveCall(name string) string {
	if w.currentMethods()[name] {
		return w.factName(name)
	}
	if cppBuiltins[name] {
		return ""
	}
	return w.dir + "." + name
}

type calleeKind int

const (
	calleeNone calleeKind = iota
	calleePlain
	calleeQualified
	calleeField
)

// calleeInfo inspects a call_expression's callee node and returns the called leaf
// name, its kind, and (for qualified/field calls) the relevant root: the immediate
// scope for "A::B::f" or the receiver identifier ("this" for this->m()).
func calleeInfo(kinds *tsutil.KindTable, callee *sitter.Node, src []byte) (name string, kind calleeKind, root string) {
	if callee == nil {
		return "", calleeNone, ""
	}
	switch kindOf(kinds, callee) {
	case "identifier":
		return nodeText(callee, src), calleePlain, ""
	case "qualified_identifier":
		scopes, leaf := splitQualified(kinds, callee, src)
		if leaf == "" || len(scopes) == 0 {
			return "", calleeNone, ""
		}
		return leaf, calleeQualified, scopes[len(scopes)-1]
	case "field_expression":
		field := callee.ChildByFieldName("field")
		arg := callee.ChildByFieldName("argument")
		if field == nil {
			return "", calleeNone, ""
		}
		r := ""
		if arg != nil && kindOf(kinds, arg) == "this" {
			r = "this"
		} else if arg != nil {
			r = nodeText(arg, src)
		}
		return identifierLeaf(kinds, field, src), calleeField, r
	case "template_function":
		// foo<T>(...) — the name is under the "name" field.
		if n := callee.ChildByFieldName("name"); n != nil {
			return identifierLeaf(kinds, n, src), calleePlain, ""
		}
	}
	return "", calleeNone, ""
}

// --- node helpers ---

// findFunctionDeclarator descends through pointer/reference/array/parenthesized
// declarator wrappers to the function_declarator, or returns nil.
func findFunctionDeclarator(kinds *tsutil.KindTable, node *sitter.Node) *sitter.Node {
	for node != nil {
		switch kindOf(kinds, node) {
		case "function_declarator":
			return node
		case "pointer_declarator", "reference_declarator", "array_declarator",
			"parenthesized_declarator", "init_declarator":
			node = node.ChildByFieldName("declarator")
			if node == nil {
				return nil
			}
		default:
			return nil
		}
	}
	return nil
}

// declaratorLeafName descends a declarator subtree to its leaf name (identifier,
// field_identifier, qualified_identifier text, operator_name or destructor_name).
func declaratorLeafName(kinds *tsutil.KindTable, node *sitter.Node, src []byte) string {
	for node != nil {
		switch kindOf(kinds, node) {
		case "identifier", "field_identifier", "type_identifier":
			return nodeText(node, src)
		case "operator_name", "destructor_name":
			return strings.Join(strings.Fields(nodeText(node, src)), " ")
		case "qualified_identifier":
			scopes, leaf := splitQualified(kinds, node, src)
			if leaf == "" {
				return ""
			}
			return strings.Join(append(scopes, leaf), "::")
		case "function_declarator", "pointer_declarator", "reference_declarator",
			"array_declarator", "init_declarator":
			node = node.ChildByFieldName("declarator")
		case "parenthesized_declarator":
			// A function-pointer declarator nests the name in an unnamed-field
			// child, e.g. "(*g_cb)(void)" → parenthesized_declarator → pointer_
			// declarator → identifier. Fall back to the first named child.
			if d := node.ChildByFieldName("declarator"); d != nil {
				node = d
			} else {
				node = firstNamedChild(node)
			}
		default:
			node = firstNamedChild(node)
		}
	}
	return ""
}

// identifierLeaf returns the simple identifier text of a (possibly qualified or
// templated) name node.
func identifierLeaf(kinds *tsutil.KindTable, node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch kindOf(kinds, node) {
	case "identifier", "field_identifier", "type_identifier":
		return nodeText(node, src)
	case "qualified_identifier":
		_, leaf := splitQualified(kinds, node, src)
		return leaf
	case "template_method", "template_function", "template_type":
		if n := node.ChildByFieldName("name"); n != nil {
			return identifierLeaf(kinds, n, src)
		}
	}
	if id := findFirstIdentifier(kinds, node, src); id != nil {
		return nodeText(id, src)
	}
	return ""
}

// splitQualified descends the "name" chain of a qualified_identifier, returning
// the ordered scope components and the final leaf name. For "A::B::method" it
// returns (["A","B"], "method").
func splitQualified(kinds *tsutil.KindTable, node *sitter.Node, src []byte) (scopes []string, leaf string) {
	cur := node
	for cur != nil && kindOf(kinds, cur) == "qualified_identifier" {
		if s := cur.ChildByFieldName("scope"); s != nil {
			scopes = append(scopes, nodeText(s, src))
		}
		cur = cur.ChildByFieldName("name")
	}
	if cur == nil {
		return scopes, ""
	}
	switch kindOf(kinds, cur) {
	case "operator_name", "destructor_name":
		return scopes, strings.Join(strings.Fields(nodeText(cur, src)), " ")
	case "template_type", "template_function", "template_method":
		if n := cur.ChildByFieldName("name"); n != nil {
			return scopes, nodeText(n, src)
		}
	}
	return scopes, nodeText(cur, src)
}

// baseClassNames returns the simple base-class names from a class/struct's
// base_class_clause (access specifiers and virtual keywords are anonymous tokens
// and are skipped; template arguments and namespaces are stripped).
func baseClassNames(kinds *tsutil.KindTable, node *sitter.Node, src []byte) []string {
	bc := findChildByKind(kinds, node, "base_class_clause")
	if bc == nil {
		return nil
	}
	var names []string
	for i := uint(0); i < bc.ChildCount(); i++ {
		c := bc.Child(i)
		switch kindOf(kinds, c) {
		case "type_identifier", "qualified_identifier", "template_type", "qualified_type":
			if name := simpleTypeName(c, src); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// collectMethodNames returns the set of method names declared directly in a class
// body (from both inline definitions and prototypes), used to resolve same-type
// bare/this calls.
func collectMethodNames(kinds *tsutil.KindTable, body *sitter.Node, src []byte) map[string]bool {
	methods := make(map[string]bool)
	if body == nil {
		return methods
	}
	for i := uint(0); i < body.ChildCount(); i++ {
		c := body.Child(i)
		var fdecl *sitter.Node
		switch kindOf(kinds, c) {
		case "function_definition", "field_declaration":
			fdecl = findFunctionDeclarator(kinds, c.ChildByFieldName("declarator"))
		}
		if fdecl == nil {
			continue
		}
		if leaf := declaratorLeafName(kinds, fdecl.ChildByFieldName("declarator"), src); leaf != "" {
			methods[leaf] = true
		}
	}
	return methods
}

// isScopedEnum reports whether an enum_specifier is "enum class" / "enum struct".
func isScopedEnum(node *sitter.Node, src []byte) bool {
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if c.IsNamed() {
			continue
		}
		switch nodeText(c, src) {
		case "class", "struct":
			return true
		}
	}
	return false
}

// declTypePrefix returns the type-field text of a declaration/field_declaration
// (used to detect const data members).
func declTypePrefix(node *sitter.Node, src []byte) string {
	if t := node.ChildByFieldName("type"); t != nil {
		// Include any leading type_qualifier siblings (const).
		return string(src[node.StartByte():t.EndByte()])
	}
	return ""
}

// simpleTypeName extracts the simple (rightmost, generics-stripped) type name from
// a type node.
func simpleTypeName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	s := nodeText(node, src)
	// Strip template arguments.
	if idx := strings.IndexByte(s, '<'); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	// Keep the rightmost "::" component.
	if idx := strings.LastIndex(s, "::"); idx >= 0 {
		s = s[idx+2:]
	}
	return strings.TrimSpace(s)
}

func findChildByKind(kinds *tsutil.KindTable, node *sitter.Node, kind string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if c := node.Child(i); kindOf(kinds, c) == kind {
			return c
		}
	}
	return nil
}

func firstNamedChild(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if c := node.Child(i); c.IsNamed() {
			return c
		}
	}
	return nil
}

func findFirstIdentifier(kinds *tsutil.KindTable, node *sitter.Node, src []byte) *sitter.Node {
	if node == nil {
		return nil
	}
	switch kindOf(kinds, node) {
	case "identifier", "field_identifier", "type_identifier":
		return node
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if !c.IsNamed() {
			continue
		}
		if found := findFirstIdentifier(kinds, c, src); found != nil {
			return found
		}
	}
	return nil
}

func nodeText(node *sitter.Node, src []byte) string { return tsutil.Text(node, src) }

func isCapitalized(s string) bool {
	if s == "" {
		return false
	}
	return unicode.IsUpper([]rune(s)[0])
}

// isTypeName reports whether s looks like a constructor/type name (capitalized
// identifier), filtering out junk before it becomes an instantiation edge.
func isTypeName(s string) bool {
	if !isCapitalized(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

// systemNamespaces are scope roots whose qualified calls are external and should
// not become RelCalls edges.
var systemNamespaces = map[string]bool{
	"std": true, "__gnu_cxx": true, "detail": true,
}

// cppBuiltins are global/standard-library functions that appear as bare calls.
// Resolving them would create dangling phantom edges, so they are suppressed.
var cppBuiltins = map[string]bool{
	"printf": true, "fprintf": true, "sprintf": true, "snprintf": true, "scanf": true,
	"malloc": true, "calloc": true, "realloc": true, "free": true,
	"memcpy": true, "memmove": true, "memset": true, "memcmp": true,
	"strlen": true, "strcmp": true, "strncmp": true, "strcpy": true, "strncpy": true,
	"strcat": true, "strstr": true, "strchr": true, "atoi": true, "atof": true,
	"abs": true, "fabs": true, "min": true, "max": true, "sqrt": true, "pow": true,
	"sin": true, "cos": true, "tan": true, "exp": true, "log": true, "floor": true, "ceil": true,
	"assert": true, "exit": true, "abort": true, "move": true, "forward": true,
	"sizeof": true, "static_cast": true, "dynamic_cast": true, "reinterpret_cast": true,
	"const_cast": true, "make_pair": true, "make_shared": true, "make_unique": true,
}

// cppBuiltinTypes are capitalized names that look like constructors but are
// builtins/macros, so they should not become RelInstantiates edges.
var cppBuiltinTypes = map[string]bool{
	"T": true, "U": true, "K": true, "V": true,
}
