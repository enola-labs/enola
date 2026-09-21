package rustextractor

import (
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
)

// extractFileAST parses a Rust file with tree-sitter and emits architectural
// facts: module-directory-scoped symbols (functions, methods, structs, enums,
// traits, type aliases, consts/statics), dependency facts for `use`
// declarations, RelCalls/RelInstantiates relations, per-function cyclomatic
// complexity, and (Axum) route facts. impl-trait observations are returned
// separately (see implPair) because attaching them requires the full, merged
// fact set.
// extractFileAST is the two-value form used by tests; production code calls
// extractFileASTFull to also receive the file's Axum router-builder observations
// (consumed by composeAxumPrefixes in the crate-wide pass).
func extractFileAST(src []byte, relFile string, crates []crateInfo, moduleDirs map[string]bool) ([]facts.Fact, []implPair) {
	ff, impls, _ := extractFileASTFull(src, relFile, crates, moduleDirs)
	return ff, impls
}

func extractFileASTFull(src []byte, relFile string, crates []crateInfo, moduleDirs map[string]bool) ([]facts.Fact, []implPair, []axumBuilder) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(rust.Language())); err != nil {
		return nil, nil, nil
	}

	tree := parser.Parse(src, nil)
	defer tree.Close()
	root := tree.RootNode()

	dir := factpath.Dir(relFile)
	w := &astWalker{
		src:        src,
		relFile:    relFile,
		dir:        dir,
		crateDir:   nearestCrateDir(dir, crates),
		crates:     crates,
		moduleDirs: moduleDirs,
		fileRefIdx: -1,
	}
	w.walkSourceFile(root)

	// A #[cfg(test)] module's calls into production code are collected across
	// the whole file (see enterTestMod) into a single reference-only fact,
	// rather than one per test function, matching the KindTestRef convention
	// used by the Ruby extractor's spec-file handling.
	if len(w.testRefRels) > 0 {
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindTestRef,
			Name:      relFile,
			File:      relFile,
			Props:     map[string]any{"language": "rust"},
			Relations: w.testRefRels,
		})
	}

	w.out = append(w.out, extractAxumRoutes(root, src, relFile, dir)...)
	w.out = append(w.out, extractUtoipaRoutes(root, src, relFile, dir)...)

	return w.out, w.impls, collectAxumBuilders(root, src, relFile)
}

type astWalker struct {
	src      []byte
	relFile  string
	dir      string
	crateDir string

	crates     []crateInfo
	moduleDirs map[string]bool

	// out is the accumulating fact list; impls collects impl-Trait-for-Type
	// observations resolved later by applyImplements.
	out   []facts.Fact
	impls []implPair

	// ownerStack[len-1] indexes into out for the symbol fact currently being
	// constructed. Stored as an index, not a *facts.Fact: a nested item (a
	// local `fn` inside a function body) appends to out too, which can
	// reallocate its backing array and strand a raw pointer.
	ownerStack []int

	// fileRefIdx: index into out of the lazily-created file-scope reference
	// fact (facts.KindFileRef), or -1 until first used. Catches calls/
	// references made in macro content with no enclosing symbol — a
	// macro_rules! template body, or an item-level macro invocation standing
	// in for a whole function (e.g. `ffi_fn! { fn foo() { ... } }`) — which
	// emitEdge would otherwise silently drop for lack of an owner.
	fileRefIdx int

	// implTrait: the trait name of the impl block currently being walked
	// ("" outside any impl, or for a plain inherent impl). Lets handleFunction
	// recognize compilerOrRuntimeInvokedMethods (Drop::drop, Future::poll).
	implTrait string

	// modStack/typeStack hold the enclosing inline-`mod { }` and
	// impl/trait-block names, so a nested declaration's canonical name is
	// "<dir>.<mod1>.<mod2>...<Type>.<name>" — the same qualification scheme
	// the Kotlin/Java extractors use for nested types. methodStack is
	// parallel to typeStack: the set of method names declared directly in the
	// innermost enclosing impl/trait block, used to resolve a same-block
	// sibling call.
	modStack    []string
	typeStack   []string
	methodStack []map[string]bool

	// modFnStack[len-1]: function names declared directly in the enclosing
	// mod/file scope, order-independent. Parallel to modStack.
	modFnStack []map[string]bool

	// modSubmoduleStack[len-1]: body-less `mod foo;` names declared directly
	// in the enclosing mod/file scope. Parallel to modStack.
	modSubmoduleStack []map[string]bool

	// decisions counts cyclomatic decision points in the function/method body
	// currently being walked; saved/restored around nested function items.
	decisions int

	// Loop/IO complexity state, mirroring the Python/Kotlin extractors so the
	// performance analyzer works for Rust too. The walk-time depth
	// counters track the loop nesting at the current node; the fn-prefixed
	// accumulators collect the per-function peak/collection that becomes the
	// symbol's loop_depth/scaling_loop_depth/calls_in_loop/... props. All are
	// saved/zeroed/restored around each function body alongside `decisions`.
	loopDepth    int // syntactic loop nesting (for/while/loop)
	scalingDepth int // nesting of loops with a data-dependent (non-constant) trip count

	fnMaxLoop        int             // peak loopDepth seen in the current function
	fnMaxScaling     int             // peak scalingDepth seen in the current function
	fnLoopCount      int             // number of loop constructs in the current function
	fnCallsInLoop    []string        // resolved callees invoked at loopDepth > 0
	fnCallsInScaling []string        // resolved callees invoked at repeatDepth > 0 (scaling subset)
	fnInLoopSeen     map[string]bool // dedup set for fnCallsInLoop
	fnInScalingSeen  map[string]bool // dedup set for fnCallsInScaling
	fnIODirect       bool            // the current function makes a direct I/O call
	fnRecursive      bool            // the current function calls itself
	fnSelfName       string          // canonical name of the current function (for recursion)

	// importMap maps a `use`-imported simple name to its canonical symbol fact
	// name (e.g. "run" -> "src/helper.run") when the import resolved to a known
	// internal directory, or to "" when it resolved to an external/stdlib crate
	// (imported, but no local fact — a bare call to it must not be resolved).
	// Populated by emitDependency; consulted by resolveCall.
	importMap map[string]string

	// inTestMod is true while walking inside a #[cfg(test)] module's subtree.
	// No symbol facts are emitted there (test functions/fixtures never become
	// production symbols), and calls are redirected into testRefRels instead of
	// the owner stack — see enterTestMod.
	inTestMod bool
	// testRefRels accumulates the (deduplicated) RelCalls edges a #[cfg(test)]
	// module makes into production code, later emitted as a single KindTestRef
	// fact for the file. Built as a plain slice rather than a pointer held into
	// w.out, since appends to w.out elsewhere in the walk can reallocate its
	// backing array and silently orphan writes through a stale pointer.
	testRefRels []facts.Relation
	testRefSeen map[string]bool
}

// calleeForm classifies how a call_expression's callee was written, so the
// walker knows which resolution strategy is safe to apply.
type calleeForm int

const (
	calleeBare    calleeForm = iota // foo() — same-module lexical scope
	calleeSelfRef                   // self.foo() / Self::foo() / Type::foo() (Type == enclosing impl type)
	calleeOther                     // recv.foo() / other::path::foo() — receiver/path type unknown
)

func (w *astWalker) qualify(name string) string {
	parts := make([]string, 0, len(w.modStack)+len(w.typeStack)+1)
	parts = append(parts, w.modStack...)
	parts = append(parts, w.typeStack...)
	parts = append(parts, name)
	return strings.Join(parts, ".")
}

// qualifyMod is like qualify but drops typeStack — used by resolveCall's
// same-scope fallback, so it doesn't mistake a bare call to an unrelated
// top-level function for a call scoped to the caller's enclosing impl block.
func (w *astWalker) qualifyMod(name string) string {
	parts := make([]string, 0, len(w.modStack)+1)
	parts = append(parts, w.modStack...)
	parts = append(parts, name)
	return strings.Join(parts, ".")
}

func (w *astWalker) pushOwner(idx int) { w.ownerStack = append(w.ownerStack, idx) }
func (w *astWalker) popOwner()         { w.ownerStack = w.ownerStack[:len(w.ownerStack)-1] }
func (w *astWalker) currentOwner() *facts.Fact {
	if len(w.ownerStack) == 0 {
		return nil
	}
	return &w.out[w.ownerStack[len(w.ownerStack)-1]]
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

func (w *astWalker) walkSourceFile(root *sitter.Node) {
	w.modFnStack = append(w.modFnStack, collectFnNames(root, w.src))
	w.modSubmoduleStack = append(w.modSubmoduleStack, collectSubmoduleNames(root, w.src))
	w.walkItemsTrackingAttrs(root)
	w.modSubmoduleStack = w.modSubmoduleStack[:len(w.modSubmoduleStack)-1]
	w.modFnStack = w.modFnStack[:len(w.modFnStack)-1]
}

// isKnownFn checks every enclosing scope (function-local, then outward
// through mod/file scope), not just the innermost, so a value-reference
// inside a function body still resolves to an outer mod-level sibling.
func (w *astWalker) isKnownFn(name string) bool {
	for i := len(w.modFnStack) - 1; i >= 0; i-- {
		if w.modFnStack[i][name] {
			return true
		}
	}
	return false
}

// isKnownSubmodule checks every enclosing mod/file scope for a body-less
// `mod name;` declaration, so `use name::item;` (no self::/crate:: prefix)
// is recognized as a reference to that sibling file, not an external crate.
func (w *astWalker) isKnownSubmodule(name string) bool {
	for i := len(w.modSubmoduleStack) - 1; i >= 0; i-- {
		if w.modSubmoduleStack[i][name] {
			return true
		}
	}
	return false
}

// walkItemsTrackingAttrs iterates parent's children like walkChild, but first
// tracks preceding attribute_item siblings so a `#[cfg(test)] mod { ... }` can
// be detected and routed into test mode (enterTestMod) instead of being walked
// as ordinary production code. Attributes are separate sibling nodes in the
// grammar, not children of the item they annotate, so this bookkeeping can't
// live in walkChild itself. Used at file scope and inside mod bodies — the two
// places a Rust test module can appear; impl/trait bodies never contain one.
func (w *astWalker) walkItemsTrackingAttrs(parent *sitter.Node) {
	sawCfgTest := false
	sawTestAttr := false
	for i := uint(0); i < uint(parent.ChildCount()); i++ {
		c := parent.Child(i)
		if kindOf(c) == "attribute_item" {
			text := nodeText(c, w.src)
			if isCfgTestAttribute(text) {
				sawCfgTest = true
			}
			if isTestAttribute(text) {
				sawTestAttr = true
			}
			continue
		}
		switch {
		case kindOf(c) == "mod_item" && sawCfgTest:
			w.enterTestMod(c)
		case kindOf(c) == "function_item" && sawTestAttr:
			saved := w.inTestMod
			w.inTestMod = true
			w.walkTestItem(c)
			w.inTestMod = saved
		default:
			w.walkChild(c)
		}
		sawCfgTest = false
		sawTestAttr = false
	}
}

// isTestAttribute reports whether an attribute_item marks its function as a
// test (#[test], #[tokio::test], #[wasm_bindgen_test]) — catching a #[test]
// fn wherever it lives (a plain tests.rs file, an un-gated mod tests {}),
// not just inside a #[cfg(test)] module.
func isTestAttribute(text string) bool {
	inner := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(text), "#["), "]")
	if i := strings.IndexAny(inner, "(["); i >= 0 {
		inner = inner[:i]
	}
	inner = strings.TrimSpace(inner)
	return inner == "test" || strings.HasSuffix(inner, "::test") || inner == "wasm_bindgen_test"
}

// isCfgTestAttribute reports whether an attribute_item's raw text is a
// `#[cfg(test)]`-shaped gate. A coarse substring check (rather than requiring
// an exact `cfg(test)` match) also catches compound forms like
// `#[cfg(all(test, feature = "x"))]`, at the cost of a vanishingly unlikely
// false positive (an attribute mentioning both words for an unrelated reason).
func isCfgTestAttribute(text string) bool {
	return strings.Contains(text, "cfg") && strings.Contains(text, "test")
}

// enterTestMod walks a #[cfg(test)] module's body in test mode: no symbol
// facts are emitted for anything declared inside (so test helpers/fixtures
// never pollute production symbol/complexity stats), and calls made from test
// function bodies are credited to the file's single KindTestRef fact instead —
// letting the dead-code detector see that a production symbol is exercised by
// a test without treating the test code itself as production surface.
func (w *astWalker) enterTestMod(node *sitter.Node) {
	body := node.ChildByFieldName("body")
	if body == nil {
		return
	}
	saved := w.inTestMod
	w.inTestMod = true
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		w.walkTestItem(body.Child(i))
	}
	w.inTestMod = saved
}

// walkTestItem dispatches one item inside a #[cfg(test)] module. Only
// function bodies are walked (for the calls they make into production code);
// struct/enum/trait/impl/const/static declarations are test-only fixtures and
// are skipped entirely — no fact, no further descent, since walking their
// internals for calls into production code is low-value here. A nested `mod`
// stays in test mode (module qualification doesn't matter — no symbol names
// are being built), and `use` is still processed normally (e.g. `use super::*;`
// contributes nothing resolvable to importMap, but is harmless to record).
func (w *astWalker) walkTestItem(c *sitter.Node) {
	switch kindOf(c) {
	case "function_item":
		if body := c.ChildByFieldName("body"); body != nil {
			w.walkForCalls(body)
		}
	case "mod_item":
		if body := c.ChildByFieldName("body"); body != nil {
			for i := uint(0); i < uint(body.ChildCount()); i++ {
				w.walkTestItem(body.Child(i))
			}
		}
	case "use_declaration":
		w.handleUse(c)
	case "impl_item", "struct_item", "enum_item", "trait_item",
		"type_item", "const_item", "static_item", "function_signature_item":
		// Test-only fixtures: intentionally not emitted or descended into.
	default:
		w.walkForCalls(c)
	}
}

// walkChild dispatches an item declaration to its own handler (creating a new
// owner/type scope as needed) or, for anything else, falls through to
// walkForCalls so calls/decisions nested in ordinary statements are still
// found. Used uniformly at file scope, inside mod/impl/trait bodies, and for
// item declarations nested inside a function body (Rust allows local `fn`,
// `struct`, etc.).
func (w *astWalker) walkChild(c *sitter.Node) {
	switch kindOf(c) {
	case "use_declaration":
		w.handleUse(c)
	case "extern_crate_declaration":
		w.handleExternCrate(c)
	case "mod_item":
		w.handleMod(c)
	case "struct_item":
		w.handleStruct(c)
	case "enum_item":
		w.handleEnum(c)
	case "trait_item":
		w.handleTrait(c)
	case "impl_item":
		w.handleImpl(c)
	case "function_item":
		w.handleFunction(c)
	case "function_signature_item":
		w.handleFunctionSignature(c)
	case "type_item":
		w.handleTypeAlias(c)
	case "const_item":
		w.handleConstOrStatic(c, facts.SymbolConstant)
	case "static_item":
		w.handleConstOrStatic(c, facts.SymbolVariable)
	default:
		w.walkForCalls(c)
	}
}

func (w *astWalker) handleMod(node *sitter.Node) {
	body := node.ChildByFieldName("body")
	if body == nil {
		return // `mod foo;` declares another file, parsed independently.
	}
	name := ""
	if n := node.ChildByFieldName("name"); n != nil {
		name = nodeText(n, w.src)
	}
	w.modStack = append(w.modStack, name)
	w.modFnStack = append(w.modFnStack, collectFnNames(body, w.src))
	w.modSubmoduleStack = append(w.modSubmoduleStack, collectSubmoduleNames(body, w.src))
	w.walkItemsTrackingAttrs(body)
	w.modSubmoduleStack = w.modSubmoduleStack[:len(w.modSubmoduleStack)-1]
	w.modFnStack = w.modFnStack[:len(w.modFnStack)-1]
	w.modStack = w.modStack[:len(w.modStack)-1]
}

func isExported(node *sitter.Node) bool {
	return findChildByKind(node, "visibility_modifier") != nil
}

func (w *astWalker) handleStruct(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolStruct,
			"exported":    isExported(node),
			"language":    "rust",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
	ownerIdx := len(w.out) - 1
	w.pushOwner(ownerIdx)
	w.scanAttributeFnRefs(node.ChildByFieldName("body"))
	w.popOwner()
}

func (w *astWalker) handleEnum(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolEnum,
			"exported":    isExported(node),
			"language":    "rust",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
	ownerIdx := len(w.out) - 1
	w.pushOwner(ownerIdx)
	w.scanAttributeFnRefs(node.ChildByFieldName("body"))
	w.popOwner()
}

func (w *astWalker) handleTrait(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolInterface,
			"exported":    isExported(node),
			"language":    "rust",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})

	body := node.ChildByFieldName("body")
	w.pushType(name, collectFnNames(body, w.src))
	if body != nil {
		for i := uint(0); i < uint(body.ChildCount()); i++ {
			w.walkChild(body.Child(i))
		}
	}
	w.popType()
}

func (w *astWalker) handleImpl(node *sitter.Node) {
	typeNode := node.ChildByFieldName("type")
	typeName := simpleTypeName(typeNode, w.src)
	if typeName == "" {
		return
	}
	traitName := ""
	if traitNode := node.ChildByFieldName("trait"); traitNode != nil {
		if traitName = simpleTypeName(traitNode, w.src); traitName != "" {
			w.impls = append(w.impls, implPair{
				typeName:  w.dir + "." + w.qualify(typeName),
				traitName: traitName,
			})
		}
	}

	body := node.ChildByFieldName("body")
	w.pushType(typeName, collectFnNames(body, w.src))
	savedImplTrait := w.implTrait
	w.implTrait = traitName
	if body != nil {
		for i := uint(0); i < uint(body.ChildCount()); i++ {
			w.walkChild(body.Child(i))
		}
	}
	w.implTrait = savedImplTrait
	w.popType()
}

func (w *astWalker) handleFunction(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)

	symbolKind := facts.SymbolFunc
	if len(w.typeStack) > 0 {
		symbolKind = facts.SymbolMethod
	}

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    isExported(node),
			"language":    "rust",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	if len(w.typeStack) > 0 {
		f.SetProp("receiver", w.typeStack[len(w.typeStack)-1])
		if !hasSelfParam(node.ChildByFieldName("parameters")) {
			f.SetProp("static", true)
		}
	}
	if compilerInvokedTraitMethods[w.implTrait][name] {
		f.SetProp("override", true)
	}

	w.out = append(w.out, f)
	ownerIdx := len(w.out) - 1
	w.pushOwner(ownerIdx)

	// Save/zero the cyclomatic + loop/IO state around this body so a nested
	// `fn` item does not leak its metrics into the enclosing function.
	savedDecisions := w.decisions
	savedLoopDepth, savedScaling := w.loopDepth, w.scalingDepth
	savedMaxLoop, savedMaxScaling, savedLoopCount := w.fnMaxLoop, w.fnMaxScaling, w.fnLoopCount
	savedCIL, savedCIS := w.fnCallsInLoop, w.fnCallsInScaling
	savedILSeen, savedISSeen := w.fnInLoopSeen, w.fnInScalingSeen
	savedIO, savedRec, savedSelf := w.fnIODirect, w.fnRecursive, w.fnSelfName
	w.decisions = 0
	w.loopDepth, w.scalingDepth = 0, 0
	w.fnMaxLoop, w.fnMaxScaling, w.fnLoopCount = 0, 0, 0
	w.fnCallsInLoop, w.fnCallsInScaling = nil, nil
	w.fnInLoopSeen, w.fnInScalingSeen = nil, nil
	w.fnIODirect, w.fnRecursive = false, false
	w.fnSelfName = f.Name

	if body := node.ChildByFieldName("body"); body != nil {
		w.modFnStack = append(w.modFnStack, collectFnNames(body, w.src))
		w.walkForCalls(body)
		w.modFnStack = w.modFnStack[:len(w.modFnStack)-1]
	}
	w.out[ownerIdx].SetProp("cyclomatic", 1+w.decisions)
	// Emit the loop/IO props using the identical string keys the Python/Kotlin
	// extractors and internal/perf already consume. The scaling values and
	// call collections are emitted whenever the function contains any loop (even
	// when the scaling subset is empty) so the consumer can tell "all bounded"
	// from "no loop signal at all".
	if w.fnLoopCount > 0 {
		w.out[ownerIdx].SetProp("loop_depth", w.fnMaxLoop)
		w.out[ownerIdx].SetProp("loop_count", w.fnLoopCount)
		w.out[ownerIdx].SetProp("scaling_loop_depth", w.fnMaxScaling)
		w.out[ownerIdx].SetProp("calls_in_loop", nonNilStrings(w.fnCallsInLoop))
		w.out[ownerIdx].SetProp("calls_in_scaling_loop", nonNilStrings(w.fnCallsInScaling))
	}
	if w.fnRecursive {
		w.out[ownerIdx].SetProp("recursive_self", true)
	}
	if w.fnIODirect {
		w.out[ownerIdx].SetProp("io_direct", true)
	}

	w.decisions = savedDecisions
	w.loopDepth, w.scalingDepth = savedLoopDepth, savedScaling
	w.fnMaxLoop, w.fnMaxScaling, w.fnLoopCount = savedMaxLoop, savedMaxScaling, savedLoopCount
	w.fnCallsInLoop, w.fnCallsInScaling = savedCIL, savedCIS
	w.fnInLoopSeen, w.fnInScalingSeen = savedILSeen, savedISSeen
	w.fnIODirect, w.fnRecursive, w.fnSelfName = savedIO, savedRec, savedSelf

	w.popOwner()
}

// nonNilStrings returns s, or an empty (non-nil) slice when s is nil, so a
// prop marked "present but empty" survives JSON round-trips as [] not null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// handleFunctionSignature emits a symbol fact for a trait method declared
// without a body (`fn foo(&self);`) — a real part of the trait's contract,
// but with nothing to walk for calls/complexity.
func (w *astWalker) handleFunctionSignature(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	symbolKind := facts.SymbolFunc
	if len(w.typeStack) > 0 {
		symbolKind = facts.SymbolMethod
	}
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    isExported(node),
			"language":    "rust",
			"cyclomatic":  1,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	if len(w.typeStack) > 0 {
		f.SetProp("receiver", w.typeStack[len(w.typeStack)-1])
	}
	w.out = append(w.out, f)
}

func (w *astWalker) handleTypeAlias(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolType,
			"exported":    isExported(node),
			"language":    "rust",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

func (w *astWalker) handleConstOrStatic(node *sitter.Node, symbolKind string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.dir + "." + w.qualify(name),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": symbolKind,
			"exported":    isExported(node),
			"language":    "rust",
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	}
	w.out = append(w.out, f)
	w.pushOwner(len(w.out) - 1)
	if valueNode := node.ChildByFieldName("value"); valueNode != nil {
		w.walkForCalls(valueNode)
	}
	w.popOwner()
}

func (w *astWalker) handleUse(node *sitter.Node) {
	argNode := node.ChildByFieldName("argument")
	if argNode == nil {
		return
	}
	line := int(node.StartPosition().Row) + 1
	for _, segs := range collectUseItems(argNode, w.src, nil) {
		w.emitDependency(segs, line)
	}
}

func (w *astWalker) handleExternCrate(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	w.emitDependency([]string{nodeText(nameNode, w.src)}, int(node.StartPosition().Row)+1)
}

// emitDependency classifies a `use`/`extern crate` path, emits the dependency
// fact, and records the imported simple name in importMap so a later bare call
// to it resolves (see resolveCall).
func (w *astWalker) emitDependency(segs []string, line int) {
	if len(segs) == 0 || segs[len(segs)-1] == "" {
		return
	}
	var target, source string
	switch {
	case segs[0] != "self" && segs[0] != "super" && segs[0] != "crate" && w.isKnownSubmodule(segs[0]):
		// An unprefixed `use foo::bar;` where "foo" is a body-less `mod foo;`
		// declared in this same file/mod scope is a reference to that sibling
		// file, not an external crate — classifyUsePath can't see that on its
		// own since foo.rs shares its parent's directory (no subdirectory to
		// find in moduleDirs).
		target, source = joinRustPath(w.dir, segs, w.moduleDirs), "internal"
	default:
		target, source = classifyUsePath(segs, w.dir, w.crateDir, w.crates, w.moduleDirs)
	}
	raw := strings.Join(segs, "::")
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindDependency,
		Name: w.dir + " -> " + raw,
		File: w.relFile,
		Line: line,
		Props: map[string]any{
			"language": "rust",
			"source":   source,
		},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}},
	})
	w.recordImportMapping(segs, target, source)
}

// recordImportMapping maps the imported item's simple name — the last "::"
// segment, e.g. "run" in "self::helper::run" — to its canonical symbol fact
// name (target + "." + name) when the import resolved internally, or to ""
// when it resolved to an external/stdlib crate (imported, but no local fact
// exists, so a bare call to it must not be resolved). A wildcard import
// (`use foo::*;`, last segment "*") brings an unknown set of names into scope
// and is skipped — recording nothing is safe; it just leaves those calls
// unresolved rather than guessing.
func (w *astWalker) recordImportMapping(segs []string, target, source string) {
	last := segs[len(segs)-1]
	if last == "" || last == "*" {
		return
	}
	if w.importMap == nil {
		w.importMap = make(map[string]string)
	}
	if source == "internal" {
		w.importMap[last] = target + "." + last
	} else {
		w.importMap[last] = ""
	}
}

// collectUseItems expands a `use` declaration's argument tree into one
// "::"-joined segment list per leaf path, so `use std::{fmt, collections::HashMap};`
// yields two independent paths.
func collectUseItems(node *sitter.Node, src []byte, prefix []string) [][]string {
	if node == nil {
		return nil
	}
	switch kindOf(node) {
	case "identifier", "self", "super", "crate", "metavariable":
		return [][]string{appendSegs(prefix, nodeText(node, src))}
	case "scoped_identifier":
		return [][]string{appendSegs(prefix, strings.Split(nodeText(node, src), "::")...)}
	case "use_as_clause":
		if p := node.ChildByFieldName("path"); p != nil {
			return collectUseItems(p, src, prefix)
		}
	case "use_wildcard":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			if c := node.Child(i); c.IsNamed() {
				items := collectUseItems(c, src, prefix)
				for i2 := range items {
					items[i2] = append(items[i2], "*")
				}
				return items
			}
		}
	case "scoped_use_list":
		newPrefix := prefix
		if p := node.ChildByFieldName("path"); p != nil {
			newPrefix = appendSegs(prefix, strings.Split(nodeText(p, src), "::")...)
		}
		if l := node.ChildByFieldName("list"); l != nil {
			return collectUseItems(l, src, newPrefix)
		}
	case "use_list":
		var out [][]string
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			if c := node.Child(i); c.IsNamed() {
				out = append(out, collectUseItems(c, src, prefix)...)
			}
		}
		return out
	}
	return nil
}

func appendSegs(prefix []string, segs ...string) []string {
	out := make([]string, 0, len(prefix)+len(segs))
	out = append(out, prefix...)
	out = append(out, segs...)
	return out
}

// walkForCalls recursively scans a subtree for cyclomatic decision points and
// call_expression nodes, attributing calls/instantiations to the current
// owner. It dispatches nested item declarations (a local `fn`/`struct`/etc.
// inside a function body, which Rust allows) back through walkChild so they
// get their own owner/type scope instead of being treated as part of the
// enclosing function's body.
func (w *astWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "if_expression", "match_arm", "try_expression":
		w.decisions++
	case "while_expression", "for_expression", "loop_expression":
		w.decisions++
		w.walkLoop(node)
		return // walkLoop brackets its own child recursion with depth tracking
	case "binary_expression":
		if rustBooleanOp(node) {
			w.decisions++
		}
	case "call_expression":
		w.handleCallExpression(node)
	case "struct_expression":
		w.handleStructExpression(node)
	case "token_tree":
		w.scanTokenTreeCalls(node)
	case "arguments", "array_expression":
		w.scanArgumentReferences(node)
	case "field_initializer":
		w.emitValueReference(node.ChildByFieldName("value"))
	case "reference_expression":
		w.emitValueReference(node.ChildByFieldName("value"))
	case "scoped_identifier":
		w.scanScopedVariantReference(node)
		return // path segments hold nothing else worth walking
	case "identifier":
		if name := nodeText(node, w.src); isCapitalized(name) {
			w.emitEdge(facts.RelInstantiates, name)
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkChild(node.Child(i))
	}
}

// walkLoop brackets the child recursion of a for/while/loop node with loop-depth
// tracking, mirroring the Python extractor's walkForCalls loop handling. It
// peak-tracks the per-function loop/scaling depth *before* descending, increments
// the walk-time depth counters, recurses, then restores them. A "bounded" loop
// (a constant-trip `for` over a literal range/array, or an infinite `loop {}`/
// `while true`) contributes to loop_depth but not to scaling_loop_depth: it adds
// no data-dependent Big-O factor, so an in-loop call is not counted as N+1.
func (w *astWalker) walkLoop(node *sitter.Node) {
	bounded := w.rustLoopBounded(node)

	w.fnLoopCount++
	if w.loopDepth+1 > w.fnMaxLoop {
		w.fnMaxLoop = w.loopDepth + 1
	}
	if !bounded && w.scalingDepth+1 > w.fnMaxScaling {
		w.fnMaxScaling = w.scalingDepth + 1
	}

	w.loopDepth++
	if !bounded {
		w.scalingDepth++
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkChild(node.Child(i))
	}
	w.loopDepth--
	if !bounded {
		w.scalingDepth--
	}
}

// rustLoopBounded reports whether a loop has a constant (data-independent) trip
// count and so introduces no Big-O factor. A `for` over a literal integer range
// (`0..10`) or an array/tuple literal is constant; `loop {}` and `while true`
// are infinite (their exponent doesn't scale with input size — a daemon poll,
// not an N+1); everything else (`for x in items`, `while cond`) scales.
func (w *astWalker) rustLoopBounded(node *sitter.Node) bool {
	switch kindOf(node) {
	case "for_expression":
		return rustConstIterable(node.ChildByFieldName("value"), w.src)
	case "while_expression":
		return rustCondIsTrue(node.ChildByFieldName("condition"), w.src)
	case "loop_expression":
		return true
	}
	return false
}

// rustConstIterable reports whether a `for` loop's iterable has a compile-time
// constant length: a literal integer range (`0..10`) or an array/tuple literal.
// A range with a non-literal bound (`0..items.len()`) or any other expression
// (`for x in items`) scales with the data and is not constant.
func rustConstIterable(val *sitter.Node, src []byte) bool {
	if val == nil {
		return false
	}
	switch kindOf(val) {
	case "range_expression":
		return rustRangeLiteralBounds(val, src)
	case "array_expression", "tuple_expression":
		return true
	}
	return false
}

// rustRangeLiteralBounds reports whether every bound operand of a range
// expression is an integer literal (so the trip count is a constant).
func rustRangeLiteralBounds(node *sitter.Node, src []byte) bool {
	saw := false
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "..", "..=", "...":
			continue // the range operator itself
		case "integer_literal":
			saw = true
		default:
			return false // a non-literal bound (e.g. items.len()) — scales
		}
	}
	return saw
}

// rustCondIsTrue reports whether a `while` condition is the literal `true`.
func rustCondIsTrue(cond *sitter.Node, src []byte) bool {
	if cond == nil {
		return false
	}
	return kindOf(cond) == "true" || nodeText(cond, src) == "true"
}

// scanTokenTreeCalls finds calls inside a macro's unparsed token_tree body
// (e.g. `bail!(f(x))`), invisible to handleCallExpression otherwise: an
// `identifier` immediately followed by a parenthesized token_tree sibling.
func (w *astWalker) scanTokenTreeCalls(node *sitter.Node) {
	n := node.ChildCount()
	for i := uint(0); i < n; i++ {
		c := node.Child(i)
		if kindOf(c) != "identifier" {
			continue
		}
		var next *sitter.Node
		if i+1 < n {
			next = node.Child(i + 1)
		}
		if next != nil && kindOf(next) == "token_tree" && next.ChildCount() > 0 && kindOf(next.Child(0)) == "(" {
			name := nodeText(c, w.src)
			if isCapitalized(name) {
				w.emitEdge(facts.RelInstantiates, name)
				continue
			}
			if i > 0 && kindOf(node.Child(i-1)) == "." {
				w.emitEdge(facts.RelCalls, name)
				continue
			}
			if target := w.resolveCall(name); target != "" {
				w.emitEdge(facts.RelCalls, target)
			}
			continue
		}
		// Not applied as a call: may still be a function passed by name as a
		// value nested inside a macro's own argument, e.g. Box::new(f) inside
		// vec![...]. Skip path segments (preceded/followed by "."/"::") and
		// attribute-style `key = value` pairs, already handled by scanAttribute.
		if i > 0 {
			if pk := kindOf(node.Child(i - 1)); pk == "." || pk == "::" {
				continue
			}
		}
		if next != nil {
			if nk := kindOf(next); nk == "::" || nk == "=" {
				continue
			}
		}
		if target := w.resolveValueReference(nodeText(c, w.src)); target != "" {
			w.emitEdge(facts.RelCalls, target)
		}
	}
}

// scanArgumentReferences checks each element (a call argument or array
// literal item) for a function passed by name (`.map_err(f)`, `&[f, g]`
// dispatch tables), which produces no call_expression since it isn't applied.
func (w *astWalker) scanArgumentReferences(node *sitter.Node) {
	for i := uint(0); i < node.NamedChildCount(); i++ {
		w.emitValueReference(node.NamedChild(i))
	}
}

// emitValueReference treats a bare identifier/generic-function/scoped-path
// as a reference when passed as a value rather than called (`&f`, `diff_fn:
// f`). A nested call like `foo(f())` is unaffected — f() isn't a leaf shape.
func (w *astWalker) emitValueReference(v *sitter.Node) {
	if v == nil {
		return
	}
	switch kindOf(v) {
	case "identifier":
		name := nodeText(v, w.src)
		if isCapitalized(name) {
			return
		}
		if target := w.resolveValueReference(name); target != "" {
			w.emitEdge(facts.RelCalls, target)
		}
	case "generic_function":
		w.emitValueReference(v.ChildByFieldName("function"))
	case "scoped_identifier":
		if nameNode := v.ChildByFieldName("name"); nameNode != nil {
			w.emitEdge(facts.RelCalls, nodeText(nameNode, w.src))
		}
	}
}

// handleStructExpression emits a RelInstantiates edge for a named struct
// literal (`Foo { field: value }`), the idiomatic Rust construction form —
// unlike a tuple-call construction (`Foo()`), it isn't a call_expression, so
// it needs its own case. Field values are walked for nested calls by the
// normal child recursion in walkForCalls, not here.
func (w *astWalker) handleStructExpression(node *sitter.Node) {
	name := simpleTypeName(node.ChildByFieldName("name"), w.src)
	if name == "" {
		return
	}
	w.emitEdge(facts.RelInstantiates, name)
}

func (w *astWalker) handleCallExpression(node *sitter.Node) {
	fn := node.ChildByFieldName("function")
	if rustIsIODirectCall(fn, w.src) {
		w.fnIODirect = true
	}
	name, form := w.calleeTrailing(fn)
	if name == "" {
		return
	}
	if isCapitalized(name) {
		w.emitEdge(facts.RelInstantiates, name)
		return
	}
	// Type::new() constructs Type via an associated fn, not a literal —
	// record it, since nothing else would reference Type from this call.
	if seg := scopedCalleeTypePrefix(fn, w.src); seg != "" {
		w.emitEdge(facts.RelInstantiates, seg)
	}
	switch form {
	case calleeBare:
		if target := w.resolveCall(name); target != "" {
			w.emitEdge(facts.RelCalls, target)
		}
	case calleeSelfRef:
		if methods := w.currentMethods(); methods[name] {
			w.emitEdge(facts.RelCalls, w.dir+"."+w.qualify(name))
			break
		}
		// Not a sibling of this impl block (another impl block, a trait
		// default) — still unambiguously a method call, so fall back.
		w.emitEdge(facts.RelCalls, name)
	case calleeOther:
		// Receiver/path type is unknown without full type inference. Emitting
		// the bare member name still lets short-name dead-code matching mark
		// the (unqualified) target used, mirroring the Kotlin extractor's
		// navigation_expression fallback.
		w.emitEdge(facts.RelCalls, name)
	}
}

// emitEdge records a relation either on the current production owner, or —
// while walking a #[cfg(test)] module (w.inTestMod) — as a deduplicated
// RelCalls reference into testRefRels. KindTestRef carries only RelCalls (per
// its doc comment), so a would-be RelInstantiates from a test still just
// proves the target is used, not constructed for real. With no owner and not
// in test mode, it's file-scope macro content (see fileRefIdx) — recorded
// there instead of dropped.
func (w *astWalker) emitEdge(kind, target string) {
	if w.inTestMod {
		if w.testRefSeen == nil {
			w.testRefSeen = make(map[string]bool)
		}
		if w.testRefSeen[target] {
			return
		}
		w.testRefSeen[target] = true
		w.testRefRels = append(w.testRefRels, facts.Relation{Kind: facts.RelCalls, Target: target})
		return
	}
	if kind == facts.RelCalls {
		w.recordCallMetrics(target)
	}
	owner := w.currentOwner()
	if owner == nil {
		idx := w.ensureFileRefFact()
		w.out[idx].Relations = append(w.out[idx].Relations, facts.Relation{Kind: kind, Target: target})
		return
	}
	owner.Relations = append(owner.Relations, facts.Relation{Kind: kind, Target: target})
}

// recordCallMetrics attributes a resolved production call to the current
// function's loop/recursion metrics: it feeds calls_in_loop (any enclosing
// loop), the calls_in_scaling_loop subset (a data-dependent loop only), and
// recursive_self (a call whose target is the function itself).
func (w *astWalker) recordCallMetrics(target string) {
	if w.fnSelfName != "" && target == w.fnSelfName {
		w.fnRecursive = true
	}
	if w.loopDepth > 0 {
		if w.fnInLoopSeen == nil {
			w.fnInLoopSeen = make(map[string]bool)
		}
		if !w.fnInLoopSeen[target] {
			w.fnInLoopSeen[target] = true
			w.fnCallsInLoop = append(w.fnCallsInLoop, target)
		}
	}
	if w.scalingDepth > 0 {
		if w.fnInScalingSeen == nil {
			w.fnInScalingSeen = make(map[string]bool)
		}
		if !w.fnInScalingSeen[target] {
			w.fnInScalingSeen[target] = true
			w.fnCallsInScaling = append(w.fnCallsInScaling, target)
		}
	}
}

// ensureFileRefFact returns the index of this file's lazily-created
// file-scope reference fact (facts.KindFileRef), creating it on first use.
func (w *astWalker) ensureFileRefFact() int {
	if w.fileRefIdx < 0 {
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindFileRef,
			Name:  w.relFile,
			File:  w.relFile,
			Props: map[string]any{"language": "rust"},
		})
		w.fileRefIdx = len(w.out) - 1
	}
	return w.fileRefIdx
}

// attrFnRefKeys: field-attribute options whose value names a function —
// serde's #[serde(default = "some_fn")] (string), clap's
// #[arg(value_parser = some_fn)] (bare path), or the merge crate's
// #[merge(strategy = mod::path)] (scoped path) — resolved by that macro.
var attrFnRefKeys = map[string]bool{
	"default": true, "skip_serializing_if": true,
	"serialize_with": true, "deserialize_with": true, "with": true,
	"value_parser": true, "strategy": true, "schema_with": true,
}

// attrFnRefMacros: attribute macro names worth scanning for attrFnRefKeys.
var attrFnRefMacros = map[string]bool{"serde": true, "arg": true, "merge": true, "schemars": true}

// scanAttributeFnRefs walks a struct/enum body for #[serde(...)]/#[arg(...)]
// attributes referencing a function by name.
func (w *astWalker) scanAttributeFnRefs(body *sitter.Node) {
	if body == nil {
		return
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if kindOf(n) == "attribute" {
			w.scanAttribute(n)
			// Beyond the curated key=value macros above, an attribute can embed
			// an ordinary call — thiserror's #[error("{}", helper(x))] — using
			// the exact same flattened shape as a macro invocation's token_tree,
			// so the same scan applies regardless of which macro this is.
			if tree := findChildByKind(n, "token_tree"); tree != nil {
				w.scanTokenTreeCalls(tree)
			}
			return
		}
		for i := uint(0); i < uint(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(body)
}

// scanAttribute scans one attribute's token_tree for `key = value` pairs
// keyed by attrFnRefKeys, where value is a string literal or a bare path.
func (w *astWalker) scanAttribute(attr *sitter.Node) {
	if attr.NamedChildCount() == 0 || !attrFnRefMacros[nodeText(attr.NamedChild(0), w.src)] {
		return
	}
	tree := findChildByKind(attr, "token_tree")
	if tree == nil {
		return
	}
	n := tree.ChildCount()
	for i := uint(0); i+2 < n; i++ {
		if !attrFnRefKeys[nodeText(tree.Child(i), w.src)] || kindOf(tree.Child(i+1)) != "=" {
			continue
		}
		name := ""
		switch v := tree.Child(i + 2); kindOf(v) {
		case "string_literal":
			if content := findChildByKind(v, "string_content"); content != nil {
				name = nodeText(content, w.src)
				if idx := strings.LastIndex(name, "::"); idx >= 0 {
					name = name[idx+2:]
				}
			}
		case "identifier":
			// A scoped path (mod::path::fn) is flattened into separate
			// identifier/"::" siblings inside a macro-like token_tree, not a
			// single scoped_identifier node — walk to the last segment.
			j := i + 2
			for j+2 < n && kindOf(tree.Child(j+1)) == "::" && kindOf(tree.Child(j+2)) == "identifier" {
				j += 2
			}
			name = nodeText(tree.Child(j), w.src)
		case "scoped_identifier":
			name = nodeText(v, w.src)
			if idx := strings.LastIndex(name, "::"); idx >= 0 {
				name = name[idx+2:]
			}
		}
		if name == "" {
			continue
		}
		w.emitEdge(facts.RelCalls, name)
	}
}

// resolveCall maps a bare call name to a canonical symbol fact name, in order
// of preference: a sibling method of the enclosing impl/trait block, a name
// brought into scope by a `use` import (internal target, or "" to suppress an
// external/stdlib one), or a same-directory top-level function as the final
// fallback.
func (w *astWalker) resolveCall(name string) string {
	if methods := w.currentMethods(); methods[name] {
		return w.dir + "." + w.qualify(name)
	}
	if target, ok := w.importMap[name]; ok {
		return target
	}
	if rustBuiltins[name] {
		return ""
	}
	return w.dir + "." + w.qualifyMod(name)
}

// resolveValueReference is resolveCall without the final directory guess,
// since a bare value is more often a variable than a function name.
func (w *astWalker) resolveValueReference(name string) string {
	if methods := w.currentMethods(); methods[name] {
		return w.dir + "." + w.qualify(name)
	}
	if w.isKnownFn(name) {
		return w.dir + "." + w.qualifyMod(name)
	}
	if target, ok := w.importMap[name]; ok {
		return target
	}
	return ""
}

// calleeTrailing extracts a call_expression's callee simple name and reports
// how it was written, so handleCallExpression knows which resolution strategy
// applies.
func (w *astWalker) calleeTrailing(fn *sitter.Node) (string, calleeForm) {
	if fn == nil {
		return "", calleeOther
	}
	switch kindOf(fn) {
	case "identifier":
		return nodeText(fn, w.src), calleeBare
	case "field_expression":
		fieldNode := fn.ChildByFieldName("field")
		if fieldNode == nil || kindOf(fieldNode) != "field_identifier" {
			return "", calleeOther
		}
		name := nodeText(fieldNode, w.src)
		if valueNode := fn.ChildByFieldName("value"); valueNode != nil && kindOf(valueNode) == "self" {
			return name, calleeSelfRef
		}
		return name, calleeOther
	case "scoped_identifier":
		nameNode := fn.ChildByFieldName("name")
		if nameNode == nil {
			return "", calleeOther
		}
		name := nodeText(nameNode, w.src)
		if pathNode := fn.ChildByFieldName("path"); pathNode != nil {
			pathText := nodeText(pathNode, w.src)
			currentType := ""
			if len(w.typeStack) > 0 {
				currentType = w.typeStack[len(w.typeStack)-1]
			}
			if pathText == "Self" || (currentType != "" && pathText == currentType) {
				return name, calleeSelfRef
			}
		}
		return name, calleeOther
	case "generic_function":
		return w.calleeTrailing(fn.ChildByFieldName("function"))
	default:
		return "", calleeOther
	}
}

// rustBooleanOp reports whether a binary_expression's operator is a logical
// connective — each adds a path for cyclomatic complexity.
func rustBooleanOp(node *sitter.Node) bool {
	op := node.ChildByFieldName("operator")
	if op == nil {
		return false
	}
	switch kindOf(op) {
	case "&&", "||":
		return true
	}
	return false
}

// rustIOMethods are distinctive method names that unambiguously perform I/O in
// idiomatic Rust — database driver query/execute methods (sqlx/diesel/tokio-
// postgres), file read/write, directory listing, and the reqwest request
// terminator. Ambiguous bare verbs (`get`/`read`/`write`) are deliberately
// excluded (they collide with in-memory accessors), mirroring the Python
// extractor's curated I/O set.
var rustIOMethods = map[string]bool{
	"execute": true, "fetch_one": true, "fetch_all": true, "fetch_optional": true,
	"query_as": true, "read_to_string": true, "read_to_end": true,
	"write_all": true, "read_dir": true, "send": true,
}

// rustIsIODirectCall reports whether a call_expression's callee node is a direct
// I/O primitive: a method call whose method segment is in rustIOMethods, or a
// scoped path to a filesystem/HTTP primitive (File::open, fs::read*, fs::write*,
// reqwest::get/post/...). Type inference isn't available, so this keys off the
// syntactic callee shape only, like Python's pyIsIODirectCall.
func rustIsIODirectCall(fn *sitter.Node, src []byte) bool {
	if fn == nil {
		return false
	}
	switch kindOf(fn) {
	case "field_expression":
		if f := fn.ChildByFieldName("field"); f != nil && kindOf(f) == "field_identifier" {
			return rustIOMethods[nodeText(f, src)]
		}
	case "scoped_identifier":
		name, path := "", ""
		if n := fn.ChildByFieldName("name"); n != nil {
			name = nodeText(n, src)
		}
		if p := fn.ChildByFieldName("path"); p != nil {
			path = nodeText(p, src)
		}
		if rustIOMethods[name] {
			return true
		}
		return rustIOScopedPrimitive(path, name)
	case "generic_function":
		return rustIsIODirectCall(fn.ChildByFieldName("function"), src)
	}
	return false
}

// rustIOScopedPrimitive reports whether a scoped call `<path>::<name>` names a
// filesystem or HTTP I/O primitive, keyed off the last path segment (so both
// `File::open` and `std::fs::File::open` match) plus the callee name.
func rustIOScopedPrimitive(path, name string) bool {
	leaf := path
	if i := strings.LastIndex(path, "::"); i >= 0 {
		leaf = path[i+2:]
	}
	switch leaf {
	case "File":
		return name == "open" || name == "create"
	case "fs":
		switch name {
		case "read", "write", "read_to_string", "read_to_end",
			"read_dir", "remove_file", "copy", "create_dir", "create_dir_all":
			return true
		}
	case "reqwest":
		switch name {
		case "get", "post", "put", "delete", "patch":
			return true
		}
	}
	return false
}

// rustBuiltins are free functions always in scope via the Rust prelude. They
// are not project symbols, so resolving them as same-directory calls would
// produce dangling phantom edges.
var rustBuiltins = map[string]bool{
	"drop": true, "panic": true, "print": true, "println": true,
	"format": true, "assert": true, "matches": true,
}

// compilerInvokedTraitMethods: trait methods invoked exclusively by the
// compiler (Drop::drop, at scope exit — calling it directly is a compile
// error) or the async runtime (Future::poll, driven by .await), never by
// their literal method name in ordinary code. Unlike fmt/eq/hash/clone/
// default — which sometimes genuinely are called by name — these have no
// legitimate direct-call precedent, so they're always safe to exclude.
var compilerInvokedTraitMethods = map[string]map[string]bool{
	"Drop":   {"drop": true},
	"Future": {"poll": true},
}

// hasSelfParam reports whether a `parameters` node's first parameter is
// `self`/`&self`/`&mut self`, distinguishing a method from an associated
// (static) function declared in the same impl/trait block.
func hasSelfParam(params *sitter.Node) bool {
	if params == nil {
		return false
	}
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		if kindOf(params.Child(i)) == "self_parameter" {
			return true
		}
	}
	return false
}

// collectFnNames returns the set of function names declared directly in an
// impl/trait `declaration_list` body (both with and without a body), used to
// resolve same-block sibling calls regardless of declaration order.
func collectFnNames(body *sitter.Node, src []byte) map[string]bool {
	names := make(map[string]bool)
	if body == nil {
		return names
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if kindOf(c) != "function_item" && kindOf(c) != "function_signature_item" {
			continue
		}
		if n := c.ChildByFieldName("name"); n != nil {
			names[nodeText(n, src)] = true
		}
	}
	return names
}

// collectSubmoduleNames returns the names declared by body-less `mod foo;`
// items directly in body — a file-based submodule (foo.rs or foo/mod.rs),
// as opposed to an inline `mod foo { ... }` block. Used to recognize an
// unprefixed `use foo::bar;` as a reference to that sibling file rather than
// an external crate.
func collectSubmoduleNames(body *sitter.Node, src []byte) map[string]bool {
	names := make(map[string]bool)
	if body == nil {
		return names
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if kindOf(c) != "mod_item" || c.ChildByFieldName("body") != nil {
			continue
		}
		if n := c.ChildByFieldName("name"); n != nil {
			names[nodeText(n, src)] = true
		}
	}
	return names
}

// simpleTypeName returns a type node's simple (rightmost) identifier, e.g.
// "Wrapper" for "Wrapper<T>" or "MyError" for "&MyError". Used for impl-block
// types/traits, where generics and references must be stripped.
func simpleTypeName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch kindOf(node) {
	case "type_identifier":
		return nodeText(node, src)
	case "generic_type":
		return simpleTypeName(node.ChildByFieldName("type"), src)
	case "scoped_type_identifier":
		return simpleTypeName(node.ChildByFieldName("name"), src)
	case "reference_type":
		return simpleTypeName(node.ChildByFieldName("type"), src)
	}
	// Fallback: strip references/generics from the raw text.
	t := strings.TrimSpace(nodeText(node, src))
	t = strings.TrimLeft(t, "&")
	t = strings.TrimPrefix(strings.TrimSpace(t), "mut ")
	if i := strings.IndexAny(t, "<("); i >= 0 {
		t = t[:i]
	}
	if i := strings.LastIndex(t, "::"); i >= 0 {
		t = t[i+2:]
	}
	return strings.TrimSpace(t)
}

// scopedCalleeTypePrefix returns the capitalized type name leading a scoped
// call — "EventRecorder" in EventRecorder::new() — or "" if fn isn't a
// scoped call, has no path, or the path's last segment isn't a type (a
// module path, or literal "Self").
func scopedCalleeTypePrefix(fn *sitter.Node, src []byte) string {
	if fn == nil {
		return ""
	}
	if kindOf(fn) == "generic_function" {
		fn = fn.ChildByFieldName("function")
		if fn == nil {
			return ""
		}
	}
	if kindOf(fn) != "scoped_identifier" {
		return ""
	}
	seg := simpleTypeName(fn.ChildByFieldName("path"), src)
	if seg == "" || seg == "Self" || !isCapitalized(seg) {
		return ""
	}
	return seg
}

// scanScopedVariantReference: Type::Variant (capitalized trailing segment)
// marks Type used, whether it's a plain value or inside a match pattern.
func (w *astWalker) scanScopedVariantReference(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	if !isCapitalized(name) {
		return
	}
	w.emitEdge(facts.RelInstantiates, name)
	if seg := simpleTypeName(node.ChildByFieldName("path"), w.src); seg != "" && seg != "Self" && isCapitalized(seg) {
		w.emitEdge(facts.RelInstantiates, seg)
	}
}

func isCapitalized(s string) bool {
	if s == "" {
		return false
	}
	r := []rune(s)[0]
	return unicode.IsUpper(r)
}

// --- tree-sitter helpers ---

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

func nodeText(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	return string(src[node.StartByte():node.EndByte()])
}
