package scalaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	scala "github.com/tree-sitter/tree-sitter-scala/bindings/go"
)

// extractFileAST parses one Scala file with tree-sitter and emits its declaration
// symbols, import dependencies, and the type-reference edges visible from this file
// alone. Edge targets are emitted as fully qualified names (resolved through the
// file's own imports and package), which canonicalizeTargets rewrites to canonical
// "<dir>.<Type>" fact names once every file has been merged.
func extractFileAST(src []byte, relFile string, packageIndex map[string]string) []facts.Fact {
	ff, _ := extractFileASTFull(src, relFile, packageIndex)
	return ff
}

// extractFileASTFull additionally returns the file's declared package, which
// canonicalizeTargets needs to resolve a BARE type reference: only the file knows
// which package a name like `Base` would be in, and only the merged fact set knows
// whether such a type exists. Neither half can decide alone, so the walker records
// the package rather than guessing with it (see resolveTypeName).
func extractFileASTFull(src []byte, relFile string, packageIndex map[string]string) ([]facts.Fact, string) {
	return withScalaTree(src, func(root *sitter.Node) ([]facts.Fact, string) {
		return walkFileAST(root, src, relFile, packageIndex)
	})
}

// extractFileFacts runs BOTH Scala source passes over ONE parse of the file: the
// declaration walk and the route/storage DSL walks. They used to parse the same
// bytes twice, once each, which made Scala the corpus's allocation outlier at
// 50 MiB per file against Java's 0.5 on the same parser infrastructure.
func extractFileFacts(src []byte, relFile string, packageIndex map[string]string) ([]facts.Fact, string) {
	return withScalaTree(src, func(root *sitter.Node) ([]facts.Fact, string) {
		ff, pkg := walkFileAST(root, src, relFile, packageIndex)
		return append(ff, dslFactsFrom(root, src, relFile)...), pkg
	})
}

// withScalaTree parses src once and hands the root to fn, closing the tree after.
// A nil-returning parser setup yields the zero value, as every caller expects.
func withScalaTree(src []byte, fn func(root *sitter.Node) ([]facts.Fact, string)) ([]facts.Fact, string) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(scala.Language())); err != nil {
		return nil, ""
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	return fn(tree.RootNode())
}

// walkFileAST is extractFileASTFull over a tree the caller already parsed.
func walkFileAST(root *sitter.Node, src []byte, relFile string, packageIndex map[string]string) ([]facts.Fact, string) {
	w := &astWalker{
		src:          src,
		relFile:      relFile,
		dir:          factpath.Dir(relFile),
		packageIndex: packageIndex,
		imports:      map[string]string{},
		ownerStack:   []int{},
	}
	w.walk(root)
	return w.out, w.pkg
}

type astWalker struct {
	src     []byte
	relFile string
	dir     string

	packageIndex map[string]string

	// pkg is the file's package, accumulated across chained `package a.b` /
	// `package c` clauses, which Scala permits and which together name one package.
	pkg string

	// imports maps a simple type name to the FQN it was imported as, so a bare
	// `extends Base` can be resolved the way Java resolves one. A renamed import
	// (`import a.b.{Helper => H}`) is keyed on the LOCAL name, because that is what
	// the reference site writes.
	imports map[string]string

	out []facts.Fact

	// typeStack holds the enclosing type names so a nested declaration is named
	// "<dir>.<Outer>.<Inner>", the same qualification the Java and Kotlin
	// extractors use.
	typeStack []string

	// memberStack[len-1] is the set of names the innermost enclosing type declares
	// directly, so a bare call can be qualified to it only when it really is a
	// sibling member. Parallel to typeStack.
	memberStack []map[string]bool

	// localTypes is a scope stack of binding name -> declared type name, built from
	// class parameters, method parameters and type-ascribed vals. Scala writes those
	// types down, so `repo.find(id)` where `repo: UserRepo` is resolvable without
	// inference — and resolving it is what lets the I/O closure cross the
	// constructor-injection boundary that essentially every Scala service is built
	// on. Without it the edge is a bare short name and the closure stops dead at the
	// first injected dependency.
	localTypes []map[string]string

	// ownerStack[len-1] indexes into out for the symbol currently being built.
	// An index rather than a pointer: nested declarations append to out, which can
	// reallocate the backing array and strand a raw pointer.
	ownerStack []int

	// inExtension is set while walking an `extension (x: T) def f = …` block, so
	// the methods it contributes are tagged rather than read as free functions.
	inExtension bool

	// --- per-function metric state (see complexity.go for the loop model) ---
	//
	// The depth counters track nesting at the CURRENT node; the fn-prefixed
	// accumulators collect the per-function peak that becomes the symbol's props.
	// All are saved and restored around a nested definition, so an inner `def` does
	// not inherit or leak its parent's counters.
	m metrics
}

// metrics is one function's complexity accumulation. Two depths are tracked because
// they answer different questions: loopDepth is "how deeply is this nested in
// anything that repeats", scalingDepth is "…in anything that repeats WITH THE INPUT".
// Scala needs both because its ambiguous constructs are real repetition on a
// collection and a single application on an effect (see complexity.go).
type metrics struct {
	inFunction bool

	loopDepth    int // current nesting, all repetition constructs
	scalingDepth int // current nesting, input-scaling constructs only

	maxLoopDepth    int
	maxScalingDepth int
	loopCount       int
	decisions       int // cyclomatic decision points
	recursiveSelf   bool
	ioDirect        bool

	callsInLoop        []string
	callsInScalingLoop []string
	seenInLoop         map[string]bool
	seenInScalingLoop  map[string]bool

	// selfName is the unqualified name of the function being walked, so a call to
	// it can be recognised as direct recursion.
	selfName string
}

// enterLoop raises the counters for a repetition construct and returns a function
// restoring them. scaling says whether the construct repeats with the input size.
func (m *metrics) enterLoop(scaling bool) func() {
	if !m.inFunction {
		return func() {}
	}
	m.loopCount++
	m.loopDepth++
	if m.loopDepth > m.maxLoopDepth {
		m.maxLoopDepth = m.loopDepth
	}
	if scaling {
		m.scalingDepth++
		if m.scalingDepth > m.maxScalingDepth {
			m.maxScalingDepth = m.scalingDepth
		}
	}
	return func() {
		m.loopDepth--
		if scaling {
			m.scalingDepth--
		}
	}
}

// recordCall notes a call made at the current nesting, into the raw list and — only
// when every enclosing construct scales — into the scaling list the analyzer reads
// for N+1 candidates.
func (m *metrics) recordCall(name string) {
	if !m.inFunction || name == "" {
		return
	}
	if m.loopDepth > 0 && !m.seenInLoop[name] {
		if m.seenInLoop == nil {
			m.seenInLoop = map[string]bool{}
		}
		m.seenInLoop[name] = true
		m.callsInLoop = append(m.callsInLoop, name)
	}
	if m.scalingDepth > 0 && !m.seenInScalingLoop[name] {
		if m.seenInScalingLoop == nil {
			m.seenInScalingLoop = map[string]bool{}
		}
		m.seenInScalingLoop[name] = true
		m.callsInScalingLoop = append(m.callsInScalingLoop, name)
	}
}

// applyTo writes the accumulated metrics onto a function/method fact.
//
// The scaling variants are emitted whenever the body contains ANY loop — including
// when they are zero or empty, which is exactly the case that matters. The analyzer
// detects the discount by the PRESENCE of the key (`_, ok := Props[...]`) and falls
// back to the raw values when it is absent, so a function whose only loop is an
// ambiguous `for … yield` must still publish `scaling_loop_depth: 0` and an empty
// `calls_in_scaling_loop`. Omitting them because they are empty would opt that
// function out of the discount and hand every one of its in-loop calls back as an
// N+1 candidate — precisely the false positives the loop model exists to prevent.
//
// A loop-free body emits neither, matching every other extractor: with no loops the
// raw values are zero and empty too, so the fallback is identical and the props
// would be pure bloat on a repository with a hundred thousand symbols.
func (m *metrics) applyTo(props map[string]any) {
	props["cyclomatic"] = m.decisions + 1
	if m.recursiveSelf {
		props["recursive_self"] = true
	}
	if m.ioDirect {
		props["io_direct"] = true
	}
	if m.loopCount == 0 {
		return
	}
	props["loop_depth"] = m.maxLoopDepth
	props["loop_count"] = m.loopCount
	props["scaling_loop_depth"] = m.maxScalingDepth
	if len(m.callsInLoop) > 0 {
		props["calls_in_loop"] = m.callsInLoop
	}
	// Always present alongside scaling_loop_depth, empty included — see above.
	calls := m.callsInScalingLoop
	if calls == nil {
		calls = []string{}
	}
	props["calls_in_scaling_loop"] = calls
}

// text returns a node's source text.
func (w *astWalker) text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	s, e := n.StartByte(), n.EndByte()
	if e > uint(len(w.src)) {
		e = uint(len(w.src))
	}
	return string(w.src[s:e])
}

func (w *astWalker) line(n *sitter.Node) int {
	if n == nil {
		return 0
	}
	return int(n.StartPosition().Row) + 1
}

// walk dispatches on node kind. Anything not recognized is descended into, so a
// declaration nested inside an unhandled expression is still found.
func (w *astWalker) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "package_clause":
		w.handlePackage(n)
		return
	case "import_declaration":
		w.handleImport(n)
		return
	case "class_definition":
		w.handleTypeLike(n, facts.SymbolClass)
		return
	case "object_definition", "package_object":
		w.handleTypeLike(n, facts.SymbolClass)
		return
	case "trait_definition":
		w.handleTypeLike(n, facts.SymbolInterface)
		return
	case "enum_definition":
		w.handleTypeLike(n, facts.SymbolEnum)
		return
	case "simple_enum_case", "full_enum_case":
		w.handleEnumCase(n)
		return
	case "function_definition", "function_declaration":
		w.handleFunction(n)
		return
	case "val_definition", "val_declaration":
		w.handleValue(n, facts.SymbolConstant)
		return
	case "var_definition", "var_declaration":
		w.handleValue(n, facts.SymbolVariable)
		return
	case "type_definition":
		w.handleTypeAlias(n)
		return
	case "given_definition":
		w.handleGiven(n)
		return
	case "extension_definition":
		prev := w.inExtension
		w.inExtension = true
		w.walkChildren(n)
		w.inExtension = prev
		return
	case "instance_expression":
		// `new Foo(...)` — the one construction form that is unambiguous without
		// type information. Scala's dominant form, `Foo(...)` through a companion
		// `apply`, is syntactically a call; handleCall recognises it by name shape.
		w.handleInstanceExpression(n)
		return

	// --- repetition constructs (see complexity.go for why the split is here) ---
	case "for_expression":
		w.handleFor(n)
		return
	case "while_expression":
		w.m.decisions++
		defer w.m.enterLoop(true)()
		w.walkChildren(n)
		return
	case "do_while_expression":
		w.m.decisions++
		defer w.m.enterLoop(true)()
		w.walkChildren(n)
		return

	// --- cyclomatic decision points ---
	case "if_expression":
		w.m.decisions++
	case "case_clause":
		// A match arm and a catch clause are the same node; both branch.
		w.m.decisions++
	case "guard":
		w.m.decisions++
	case "infix_expression":
		if w.isBooleanOperator(n) {
			w.m.decisions++
		}
	case "call_expression":
		w.handleCall(n)
		return
	case "field_expression":
		w.handleFieldExpression(n)
		return
	}
	w.walkChildren(n)
}

// isBooleanOperator reports whether an infix expression's operator is a
// short-circuiting boolean, which adds a branch. Scala spells them `&&`/`||` and
// their symbolic aliases.
func (w *astWalker) isBooleanOperator(n *sitter.Node) bool {
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if kindOf(c) != "operator_identifier" {
			continue
		}
		switch w.text(c) {
		case "&&", "||":
			return true
		}
	}
	return false
}

// handleFor walks a for-comprehension, which is the construct the whole loop model
// turns on: WITH `yield` it desugars to map/flatMap and is a monadic bind more often
// than an iteration (60.4% of corpus sites sit in effect-importing files), WITHOUT
// it is a side-effecting iteration nine times out of ten. So both raise loop_depth,
// only the second raises scaling depth.
//
// The enumerators themselves are walked OUTSIDE the loop: `for (u <- loadUsers())`
// evaluates its generator once, so a call there is not per-iteration work.
func (w *astWalker) handleFor(n *sitter.Node) {
	w.m.decisions++

	var body []*sitter.Node
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() {
			continue
		}
		if kindOf(c) == "enumerators" {
			w.walk(c) // evaluated once, at the enclosing depth
			continue
		}
		body = append(body, c)
	}

	defer w.m.enterLoop(forExpressionScales(n))()
	for _, c := range body {
		w.walk(c)
	}
}

// forExpressionScales reports whether a for-expression repeats with the input size.
// The discriminator is the `yield` keyword; see complexity.go for the measurement
// behind it.
func forExpressionScales(n *sitter.Node) bool {
	return !hasTokenChild(n, "yield")
}

// handleCall processes a call site: the edge it draws, the metrics it contributes,
// and — when it carries a lambda — the loop it may represent.
//
// Target resolution is deliberately conservative, mirroring the Rust and Swift
// extractors. A bare or `this.`-qualified call resolves against the enclosing type,
// which is knowable from the walk. A call on a capitalized receiver is offered as
// `<Type>.<method>` for the merge pass to bind. Anything else — a call on a local
// variable, a parameter, an implicit conversion — becomes a bare short name: enough
// for dead-code matching to see the method used, without inventing a canonical
// target that the extractor cannot verify. Scala's implicit resolution and extension
// methods are not recoverable without a compiler, and a wrong edge is worse than a
// short one.
func (w *astWalker) handleCall(n *sitter.Node) {
	fn := firstNamedChild(n)
	if fn == nil {
		w.walkChildren(n)
		return
	}
	// `f[T](x)` wraps the callee in a generic_function; unwrap to the callee.
	if kindOf(fn) == "generic_function" {
		if inner := firstNamedChild(fn); inner != nil {
			fn = inner
		}
	}

	var recvNode *sitter.Node
	receiver, method := "", ""
	// walkCallee is true when the callee subtree still has to be walked — the
	// curried case, where the inner call emits its own edge.
	walkCallee := false

	switch kindOf(fn) {
	case "field_expression":
		recvNode, method = w.splitFieldExpression(fn)
		receiver = w.simpleName(recvNode)
	case "identifier", "operator_identifier":
		method = w.text(fn)
	case "call_expression":
		// A curried application: `xs.foldLeft(0) { (a, x) => … }` is a call whose
		// callee is itself a call. The trailing block belongs to the INNER call's
		// method, so its name is what decides whether this is a loop — but the edge
		// is emitted by the inner call when it is walked, not twice here.
		method = w.calleeName(fn)
		walkCallee = true
	default:
		w.walkChildren(n)
		return
	}
	if method == "" {
		w.walkChildren(n)
		return
	}

	if !walkCallee {
		if isIOCall(receiver, method) {
			w.m.ioDirect = true
		}
		// `Foo(...)` on a capitalized bare name is a companion `apply` — Scala's
		// dominant construction form, and the reason `new` alone under-reports
		// instantiation badly. Treated as construction when the name resolves to a
		// type the merge pass knows; otherwise it stays an ordinary call.
		if recvNode == nil && isTypeName(method) && !effectConstructors[method] {
			w.addEdge(facts.RelInstantiates, w.resolveTypeName(method))
		} else {
			w.emitCallEdge(recvNode, receiver, method)
		}
		// The receiver is evaluated ONCE, at the enclosing depth: in
		// `loadUsers().map(f)` the load is not per-iteration work.
		if recvNode != nil {
			w.walk(recvNode)
		}
	} else {
		// Same rule, one level out: the curried callee (receiver and first argument
		// list alike) is evaluated once, before any iteration begins.
		w.walk(fn)
	}

	kind := blockCallNone
	if callHasBlockArg(n) {
		kind = classifyBlockCall(method)
		// A combinator applied to an Option repeats at most once, however
		// iteration-shaped its name is. Demote it to the discounted tier rather
		// than dropping it: the construct is still repetition-shaped, it just
		// cannot scale with the input.
		if kind == blockCallScaling && w.receiverIsAtMostOnce(recvNode) {
			kind = blockCallAmbiguous
		}
	}
	if kind != blockCallNone {
		// A combinator loop carries the same back edge a `for` does, so it counts
		// as a decision point for the same reason.
		w.m.decisions++
		defer w.m.enterLoop(kind == blockCallScaling)()
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() || sameNode(c, fn) {
			continue
		}
		w.walk(c)
	}
}

// receiverIsAtMostOnce reports whether a combinator's receiver holds at most one
// element, by reading the trailing method of the receiver expression. `xs.find(p)`
// yields an Option, so `.foreach` on it runs zero or one times.
//
// Only a literal, visible producer counts. A receiver whose type comes from
// inference or from a declaration elsewhere is left alone, so the demotion applies
// exactly where the evidence is in front of the walker.
func (w *astWalker) receiverIsAtMostOnce(recvNode *sitter.Node) bool {
	if recvNode == nil {
		return false
	}
	switch kindOf(recvNode) {
	case "identifier", "type_identifier":
		return atMostOnceReceivers[w.text(recvNode)]
	case "field_expression":
		_, member := w.splitFieldExpression(recvNode)
		return atMostOnceReceivers[member]
	case "call_expression":
		return atMostOnceReceivers[w.calleeName(recvNode)]
	case "generic_function":
		if inner := firstNamedChild(recvNode); inner != nil {
			return w.receiverIsAtMostOnce(inner)
		}
	}
	return false
}

// calleeName returns the method name a (possibly curried, possibly generic) call
// expression invokes, without emitting anything.
func (w *astWalker) calleeName(n *sitter.Node) string {
	fn := firstNamedChild(n)
	if fn == nil {
		return ""
	}
	switch kindOf(fn) {
	case "generic_function":
		if inner := firstNamedChild(fn); inner != nil {
			return w.calleeName(inner.Parent())
		}
	case "field_expression":
		_, method := w.splitFieldExpression(fn)
		return method
	case "identifier", "operator_identifier":
		return w.text(fn)
	case "call_expression":
		return w.calleeName(fn)
	}
	return ""
}

// emitCallEdge records the call relation and the in-loop metric for one call site.
func (w *astWalker) emitCallEdge(recvNode *sitter.Node, receiver, method string) {
	target := w.memberTarget(recvNode, receiver, method)
	if recvNode == nil || receiver == "this" || receiver == "self" {
		if method == w.m.selfName {
			w.m.recursiveSelf = true
		}
	}
	w.addEdge(facts.RelCalls, target)
	// A recognised block construct is syntax, not a callee: recording `foreach` and
	// `synchronized` as in-loop calls buries the real per-iteration work under
	// language machinery in every piece of evidence the analyzer prints.
	if !isStructuralBlockCall(method) {
		w.m.recordCall(target)
	}
}

// handleFieldExpression records a member ACCESS that is not a call.
//
// Scala's uniform access principle makes this necessary rather than optional: a
// parameterless method is invoked without parentheses, so `xa.transaction` and
// `stream.union2` are calls that the grammar reports as field_expression, exactly
// like a field read. Treating only call_expression as a reference left every such
// method with no inbound edge, which reported live code as dead — two of the
// surviving high-confidence findings on the corpus were this and nothing else.
//
// The same node covers a value passed by name (`WebHook.Create`, `Role.ADMIN`) and a
// method used as a value (`Form.apply`), which are references by any reading. The
// walker cannot distinguish these cases without types, and does not need to: they
// are all "this name is used here".
//
// The edge is emitted but NOT recorded as an in-loop call. A field read inside a loop
// is not per-iteration work in the sense the N+1 heuristic means, and admitting every
// one of them would bury the real callees in the analyzer's evidence.
func (w *astWalker) handleFieldExpression(n *sitter.Node) {
	recvNode, member := w.splitFieldExpression(n)
	if member == "" {
		w.walkChildren(n)
		return
	}
	receiver := w.simpleName(recvNode)
	w.addEdge(facts.RelCalls, w.memberTarget(recvNode, receiver, member))

	// Walk the receiver so a chain (`Role.ADMIN.name`, `a.b.c`) records every hop.
	if recvNode != nil {
		w.walk(recvNode)
	}
}

// memberTarget resolves a member reference — a call or a bare access — to the name
// the edge should carry. The three cases are the three scopes Scala lets a reference
// resolve through without a compiler.
func (w *astWalker) memberTarget(recvNode *sitter.Node, receiver, member string) string {
	switch {
	case recvNode == nil, receiver == "this", receiver == "self":
		// A bare reference qualifies to the enclosing type ONLY when that type
		// actually declares the name. Qualifying unconditionally was wrong in the
		// common case: most bare names in a Scala body are imported functions,
		// inherited members, or implicit extensions, and turning `load(u)` into
		// `dir.Service.load` invents a member the type never declared — which, in a
		// name-keyed graph, becomes a phantom node that dead-code and impact
		// analysis then reason about. Unqualified is honest and still matches by
		// short name.
		if w.declaresMember(member) {
			return w.qualify(member)
		}
	case isTypeName(receiver):
		// An object or companion: `Registry.next()`, `WebHook.Create`. Offered
		// qualified for the merge pass to bind against the declaring type.
		return w.resolveTypeName(receiver) + "." + member
	default:
		// A binding whose type the source declares — a constructor parameter, a
		// method parameter, an ascribed val. `repo.find(id)` with `repo: UserRepo`
		// binds to UserRepo.find. The declared type may be a trait the runtime
		// value overrides, which is the honest answer rather than a guess: the
		// implements edges say who else could serve it.
		if typeName := w.lookupLocalType(receiver); typeName != "" {
			return w.resolveTypeName(typeName) + "." + member
		}
	}
	return member
}

// lookupLocalType returns the declared type of a binding visible at the current
// scope, innermost first, or "" when the name is not one enola can type.
func (w *astWalker) lookupLocalType(name string) string {
	if name == "" {
		return ""
	}
	for i := len(w.localTypes) - 1; i >= 0; i-- {
		if t, ok := w.localTypes[i][name]; ok {
			return t
		}
	}
	return ""
}

// pushScope collects `name: Type` bindings from a declaration's parameter lists and
// pushes them as a scope frame, returning the pop.
func (w *astWalker) pushScope(n *sitter.Node) func() {
	scope := map[string]string{}
	var scan func(node *sitter.Node)
	scan = func(node *sitter.Node) {
		for i := uint(0); i < node.ChildCount(); i++ {
			c := node.Child(i)
			if !c.IsNamed() {
				continue
			}
			switch kindOf(c) {
			case "parameters", "class_parameters", "parameter_types":
				scan(c)
			case "parameter", "class_parameter":
				var name, typeName string
				for j := uint(0); j < c.ChildCount(); j++ {
					g := c.Child(j)
					if !g.IsNamed() {
						continue
					}
					if kindOf(g) == "identifier" && name == "" {
						name = w.text(g)
						continue
					}
					if isTypeNode(kindOf(g)) && typeName == "" {
						typeName = w.baseTypeName(g)
					}
				}
				if name != "" && typeName != "" {
					scope[name] = typeName
				}
			}
		}
	}
	scan(n)
	w.localTypes = append(w.localTypes, scope)
	return func() { w.localTypes = w.localTypes[:len(w.localTypes)-1] }
}

// isStructuralBlockCall reports whether a method name is one of the block-taking
// constructs the loop model classifies, rather than a callee worth recording.
func isStructuralBlockCall(method string) bool {
	return scalingCombinators[method] || ambiguousCombinators[method] ||
		nonLoopBlockCalls[method] || effectConstructors[method]
}

// declaresMember reports whether the innermost enclosing type declares name. The
// member set is collected up front per template body (see collectMembers), because
// a body routinely calls a method declared further down the same block and a
// walk-order check would miss it.
func (w *astWalker) declaresMember(name string) bool {
	if len(w.memberStack) == 0 {
		return false
	}
	return w.memberStack[len(w.memberStack)-1][name]
}

// declaresAbstractMember reports whether a type body declares at least one member
// with no implementation — a `def`/`val`/`var` DECLARATION rather than a definition,
// or an abstract type member. That is what separates a trait used as an interface
// from one used as a mixin, and the grammar states it outright: a member with a body
// parses as `*_definition`, one without as `*_declaration`.
//
// Scans one level, since a nested type's abstract members belong to that type.
func (w *astWalker) declaresAbstractMember(body *sitter.Node) bool {
	if body == nil {
		return false
	}
	found := false
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		if found {
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			c := n.Child(i)
			if !c.IsNamed() {
				continue
			}
			switch kindOf(c) {
			case "function_declaration", "val_declaration", "var_declaration":
				found = true
				return
			case "type_definition":
				// `type T` with no right-hand side is an abstract type member;
				// `type T = X` is an alias and therefore concrete.
				if c.ChildByFieldName("type") == nil {
					found = true
					return
				}
			case "class_definition", "object_definition", "trait_definition",
				"enum_definition", "function_definition", "given_definition":
				continue // a nested type's members are its own
			case "template_body", "with_template_body", "indented_block", "block":
				scan(c)
			}
		}
	}
	scan(body)
	return found
}

// collectMembers returns the names a type body declares directly — one level only,
// since a nested type's members belong to it rather than to its enclosing type.
func (w *astWalker) collectMembers(body *sitter.Node) map[string]bool {
	members := map[string]bool{}
	if body == nil {
		return members
	}
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		for i := uint(0); i < n.ChildCount(); i++ {
			c := n.Child(i)
			if !c.IsNamed() {
				continue
			}
			switch kindOf(c) {
			case "function_definition", "function_declaration", "type_definition",
				"class_definition", "object_definition", "trait_definition",
				"enum_definition":
				if nm := w.text(c.ChildByFieldName("name")); nm != "" {
					members[nm] = true
				}
				continue // do not descend: nested members belong to the nested type
			case "val_definition", "var_definition", "val_declaration", "var_declaration":
				p := c.ChildByFieldName("pattern")
				if p == nil {
					p = c.ChildByFieldName("name")
				}
				if p != nil && kindOf(p) == "identifier" {
					members[w.text(p)] = true
				}
				continue
			case "template_body", "indented_block", "with_template_body", "block":
				scan(c)
			}
		}
	}
	scan(body)
	return members
}

// splitFieldExpression returns the receiver node and the member name of a
// `receiver.member` expression.
func (w *astWalker) splitFieldExpression(fn *sitter.Node) (*sitter.Node, string) {
	var named []*sitter.Node
	for i := uint(0); i < fn.ChildCount(); i++ {
		if c := fn.Child(i); c.IsNamed() {
			named = append(named, c)
		}
	}
	if len(named) == 0 {
		return nil, ""
	}
	method := w.text(named[len(named)-1])
	if len(named) == 1 {
		return nil, method
	}
	return named[len(named)-2], method
}

// simpleName returns a receiver's text when it is a plain identifier, and "" when
// it is anything more complex (a nested call, a literal, a chain). Callers use it
// only to recognise a named object or a known I/O receiver, so a complex receiver
// correctly yields no signal rather than a misleading one.
func (w *astWalker) simpleName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch kindOf(n) {
	case "identifier", "type_identifier":
		return w.text(n)
	}
	return ""
}

func firstNamedChild(n *sitter.Node) *sitter.Node {
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); c.IsNamed() {
			return c
		}
	}
	return nil
}

// callHasBlockArg reports whether a call carries a lambda or a brace block, which is
// what makes a combinator name a candidate repetition construct rather than a plain
// method call — `xs.map` without one is a method reference, not an iteration.
func callHasBlockArg(call *sitter.Node) bool {
	for i := uint(0); i < call.ChildCount(); i++ {
		c := call.Child(i)
		switch kindOf(c) {
		case "block", "lambda_expression":
			return true
		case "arguments":
			for j := uint(0); j < c.ChildCount(); j++ {
				switch kindOf(c.Child(j)) {
				case "block", "lambda_expression", "case_block":
					return true
				}
			}
		}
	}
	return false
}

func (w *astWalker) walkChildren(n *sitter.Node) {
	for i := uint(0); i < n.ChildCount(); i++ {
		w.walk(n.Child(i))
	}
}

// handlePackage accumulates the file's package. Scala allows chained clauses
// (`package com.example` followed by `package model`), which together name
// `com.example.model`, and a clause may carry a body whose declarations belong to
// the package it opens.
func (w *astWalker) handlePackage(n *sitter.Node) {
	if name := n.ChildByFieldName("name"); name != nil {
		part := normalizeDotted(w.text(name))
		if part != "" {
			if w.pkg == "" {
				w.pkg = part
			} else {
				w.pkg += "." + part
			}
		}
	}
	if body := n.ChildByFieldName("body"); body != nil {
		w.walkChildren(body)
		return
	}
	// A clause with no body scopes the rest of the file; its siblings are walked
	// by the caller, so there is nothing further to do here.
}

// handleImport emits one dependency fact per imported name and records the local
// name -> FQN mapping the reference sites resolve through.
//
// Every form the grammar produces is covered: a plain `import a.b.C`, a selector
// list `import a.b.{C, D => E}`, and a wildcard `import a.b._` (Scala 2) or
// `import a.b.*` (Scala 3).
func (w *astWalker) handleImport(n *sitter.Node) {
	var prefix []string
	var selectors []importSelector
	wildcard := false

	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() {
			continue
		}
		switch kindOf(c) {
		case "namespace_selectors":
			for j := uint(0); j < c.ChildCount(); j++ {
				s := c.Child(j)
				if !s.IsNamed() {
					continue
				}
				switch kindOf(s) {
				case "arrow_renamed_identifier", "renamed_identifier":
					// `Helper => H`: the FQN is built from the ORIGINAL name, the
					// lookup key is the LOCAL one, because that is what the code
					// that uses it writes.
					orig, local := "", ""
					for k := uint(0); k < s.ChildCount(); k++ {
						if id := s.Child(k); id.IsNamed() {
							if orig == "" {
								orig = w.text(id)
							} else {
								local = w.text(id)
							}
						}
					}
					if orig != "" {
						selectors = append(selectors, importSelector{orig: orig, local: firstNonEmpty(local, orig)})
					}
				case "namespace_wildcard":
					wildcard = true
				default:
					t := w.text(s)
					selectors = append(selectors, importSelector{orig: t, local: t})
				}
			}
		case "namespace_wildcard":
			wildcard = true
		default:
			if t := normalizeDotted(w.text(c)); t != "" {
				prefix = append(prefix, t)
			}
		}
	}

	base := strings.Join(prefix, ".")
	if base == "" {
		return
	}

	switch {
	case len(selectors) > 0:
		for _, sel := range selectors {
			fqn := base + "." + sel.orig
			w.imports[sel.local] = fqn
			w.emitDependency(n, fqn, false)
		}
		if wildcard {
			w.emitDependency(n, base, true)
		}
	case wildcard:
		w.emitDependency(n, base, true)
	default:
		// `import a.b.C` — the last segment is the local name.
		if i := strings.LastIndex(base, "."); i >= 0 {
			w.imports[base[i+1:]] = base
		}
		w.emitDependency(n, base, false)
	}
}

type importSelector struct{ orig, local string }

// emitDependency records one import as a dependency fact. `source` is classified
// stdlib here and otherwise left as external; canonicalizeTargets promotes it to
// internal once it can see whether the target is a package this repository
// declares, which no single file can know.
func (w *astWalker) emitDependency(n *sitter.Node, fqn string, wildcard bool) {
	props := map[string]any{
		"import":         fqn,
		"language":       "scala",
		facts.PropSource: depSource(fqn),
	}
	if wildcard {
		props["wildcard"] = true
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindDependency,
		Name:      w.dir + " -> " + fqn,
		File:      w.relFile,
		Line:      w.line(n),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: fqn}},
	})
}

// handleTypeLike emits a class / object / trait / enum symbol and walks its body
// with the type pushed onto the stack, so members are named through it.
func (w *astWalker) handleTypeLike(n *sitter.Node, symbolKind string) {
	name := w.text(n.ChildByFieldName("name"))
	if name == "" {
		w.walkChildren(n)
		return
	}

	props := map[string]any{
		"language":    "scala",
		"symbol_kind": symbolKind,
		"exported":    w.isExported(n),
	}
	if fqn := w.fqnFor(name); fqn != "" {
		props["fqn"] = fqn
	}
	// A Scala `object` is a singleton — a class at the bytecode level and a value
	// at the source level. It keeps symbol_kind=class (no new vocabulary) and says
	// which it is in a prop, because "how many classes does this repo have" and
	// "which of them are singletons" are different questions.
	switch kindOf(n) {
	case "object_definition":
		props["scala_object"] = true
	case "package_object":
		props["scala_object"] = true
		props["scala_package_object"] = true
	}
	if hasTokenChild(n, "case") {
		props["case_class"] = true
		// A case class is a value carrier, the same signal a Kotlin data class or a
		// Java/C# record gives. Package metrics reads it to stop advising "extract
		// interfaces" on a package that is mostly data: one corpus package of 92
		// header value types was reported rigid with exactly that suggestion, which
		// is not actionable for types whose whole content is their fields.
		if kindOf(n) != "object_definition" {
			props["data_holder"] = true
		}
	}
	if mods := w.modifierText(n); mods != "" {
		if strings.Contains(mods, "sealed") {
			props["sealed"] = true
		}
		if strings.Contains(mods, "abstract") {
			props["abstract"] = true
		}
	}
	// A Scala trait is only an ABSTRACTION when it declares something abstract.
	//
	// Scala traits carry implementations, and the idiom leans on it hard: a mixin
	// with a self-type and a body full of concrete definitions is the ordinary way
	// to compose a service. Counting every trait as abstract read one corpus
	// package — sixteen controller traits whose bodies ARE the REST API, with not a
	// single abstract member between them — as A=1.00 and reported it "useless
	// (unstable + abstract — abstractions almost nothing depends on)".
	//
	// The `abstract` prop is authoritative for package metrics and can demote as
	// well as promote, which is the same hook Ruby uses to keep namespace modules
	// from inflating abstractness. Declared explicitly here (true AND false), so a
	// trait's classification is measured rather than assumed from its keyword.
	if symbolKind == facts.SymbolInterface {
		props["abstract"] = w.declaresAbstractMember(n.ChildByFieldName("body"))
	}

	idx := len(w.out)
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      w.qualify(name),
		File:      w.relFile,
		Line:      w.line(n),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})

	w.addSupertypes(idx, n)

	w.typeStack = append(w.typeStack, name)
	w.memberStack = append(w.memberStack, w.collectMembers(n.ChildByFieldName("body")))
	popScope := w.pushScope(n) // constructor parameters are in scope for the body
	w.ownerStack = append(w.ownerStack, idx)
	w.walkBodyAndParams(n)
	w.ownerStack = w.ownerStack[:len(w.ownerStack)-1]
	popScope()
	w.memberStack = w.memberStack[:len(w.memberStack)-1]
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
}

// walkBodyAndParams walks everything under a type declaration EXCEPT the name and
// the extends clause, which have already been consumed. Class parameters are
// walked so a default-argument expression that constructs something is not lost.
func (w *astWalker) walkBodyAndParams(n *sitter.Node) {
	nameNode := n.ChildByFieldName("name")
	extendNode := n.ChildByFieldName("extend")
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() || sameNode(c, nameNode) {
			continue
		}
		if sameNode(c, extendNode) {
			// The supertype names are already edges; only walk the constructor
			// arguments, which are ordinary expressions.
			if args := c.ChildByFieldName("arguments"); args != nil {
				w.walk(args)
			}
			continue
		}
		w.walk(c)
	}
}

// addSupertypes turns an `extends A with B` clause into implements edges. Scala,
// like C# and unlike Java, does not distinguish extending a class from mixing in a
// trait at the syntax level, and neither does enola's relation vocabulary.
func (w *astWalker) addSupertypes(idx int, n *sitter.Node) {
	ext := n.ChildByFieldName("extend")
	if ext == nil {
		return
	}
	seen := map[string]bool{}
	for i := uint(0); i < ext.ChildCount(); i++ {
		c := ext.Child(i)
		if !c.IsNamed() || kindOf(c) == "arguments" {
			continue
		}
		name := w.baseTypeName(c)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		w.out[idx].Relations = append(w.out[idx].Relations,
			facts.Relation{Kind: facts.RelImplements, Target: w.resolveTypeName(name)})
	}
}

// handleEnumCase emits an enum member. `case Active extends Status(1)` also names
// its parent, which is the same implements edge every other declaration produces.
func (w *astWalker) handleEnumCase(n *sitter.Node) {
	name := w.text(n.ChildByFieldName("name"))
	if name == "" {
		// `case Red, Green, Blue` — several names share one node.
		for i := uint(0); i < n.ChildCount(); i++ {
			if c := n.Child(i); c.IsNamed() && kindOf(c) == "identifier" {
				w.emitEnumCase(n, w.text(c))
			}
		}
		return
	}
	idx := w.emitEnumCase(n, name)
	if idx >= 0 {
		w.addSupertypes(idx, n)
	}
}

func (w *astWalker) emitEnumCase(n *sitter.Node, name string) int {
	if name == "" {
		return -1
	}
	idx := len(w.out)
	w.out = append(w.out, facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.qualify(name),
		File: w.relFile,
		Line: w.line(n),
		Props: map[string]any{
			"language":    "scala",
			"symbol_kind": facts.SymbolConstant,
			"exported":    true,
			"enum_case":   true,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
	return idx
}

// handleFunction emits a def. A def declared inside a type is a method carrying
// its receiver; one at file or package-object scope is a function.
func (w *astWalker) handleFunction(n *sitter.Node) {
	name := w.text(n.ChildByFieldName("name"))
	if name == "" {
		w.walkChildren(n)
		return
	}

	kind := facts.SymbolFunc
	props := map[string]any{
		"language": "scala",
		"exported": w.isExported(n),
	}
	if len(w.typeStack) > 0 {
		kind = facts.SymbolMethod
		props["receiver"] = w.typeStack[len(w.typeStack)-1]
	}
	props["symbol_kind"] = kind
	if kindOf(n) == "function_declaration" {
		// No body: an abstract member of a trait or abstract class.
		props["abstract"] = true
	}
	if w.inExtension {
		props["scala_extension"] = true
	}
	if mods := w.modifierText(n); strings.Contains(mods, "implicit") {
		props["implicit"] = true
	}

	idx := len(w.out)
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      w.qualify(name),
		File:      w.relFile,
		Line:      w.line(n),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})

	// A def body may declare local types and construct things; walk it with this
	// def as the owner so those edges attach here. Locals nested inside a def do
	// NOT push typeStack beyond the def, so a local class is named through its
	// enclosing type rather than through the method.
	//
	// Metrics are saved and restored around the body rather than accumulated
	// globally, so a `def` nested inside another neither inherits its parent's loop
	// nesting nor leaks its own back out.
	saved := w.m
	w.m = metrics{inFunction: true, selfName: name}
	popScope := w.pushScope(n)
	w.ownerStack = append(w.ownerStack, idx)
	w.walkMembers(n, n.ChildByFieldName("name"))
	w.ownerStack = w.ownerStack[:len(w.ownerStack)-1]
	popScope()
	w.m.applyTo(props)
	w.m = saved
}

// handleValue emits a val/var. Scala's `val` is an immutable binding, so it maps
// to constant and `var` to variable — the distinction downstream analyses read.
func (w *astWalker) handleValue(n *sitter.Node, symbolKind string) {
	// val/var bind a PATTERN, not a name: `val (a, b) = pair` is legal. Only a
	// plain identifier yields a symbol; a destructuring binding is skipped rather
	// than guessed at, and its initializer is still walked for edges.
	nameNode := n.ChildByFieldName("pattern")
	if nameNode == nil {
		nameNode = n.ChildByFieldName("name")
	}
	name := ""
	if nameNode != nil && (kindOf(nameNode) == "identifier" || kindOf(nameNode) == "stable_identifier") {
		name = w.text(nameNode)
	}
	if name == "" {
		w.walkMembers(n, nil)
		return
	}

	props := map[string]any{
		"language":    "scala",
		"symbol_kind": symbolKind,
		"exported":    w.isExported(n),
	}
	if mods := w.modifierText(n); strings.Contains(mods, "implicit") {
		props["implicit"] = true
	}
	if len(w.typeStack) > 0 {
		props["receiver"] = w.typeStack[len(w.typeStack)-1]
	}

	idx := len(w.out)
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      w.qualify(name),
		File:      w.relFile,
		Line:      w.line(n),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})

	w.ownerStack = append(w.ownerStack, idx)
	w.walkMembers(n, nameNode)
	w.ownerStack = w.ownerStack[:len(w.ownerStack)-1]
}

// handleTypeAlias emits a `type X = Y` member.
func (w *astWalker) handleTypeAlias(n *sitter.Node) {
	name := w.text(n.ChildByFieldName("name"))
	if name == "" {
		w.walkChildren(n)
		return
	}
	props := map[string]any{
		"language":    "scala",
		"symbol_kind": facts.SymbolType,
		"exported":    w.isExported(n),
	}
	if fqn := w.fqnFor(name); fqn != "" {
		props["fqn"] = fqn
	}
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      w.qualify(name),
		File:      w.relFile,
		Line:      w.line(n),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})
}

// handleGiven emits a Scala 3 `given` instance. A given is a value the compiler
// supplies at call sites rather than one anybody names, so it is a variable that
// says what it is in a prop; the type it provides becomes an implements edge,
// which is what makes "who provides this typeclass instance" answerable.
func (w *astWalker) handleGiven(n *sitter.Node) {
	name := w.text(n.ChildByFieldName("name"))
	anonymous := name == ""
	if anonymous {
		// `given Ordering[Color] with …` — an anonymous given is named for the
		// type it provides, which is the only stable identifier it has.
		if rt := n.ChildByFieldName("return_type"); rt != nil {
			name = w.baseTypeName(rt)
		}
		if name == "" {
			for i := uint(0); i < n.ChildCount(); i++ {
				c := n.Child(i)
				if c.IsNamed() && isTypeNode(kindOf(c)) {
					name = w.baseTypeName(c)
					break
				}
			}
		}
		if name != "" {
			name = "given_" + name
		}
	}
	if name == "" {
		w.walkChildren(n)
		return
	}

	props := map[string]any{
		"language":    "scala",
		"symbol_kind": facts.SymbolVariable,
		"exported":    w.isExported(n),
		"scala_given": true,
	}
	if anonymous {
		props["anonymous"] = true
	}

	idx := len(w.out)
	w.out = append(w.out, facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      w.qualify(name),
		File:      w.relFile,
		Line:      w.line(n),
		Props:     props,
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}},
	})

	if rt := n.ChildByFieldName("return_type"); rt != nil {
		if t := w.baseTypeName(rt); t != "" {
			w.out[idx].Relations = append(w.out[idx].Relations,
				facts.Relation{Kind: facts.RelImplements, Target: w.resolveTypeName(t)})
		}
	}

	// A `given … with { def … }` body declares real methods; push the given as
	// their enclosing type so they are named and attributed through it.
	w.typeStack = append(w.typeStack, name)
	w.memberStack = append(w.memberStack, w.collectMembers(n.ChildByFieldName("body")))
	w.ownerStack = append(w.ownerStack, idx)
	w.walkMembers(n, n.ChildByFieldName("name"))
	w.ownerStack = w.ownerStack[:len(w.ownerStack)-1]
	w.memberStack = w.memberStack[:len(w.memberStack)-1]
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
}

// handleInstanceExpression records `new Foo(...)` as an instantiates edge on the
// enclosing symbol, then walks the arguments for nested constructions.
func (w *astWalker) handleInstanceExpression(n *sitter.Node) {
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() {
			continue
		}
		switch kindOf(c) {
		case "arguments", "template_body", "with_template_body", "indented_block", "block":
			continue // handled below
		}
		if t := w.baseTypeName(c); t != "" {
			w.addEdge(facts.RelInstantiates, w.resolveTypeName(t))
			break
		}
	}

	// Walk the constructor arguments AND the anonymous class body.
	//
	// The body is the part that was missed, and missing it was not a small gap: an
	// anonymous class is where Scala puts implementations, so `new: …` (Scala 3,
	// braceless) and `new T { … }` routinely hold a whole object's worth of vals and
	// defs. Walking only `arguments` dropped every declaration and every call inside
	// them — corpus-wide, 1,817 bodies carrying 5,673 declarations and 9,637 calls —
	// which is what made an extension method called only from inside such a body read
	// as dead code.
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() {
			continue
		}
		switch kindOf(c) {
		case "arguments", "template_body", "with_template_body", "indented_block", "block":
			w.walk(c)
		}
	}
}

// addEdge attaches a relation to the innermost enclosing symbol. An edge with no
// owner — a construction in file-scope code — is dropped rather than hung on an
// arbitrary fact; file-scope reference capture belongs with the call pass.
func (w *astWalker) addEdge(kind, target string) {
	if target == "" || len(w.ownerStack) == 0 {
		return
	}
	idx := w.ownerStack[len(w.ownerStack)-1]
	for _, r := range w.out[idx].Relations {
		if r.Kind == kind && r.Target == target {
			return
		}
	}
	w.out[idx].Relations = append(w.out[idx].Relations, facts.Relation{Kind: kind, Target: target})
}

// walkMembers walks every named child except skip.
func (w *astWalker) walkMembers(n *sitter.Node, skip *sitter.Node) {
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if !c.IsNamed() || sameNode(c, skip) {
			continue
		}
		w.walk(c)
	}
}

// --- naming and resolution helpers ---

// qualify builds the canonical fact name: "<dir>.<Outer>.<Inner>.<member>".
func (w *astWalker) qualify(name string) string {
	parts := make([]string, 0, len(w.typeStack)+1)
	parts = append(parts, w.typeStack...)
	parts = append(parts, name)
	return w.dir + "." + strings.Join(parts, ".")
}

// fqnFor builds the Scala fully qualified name of a declaration — the package plus
// the enclosing type chain. It is what other files' references resolve against,
// and it is frequently NOT derivable from the directory: Scala, like C#, lets a
// package disagree with the file system.
func (w *astWalker) fqnFor(name string) string {
	parts := make([]string, 0, len(w.typeStack)+2)
	if w.pkg != "" {
		parts = append(parts, w.pkg)
	}
	parts = append(parts, w.typeStack...)
	parts = append(parts, name)
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ".")
}

// resolveTypeName turns a reference as written into the FQN it names, using the
// file's imports — the one scope a single file can resolve through with certainty.
// An already-dotted reference is taken as written.
//
// A bare name with no matching import is deliberately left BARE rather than
// qualified with the file's own package. Java can make that assumption because its
// imports are explicit; Scala cannot, because it auto-imports `scala.*`,
// `java.lang.*` and `scala.Predef.*`, so a bare `Ordering`, `Seq` or `Exception` is
// far more often stdlib than same-package. Qualifying it would publish an edge to
// `com.example.app.Ordering` — a type in a package that does not declare it — and
// because the graph is name-keyed, that fabricated name would materialize as a
// phantom node. canonicalizeTargets resolves the same-package case afterwards,
// where it can check the merged fact set instead of assuming.
func (w *astWalker) resolveTypeName(name string) string {
	if name == "" {
		return ""
	}
	if strings.Contains(name, ".") {
		return name
	}
	if fqn, ok := w.imports[name]; ok {
		return fqn
	}
	return name
}

// baseTypeName strips type arguments and qualifies down to the type being named:
// `Option[A]` -> `Option`, `scala.collection.Map` -> `scala.collection.Map`.
func (w *astWalker) baseTypeName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch kindOf(n) {
	case "generic_type":
		// The type constructor is the first named child; the rest are arguments.
		for i := uint(0); i < n.ChildCount(); i++ {
			if c := n.Child(i); c.IsNamed() && kindOf(c) != "type_arguments" {
				return w.baseTypeName(c)
			}
		}
		return ""
	case "type_identifier", "identifier":
		return w.text(n)
	case "stable_type_identifier", "stable_identifier", "projected_type":
		return normalizeDotted(w.text(n))
	case "annotated_type", "compound_type", "singleton_type":
		for i := uint(0); i < n.ChildCount(); i++ {
			if c := n.Child(i); c.IsNamed() {
				if t := w.baseTypeName(c); t != "" {
					return t
				}
			}
		}
		return ""
	}
	if isTypeNode(kindOf(n)) {
		return normalizeDotted(w.text(n))
	}
	return ""
}

func isTypeNode(kind string) bool {
	switch kind {
	case "type_identifier", "generic_type", "stable_type_identifier",
		"projected_type", "compound_type", "annotated_type", "singleton_type":
		return true
	}
	return false
}

// isExported reports whether a declaration is part of the public surface. Scala
// defaults to public, so only an explicit private/protected removes it. A
// qualified `private[pkg]` is package-private and counted as non-exported, the
// same reading Kotlin's `internal` gets.
func (w *astWalker) isExported(n *sitter.Node) bool {
	mods := w.modifierText(n)
	return !strings.Contains(mods, "private") && !strings.Contains(mods, "protected")
}

// modifierText returns the text of the declaration's `modifiers` node, or "".
func (w *astWalker) modifierText(n *sitter.Node) string {
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); kindOf(c) == "modifiers" {
			return w.text(c)
		}
	}
	return ""
}

// sameNode reports whether two node handles refer to the same tree node. Node is a
// value type over a C struct, so handles to one node are distinct Go pointers and
// must be compared by identity rather than by ==.
func sameNode(a, b *sitter.Node) bool {
	return a != nil && b != nil && a.Id() == b.Id()
}

// hasTokenChild reports whether n has a direct child of the given (anonymous)
// token kind — how the grammar represents `case` on a class or object.
func hasTokenChild(n *sitter.Node, kind string) bool {
	for i := uint(0); i < n.ChildCount(); i++ {
		if kindOf(n.Child(i)) == kind {
			return true
		}
	}
	return false
}

// normalizeDotted collapses whitespace and backticks out of a dotted path, so a
// path split across lines compares equal to one written inline.
func normalizeDotted(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	parts := strings.Split(s, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ".")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
