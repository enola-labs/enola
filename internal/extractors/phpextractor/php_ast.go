package phpextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// extractFileAST parses a PHP file with tree-sitter and emits architectural facts:
// symbols (classes, interfaces, traits, enums, functions, methods, constants),
// `use` import dependencies, inheritance edges, a call / instantiation graph, and
// per-function cyclomatic complexity. WordPress hook routes are added separately by
// extractHooks. LanguagePHP (not LanguagePHPOnly) is used so template files that
// interleave HTML with <?php blocks parse correctly.
func extractFileAST(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	w := &phpWalker{
		src:       src,
		relFile:   relFile,
		dir:       factpath.Dir(relFile),
		importMap: map[string]string{},
	}
	w.walkProgram(tree.RootNode())
	return w.out
}

type phpWalker struct {
	src     []byte
	relFile string
	dir     string

	// namespace is the current PHP namespace (e.g. "App\Models"), "" at global scope.
	namespace string
	// typeStack holds the fully-qualified names of the enclosing class/interface/
	// trait/enum scopes so members get qualified names.
	typeStack []string
	// importMap maps a short name / alias to its fully-qualified target, populated
	// from `use` statements, used to expand inheritance and reference targets.
	importMap map[string]string

	out []facts.Fact

	// Per-function complexity state, set up by handleCallable around walkForCalls.
	// metrics is nil outside a function/method body walk.
	metrics   *phpBodyMetrics
	loopDepth int
	// scalingDepth counts only input-scaling (unbounded) loops — the Big-O exponent.
	// repeatDepth counts loops that run a non-constant number of times; it differs from
	// scalingDepth only for while(true)/for(;;), which add no factor of n but whose body
	// still runs many times, so a query inside stays an N+1 candidate.
	scalingDepth int
	repeatDepth  int
	selfName     string // enclosing callable's qualified name (for recursion detection)
	selfShort    string // enclosing callable's short name
}

// phpBodyMetrics accumulates per-function complexity signals during the single
// walkForCalls body traversal — mirrors the Go/Ruby/Python extractors.
type phpBodyMetrics struct {
	loopDepth          int             // max loop nesting depth
	loopCount          int             // number of loop constructs
	decisions          int             // decision points (cyclomatic = 1 + decisions)
	callsInLoop        []string        // distinct call targets invoked at loop depth >= 1
	inLoopSeen         map[string]bool // dedup set for callsInLoop
	scalingLoopDepth   int             // max nesting counting only unbounded (input-scaling) loops
	callsInScalingLoop []string        // distinct targets invoked inside a repeating loop (N+1 candidates)
	inScalingSeen      map[string]bool // dedup set for callsInScalingLoop
	recursive          bool            // body directly calls the enclosing callable
	ioDirect           bool            // body makes a direct filesystem/DB/HTTP call
}

// --- program / namespace ---

// walkProgram dispatches each top-level statement. Unknown / control-flow wrappers
// are descended so declarations nested inside conditionals are still captured.
func (w *phpWalker) walkProgram(node *sitter.Node) {
	if node == nil {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkNode(node.Child(i))
	}
}

func (w *phpWalker) walkNode(node *sitter.Node) {
	if node == nil || !node.IsNamed() {
		return
	}
	switch kindOf(node) {
	case "namespace_definition":
		w.handleNamespace(node)
	case "namespace_use_declaration":
		w.handleUse(node)
	case "class_declaration":
		w.handleClassLike(node, facts.SymbolClass)
	case "interface_declaration":
		w.handleClassLike(node, facts.SymbolInterface)
	case "trait_declaration":
		w.handleClassLike(node, facts.SymbolClass) // PHP has no trait symbol kind; flagged via Props["trait"]
	case "enum_declaration":
		w.handleClassLike(node, facts.SymbolEnum)
	case "function_definition":
		w.handleCallable(node, false)
	case "method_declaration":
		w.handleCallable(node, true)
	case "const_declaration":
		w.handleConst(node)
	case "enum_case":
		w.handleEnumCase(node)
	case "property_declaration":
		w.handleProperty(node)
	case "use_declaration":
		w.handleTraitUse(node)
	default:
		// Control-flow / grouping containers: descend so nested declarations and
		// conditionally-defined classes/functions are still captured.
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkNode(node.Child(i))
		}
	}
}

func (w *phpWalker) handleNamespace(node *sitter.Node) {
	name := phpText(node.ChildByFieldName("name"), w.src)
	body := node.ChildByFieldName("body")
	if body == nil {
		// `namespace Foo;` form: applies to the rest of the file.
		w.namespace = name
		return
	}
	// `namespace Foo { ... }` form: scoped to the braced body.
	prev := w.namespace
	w.namespace = name
	w.walkProgram(body)
	w.namespace = prev
}

// --- use imports ---

// handleUse records `use Foo\Bar;` / `use Foo\Bar as Baz;` statements: it populates
// importMap (short/alias name -> FQN) and emits a dependency fact with an imports
// relation so resolve.go can classify it internal/external.
func (w *phpWalker) handleUse(node *sitter.Node) {
	for i := uint(0); i < node.ChildCount(); i++ {
		clause := node.Child(i)
		if kindOf(clause) != "namespace_use_clause" && kindOf(clause) != "namespace_use_group_clause" {
			continue
		}
		fqn := ""
		alias := ""
		for j := uint(0); j < clause.ChildCount(); j++ {
			c := clause.Child(j)
			switch kindOf(c) {
			case "qualified_name", "name", "namespace_name":
				if fqn == "" {
					fqn = strings.TrimPrefix(phpText(c, w.src), "\\")
				}
			}
			if clause.FieldNameForChild(uint32(j)) == "alias" {
				alias = phpText(c, w.src)
			}
		}
		if fqn == "" {
			continue
		}
		short := alias
		if short == "" {
			short = lastNsSegment(fqn)
		}
		w.importMap[short] = fqn

		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindDependency,
			Name:      w.dir + " -> " + fqn,
			File:      w.relFile,
			Line:      line(node),
			Props:     map[string]any{"language": "php"},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: fqn}},
		})
	}
}

// --- classes / interfaces / traits / enums ---

func (w *phpWalker) handleClassLike(node *sitter.Node, kind string) {
	name := phpText(node.ChildByFieldName("name"), w.src)
	if name == "" {
		return
	}
	fqn := w.qualify(name)

	props := map[string]any{
		"symbol_kind": kind,
		"exported":    true,
		"language":    "php",
	}
	if kindOf(node) == "trait_declaration" {
		props["trait"] = true
	}
	if hasModifier(node, "abstract_modifier") {
		props["abstract"] = true
	}
	if hasModifier(node, "final_modifier") {
		props["final"] = true
	}

	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}}
	// extends (base_clause) and implements (class_interface_clause). An interface's
	// `extends` list is also a base_clause. Each parent becomes an implements edge.
	for _, parent := range w.parentRefs(node) {
		rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: parent})
	}

	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      fqn,
		File:      w.relFile,
		Line:      line(node),
		Props:     props,
		Relations: rels,
	})

	w.typeStack = append(w.typeStack, fqn)
	w.walkProgram(node.ChildByFieldName("body"))
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
}

// parentRefs returns the resolved FQNs of every base class / implemented interface
// of a class/interface/enum declaration.
func (w *phpWalker) parentRefs(node *sitter.Node) []string {
	var out []string
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if kindOf(c) != "base_clause" && kindOf(c) != "class_interface_clause" {
			continue
		}
		for j := uint(0); j < c.ChildCount(); j++ {
			gc := c.Child(j)
			switch kindOf(gc) {
			case "name", "qualified_name":
				if ref := w.resolveRef(phpText(gc, w.src)); ref != "" {
					out = append(out, ref)
				}
			}
		}
	}
	return out
}

// handleTraitUse records a `use SomeTrait;` inside a class body as an implements
// edge on the enclosing type, so trait composition counts toward coupling.
func (w *phpWalker) handleTraitUse(node *sitter.Node) {
	if len(w.typeStack) == 0 {
		return
	}
	ownerIdx := w.typeFactIndex()
	if ownerIdx < 0 {
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "name", "qualified_name":
			if ref := w.resolveRef(phpText(c, w.src)); ref != "" {
				w.out[ownerIdx].Relations = append(w.out[ownerIdx].Relations,
					facts.Relation{Kind: facts.RelImplements, Target: ref})
			}
		}
	}
}

// --- functions / methods ---

// handleCallable emits a symbol fact for a function or method and walks its body
// once to collect call / instantiation edges and complexity metrics.
func (w *phpWalker) handleCallable(node *sitter.Node, isMethod bool) {
	name := phpText(node.ChildByFieldName("name"), w.src)
	if name == "" {
		return
	}

	var fullName string
	symbolKind := facts.SymbolFunc
	exported := true
	if isMethod {
		symbolKind = facts.SymbolMethod
		owner := ""
		if len(w.typeStack) > 0 {
			owner = w.typeStack[len(w.typeStack)-1]
		}
		fullName = owner + "::" + name
		exported = methodVisibility(node, w.src) == "public"
	} else {
		fullName = w.qualify(name)
	}

	props := map[string]any{
		"symbol_kind": symbolKind,
		"exported":    exported,
		"language":    "php",
	}
	if isMethod {
		props["visibility"] = methodVisibility(node, w.src)
		if hasModifier(node, "static_modifier") {
			props["static"] = true
		}
		if hasModifier(node, "abstract_modifier") {
			props["abstract"] = true
		}
	}

	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      fullName,
		File:      w.relFile,
		Line:      line(node),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
	ownerIdx := len(w.out) - 1

	// Walk the body once: accumulate RelCalls / RelInstantiates (deduplicated) and
	// compute complexity in the same pass. props is shared by reference with the
	// fact just appended, so writes after the walk update the emitted fact.
	seen := make(map[string]bool)
	w.metrics = &phpBodyMetrics{}
	w.loopDepth = 0
	w.scalingDepth = 0
	w.repeatDepth = 0
	w.selfName = fullName
	w.selfShort = name
	w.walkForCalls(node.ChildByFieldName("body"), ownerIdx, seen)
	props["cyclomatic"] = 1 + w.metrics.decisions
	if w.metrics.loopDepth > 0 {
		props["loop_depth"] = w.metrics.loopDepth
		// Emit the scaling depth (bounded loops discounted) alongside — even when 0 — so
		// the consumer distinguishes "all loops bounded" from "signal absent".
		props["scaling_loop_depth"] = w.metrics.scalingLoopDepth
	}
	if w.metrics.loopCount > 0 {
		props["loop_count"] = w.metrics.loopCount
	}
	if len(w.metrics.callsInLoop) > 0 {
		props["calls_in_loop"] = w.metrics.callsInLoop
		// Emit the N+1 subset alongside — even when EMPTY — so the consumer distinguishes
		// "no call repeats" from "signal absent" and does not fall back to calls_in_loop.
		if w.metrics.callsInScalingLoop == nil {
			w.metrics.callsInScalingLoop = []string{}
		}
		props["calls_in_scaling_loop"] = w.metrics.callsInScalingLoop
	}
	if w.metrics.recursive {
		props["recursive_self"] = true
	}
	if w.metrics.ioDirect {
		props["io_direct"] = true
	}
	w.metrics = nil
}

// walkForCalls recursively scans a callable body for call / instantiation
// expressions and appends edges to the owner fact, counting decision points and
// loops in the same pass. It does not descend into nested named function / class
// definitions — those receive their own owner.
func (w *phpWalker) walkForCalls(node *sitter.Node, ownerIdx int, seen map[string]bool) {
	if node == nil || !node.IsNamed() {
		return
	}

	if w.metrics != nil {
		switch kindOf(node) {
		case "if_statement", "else_if_clause", "case_statement", "catch_clause",
			"conditional_expression", "match_conditional_expression":
			w.metrics.decisions++
		case "binary_expression":
			if isLogicalOp(phpText(node.ChildByFieldName("operator"), w.src)) {
				w.metrics.decisions++
			}
		}
	}

	switch kindOf(node) {
	case "function_definition", "method_declaration", "class_declaration",
		"interface_declaration", "trait_declaration", "enum_declaration":
		return
	case "for_statement", "foreach_statement", "while_statement", "do_statement":
		// A constant-count loop raises loop_depth but not scaling_loop_depth (the Big-O
		// exponent); an infinite loop is discounted from the exponent but still repeats.
		class := phpSyntacticLoopClass(node, w.src)
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
			w.walkForCalls(node.Child(i), ownerIdx, seen)
		}
		w.loopDepth--
		if class.scales() {
			w.scalingDepth--
		}
		if class.repeats() {
			w.repeatDepth--
		}
		return
	case "function_call_expression":
		fn := node.ChildByFieldName("function")
		if fn != nil && (kindOf(fn) == "name" || kindOf(fn) == "qualified_name") {
			target := w.resolveRef(phpText(fn, w.src))
			w.addCall(ownerIdx, seen, target)
			w.recordCallMetrics(target)
			w.recordIODirect(lastNsSegment(phpText(fn, w.src)), phpIOGlobalFuncs)
		}
		w.walkChildrenExcept(node, ownerIdx, seen, fn)
		return
	case "scoped_call_expression":
		scope := node.ChildByFieldName("scope")
		method := node.ChildByFieldName("name")
		if scope != nil && method != nil {
			target := w.resolveRef(phpText(scope, w.src)) + "::" + phpText(method, w.src)
			w.addCall(ownerIdx, seen, target)
			w.recordCallMetrics(target)
			w.recordIODirect(phpText(method, w.src), phpIOMethods)
		}
		w.walkChildrenExcept(node, ownerIdx, seen, method)
		return
	case "member_call_expression":
		// Instance call ($obj->method()). The receiver type is unknown, so the bare
		// method name is recorded — useful for $this->method() self-references and
		// dead-code analysis (mirrors the Ruby extractor's bare-call handling).
		method := node.ChildByFieldName("name")
		if method != nil && kindOf(method) == "name" {
			target := phpText(method, w.src)
			w.addCall(ownerIdx, seen, target)
			w.recordCallMetrics(target)
			// $wpdb->get_results(...) / PDO->fetchAll() — the receiver type is unknown,
			// so a distinctive DB round-trip method name is our only I/O signal.
			w.recordIODirect(target, phpIOMethods)
		}
		w.walkChildrenExcept(node, ownerIdx, seen, method)
		return
	case "object_creation_expression":
		if cls := w.creationClass(node); cls != "" {
			target := w.resolveRef(cls)
			w.addRel(ownerIdx, seen, facts.RelInstantiates, target)
			w.recordCallMetrics(target)
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			w.walkForCalls(node.Child(i), ownerIdx, seen)
		}
		return
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		w.walkForCalls(node.Child(i), ownerIdx, seen)
	}
}

// walkChildrenExcept recurses into every child of node except skip (the already
// consumed callee), so the callee name is not re-counted as a nested reference.
func (w *phpWalker) walkChildrenExcept(node *sitter.Node, ownerIdx int, seen map[string]bool, skip *sitter.Node) {
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if skip != nil && c.StartByte() == skip.StartByte() && c.EndByte() == skip.EndByte() {
			continue
		}
		w.walkForCalls(c, ownerIdx, seen)
	}
}

// creationClass returns the class name of a `new X(...)` expression, or "" when the
// class is dynamic (new $var / new (expr)).
func (w *phpWalker) creationClass(node *sitter.Node) string {
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "name", "qualified_name":
			return phpText(c, w.src)
		}
	}
	return ""
}

// addCall appends a deduplicated RelCalls edge to the owner fact.
func (w *phpWalker) addCall(ownerIdx int, seen map[string]bool, target string) {
	w.addRel(ownerIdx, seen, facts.RelCalls, target)
}

// addRel appends a deduplicated relation of relKind to the owner fact. The dedup
// key includes relKind so a call and an instantiation of the same target coexist.
func (w *phpWalker) addRel(ownerIdx int, seen map[string]bool, relKind, target string) {
	if target == "" || target == "::" {
		return
	}
	key := relKind + " " + target
	if seen[key] {
		return
	}
	seen[key] = true
	w.out[ownerIdx].Relations = append(w.out[ownerIdx].Relations,
		facts.Relation{Kind: relKind, Target: target})
}

// recordCallMetrics flags direct recursion and records calls made inside loops.
func (w *phpWalker) recordCallMetrics(target string) {
	if w.metrics == nil || target == "" {
		return
	}
	if target == w.selfShort || target == w.selfName {
		w.metrics.recursive = true
	}
	if w.loopDepth == 0 {
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
	// candidate. A constant loop (literal-bounded for, foreach over an array literal)
	// excludes its calls; while(true)/for(;;) repeats, so its calls stay candidates even
	// though its depth is discounted from the Big-O exponent.
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

// phpIOGlobalFuncs are global functions that perform a direct filesystem / DB /
// HTTP round-trip. Matched against the trailing segment of a resolved call name,
// so it is not affected by a leading "\" or a namespace prefix.
var phpIOGlobalFuncs = map[string]bool{
	"file_get_contents": true, "file_put_contents": true, "fopen": true,
	"fread": true, "fwrite": true, "fgets": true, "readfile": true,
	"curl_exec": true, "fsockopen": true,
	"mysqli_query": true, "mysqli_connect": true, "mysql_query": true, "pg_query": true,
	"wp_remote_get": true, "wp_remote_post": true, "wp_remote_request": true, "wp_remote_head": true,
}

// phpIOMethods are distinctive instance/static method names that unambiguously
// perform a DB round-trip. The receiver type is unknown at parse time, so only
// names that are I/O across the common drivers are listed — WordPress `$wpdb`
// (get_results/get_row/get_var/get_col), PDO/mysqli (fetch_all/fetchAll/
// fetch_assoc, query, execute). Ambiguous accessors (get/read/load) are excluded.
var phpIOMethods = map[string]bool{
	"get_results": true, "get_row": true, "get_var": true, "get_col": true,
	"fetch_all": true, "fetchAll": true, "fetch_assoc": true,
	"query": true, "execute": true,
}

// recordIODirect flags the current callable as performing direct I/O when name is
// in set. Keyed off the syntactic callee (global-function name or method segment),
// mirroring the Rust/Python extractors' io_direct detection.
func (w *phpWalker) recordIODirect(name string, set map[string]bool) {
	if w.metrics != nil && set[name] {
		w.metrics.ioDirect = true
	}
}

// phpLoopClass classifies a loop by two independent properties: whether it adds a
// factor of n to Big-O (scales) and whether its body runs a non-constant number of
// times (repeats). A constant-count loop — `for ($i = 0; $i < 3; $i++)`, a foreach over
// an array literal or ALL_CAPS constant — does neither. An infinite `for (;;)` /
// `while (true)` does not scale but still repeats. Collapsing the two deletes true
// positives (cache.go v99).
type phpLoopClass int

const (
	phpLoopScaling phpLoopClass = iota
	phpLoopConstant
	phpLoopInfinite
)

func (c phpLoopClass) scales() bool  { return c == phpLoopScaling }
func (c phpLoopClass) repeats() bool { return c != phpLoopConstant }

// phpSyntacticLoopClass classifies a for / foreach / while / do-while statement.
func phpSyntacticLoopClass(node *sitter.Node, src []byte) phpLoopClass {
	switch kindOf(node) {
	case "for_statement":
		cond := node.ChildByFieldName("condition")
		if cond == nil {
			return phpLoopInfinite // for (;;)
		}
		if phpConstantForCondition(cond, src) {
			return phpLoopConstant
		}
	case "foreach_statement":
		if phpConstantIterable(phpForeachIterable(node), src) {
			return phpLoopConstant
		}
	case "while_statement", "do_statement":
		if phpIsTrueCondition(node.ChildByFieldName("condition"), src) {
			return phpLoopInfinite
		}
	}
	return phpLoopScaling
}

// phpForeachIterable returns the iterated expression of a foreach — its first named
// child, the `foreach (<expr> as ...)` collection, which precedes the value variable
// and body.
func phpForeachIterable(node *sitter.Node) *sitter.Node {
	if node.NamedChildCount() == 0 {
		return nil
	}
	return node.NamedChild(0)
}

// phpConstantForCondition reports whether a for's condition compares the loop variable
// against an integer literal (`$i < 3`), so the loop runs a statically fixed number of
// times. A data-derived bound (`$i < $n`, `$i < count($xs)`) is conservatively treated
// as scaling — no genuine O(n) finding is deleted.
func phpConstantForCondition(cond *sitter.Node, src []byte) bool {
	if cond == nil || kindOf(cond) != "binary_expression" {
		return false
	}
	switch phpText(cond.ChildByFieldName("operator"), src) {
	case "<", "<=", ">", ">=", "!=", "<>":
	default:
		return false
	}
	l := cond.ChildByFieldName("left")
	r := cond.ChildByFieldName("right")
	return (l != nil && kindOf(l) == "integer") || (r != nil && kindOf(r) == "integer")
}

// phpConstantIterable reports whether a foreach iterates a compile-time-fixed number of
// times: an array literal (`[1, 2, 3]` / `array(...)`) or an ALL_CAPS constant. A
// variable receiver scales. Mirrors kotlinConstantIterable / pyIterableBounded.
func phpConstantIterable(it *sitter.Node, src []byte) bool {
	if it == nil {
		return false
	}
	switch kindOf(it) {
	case "array_creation_expression":
		return true
	case "name":
		return phpIsScreamingConst(phpText(it, src))
	}
	return false
}

// phpIsScreamingConst reports whether a name is SCREAMING_SNAKE_CASE — a conventional
// PHP compile-time constant, whose iteration count is input-independent.
func phpIsScreamingConst(s string) bool {
	if s == "" {
		return false
	}
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

// phpIsTrueCondition reports whether a loop condition is the literal `true`
// (case-insensitive, PHP-style) — `while (true)`, `do … while (TRUE)`. A named constant
// is not matched. The condition arrives wrapped in parentheses, so strip them first.
func phpIsTrueCondition(cond *sitter.Node, src []byte) bool {
	if cond == nil {
		return false
	}
	t := strings.TrimSpace(phpText(cond, src))
	for strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	return strings.EqualFold(t, "true")
}

// --- constants / enum cases / properties ---

func (w *phpWalker) handleConst(node *sitter.Node) {
	owner := ""
	if len(w.typeStack) > 0 {
		owner = w.typeStack[len(w.typeStack)-1]
	}
	exported := true
	if owner != "" {
		exported = modifierVisibility(node, w.src) == "public"
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		el := node.Child(i)
		if kindOf(el) != "const_element" {
			continue
		}
		name := ""
		for j := uint(0); j < el.ChildCount(); j++ {
			if c := el.Child(j); kindOf(c) == "name" {
				name = phpText(c, w.src)
				break
			}
		}
		if name == "" {
			continue
		}
		full := w.qualify(name)
		if owner != "" {
			full = owner + "::" + name
		}
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: full,
			File: w.relFile,
			Line: line(el),
			Props: map[string]any{
				"symbol_kind": facts.SymbolConstant,
				"exported":    exported,
				"language":    "php",
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
		})
	}
}

func (w *phpWalker) handleEnumCase(node *sitter.Node) {
	if len(w.typeStack) == 0 {
		return
	}
	name := phpText(node.ChildByFieldName("name"), w.src)
	if name == "" {
		return
	}
	owner := w.typeStack[len(w.typeStack)-1]
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: owner + "::" + name,
		File: w.relFile,
		Line: line(node),
		Props: map[string]any{
			"symbol_kind": facts.SymbolConstant,
			"exported":    true,
			"language":    "php",
			"enum_case":   true,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

func (w *phpWalker) handleProperty(node *sitter.Node) {
	if len(w.typeStack) == 0 {
		return
	}
	owner := w.typeStack[len(w.typeStack)-1]
	exported := modifierVisibility(node, w.src) == "public"
	for i := uint(0); i < node.ChildCount(); i++ {
		el := node.Child(i)
		if kindOf(el) != "property_element" {
			continue
		}
		vn := el.ChildByFieldName("name")
		if vn == nil {
			continue
		}
		name := strings.TrimPrefix(phpText(vn, w.src), "$")
		if name == "" {
			continue
		}
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: owner + "::$" + name,
			File: w.relFile,
			Line: line(el),
			Props: map[string]any{
				"symbol_kind": facts.SymbolVariable,
				"exported":    exported,
				"language":    "php",
				"visibility":  modifierVisibility(node, w.src),
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
		})
	}
}

// --- name helpers ---

// qualify joins the current namespace with a simple name to form a FQN.
func (w *phpWalker) qualify(name string) string {
	if w.namespace == "" {
		return name
	}
	return w.namespace + "\\" + name
}

// resolveRef expands a reference using the file's `use` imports: a leading "\" is
// stripped (already FQN); an unqualified or first-segment name matching an import
// alias is expanded to its FQN. Otherwise the raw text is returned and resolve.go's
// qualified+bare matching handles placement. self/static/parent pass through.
func (w *phpWalker) resolveRef(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	switch raw {
	case "self", "static", "parent":
		if len(w.typeStack) > 0 {
			return w.typeStack[len(w.typeStack)-1]
		}
		return raw
	}
	if strings.HasPrefix(raw, "\\") {
		return strings.TrimPrefix(raw, "\\")
	}
	first := raw
	rest := ""
	if i := strings.IndexByte(raw, '\\'); i >= 0 {
		first, rest = raw[:i], raw[i:]
	}
	if fqn, ok := w.importMap[first]; ok {
		return fqn + rest
	}
	return raw
}

// typeFactIndex returns the index into w.out of the innermost enclosing type's
// symbol fact, or -1. It scans backward for the symbol whose Name equals the top
// of typeStack.
func (w *phpWalker) typeFactIndex() int {
	if len(w.typeStack) == 0 {
		return -1
	}
	want := w.typeStack[len(w.typeStack)-1]
	for i := len(w.out) - 1; i >= 0; i-- {
		if w.out[i].Kind == facts.KindSymbol && w.out[i].Name == want {
			return i
		}
	}
	return -1
}

// --- small AST helpers ---

// hasModifier reports whether a declaration node has a direct modifier child of the
// given kind (e.g. "abstract_modifier", "final_modifier", "static_modifier").
func hasModifier(node *sitter.Node, kind string) bool {
	for i := uint(0); i < node.ChildCount(); i++ {
		if kindOf(node.Child(i)) == kind {
			return true
		}
	}
	return false
}

// methodVisibility returns "public" (the default), "protected", or "private" for a
// method_declaration based on its visibility_modifier child.
func methodVisibility(node *sitter.Node, src []byte) string {
	if v := modifierVisibility(node, src); v != "" {
		return v
	}
	return "public"
}

// modifierVisibility returns the visibility_modifier text of a declaration, or "".
func modifierVisibility(node *sitter.Node, src []byte) string {
	for i := uint(0); i < node.ChildCount(); i++ {
		c := node.Child(i)
		if kindOf(c) == "visibility_modifier" {
			return strings.ToLower(strings.TrimSpace(phpText(c, src)))
		}
	}
	return ""
}

// isLogicalOp reports whether op is a short-circuiting boolean operator that adds a
// decision point to cyclomatic complexity.
func isLogicalOp(op string) bool {
	switch op {
	case "&&", "||", "and", "or", "??":
		return true
	}
	return false
}

// lastNsSegment returns the final "\"-separated segment of a namespaced name
// ("App\Models\User" -> "User").
func lastNsSegment(s string) string {
	if i := strings.LastIndex(s, "\\"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// line returns the 1-based start line of a node.
func line(node *sitter.Node) int {
	return int(node.StartPosition().Row) + 1
}

// phpText returns the source text covered by a node (nil-safe).
func phpText(node *sitter.Node, src []byte) string { return tsutil.Text(node, src) }
