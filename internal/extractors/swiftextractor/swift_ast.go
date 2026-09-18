package swiftextractor

import (
	"strings"
	"unicode"

	swift "github.com/enola-labs/enola/internal/extractors/swiftextractor/grammar"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// extractFileAST parses a Swift file with tree-sitter and emits architectural
// facts. It is a superset of the legacy regex extractor: every declaration /
// import / iOS-classification fact is preserved, and call-graph relations
// (RelCalls / RelInstantiates / RelInjects) plus View->ViewModel RelDependsOn
// edges are attached to symbol facts when call sites, initializer parameters or
// dependency-injection property wrappers are observed.
//
// Edge targets are emitted as bare simple type names here (e.g. "AppComposition")
// and canonicalised to "<dir>.<Type>" by the post-pass in Extract, which has the
// project-wide type index needed to resolve them.
func extractFileAST(src []byte, relFile string, isiOS bool) []facts.Fact {
	return extractFileASTWithDir(src, relFile, isiOS, factpath.Dir(relFile))
}

// extractFileASTWithDir is extractFileAST with an explicit module identity dir.
// Extract passes the file's resolved target module (from moduleResolver) so
// symbols are named "<targetDir>.<Type>" and declare into the target module
// rather than the file's leaf directory.
func extractFileASTWithDir(src []byte, relFile string, isiOS bool, dir string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(swift.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	w := &astWalker{
		src:        src,
		relFile:    relFile,
		dir:        dir,
		isiOS:      isiOS,
		fileRefIdx: -1,
	}
	w.walkSourceFile(tree.RootNode())
	return w.out
}

type astWalker struct {
	src     []byte
	relFile string
	dir     string
	isiOS   bool

	// out is the accumulating fact list.
	out []facts.Fact

	// ownerStack holds INDICES into out (not pointers) of the symbol fact whose
	// body is currently being walked. Indices stay valid across append/realloc,
	// which pointers would not. New call/instantiate/inject edges append to
	// out[currentOwner()].Relations. -1 means no owner (top level / extension).
	ownerStack []int

	// typeStack holds the simple names of the enclosing class/struct/.../extension
	// declarations, so members are named "<dir>.<Type>.<member>" (parity with the
	// Go/Kotlin extractors). methodStack is parallel and holds the set of method
	// names declared directly in each enclosing type, used to resolve same-type
	// bare/self calls to "<dir>.<Type>.<method>".
	typeStack   []string
	methodStack []map[string]bool

	// Per-function complexity state, set up by handleFunction around walkForCalls.
	// metrics is nil outside a function body walk. loopDepth is the current loop
	// nesting depth; selfName/selfShort are the enclosing function's full and short
	// names, and selfParamLabels its external argument labels — both used for
	// direct-recursion detection (a self-named call is only recursion when its
	// argument labels match, distinguishing genuine recursion from a call to a
	// sibling/stdlib/super overload of the same bare name).
	metrics         *swiftBodyMetrics
	loopDepth       int
	selfName        string
	selfShort       string
	selfParamLabels []string

	// fileRefIdx is the index into out of this file's lazily-created file-scope
	// reference fact (facts.KindFileRef), or -1 before one is created. It owns the
	// call edges from top-level statements (bare `foo()` calls, `let x = foo()`
	// initializers) that have no enclosing symbol — e.g. #!/usr/bin/swift build
	// scripts that invoke their own top-level functions. Mirrors the Ruby extractor.
	fileRefIdx int

	// condDepth is the current #if/#elseif/#else conditional-compilation nesting
	// depth. Tree-sitter walks BOTH branches of a #if/#else block (it does not
	// evaluate the compile-time condition), so a type declared once per branch
	// yields two same-name symbol facts. Symbols emitted while condDepth > 0 are
	// tagged conditional=true so consumers can group/dedupe them (GAP-SW-10). The
	// grammar emits directives as flat sibling `directive` nodes, not containers,
	// so depth is tracked by the ordered walk loops (walkSourceFile, walkTypeBody).
	condDepth int
}

// swiftBodyMetrics accumulates per-function complexity signals during the single
// walkForCalls body traversal — mirrors the Go/Python/Ruby extractors.
type swiftBodyMetrics struct {
	loopDepth   int             // max loop nesting depth
	loopCount   int             // number of loop constructs (syntactic + iterator closures)
	decisions   int             // decision points (cyclomatic = 1 + decisions)
	callsInLoop []string        // distinct call targets invoked at loop depth >= 1
	inLoopSeen  map[string]bool // dedup set for callsInLoop
	recursive   bool            // body directly calls the enclosing function
	ioDirect    bool            // body directly invokes a network/file I/O primitive
}

// swiftIterators are higher-order methods whose closure runs once per element —
// i.e. a loop. A trailing closure on a method NOT in this set (Task, async,
// withAnimation, a completion handler) runs once and is not treated as a loop.
// Aggregate-or-iterate names (contains/first/min…) are safe to include because a
// closure must be present before any of these counts as a loop.
var swiftIterators = map[string]bool{
	"map": true, "forEach": true, "filter": true, "compactMap": true,
	"flatMap": true, "reduce": true, "sorted": true, "min": true, "max": true,
	"contains": true, "allSatisfy": true, "first": true, "firstIndex": true,
	"last": true, "lastIndex": true, "partition": true, "drop": true,
	"prefix": true, "removeAll": true, "split": true, "reversed": true,
}

// swiftCheapMethods are obviously-cheap methods that are not I/O. No-arg-ish
// instance calls to these inside loops are not recorded in calls_in_loop, keeping
// it focused (the analyzer's keyword gate is the real precision filter).
var swiftCheapMethods = map[string]bool{
	"append": true, "count": true, "isEmpty": true, "first": true, "last": true,
	"contains": true, "map": true, "filter": true, "forEach": true, "compactMap": true,
	"flatMap": true, "reduce": true, "sorted": true, "joined": true, "description": true,
	"hasPrefix": true, "hasSuffix": true, "uppercased": true, "lowercased": true,
	"trimmingCharacters": true, "insert": true, "remove": true, "removeAll": true,
	"keys": true, "values": true, "sorted_": true, "reversed": true, "enumerated": true,
}

// swiftIOPrimitiveMethods are method names that denote a real network / file I/O
// call regardless of receiver — the high-confidence leaves that seed the
// performs_io closure (a method with one of these in its body directly does I/O).
var swiftIOPrimitiveMethods = map[string]bool{
	"dataTask": true, "dataTaskPublisher": true, "downloadTask": true,
	"uploadTask": true, "download": true, "upload": true,
}

// swiftIONetworkRoots are receiver/type tokens whose presence in a call's receiver
// chain marks it as network I/O (URLSession.shared.data(for:), a URLRequest send).
var swiftIONetworkRoots = []string{"URLSession", "URLRequest"}

// isIOPrimitiveCall reports whether a call — decomposed by calleeInfo into name /
// root / isNav — is a high-confidence network or file-read I/O primitive: a known
// I/O method name, a URLSession/URLRequest receiver or construction, an Alamofire
// request/transfer, or a `Data(contentsOf:)` / `String(contentsOf:)` read. Core Data
// (`context.fetch`/`save`) is intentionally NOT here: telling it from an in-memory
// `fetch`/`save` needs the receiver's type, which the extractor cannot yet infer.
func isIOPrimitiveCall(name, root string, isNav bool, call *sitter.Node, src []byte) bool {
	if swiftIOPrimitiveMethods[name] {
		return true
	}
	for _, tok := range swiftIONetworkRoots {
		if name == tok || strings.Contains(root, tok) {
			return true
		}
	}
	// Alamofire: AF.request(...), session.download/upload(...).
	if isNav && (root == "AF" || root == "Session" || root == "session") {
		switch name {
		case "request", "download", "upload", "streamRequest":
			return true
		}
	}
	// Data(contentsOf:) / String(contentsOf:) — a URL/file read (contentsOf is an
	// argument label on a capitalized construction, not the callee name).
	if name == "Data" || name == "String" {
		for _, lbl := range callArgumentLabels(call, src) {
			if lbl == "contentsOf" {
				return true
			}
		}
	}
	return false
}

// swiftChainPreservesBound are collection methods that never grow a collection
// beyond its input size, so a bounded literal/constant piped through them stays
// bounded (`[a,b].sorted().forEach`, `STOP_CHARS.filter { … }.forEach`). Mirrors
// the Ruby extractor's chainPreservesBound. Size-expanding ops (flatMap, joined)
// are excluded so a bounded source is never turned unbounded's opposite.
var swiftChainPreservesBound = map[string]bool{
	"sorted": true, "reversed": true, "map": true, "compactMap": true,
	"filter": true, "prefix": true, "suffix": true, "dropFirst": true,
	"dropLast": true, "shuffled": true, "enumerated": true, "lazy": true,
}

// isScreamingSnake reports whether s is SCREAMING_SNAKE_CASE — only uppercase
// letters, digits, and underscores, with at least one letter (`STOP_CHARS`,
// `MAX_RETRIES`). A data constant with a fixed element count, unlike a mixed-case
// type/relation (`Users`) whose `.forEach` is not a bounded literal.
func isScreamingSnake(s string) bool {
	hasLetter := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}

// swiftConstantBoundReceiver reports whether an iterator's receiver is provably
// bounded by a compile-time-fixed number of elements, so the closure runs a fixed
// number of times regardless of the method's input (O(1) in n): an array or
// dictionary literal (`[a,b].forEach`), or an ALL-CAPS data constant
// (`STOP_CHARS.forEach`). A trailing size-preserving chain method
// (`[a,b].sorted().forEach`) is unwrapped and its receiver re-checked. Mixed-case
// identifiers (a `var`/property/relation) are NOT bounded. Mirrors Ruby's
// constantBoundReceiver so literal/constant loops don't inflate a genuine O(n)
// into a false O(n²)/O(n³).
func swiftConstantBoundReceiver(recv *sitter.Node, src []byte) bool {
	if recv == nil {
		return false
	}
	switch kindOf(recv) {
	case "array_literal", "dictionary_literal":
		return true
	case "simple_identifier", "identifier":
		return isScreamingSnake(nodeText(recv, src))
	case "call_expression":
		// A trailing size-preserving chain (`[a,b].sorted().forEach`): the chain
		// method is the navigation suffix of this call's callee — unwrap to the
		// chain's base receiver and re-check.
		if callee := firstNamedChild(recv); callee != nil && kindOf(callee) == "navigation_expression" {
			if name, isNav, _, _ := calleeInfo(callee, src); isNav && swiftChainPreservesBound[name] {
				return swiftConstantBoundReceiver(callee.ChildByFieldName("target"), src)
			}
		}
	}
	return false
}

// swiftBoundedForCollection reports whether a `for … in <collection>` iterates a
// compile-time-fixed number of times: a literal integer range (`for i in 0..<10`,
// `1...5`) or a `stride(...)` whose bounds are all integer literals. A range with a
// variable endpoint (`0..<items.count`) or a stride over a variable bound scales
// with n and is NOT bounded.
func swiftBoundedForCollection(coll *sitter.Node, src []byte) bool {
	if coll == nil {
		return false
	}
	switch kindOf(coll) {
	case "range_expression":
		return isIntegerLiteralNode(coll.ChildByFieldName("start")) &&
			isIntegerLiteralNode(coll.ChildByFieldName("end"))
	case "call_expression":
		if callee := firstNamedChild(coll); callee != nil {
			if name, isNav, _, _ := calleeInfo(callee, src); !isNav && name == "stride" {
				return strideBoundsAreLiteral(coll)
			}
		}
	}
	return false
}

func isIntegerLiteralNode(n *sitter.Node) bool {
	return n != nil && kindOf(n) == "integer_literal"
}

// isSubscriptCall reports whether a call_expression is actually a subscript access
// (`dict[key]`, `parameters["x"]`) rather than a function call. The tree-sitter
// grammar models both as a call_expression, but a subscript's call_suffix is
// bracketed with `[` where a real call uses `(` (or a `{` trailing closure) — so
// the leading delimiter of the call_suffix distinguishes them. Without this, a
// subscript on an identifier that shares a method's name is mis-recorded as a call
// (and, when the names match, as self-recursion).
func isSubscriptCall(call *sitter.Node, src []byte) bool {
	suffix := findChildByKind(call, "call_suffix")
	if suffix == nil {
		return false
	}
	t := nodeText(suffix, src)
	return len(t) > 0 && t[0] == '['
}

// strideBoundsAreLiteral reports whether every argument of a `stride(...)` call is
// an integer literal — so the iteration count is fixed. A single non-literal bound
// (`stride(from: 0, to: n, by: 2)`) makes it scale with n.
func strideBoundsAreLiteral(call *sitter.Node) bool {
	suffix := findChildByKind(call, "call_suffix")
	if suffix == nil {
		return false
	}
	args := findChildByKind(suffix, "value_arguments")
	if args == nil {
		return false
	}
	sawArg := false
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if kindOf(a) != "value_argument" {
			continue
		}
		sawArg = true
		if !isIntegerLiteralNode(a.ChildByFieldName("value")) {
			return false
		}
	}
	return sawArg
}

// recordCallMetrics notes a resolved call target against the current function's
// complexity metrics: flags direct recursion and records calls made inside loops.
// call is the call_expression node, used to compare argument labels so a call that
// merely shares the enclosing function's bare name but targets a different overload
// (`decode(_:forKey:)` from `decode(key:)`, a sibling `loadMore(service:)` from
// `loadMore(completion:)`) is not mistaken for recursion.
func (w *astWalker) recordCallMetrics(target string, call *sitter.Node) {
	if w.metrics == nil || target == "" {
		return
	}
	if (target == w.selfShort || target == w.selfName || target == "self."+w.selfShort) &&
		w.callArgsMatchSelf(call) {
		w.metrics.recursive = true
	}
	w.recordInLoopCall(target)
}

// callArgsMatchSelf reports whether a call's argument labels match the enclosing
// function's external parameter labels — the signal that a same-named call could
// actually be the enclosing function (genuine recursion) rather than a different
// overload. An exact match is required; a default-omitted or trailing-closure call
// that differs in arity is treated as a different overload (a conservative false
// negative, consistent with this analysis erring away from false recursion).
func (w *astWalker) callArgsMatchSelf(call *sitter.Node) bool {
	return labelSignaturesMatch(w.selfParamLabels, callArgumentLabels(call, w.src))
}

func labelSignaturesMatch(params, args []string) bool {
	if len(params) != len(args) {
		return false
	}
	for i := range params {
		if params[i] != args[i] {
			return false
		}
	}
	return true
}

// parameterLabels returns a function declaration's external argument labels in
// order — the `external_name` when present (including `_` for an unlabeled
// parameter), otherwise the parameter's own name (which doubles as the label in
// `name: Type`). Used to build the enclosing function's call signature.
func parameterLabels(fn *sitter.Node, src []byte) []string {
	var labels []string
	for i := uint(0); i < uint(fn.ChildCount()); i++ {
		p := fn.Child(i)
		if kindOf(p) != "parameter" {
			continue
		}
		if ext := p.ChildByFieldName("external_name"); ext != nil {
			labels = append(labels, nodeText(ext, src))
		} else if nm := p.ChildByFieldName("name"); nm != nil {
			labels = append(labels, nodeText(nm, src))
		} else {
			labels = append(labels, "_")
		}
	}
	return labels
}

// callArgumentLabels returns a call's argument labels in order — the
// value_argument_label when present, `_` for a positional argument. A trailing
// closure (outside value_arguments) is not counted, matching parameterLabels which
// lists only declared parameters positionally.
func callArgumentLabels(call *sitter.Node, src []byte) []string {
	if call == nil {
		return nil
	}
	suffix := findChildByKind(call, "call_suffix")
	if suffix == nil {
		return nil
	}
	args := findChildByKind(suffix, "value_arguments")
	if args == nil {
		return nil
	}
	var labels []string
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		a := args.Child(i)
		if kindOf(a) != "value_argument" {
			continue
		}
		if lbl := a.ChildByFieldName("name"); lbl != nil {
			labels = append(labels, strings.TrimSuffix(nodeText(lbl, src), ":"))
		} else {
			labels = append(labels, "_")
		}
	}
	return labels
}

// recordInLoopCall adds a target to calls_in_loop (deduped) when inside a loop,
// without the recursion check — used for raw instance-method names whose name must
// not be mistaken for self-recursion.
func (w *astWalker) recordInLoopCall(target string) {
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
}

func (w *astWalker) pushType(name string, methods map[string]bool) {
	w.typeStack = append(w.typeStack, name)
	w.methodStack = append(w.methodStack, methods)
}

func (w *astWalker) popType() {
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
	w.methodStack = w.methodStack[:len(w.methodStack)-1]
}

func (w *astWalker) enclosingType() string { return strings.Join(w.typeStack, ".") }

func (w *astWalker) currentMethods() map[string]bool {
	if len(w.methodStack) == 0 {
		return nil
	}
	return w.methodStack[len(w.methodStack)-1]
}

// qualify prepends the enclosing type path to a declaration's name when inside a
// type ("<Type>.<name>"); at top level it returns name unchanged.
func (w *astWalker) qualify(name string) string {
	if t := w.enclosingType(); t != "" {
		return t + "." + name
	}
	return name
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

// addTentativeMethodCall emits a RelCalls edge to the bare method short name for a
// member call whose receiver type is unknown at walk time (extraction is
// parallel-per-file, so the project-wide method set is not yet available). The
// serial resolveMethodCalls post-pass then rewrites it to a qualified
// dir.Type.method target when the name maps to exactly one project method, keeps
// it bare when the name is ambiguous (still short-name matched by dead-code
// detection), and drops it when no project method matches (a stdlib/framework call
// like .map()/.dismissAnimated()). Known stdlib/iterator/cheap names and
// capitalized names (nested-type constructors) are skipped up front to cut noise.
func (w *astWalker) addTentativeMethodCall(name string) {
	if name == "" || isCapitalized(name) {
		return
	}
	if swiftBuiltins[name] || swiftIterators[name] || swiftCheapMethods[name] {
		return
	}
	w.addOwnerEdge(facts.RelCalls, name)
}

// applyDirective adjusts conditional-compilation nesting depth for a `directive`
// node (#if / #elseif / #else / #endif) encountered while iterating declarations in
// order, and reports whether the node was a directive so the caller skips it. The
// grammar emits directives as flat siblings of the declarations they guard, so depth
// must be tracked here rather than by recursion. #else/#elseif keep the same block
// open (no depth change); the token is matched exactly so #else never prefix-matches
// #elseif.
func (w *astWalker) applyDirective(node *sitter.Node) bool {
	if kindOf(node) != "directive" {
		return false
	}
	if fields := strings.Fields(nodeText(node, w.src)); len(fields) > 0 {
		switch fields[0] {
		case "#if":
			w.condDepth++
		case "#endif":
			if w.condDepth > 0 {
				w.condDepth--
			}
		}
	}
	return true
}

// stampConditional marks every symbol fact appended at index >= from with
// conditional=true when the walker is currently inside a #if/#elseif/#else branch.
// Called after each declaration handler so a conditional type and all its members
// (appended during the recursive body walk) are tagged together. Idempotent, so a
// member already tagged by a nested walkTypeBody is harmlessly re-tagged.
func (w *astWalker) stampConditional(from int) {
	if w.condDepth <= 0 {
		return
	}
	for i := from; i < len(w.out); i++ {
		if w.out[i].Kind != facts.KindSymbol {
			continue
		}
		if w.out[i].Props == nil {
			w.out[i].Props = map[string]any{}
		}
		w.out[i].Props["conditional"] = true
	}
}

func (w *astWalker) walkSourceFile(root *sitter.Node) {
	if root == nil {
		return
	}
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		child := root.Child(i)
		if w.applyDirective(child) {
			continue
		}
		before := len(w.out)
		switch kindOf(child) {
		case "import_declaration":
			w.handleImport(child)
		case "class_declaration":
			w.handleClassDeclaration(child)
		case "protocol_declaration":
			w.handleProtocol(child)
		case "function_declaration":
			w.handleFunction(child)
		case "property_declaration":
			w.handleProperty(child)
		case "typealias_declaration":
			w.handleTypeAlias(child)
		}
		w.stampConditional(before)
	}
	w.walkFileScopeCalls(root)
}

// fileScopeDecls are the top-level child kinds whose call edges are already
// captured under their own owner (function/class/protocol bodies) or that carry no
// calls (import/typealias). walkFileScopeCalls skips them and captures calls from
// every other top-level statement against the file-scope reference fact.
var fileScopeDecls = map[string]bool{
	"import_declaration":    true,
	"class_declaration":     true,
	"protocol_declaration":  true,
	"function_declaration":  true,
	"typealias_declaration": true,
}

// walkFileScopeCalls captures calls made by top-level statements — bare `foo()`
// calls and `let x = foo()` initializers that have no enclosing symbol — attaching
// them to a lazily-created facts.KindFileRef fact so the dead-code detector treats
// the callees as used. The file-ref fact is dropped again if it accrued no edges.
func (w *astWalker) walkFileScopeCalls(root *sitter.Node) {
	// Most Swift files contain only top-level declarations; skip the file-ref
	// machinery entirely unless there is a genuine top-level statement.
	hasStmt := false
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		if !fileScopeDecls[kindOf(root.Child(i))] {
			hasStmt = true
			break
		}
	}
	if !hasStmt {
		return
	}

	w.pushOwner(w.ensureFileRefFact())
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		child := root.Child(i)
		if fileScopeDecls[kindOf(child)] {
			continue
		}
		w.walkForCalls(child)
	}
	w.popOwner()

	// Drop the file-ref fact if none of the statements produced a call edge.
	if w.fileRefIdx >= 0 && len(w.out[w.fileRefIdx].Relations) == 0 {
		w.out = append(w.out[:w.fileRefIdx], w.out[w.fileRefIdx+1:]...)
		w.fileRefIdx = -1
	}
}

// ensureFileRefFact returns the index of this file's lazily-created file-scope
// reference fact (facts.KindFileRef), creating it on first use.
func (w *astWalker) ensureFileRefFact() int {
	if w.fileRefIdx < 0 {
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindFileRef,
			Name:  w.relFile,
			File:  w.relFile,
			Props: map[string]any{"language": "swift"},
		})
		w.fileRefIdx = len(w.out) - 1
	}
	return w.fileRefIdx
}

func (w *astWalker) handleImport(node *sitter.Node) {
	if m := importRe.FindStringSubmatch(nodeText(node, w.src)); m != nil {
		name := m[1]
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindDependency,
			Name: w.dir + " -> " + name,
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"language": "swift",
			},
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: name},
			},
		})
	}
}

// handleClassDeclaration handles class_declaration, which in the Swift grammar
// covers struct / class / enum / actor / extension (distinguished by the leading
// keyword token).
func (w *astWalker) handleClassDeclaration(node *sitter.Node) {
	keyword := declKeyword(node, w.src)
	if keyword == "extension" {
		w.handleExtension(node)
		return
	}

	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}

	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)
	attrs := attributeNames(modifiers, w.src)
	supertypes := inheritanceNames(node, w.src)

	symbolKind := facts.SymbolClass
	switch keyword {
	case "struct":
		symbolKind = facts.SymbolStruct
	case "enum", "actor", "class":
		symbolKind = facts.SymbolClass
	}

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if keyword == "enum" {
		f.Props["enum"] = true
	}
	if keyword == "actor" {
		f.Props["concurrency"] = "actor"
	}
	if strings.Contains(modifierText, "final") {
		f.Props["final"] = true
	}
	if containsAnnotation(attrs, "MainActor") {
		f.Props["main_actor"] = true
	}
	for _, st := range supertypes {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: st})
	}

	body := typeBody(node)
	if sig, published := computeSignature(body, w.src); sig != "" || len(published) > 0 {
		if sig != "" {
			f.Props["signature"] = sig
		}
		if len(published) > 0 {
			f.Props["reactive"] = true
			f.Props["published_properties"] = strings.Join(published, ",")
		}
	}

	if w.isiOS {
		addIOSProps(&f, name, attrs, strings.Join(supertypes, ", "))
	}
	iosComponent, _ := f.Props["ios_component"].(string)

	w.out = append(w.out, f)
	ownerIdx := len(w.out) - 1

	w.pushOwner(ownerIdx)
	w.pushType(name, collectMethodNames(body, w.src))
	w.walkTypeBody(body, iosComponent)
	w.popType()
	w.popOwner()
}

func (w *astWalker) handleProtocol(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)
	attrs := attributeNames(modifiers, w.src)
	supertypes := inheritanceNames(node, w.src)

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolInterface,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	for _, st := range supertypes {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: st})
	}
	body := findChildByKind(node, "protocol_body")
	if sig, _ := computeSignature(body, w.src); sig != "" {
		f.Props["signature"] = sig
	}
	if w.isiOS {
		addIOSProps(&f, name, attrs, strings.Join(supertypes, ", "))
	}
	w.out = append(w.out, f)
}

// handleExtension preserves the legacy behaviour: one symbol fact per adopted
// protocol named "<dir>.<Base>+<Proto>". It additionally walks the extension body
// so methods declared in extensions get symbol facts ("<dir>.<Base>.<method>")
// and contribute call-graph edges.
func (w *astWalker) handleExtension(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	base := simpleTypeName(nameNode, w.src)
	if base == "" {
		return
	}
	for _, proto := range inheritanceNames(node, w.src) {
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: w.dir + "." + base + "+" + proto,
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"symbol_kind": "extension",
				"exported":    true,
				"language":    "swift",
			},
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: w.dir},
				{Kind: facts.RelImplements, Target: proto},
			},
		})
	}

	body := typeBody(node)
	// No single type owner for an extension; methods push their own owner.
	w.pushType(base, collectMethodNames(body, w.src))
	w.walkTypeBody(body, "")
	w.popType()
}

// walkTypeBody iterates the direct members of a type body, emitting member symbol
// facts and attaching call-graph edges. iosComponent is the enclosing type's iOS
// classification (used to detect SwiftUI View->ViewModel dependencies).
func (w *astWalker) walkTypeBody(body *sitter.Node, iosComponent string) {
	if body == nil {
		return
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if w.applyDirective(c) {
			continue
		}
		before := len(w.out)
		switch kindOf(c) {
		case "function_declaration":
			w.handleFunction(c)
		case "property_declaration":
			w.handleProperty(c)
			w.handlePropertyInjection(c, iosComponent)
		case "init_declaration":
			w.handleInit(c)
		case "class_declaration":
			w.handleClassDeclaration(c)
		case "protocol_declaration":
			w.handleProtocol(c)
		case "typealias_declaration":
			w.handleTypeAlias(c)
		}
		w.stampConditional(before)
	}
}

func (w *astWalker) handleFunction(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	if name == "" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)
	attrs := attributeNames(modifiers, w.src)
	body := findChildByKind(node, "function_body")
	header := headerText(node, body, w.src)

	// A function declared inside a type is a method (dispatch/reflection usage is
	// not edge-tracked), so it must be classified SymbolMethod — not SymbolFunc —
	// or the dead-code detector mislabels it high-confidence "safest to remove".
	// This matches the Python/Java extractors. Free functions stay SymbolFunc.
	enclosing := w.enclosingType()
	symbolKind := facts.SymbolFunc
	if enclosing != "" {
		symbolKind = facts.SymbolMethod
	}

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if enclosing != "" {
		f.Props["receiver"] = enclosing
	}
	// An `override` is dispatched polymorphically through its supertype — UIKit /
	// SwiftUI lifecycle callbacks (viewDidLoad, viewWillAppear, …) are invoked by
	// the framework, never by the override's own literal name, so the dead-code
	// detector must not report them as orphans. Mirrors kotlin_ast.go.
	if strings.Contains(modifierText, "override") {
		f.Props["override"] = true
	}
	if strings.Contains(header, " async") {
		f.Props["async"] = true
	}
	if strings.Contains(header, " throws") {
		f.Props["throws"] = true
	}
	if strings.Contains(header, "nonisolated") {
		f.Props["nonisolated"] = true
	}
	if strings.Contains(header, "@MainActor") || containsAnnotation(attrs, "MainActor") {
		f.Props["main_actor"] = true
	}

	w.out = append(w.out, f)
	ownerIdx := len(w.out) - 1
	w.pushOwner(ownerIdx)
	// Set up per-function complexity tracking. The Props map is shared by
	// reference with the fact in w.out, so writing to it after the walk updates
	// the emitted fact.
	w.metrics = &swiftBodyMetrics{}
	w.loopDepth = 0
	w.selfName = f.Name
	w.selfShort = name
	w.selfParamLabels = parameterLabels(node, w.src)
	w.walkForCalls(body)
	w.finishMetrics(w.out[ownerIdx].Props)
	w.popOwner()
}

// finishMetrics writes the accumulated complexity signals onto props and clears
// w.metrics. cyclomatic is always written; loop_depth/loop_count/calls_in_loop/
// recursive_self only when non-zero (so a loop-free body stays clean). Shared by
// handleFunction, handleProperty (computed getters / observers), and handleInit.
func (w *astWalker) finishMetrics(props map[string]any) {
	if w.metrics == nil {
		return
	}
	props["cyclomatic"] = 1 + w.metrics.decisions
	if w.metrics.loopDepth > 0 {
		props["loop_depth"] = w.metrics.loopDepth
	}
	if w.metrics.loopCount > 0 {
		props["loop_count"] = w.metrics.loopCount
	}
	if len(w.metrics.callsInLoop) > 0 {
		props["calls_in_loop"] = w.metrics.callsInLoop
	}
	if w.metrics.recursive {
		props["recursive_self"] = true
	}
	if w.metrics.ioDirect {
		props["io_direct"] = true
	}
	w.metrics = nil
}

func (w *astWalker) handleProperty(node *sitter.Node) {
	nameNode := propertyNameNode(node, w.src)
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	if name == "" || name == "_" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	modifierText := nodeText(modifiers, w.src)

	symbolKind := facts.SymbolVariable
	if vb := findChildByKind(node, "value_binding_pattern"); vb != nil && strings.Contains(nodeText(vb, w.src), "let") {
		symbolKind = facts.SymbolConstant
	}

	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    !isPrivateAccess(modifierText),
			"language":    "swift",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	})
	propIdx := len(w.out) - 1

	// A computed getter or a willSet/didSet observer has a real body that can hold a
	// loop or per-iteration I/O, so measure its complexity like a function's — a
	// hotspot inside `var items: [X] { … }` or `didSet { for … }` is otherwise
	// invisible to analyze_performance. Stored properties with a plain initializer
	// get no metrics (avoids a cyclomatic=1 prop on every field).
	measure := findChildByKind(node, "computed_property") != nil ||
		findChildByKind(node, "willset_didset_block") != nil
	if measure {
		w.metrics = &swiftBodyMetrics{}
		w.loopDepth = 0
		w.selfName = w.out[propIdx].Name
		w.selfShort = name
		w.selfParamLabels = nil // a property takes no arguments
	}

	// A property initializer or computed-getter body may call/construct
	// (`let x = Foo()`, `var y: T { return helper() }`). Inside a type body the
	// enclosing type is already the owner and captures these edges. But an
	// `extension` pushes NO owner (handleExtension), so at extension scope
	// currentOwner() == -1 and the edges would be dropped — a helper called only
	// from an extension property then looked dead. Only in that ownerless case do we
	// push the property itself as owner, keeping class-property attribution (and the
	// coupling graph) unchanged.
	if w.currentOwner() < 0 {
		w.pushOwner(propIdx)
		w.walkForCalls(node)
		w.popOwner()
	} else {
		w.walkForCalls(node)
	}

	if measure {
		w.finishMetrics(w.out[propIdx].Props)
	}
}

// handlePropertyInjection emits DI edges from the enclosing type for a property
// that carries a dependency-injection property wrapper. For SwiftUI Views,
// @StateObject/@ObservedObject/@EnvironmentObject also yield the legacy
// View->ViewModel RelDependsOn edge.
func (w *astWalker) handlePropertyInjection(node *sitter.Node, iosComponent string) {
	modifiers := findChildByKind(node, "modifiers")
	attrs := attributeNames(modifiers, w.src)
	if len(attrs) == 0 {
		return
	}
	ta := findChildByKind(node, "type_annotation")
	if ta == nil {
		return
	}
	typeName := simpleTypeName(firstTypeNode(ta), w.src)
	if typeName == "" || isSystemType(typeName) || !isTypeName(typeName) {
		return
	}

	injectWrappers := []string{"Injected", "Dependency", "Environment", "EnvironmentObject", "ObservedObject", "StateObject"}
	for _, wrapper := range injectWrappers {
		if containsAnnotation(attrs, wrapper) {
			w.addOwnerEdge(facts.RelInjects, typeName)
			break
		}
	}
	if iosComponent == "swiftui_view" {
		for _, wrapper := range []string{"StateObject", "ObservedObject", "EnvironmentObject"} {
			if containsAnnotation(attrs, wrapper) {
				w.addOwnerEdge(facts.RelDependsOn, typeName)
				break
			}
		}
	}
}

// handleInit processes an initializer: each non-system parameter type becomes a
// RelInjects edge on the enclosing type (Swift constructor injection needs no
// annotation), and the body is walked for construction calls.
func (w *astWalker) handleInit(node *sitter.Node) {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if kindOf(c) != "parameter" {
			continue
		}
		typeName := simpleTypeName(parameterTypeNode(c), w.src)
		if typeName != "" && !isSystemType(typeName) && isTypeName(typeName) {
			w.addOwnerEdge(facts.RelInjects, typeName)
		}
	}
	if body := findChildByKind(node, "function_body"); body != nil {
		w.walkForCalls(body)
	}
}

func (w *astWalker) handleTypeAlias(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := simpleTypeName(nameNode, w.src)
	if name == "" {
		return
	}
	modifiers := findChildByKind(node, "modifiers")
	relations := []facts.Relation{
		{Kind: facts.RelDeclares, Target: w.dir},
	}
	// A `typealias Foo = Bar` is a genuine reference to Bar, so fold the aliased
	// type in as an instantiation edge: a type reached only through its alias name
	// (the idiomatic `typealias FooViewModel = FooEditorState`) would otherwise have
	// no incoming edge and be mis-reported as an unreferenced orphan (GAP-SW-09).
	// Mirrors handleInit's type guard — skip system types and function/tuple/
	// optional RHS shapes, which yield no simple resolvable type name.
	if valueNode := node.ChildByFieldName("value"); valueNode != nil {
		if target := simpleTypeName(valueNode, w.src); target != "" && !isSystemType(target) && isTypeName(target) {
			relations = append(relations, facts.Relation{Kind: facts.RelInstantiates, Target: target})
		}
	}
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolType,
			"exported":    !isPrivateAccess(nodeText(modifiers, w.src)),
			"language":    "swift",
		},
		Relations: relations,
	})
}

// walkForCalls recursively scans an expression subtree for call_expression nodes
// and attaches edges to the current owner:
//   - capitalized callee  -> RelInstantiates (constructor)
//   - bare lowercase call -> RelCalls (resolved via resolveCall)
//   - self.method()       -> RelCalls to the enclosing type's method
//   - Type.member()       -> RelCalls to the receiver type (DI hub / static access)
func (w *astWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	kind := kindOf(node)

	// A closure is a deferred scope: its body runs when the closure is called, NOT
	// per-iteration of the enclosing loops — so reset the loop depth for its
	// subtree (e.g. a tap handler defined inside a `forEach { … }` must not be
	// counted as a per-iteration call). The iterator's OWN closure is handled in
	// the call_expression branch (its body walks at +1).
	if w.metrics != nil && kind == "lambda_literal" {
		saved := w.loopDepth
		w.loopDepth = 0
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkForCalls(node.Child(i))
		}
		w.loopDepth = saved
		return
	}

	// Complexity metrics: count decision points so the single body walk doubles
	// as the cyclomatic pass. (Statement node kinds don't collide with the
	// anonymous keyword tokens, so no IsNamed guard is needed.)
	if w.metrics != nil {
		switch kind {
		case "if_statement", "guard_statement", "switch_entry",
			"conjunction_expression", "disjunction_expression", "catch_block":
			w.metrics.decisions++
		}
	}

	// Syntactic loops: everything in the body runs per iteration. A `for` over a
	// literal integer range or a literal-bound `stride(...)` runs a fixed number of
	// times, so it counts as a loop (cyclomatic) but must not add scaling loop DEPTH
	// — otherwise a `for i in 0..<10` inflates a genuine O(n) into a false O(n²).
	switch kind {
	case "for_statement", "for_statement_await", "while_statement", "repeat_while_statement":
		bounded := (kind == "for_statement" || kind == "for_statement_await") &&
			swiftBoundedForCollection(node.ChildByFieldName("collection"), w.src)
		if w.metrics != nil {
			w.metrics.loopCount++
			w.metrics.decisions++
			if !bounded && w.loopDepth+1 > w.metrics.loopDepth {
				w.metrics.loopDepth = w.loopDepth + 1
			}
		}
		if !bounded {
			w.loopDepth++
		}
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkForCalls(node.Child(i))
		}
		if !bounded {
			w.loopDepth--
		}
		return
	}

	// A subscript access (`dict[key]`, `parameters["x"] = 1`) parses as a
	// call_expression whose callee is the base and whose `[...]` is a call_suffix —
	// indistinguishable in kind from a real `foo()` call. Skip the callee-as-call
	// handling so a local/property whose name collides with a method
	// (`parameters["x"]` inside `func parameters()`) is not mistaken for a call or
	// self-recursion; still recurse into children so calls in the receiver or the
	// subscript key (`getDict()["x"]`, `a[compute()]`) are captured.
	if kind == "call_expression" && isSubscriptCall(node, w.src) {
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkForCalls(node.Child(i))
		}
		return
	}

	if kind == "call_expression" {
		var iterClosure *sitter.Node
		if callee := firstNamedChild(node); callee != nil {
			name, isNav, root, directSelf := calleeInfo(callee, w.src)
			// Seed the performs_io closure: flag the enclosing method when its body
			// directly invokes a network/file I/O primitive. Framework I/O calls are
			// otherwise dropped by the call-edge resolver, so this is the only durable
			// signal that a method does I/O.
			if w.metrics != nil && !w.metrics.ioDirect && isIOPrimitiveCall(name, root, isNav, node, w.src) {
				w.metrics.ioDirect = true
			}
			switch {
			case name == "":
				// unresolved
			case !isNav:
				if isCapitalized(name) {
					if !isSystemType(name) {
						w.addOwnerEdge(facts.RelInstantiates, name)
					}
				} else if target := w.resolveCall(name); target != "" {
					w.addOwnerEdge(facts.RelCalls, target)
					w.recordCallMetrics(target, node)
				}
			case directSelf && w.enclosingType() != "":
				// self.foo() / self?.foo(): a member of the enclosing type. When
				// declared in the current body, emit the resolved dir.Type.method
				// edge. Otherwise (a member declared in another extension of the
				// same type, or an inherited/framework method) emit a tentative
				// bare edge for the serial post-pass to resolve or drop — this is
				// what recovers the coordinator "[weak self] switch { self?.x() }"
				// routing calls that the old currentMethods gate silently dropped.
				if w.currentMethods()[name] {
					t := w.dir + "." + w.enclosingType() + "." + name
					w.addOwnerEdge(facts.RelCalls, t)
					w.recordCallMetrics(t, node)
				} else {
					w.addTentativeMethodCall(name)
				}
			case isNav && isCapitalized(root) && !isSystemType(root):
				// e.g. AppComposition.shared.makeRepo() — depend on the root type,
				// and also credit the invoked method by short name (post-pass).
				w.addOwnerEdge(facts.RelCalls, root)
				w.recordCallMetrics(root, node)
				w.addTentativeMethodCall(name)
			case isNav:
				// Member call on a lowercase / property-chain receiver
				// (coordinator?.show(), delegate?.tap(), self.prop.foo()). Emit a
				// tentative bare short-name edge, resolved or dropped in the
				// post-pass. Preserve the existing in-loop perf metric.
				w.addTentativeMethodCall(name)
				if w.loopDepth > 0 && !swiftCheapMethods[name] {
					tgt := name
					if root != "" {
						tgt = root + "." + name
					}
					w.recordInLoopCall(tgt)
				}
			}
			// An iterator method with a trailing closure (items.map { … }) is a
			// loop: its closure body runs per element, but the receiver/arguments
			// run once.
			if w.metrics != nil && swiftIterators[name] {
				iterClosure = trailingClosure(node)
			}
		}
		if iterClosure != nil {
			// A constant-bounded iterator (`[a,b].forEach`, `STOP_CHARS.map`) runs a
			// fixed number of times: still a loop (cyclomatic) but its closure body
			// walks at the SAME depth, so any inner scaling loop or per-iteration I/O
			// is measured against the real input, not multiplied by a constant.
			bounded := false
			if callee := firstNamedChild(node); callee != nil {
				bounded = swiftConstantBoundReceiver(callee.ChildByFieldName("target"), w.src)
			}
			w.metrics.loopCount++
			w.metrics.decisions++
			delta := 1
			if bounded {
				delta = 0
			} else if w.loopDepth+1 > w.metrics.loopDepth {
				w.metrics.loopDepth = w.loopDepth + 1
			}
			for i := uint(0); i < uint(node.ChildCount()); i++ {
				if c := node.Child(i); byteContains(c, iterClosure) {
					w.walkClosureSubtree(c, iterClosure, delta)
				} else {
					w.walkForCalls(c)
				}
			}
			return
		}
	}

	// Custom operator usage (`a <- b`, prefix `<>x`): the operator token names a
	// project-declared `func <op>(…)`. Emit a tentative edge so the operator overload
	// is not seen as dead (resolved/dropped in the serial post-pass). Standard-token
	// operators (+, +=, ^, ==, …) are intentionally NOT tracked — their overloads
	// share tokens with builtin arithmetic, so edges would flood every arithmetic
	// site and inflate the operator's fan-in.
	if op := customOperatorToken(node, w.src); op != "" {
		w.addOwnerEdge(facts.RelCalls, op)
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkForCalls(node.Child(i))
	}
}

// swiftStandardOperators are Swift stdlib operators that the tree-sitter scanner
// nonetheless emits as `custom_operator` tokens (e.g. multi-char `<=`, `>=`, `??`,
// `..<`). Overloads of these share tokens with builtin uses, so tracking their
// usage would flood every arithmetic/comparison site and inflate the operator's
// fan-in — they are excluded so only genuinely user-defined operators (`<-`, `~>`,
// `|>`, …) get usage edges. Their overloads stay covered by the confidence
// downgrade in the dead-code detector.
var swiftStandardOperators = map[string]bool{
	"=": true, "+": true, "-": true, "*": true, "/": true, "%": true,
	"==": true, "!=": true, "===": true, "!==": true,
	"<": true, ">": true, "<=": true, ">=": true,
	"&&": true, "||": true, "!": true, "??": true, "~=": true,
	"&": true, "|": true, "^": true, "~": true, "<<": true, ">>": true,
	"+=": true, "-=": true, "*=": true, "/=": true, "%=": true,
	"&=": true, "|=": true, "^=": true, "<<=": true, ">>=": true,
	"&+": true, "&-": true, "&*": true, "&<<": true, "&>>": true,
	"...": true, "..<": true, "?": true,
}

// customOperatorToken returns the operator token text when node is an operator
// expression whose operator is a user-defined `custom_operator` (e.g. `<-`), else
// "". Standard-token operators — both those with dedicated grammar nodes (`+` via
// additive_expression) and stdlib multi-char operators the scanner emits as
// `custom_operator` (`<=`, `>=`, `??`, …, see swiftStandardOperators) — are excluded,
// because their overloads are indistinguishable from builtin uses and would flood.
func customOperatorToken(node *sitter.Node, src []byte) string {
	var opField string
	switch kindOf(node) {
	case "infix_expression":
		opField = "op"
	case "prefix_expression":
		opField = "operation"
	default:
		return ""
	}
	op := node.ChildByFieldName(opField)
	if op == nil || kindOf(op) != "custom_operator" {
		return ""
	}
	text := nodeText(op, src)
	if swiftStandardOperators[text] {
		return ""
	}
	return text
}

// trailingClosure returns a call's closure argument (lambda_literal) — a trailing
// closure or one passed inside the argument list — or nil if there is none.
func trailingClosure(call *sitter.Node) *sitter.Node {
	suffix := findChildByKind(call, "call_suffix")
	if suffix == nil {
		return nil
	}
	if l := findChildByKind(suffix, "lambda_literal"); l != nil {
		return l
	}
	if va := findChildByKind(suffix, "value_arguments"); va != nil {
		for i := uint(0); i < uint(va.ChildCount()); i++ {
			if arg := va.Child(i); kindOf(arg) == "value_argument" {
				if l := findChildByKind(arg, "lambda_literal"); l != nil {
					return l
				}
			}
		}
	}
	return nil
}

func byteContains(outer, inner *sitter.Node) bool {
	return inner.StartByte() >= outer.StartByte() && inner.EndByte() <= outer.EndByte()
}

// walkClosureSubtree descends toward an iterator's closure, changing the loop depth
// by delta exactly at the closure (its body is per-iteration) while walking
// everything else (the receiver, sibling arguments) at the current depth. delta is
// 1 for a scaling iterator and 0 for a constant-bounded one (whose body runs a
// fixed number of times, so an inner scaling loop is measured at the outer depth).
func (w *astWalker) walkClosureSubtree(node, closure *sitter.Node, delta int) {
	if node == nil {
		return
	}
	// Match the closure itself (kind-checked so an ancestor with the same byte span,
	// e.g. a call_suffix that wraps only the trailing closure, isn't mistaken for it).
	if kindOf(node) == "lambda_literal" && node.StartByte() == closure.StartByte() && node.EndByte() == closure.EndByte() {
		// The iterator invokes this closure per element: walk its BODY at +delta.
		// Descend into the closure's children directly rather than walkForCalls(node),
		// which would treat the closure as a deferred scope and reset the depth.
		w.loopDepth += delta
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkForCalls(node.Child(i))
		}
		w.loopDepth -= delta
		return
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); byteContains(c, closure) {
			w.walkClosureSubtree(c, closure, delta)
		} else {
			w.walkForCalls(c)
		}
	}
}

// resolveCall maps a bare (non-navigation) call name to a canonical symbol fact
// name: a same-type method, a suppressed stdlib builtin, or a same-module
// top-level function.
func (w *astWalker) resolveCall(name string) string {
	if w.currentMethods()[name] {
		return w.dir + "." + w.enclosingType() + "." + name
	}
	if swiftBuiltins[name] {
		return ""
	}
	return w.dir + "." + name
}

// swiftBuiltins are Swift global / standard-library functions that appear as bare
// calls without an import. Resolving them would create dangling phantom RelCalls
// edges, so they are suppressed (the Swift analog of kotlinBuiltins).
var swiftBuiltins = map[string]bool{
	"print": true, "debugPrint": true, "dump": true,
	"assert": true, "assertionFailure": true, "precondition": true,
	"preconditionFailure": true, "fatalError": true,
	"abs": true, "min": true, "max": true, "swap": true, "zip": true,
	"stride": true, "sequence": true, "repeatElement": true,
	"withUnsafePointer": true, "withExtendedLifetime": true, "withAnimation": true,
	"isKnownUniquelyReferenced": true, "numericCast": true,
	// `defer { … }` parses as a call to an identifier named "defer" with a trailing
	// closure; suppress it so it does not become a phantom `calls -> …defer` edge.
	"defer": true,
}

// --- node helpers ---

// declKeyword returns the declaration keyword token (struct/class/enum/actor/
// extension/protocol) of a class_declaration-like node.
func declKeyword(node *sitter.Node, src []byte) string {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.IsNamed() {
			continue
		}
		switch t := nodeText(c, src); t {
		case "struct", "class", "enum", "actor", "extension", "protocol":
			return t
		}
	}
	return "class"
}

// typeBody returns the class_body or enum_class_body child of a declaration.
func typeBody(node *sitter.Node) *sitter.Node {
	if b := findChildByKind(node, "class_body"); b != nil {
		return b
	}
	return findChildByKind(node, "enum_class_body")
}

// inheritanceNames returns the simple supertype/protocol names from a
// declaration's inheritance_specifier children.
func inheritanceNames(node *sitter.Node, src []byte) []string {
	var names []string
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if kindOf(c) != "inheritance_specifier" {
			continue
		}
		tn := c.ChildByFieldName("inherits_from")
		if tn == nil {
			tn = firstNamedChild(c)
		}
		if name := simpleTypeName(tn, src); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// attributeNames returns the simple names of attributes (e.g. ["MainActor",
// "Published"]) declared in a modifiers node.
func attributeNames(modifiers *sitter.Node, src []byte) []string {
	if modifiers == nil {
		return nil
	}
	var out []string
	for i := uint(0); i < uint(modifiers.ChildCount()); i++ {
		c := modifiers.Child(i)
		if kindOf(c) != "attribute" {
			continue
		}
		if name := simpleTypeName(firstNamedChild(c), src); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// simpleTypeName extracts the simple (rightmost, generics-stripped) type name
// from a type node by reusing the tested extractTypeName string helper.
func simpleTypeName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return extractTypeName(nodeText(node, src))
}

// firstTypeNode returns the first user_type/type-ish descendant of a node such as
// a type_annotation.
func firstTypeNode(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "user_type", "type_identifier", "optional_type", "array_type",
			"dictionary_type", "opaque_type":
			return c
		}
	}
	return firstNamedChild(node)
}

// parameterTypeNode returns the type node of a `parameter` (the second `name`
// field in the Swift grammar is the parameter type).
func parameterTypeNode(param *sitter.Node) *sitter.Node {
	if ta := findChildByKind(param, "type_annotation"); ta != nil {
		return firstTypeNode(ta)
	}
	// Grammar shape: (parameter name: (simple_identifier) name: (user_type ...))
	var last *sitter.Node
	for i := uint(0); i < uint(param.ChildCount()); i++ {
		c := param.Child(i)
		switch kindOf(c) {
		case "user_type", "optional_type", "array_type", "dictionary_type", "type_identifier":
			last = c
		}
	}
	return last
}

// propertyNameNode returns the bound identifier node of a property_declaration.
func propertyNameNode(node *sitter.Node, src []byte) *sitter.Node {
	if pat := node.ChildByFieldName("name"); pat != nil {
		if id := pat.ChildByFieldName("bound_identifier"); id != nil {
			return id
		}
		return findFirstIdentifier(pat, src)
	}
	if pat := findChildByKind(node, "pattern"); pat != nil {
		return findFirstIdentifier(pat, src)
	}
	return nil
}

// collectMethodNames returns the set of method names declared directly in a type
// body, used to resolve same-type bare/self calls.
func collectMethodNames(body *sitter.Node, src []byte) map[string]bool {
	methods := make(map[string]bool)
	if body == nil {
		return methods
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if kindOf(c) != "function_declaration" {
			continue
		}
		if n := c.ChildByFieldName("name"); n != nil {
			methods[nodeText(n, src)] = true
		}
	}
	return methods
}

// computeSignature reconstructs up to 15 direct member declaration lines from a
// type body (parity with the legacy regex signature capture) and returns the
// names of @Published properties.
func computeSignature(body *sitter.Node, src []byte) (string, []string) {
	if body == nil {
		return "", nil
	}
	const maxMembers = 15
	var members []string
	var published []string
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		kind := kindOf(c)
		if kind != "property_declaration" && kind != "function_declaration" {
			continue
		}
		if len(members) < maxMembers {
			members = append(members, memberSignature(c, src))
		}
		if kind == "property_declaration" {
			attrs := attributeNames(findChildByKind(c, "modifiers"), src)
			if containsAnnotation(attrs, "Published") {
				if nn := propertyNameNode(c, src); nn != nil {
					published = append(published, nodeText(nn, src))
				}
			}
		}
	}
	return strings.Join(members, "\n"), published
}

// memberSignature renders a member declaration as a single trimmed line, dropping
// any body/computed block.
func memberSignature(node *sitter.Node, src []byte) string {
	s := nodeText(node, src)
	if idx := strings.Index(s, "{"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// headerText returns the source text of node up to the start of its body (or the
// whole node when there is no body), used to inspect a function's signature.
func headerText(node, body *sitter.Node, src []byte) string {
	end := node.EndByte()
	if body != nil {
		end = body.StartByte()
	}
	return string(src[node.StartByte():end])
}

// calleeInfo inspects a call_expression's callee and returns the called simple
// name, whether it was a navigation (a.b.foo()) call, the leftmost receiver
// identifier ("self" for self-calls; the root type/var name otherwise), and
// whether the DIRECT receiver is self ("self.foo()"/"self?.foo()" — but NOT the
// property-chain "self.prop.foo()", whose direct receiver is a navigation node).
// The optional-chaining "?." is folded into the navigation token by the grammar's
// external scanner, so "self?.foo()" parses identically to "self.foo()".
func calleeInfo(callee *sitter.Node, src []byte) (name string, isNav bool, root string, directSelf bool) {
	switch kindOf(callee) {
	case "simple_identifier", "identifier":
		return nodeText(callee, src), false, "", false
	case "navigation_expression":
		suffix := callee.ChildByFieldName("suffix")
		if suffix == nil {
			suffix = findChildByKind(callee, "navigation_suffix")
		}
		if suffix != nil {
			if id := findFirstIdentifier(suffix, src); id != nil {
				name = nodeText(id, src)
			}
		}
		root = navigationRoot(callee, src)
		if tgt := callee.ChildByFieldName("target"); tgt != nil {
			if kindOf(tgt) == "self_expression" || nodeText(tgt, src) == "Self" {
				directSelf = true
			}
		}
		return name, true, root, directSelf
	}
	return "", false, "", false
}

// navigationRoot drills to the leftmost receiver of a (possibly nested)
// navigation_expression and returns its identifier text ("self" for self).
func navigationRoot(nav *sitter.Node, src []byte) string {
	cur := nav
	for cur != nil && kindOf(cur) == "navigation_expression" {
		target := cur.ChildByFieldName("target")
		if target == nil {
			target = firstNamedChild(cur)
		}
		cur = target
	}
	if cur == nil {
		return ""
	}
	if kindOf(cur) == "self_expression" {
		return "self"
	}
	return nodeText(cur, src)
}

func isCapitalized(s string) bool {
	if s == "" {
		return false
	}
	return unicode.IsUpper([]rune(s)[0])
}

// isOperatorToken reports whether s is a Swift operator name (e.g. "+", "<-", "^=")
// rather than an identifier — i.e. its first rune is neither a letter nor an
// underscore. Used to index operator overloads so custom-operator usage edges bind.
func isOperatorToken(s string) bool {
	if s == "" {
		return false
	}
	r := []rune(s)[0]
	return !unicode.IsLetter(r) && r != '_'
}

// isTypeName reports whether s looks like a simple Swift type name (an uppercase
// identifier), filtering out junk like "[String]" before it becomes an edge.
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

// --- tree-sitter helpers (mirrors of the Kotlin extractor's) ---

func findChildByKind(node *sitter.Node, kind string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); kindOf(c) == kind {
			return c
		}
	}
	return nil
}

func firstNamedChild(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); c.IsNamed() {
			return c
		}
	}
	return nil
}

func findFirstIdentifier(node *sitter.Node, src []byte) *sitter.Node {
	if node == nil {
		return nil
	}
	if kindOf(node) == "identifier" || kindOf(node) == "simple_identifier" || kindOf(node) == "type_identifier" {
		return node
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if !c.IsNamed() {
			continue
		}
		if found := findFirstIdentifier(c, src); found != nil {
			return found
		}
	}
	return nil
}

func nodeText(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return string(src[node.StartByte():node.EndByte()])
}
