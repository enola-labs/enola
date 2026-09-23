package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

// extractFileAST parses a single Java file with tree-sitter and emits architectural
// facts: declaration symbols (classes, interfaces, enums, records, methods, fields),
// import dependencies, and call-graph relations (RelImplements, RelInstantiates,
// RelInjects, RelCalls).
//
// Relation targets for type references (implements/instantiates/injects) are emitted
// as fully-qualified names — resolved through the file's import map, or assumed to be
// same-package when no import matches. java.go's canonicalizeTargets rewrites those
// FQNs to canonical "<dir>.<Type>" fact names once every file has been indexed.
func extractFileAST(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(java.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	w := &astWalker{
		src:       src,
		relFile:   relFile,
		dir:       factpath.Dir(relFile),
		importMap: make(map[string]string),
	}
	root := tree.RootNode()
	w.pkg = w.findPackage(root)
	w.walkProgram(root)
	return w.out
}

type astWalker struct {
	src     []byte
	relFile string
	dir     string
	pkg     string // dotted package name, e.g. "com.example.auth" ("" if none)

	out []facts.Fact

	// importMap maps an imported simple type name to its fully-qualified name
	// (e.g. "Store" -> "com.example.data.Store"). Used to resolve bare type
	// references in supertypes, constructor calls, and injected parameters.
	importMap map[string]string

	// typeStack holds the simple names of the enclosing type declarations, so a
	// method declared in class Foo is named "<dir>.Foo.method". methodStack is
	// parallel and holds the method-name set of each enclosing type, used to
	// resolve same-class bare calls.
	typeStack   []string
	methodStack []map[string]bool

	// ownerStack[len-1] is the symbol fact currently being built; call-graph edges
	// discovered while walking its body attach to it.
	ownerStack []*facts.Fact

	// routeStack is parallel to typeStack: each entry carries the Spring route
	// context (whether the enclosing type is a @Controller/@RestController and its
	// class-level base path) so method handlers can emit route facts.
	routeStack []routeScope

	// Per-method complexity state, set up by handleMethod around walkForCalls and
	// saved/restored across the re-entrant nested-type walk. metrics is nil outside
	// a method body walk. loopDepth is the current loop nesting depth;
	// selfName/selfShort are the enclosing method's full and short names (for
	// direct-recursion detection).
	metrics   *javaBodyMetrics
	loopDepth int
	// scalingDepth is the current nesting counting only input-scaling (unbounded) loops
	// — the Big-O exponent. repeatDepth counts enclosing loops that run a non-constant
	// number of times; it differs from scalingDepth only for while(true)/for(;;), which
	// add no factor of n but whose body still runs many times, so a query inside stays
	// an N+1 candidate.
	scalingDepth int
	repeatDepth  int
	selfName     string
	selfShort    string
	// selfParams is the enclosing method's declared parameter count. A resolved
	// self-call is only genuine recursion when its argument count matches — otherwise
	// it is a call to a same-named overload, not recursion.
	selfParams int
}

// javaBodyMetrics accumulates per-method complexity signals during the single
// walkForCalls body traversal — mirrors the other extractors.
type javaBodyMetrics struct {
	loopDepth          int             // max loop nesting depth
	loopCount          int             // number of loop constructs (syntactic + stream lambdas)
	decisions          int             // decision points (cyclomatic = 1 + decisions)
	callsInLoop        []string        // distinct call targets invoked at loop depth >= 1
	inLoopSeen         map[string]bool // dedup set for callsInLoop
	scalingLoopDepth   int             // max nesting counting only unbounded (input-scaling) loops
	callsInScalingLoop []string        // distinct targets invoked inside a repeating loop (N+1 candidates)
	inScalingSeen      map[string]bool // dedup set for callsInScalingLoop
	recursive          bool            // body directly calls the enclosing method
	sawSuperSelf       bool            // body calls super.<enclosingName>() (override delegation)
}

// javaIterators are Stream/Collection methods whose lambda argument runs once per
// element — i.e. a loop. A lambda passed to a method NOT in this set (Runnable,
// Comparator, a listener, a Supplier) is deferred and not treated as a loop.
var javaIterators = map[string]bool{
	"forEach": true, "forEachOrdered": true, "map": true, "mapToInt": true,
	"mapToLong": true, "mapToDouble": true, "mapToObj": true, "flatMap": true,
	"filter": true, "reduce": true, "collect": true, "anyMatch": true,
	"allMatch": true, "noneMatch": true, "peek": true, "sorted": true,
	"removeIf": true, "replaceAll": true, "computeIfAbsent": true, "takeWhile": true,
	"dropWhile": true,
}

// javaCheapMethods are obviously-cheap methods that are not I/O. Calls to these on
// an unknown receiver inside a loop are not recorded in calls_in_loop, keeping it
// focused (the analyzer's keyword gate is the real precision filter).
var javaCheapMethods = map[string]bool{
	"toString": true, "equals": true, "hashCode": true, "get": true, "set": true,
	"add": true, "remove": true, "put": true, "contains": true, "size": true,
	"isEmpty": true, "length": true, "name": true, "value": true, "builder": true,
	"build": true, "stream": true, "iterator": true, "getClass": true, "valueOf": true,
	"format": true, "append": true, "charAt": true, "substring": true, "trim": true,
	"forEach": true, "map": true, "filter": true, "collect": true, "of": true,
}

// recordCallMetrics notes a resolved call target against the current method's
// complexity metrics: flags direct recursion and records calls made inside loops.
// argCount is the invocation's argument count; recursion is flagged only when it
// matches the enclosing method's parameter count, so a call to a same-named overload
// is not mistaken for self-recursion.
func (w *astWalker) recordCallMetrics(target string, argCount int) {
	if w.metrics == nil || target == "" {
		return
	}
	if (target == w.selfName || target == w.selfShort) && argCount == w.selfParams {
		w.metrics.recursive = true
	}
	w.recordInLoop(target)
}

// javaArgCount returns the number of arguments of a method_invocation node.
func javaArgCount(node *sitter.Node) int {
	args := node.ChildByFieldName("arguments")
	if args == nil {
		return 0
	}
	n := 0
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		if args.Child(i).IsNamed() {
			n++
		}
	}
	return n
}

// javaParamCount returns the declared parameter count of a method declaration node.
func javaParamCount(node *sitter.Node) int {
	params := node.ChildByFieldName("parameters")
	if params == nil {
		params = findChildByKind(node, "formal_parameters")
	}
	if params == nil {
		return 0
	}
	n := 0
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		switch kindOf(params.Child(i)) {
		case "formal_parameter", "spread_parameter":
			n++
		}
	}
	return n
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
	// candidate. Only a genuinely constant loop (a literal-bounded for, a for-each over
	// a collection literal) excludes its calls; while(true)/for(;;) repeats, so its
	// calls stay candidates even though its depth is discounted from the Big-O exponent.
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

// javaLoopClass classifies a loop by two independent properties: whether it adds a
// factor of n to Big-O (scales) and whether its body runs a non-constant number of
// times (repeats). A constant-count loop — `for (int i = 0; i < 3; i++)`, a for-each
// over a collection literal or ALL_CAPS constant — does neither. An infinite
// `while (true)` / `for (;;)` does not scale but still repeats, so a query inside it
// stays an N+1 candidate. Collapsing constant and infinite into one "bounded" flag
// deletes true positives — see cache.go v99.
type javaLoopClass int

const (
	javaLoopScaling javaLoopClass = iota
	javaLoopConstant
	javaLoopInfinite
)

// scales reports whether the loop contributes a factor of n to Big-O.
func (c javaLoopClass) scales() bool { return c == javaLoopScaling }

// repeats reports whether the loop body runs a non-constant number of times, so a call
// inside it is an N+1 candidate.
func (c javaLoopClass) repeats() bool { return c != javaLoopConstant }

// javaSyntacticLoopClass classifies a for / enhanced-for / while / do-while statement.
func javaSyntacticLoopClass(node *sitter.Node, src []byte) javaLoopClass {
	switch kindOf(node) {
	case "for_statement":
		// Three-clause C-style for. No condition (`for (;;)`) is infinite; a condition
		// comparing against an integer literal (`i < 3`, `i <= 10`) runs a fixed number
		// of times → constant; anything data-derived (`i < n`, `i < xs.size()`) scales.
		cond := node.ChildByFieldName("condition")
		if cond == nil {
			return javaLoopInfinite
		}
		if javaConstantForCondition(cond) {
			return javaLoopConstant
		}
	case "enhanced_for_statement":
		// for (T x : <iterable>) — constant iff the iterable is a collection/array
		// literal or an ALL_CAPS constant.
		if javaConstantIterable(node.ChildByFieldName("value"), src) {
			return javaLoopConstant
		}
	case "while_statement", "do_statement":
		if javaIsTrueCondition(node.ChildByFieldName("condition"), src) {
			return javaLoopInfinite
		}
	}
	return javaLoopScaling
}

// javaConstantForCondition reports whether a three-clause for's condition compares the
// loop variable against an integer literal (`i < 3`), so the loop runs a statically
// fixed number of times. A data-derived bound (`i < n`, `i < xs.size()`) or a compound
// condition (`i < 3 && ok`) is conservatively treated as scaling — so no genuine O(n)
// finding is ever deleted, only a clearly-constant one discounted.
func javaConstantForCondition(cond *sitter.Node) bool {
	if cond == nil || kindOf(cond) != "binary_expression" {
		return false
	}
	hasCmp, hasLiteral := false, false
	for i := uint(0); i < uint(cond.ChildCount()); i++ {
		switch kindOf(cond.Child(i)) {
		case "<", "<=", ">", ">=", "!=":
			hasCmp = true
		case "decimal_integer_literal", "hex_integer_literal", "octal_integer_literal", "binary_integer_literal":
			hasLiteral = true
		}
	}
	return hasCmp && hasLiteral
}

// javaConstantIterable reports whether a for-each iterates a compile-time-fixed number
// of times: a collection-literal factory (`List.of(...)`, `Set.of(...)`, `Map.of(...)`,
// `Arrays.asList(...)`), an array literal (`new int[]{...}`), or an ALL_CAPS data
// constant (`STOP_CHARS`, `Screen.STOP_CHARS`). A variable receiver scales. Mirrors
// kotlinConstantIterable / pyIterableBounded.
func javaConstantIterable(value *sitter.Node, src []byte) bool {
	if value == nil {
		return false
	}
	switch kindOf(value) {
	case "array_creation_expression":
		return value.ChildByFieldName("value") != nil // has an array_initializer
	case "array_initializer":
		return true
	case "identifier":
		return javaIsScreamingConst(nodeText(value, src))
	case "field_access":
		if f := value.ChildByFieldName("field"); f != nil {
			return javaIsScreamingConst(nodeText(f, src))
		}
	case "method_invocation":
		name := ""
		if nn := value.ChildByFieldName("name"); nn != nil {
			name = nodeText(nn, src)
		}
		obj := ""
		if on := value.ChildByFieldName("object"); on != nil {
			obj = nodeText(on, src)
		}
		switch name {
		case "of":
			return obj == "List" || obj == "Set" || obj == "Map"
		case "asList":
			return obj == "Arrays"
		}
	}
	return false
}

// javaIsScreamingConst reports whether an identifier is SCREAMING_SNAKE_CASE — a
// conventional Java compile-time constant, whose iteration count is input-independent.
func javaIsScreamingConst(s string) bool {
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

// javaIsTrueCondition reports whether a loop condition is the literal `true`
// (`while (true)`, `do … while (true)`). A named constant that merely holds true
// (`while (RUNNING)`) is deliberately not matched — its trip count is not statically
// evident.
func javaIsTrueCondition(cond *sitter.Node, src []byte) bool {
	if cond == nil {
		return false
	}
	t := strings.TrimSpace(nodeText(cond, src))
	t = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(t, "("), ")"))
	return t == "true"
}

// javaStreamReceiverBounded reports whether a stream-iterator call's receiver is a
// constant iterable (`List.of(a, b).forEach(...)`), so the callback runs a fixed count.
func javaStreamReceiverBounded(call *sitter.Node, src []byte) bool {
	return javaConstantIterable(call.ChildByFieldName("object"), src)
}

func javaBooleanOp(node *sitter.Node) bool {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		switch kindOf(node.Child(i)) {
		case "&&", "||":
			return true
		}
	}
	return false
}

func javaByteContains(outer, inner *sitter.Node) bool {
	return inner.StartByte() >= outer.StartByte() && inner.EndByte() <= outer.EndByte()
}

// javaStreamLambda returns the lambda argument of a stream-iterator call
// (items.forEach(x -> …)), or nil if the call is not an iterator with a lambda.
func javaStreamLambda(call *sitter.Node, src []byte) *sitter.Node {
	nameNode := call.ChildByFieldName("name")
	if nameNode == nil || !javaIterators[nodeText(nameNode, src)] {
		return nil
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		if c := args.Child(i); kindOf(c) == "lambda_expression" {
			return c
		}
	}
	return nil
}

// walkJavaLambdaSubtree descends to a stream iterator's lambda and walks its BODY
// at +1 (it runs per element), while walking everything else (receiver, other args)
// at the current depth. Kind-checked so an ancestor with the same byte span isn't
// mistaken for the lambda.
// bounded is true when the iterator's receiver is a collection literal, so the callback
// runs a fixed number of times and neither scales nor repeats.
func (w *astWalker) walkJavaLambdaSubtree(node, lambda *sitter.Node, bounded bool) {
	if node == nil {
		return
	}
	if kindOf(node) == "lambda_expression" && node.StartByte() == lambda.StartByte() && node.EndByte() == lambda.EndByte() {
		w.loopDepth++
		if !bounded {
			// An iterator receiver is either a literal (constant) or data-derived
			// (scaling); it is never infinite, so repeating and scaling coincide here.
			w.scalingDepth++
			w.repeatDepth++
		}
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkForCalls(node.Child(i))
		}
		w.loopDepth--
		if !bounded {
			w.scalingDepth--
			w.repeatDepth--
		}
		return
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); javaByteContains(c, lambda) {
			w.walkJavaLambdaSubtree(c, lambda, bounded)
		} else {
			w.walkForCalls(c)
		}
	}
}

type routeScope struct {
	isController  bool
	isFeignClient bool
	feignHint     string
	basePath      string
	// isRepository is a Spring Data repository interface (extends JpaRepository/…):
	// every declared method is a DB round-trip. isDao is a Room @Dao interface.
	// Both feed the GAP-JV-02 io_direct seed in handleMethod.
	isRepository bool
	isDao        bool
}

func (w *astWalker) enclosingType() string { return strings.Join(w.typeStack, ".") }

func (w *astWalker) qualify(name string) string {
	if t := w.enclosingType(); t != "" {
		return t + "." + name
	}
	return name
}

func (w *astWalker) currentMethods() map[string]bool {
	if len(w.methodStack) == 0 {
		return nil
	}
	return w.methodStack[len(w.methodStack)-1]
}

func (w *astWalker) currentRoute() *routeScope {
	if len(w.routeStack) == 0 {
		return nil
	}
	return &w.routeStack[len(w.routeStack)-1]
}

func (w *astWalker) pushOwner(f *facts.Fact) { w.ownerStack = append(w.ownerStack, f) }
func (w *astWalker) popOwner()               { w.ownerStack = w.ownerStack[:len(w.ownerStack)-1] }
func (w *astWalker) currentOwner() *facts.Fact {
	if len(w.ownerStack) == 0 {
		return nil
	}
	return w.ownerStack[len(w.ownerStack)-1]
}

// canonicalName is the "<dir>.<QualifiedType>" fact name of a declaration.
func (w *astWalker) canonicalName(qualified string) string { return w.dir + "." + qualified }

// fqn is the fully-qualified "<package>.<QualifiedType>" name of a declaration.
func (w *astWalker) fqn(qualified string) string {
	if w.pkg == "" {
		return qualified
	}
	return w.pkg + "." + qualified
}

func (w *astWalker) findPackage(root *sitter.Node) string {
	if pd := findChildByKind(root, "package_declaration"); pd != nil {
		// The package name is the scoped_identifier / identifier child.
		for i := uint(0); i < uint(pd.ChildCount()); i++ {
			c := pd.Child(i)
			if kindOf(c) == "scoped_identifier" || kindOf(c) == "identifier" {
				return nodeText(c, w.src)
			}
		}
	}
	return ""
}

func (w *astWalker) walkProgram(root *sitter.Node) {
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		w.walkTopLevel(root.Child(i))
	}
}

func (w *astWalker) walkTopLevel(node *sitter.Node) {
	switch kindOf(node) {
	case "import_declaration":
		w.handleImport(node)
	case "class_declaration":
		w.handleClassLike(node, facts.SymbolClass)
	case "interface_declaration":
		w.handleClassLike(node, facts.SymbolInterface)
	case "enum_declaration":
		w.handleClassLike(node, facts.SymbolEnum)
	case "record_declaration":
		w.handleClassLike(node, facts.SymbolClass)
	case "annotation_type_declaration":
		w.handleClassLike(node, facts.SymbolInterface)
	}
}

func (w *astWalker) handleImport(node *sitter.Node) {
	isStatic := false
	isWildcard := false
	var pathNode *sitter.Node
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "static":
			isStatic = true
		case "asterisk":
			isWildcard = true
		case "scoped_identifier", "identifier":
			pathNode = c
		}
	}
	if pathNode == nil {
		return
	}
	importPath := nodeText(pathNode, w.src)

	props := map[string]any{
		"language": "java",
		"import":   importPath,
		"source":   "external", // refined to "internal" in canonicalizeTargets
	}
	// Mark the import shape so resolveImport can apply the parent-FQN fallback to
	// static-member / un-indexed-type imports but NOT to wildcards (whose import
	// string is already the package — walking to the grandparent would mis-resolve).
	if isStatic {
		props["static"] = true
	}
	if isWildcard {
		props["wildcard"] = true
	}

	w.out = append(w.out, facts.Fact{
		Kind:  facts.KindDependency,
		Name:  w.dir + " -> " + importPath,
		File:  w.relFile,
		Line:  int(node.StartPosition().Row) + 1,
		Props: props,
		Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: importPath},
		},
	})

	// Record a non-static, non-wildcard import's simple name so bare type
	// references resolve to its FQN. Static imports name a member, not a type;
	// wildcard imports carry no simple name.
	if isStatic || isWildcard {
		return
	}
	simple := importPath
	if i := strings.LastIndex(importPath, "."); i >= 0 {
		simple = importPath[i+1:]
	}
	if simple != "" {
		w.importMap[simple] = importPath
	}
}

func (w *astWalker) handleClassLike(node *sitter.Node, kind string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)

	modifiers := findChildByKind(node, "modifiers")
	modifierText := ""
	var annotations []javaAnnotation
	if modifiers != nil {
		modifierText = nodeText(modifiers, w.src)
		annotations = parseAnnotations(modifiers, w.src)
	}
	// A top-level type is exported when public; nested types inherit visibility
	// loosely — treat anything not explicitly private as part of the surface.
	exported := strings.Contains(modifierText, "public") ||
		(!strings.Contains(modifierText, "private") && len(w.typeStack) > 0)

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.canonicalName(w.qualify(name)),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": kind,
			"exported":    exported,
			"language":    "java",
			"fqn":         w.fqn(w.qualify(name)),
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if strings.Contains(modifierText, "abstract") {
		f.SetProp("abstract", true)
	}
	if kindOf(node) == "record_declaration" {
		f.SetProp("record", true)
	}
	if kindOf(node) == "annotation_type_declaration" {
		f.SetProp("annotation_class", true)
	}

	// Inheritance: `extends` superclass + `implements`/`extends` interfaces.
	for _, st := range w.supertypeTargets(node) {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImplements, Target: st})
	}

	// Framework classification (Spring component / JPA / Dubbo SPI) mutates props
	// and may emit a companion storage fact.
	supers := w.supertypeSimpleNames(node)
	classifyComponent(&f, name, annotations, supers)
	if sf := detectJpaStorage(name, annotations, w.relFile, int(node.StartPosition().Row)+1, w.dir); sf != nil {
		w.out = append(w.out, *sf)
	}

	w.out = append(w.out, f)
	owner := &w.out[len(w.out)-1]
	w.pushOwner(owner)

	// Enter the type scope.
	body := classBody(node)
	w.typeStack = append(w.typeStack, name)
	w.methodStack = append(w.methodStack, collectMethodNames(body, w.src))
	w.routeStack = append(w.routeStack, routeScope{
		isController:  isSpringController(annotations),
		isFeignClient: hasAnnotation(annotations, "FeignClient"),
		feignHint:     feignServiceHint(annotations),
		basePath:      requestMappingPath(annotations),
		isRepository:  isSpringDataRepository(supers),
		isDao:         hasAnnotation(annotations, "Dao"),
	})

	// Constructor-based DI: a class with a single constructor, or one annotated
	// @Autowired/@Inject, injects each of that constructor's parameter types.
	w.handleConstructorInjection(node, body, owner, annotations)
	// Field-level @Autowired/@Inject and Lombok @RequiredArgsConstructor over
	// `private final` fields also produce injection edges; emitted while walking
	// the body below.

	if body != nil {
		w.walkBody(body, owner)
	}

	w.routeStack = w.routeStack[:len(w.routeStack)-1]
	w.typeStack = w.typeStack[:len(w.typeStack)-1]
	w.methodStack = w.methodStack[:len(w.methodStack)-1]
	w.popOwner()
}

// walkBody iterates the direct members of a class/interface/enum body, handling
// nested declarations, methods, and fields. Non-declaration nodes are scanned for
// constructor calls attributed to `owner`.
func (w *astWalker) walkBody(body *sitter.Node, owner *facts.Fact) {
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		switch kindOf(c) {
		case "class_declaration":
			w.handleClassLike(c, facts.SymbolClass)
		case "interface_declaration":
			w.handleClassLike(c, facts.SymbolInterface)
		case "enum_declaration":
			w.handleClassLike(c, facts.SymbolEnum)
		case "record_declaration":
			w.handleClassLike(c, facts.SymbolClass)
		case "annotation_type_declaration":
			w.handleClassLike(c, facts.SymbolInterface)
		case "method_declaration", "constructor_declaration":
			w.handleMethod(c)
		case "field_declaration":
			w.handleField(c, owner)
		default:
			// init blocks, enum constants, etc. — scan for constructor calls.
			w.walkForCalls(c)
		}
	}
}

func (w *astWalker) handleMethod(node *sitter.Node) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)

	modifiers := findChildByKind(node, "modifiers")
	modifierText := ""
	var annotations []javaAnnotation
	if modifiers != nil {
		modifierText = nodeText(modifiers, w.src)
		annotations = parseAnnotations(modifiers, w.src)
	}
	exported := strings.Contains(modifierText, "public")

	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: w.canonicalName(w.qualify(name)),
		File: w.relFile,
		Line: int(node.StartPosition().Row) + 1,
		Props: map[string]any{
			"symbol_kind": facts.SymbolMethod,
			"exported":    exported,
			"language":    "java",
		},
		Relations: []facts.Relation{
			{Kind: facts.RelDeclares, Target: w.dir},
		},
	}
	if t := w.enclosingType(); t != "" {
		f.SetProp("receiver", t)
	}
	if strings.Contains(modifierText, "static") {
		f.SetProp("static", true)
	}

	// Request-mapping annotation on a method: a server route on a @Controller, or an
	// outbound client route on a @FeignClient interface (same annotations, the
	// class-level annotation is the discriminator).
	if rs := w.currentRoute(); rs != nil {
		line := int(node.StartPosition().Row) + 1
		switch {
		case rs.isFeignClient:
			w.out = append(w.out, feignClientFacts(rs.basePath, rs.feignHint, annotations, w.relFile, line, w.dir)...)
		case rs.isController:
			w.out = append(w.out, springRouteFacts(rs.basePath, annotations, w.relFile,
				line, w.dir, w.canonicalName(w.qualify(name)))...)
		}
	}

	w.out = append(w.out, f)
	ownerIdx := len(w.out) - 1
	w.pushOwner(&w.out[ownerIdx])
	// Set up per-method complexity tracking. walkForCalls is re-entrant (it
	// dispatches nested type declarations back through handleMethod), so save and
	// restore the outer state. Props are written via the stable index (the pointer
	// may be invalidated if the body walk grows w.out).
	savedMetrics, savedDepth := w.metrics, w.loopDepth
	savedScaling, savedRepeat := w.scalingDepth, w.repeatDepth
	savedName, savedShort := w.selfName, w.selfShort
	savedParams := w.selfParams
	w.metrics = &javaBodyMetrics{}
	w.loopDepth = 0
	w.scalingDepth = 0
	w.repeatDepth = 0
	w.selfName = f.Name
	w.selfShort = name
	w.selfParams = javaParamCount(node)
	if body := node.ChildByFieldName("body"); body != nil {
		w.walkForCalls(body)
	}
	m := w.metrics
	props := w.out[ownerIdx].Props
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
		// "no call repeats" from "signal absent". An omitted key makes perf fall back to
		// the unfiltered calls_in_loop, defeating the discount in exactly the case it
		// exists for (every in-loop call sitting inside a constant loop).
		if m.callsInScalingLoop == nil {
			m.callsInScalingLoop = []string{}
		}
		props["calls_in_scaling_loop"] = m.callsInScalingLoop
	}
	// A body that calls super.<self>() is an override delegating to a same-named
	// overload, not genuine recursion — clear the arity-matched self-call flag.
	if m.recursive && !m.sawSuperSelf {
		props["recursive_self"] = true
	}
	// GAP-JV-02: flag genuine DB/network round-trips so the performance analyzer's
	// isExpensiveJvmCall I/O index sees the in-loop callee. Seeded from type-level
	// signals (@FeignClient / Spring Data repository / @Dao) plus unambiguous query
	// annotations (@Query/@Modifying/@Procedure) — never a bare HTTP verb or
	// @GetMapping, which on server-side Java is an INBOUND handler, not I/O.
	// performs_io == io_direct: no transitive pass (Java call edges are same-class
	// only, and the consumer matches the flagged leaf callee by short name).
	if w.methodPerformsIO(annotations) {
		props["io_direct"] = true
		props["performs_io"] = true
	}
	w.metrics, w.loopDepth = savedMetrics, savedDepth
	w.scalingDepth, w.repeatDepth = savedScaling, savedRepeat
	w.selfName, w.selfShort = savedName, savedShort
	w.selfParams = savedParams
	w.popOwner()
}

func (w *astWalker) handleField(node *sitter.Node, owner *facts.Fact) {
	modifiers := findChildByKind(node, "modifiers")
	modifierText := ""
	var annotations []javaAnnotation
	if modifiers != nil {
		modifierText = nodeText(modifiers, w.src)
		annotations = parseAnnotations(modifiers, w.src)
	}
	exported := strings.Contains(modifierText, "public")

	symbolKind := facts.SymbolVariable
	if strings.Contains(modifierText, "final") {
		symbolKind = facts.SymbolConstant
	}

	// Field type — used for DI edges when @Autowired/@Inject is present.
	typeNode := node.ChildByFieldName("type")
	typeTarget := w.targetForType(typeNode)
	injected := hasAnnotation(annotations, "Autowired", "Inject", "Resource", "Reference")
	// A `static final String FOO = "literal"` constant exposes its value so that
	// references to it (e.g. @Table(name = FOO)) can be resolved in a later pass.
	captureValue := strings.Contains(modifierText, "static") &&
		strings.Contains(modifierText, "final") &&
		typeFullName(typeNode, w.src) == "String"

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if kindOf(c) != "variable_declarator" {
			continue
		}
		nameNode := c.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nodeText(nameNode, w.src)
		props := map[string]any{
			"symbol_kind": symbolKind,
			"exported":    exported,
			"language":    "java",
		}
		if captureValue && isScreamingSnake(name) {
			if val, ok := stringLiteralValue(c.ChildByFieldName("value"), w.src); ok {
				props["value"] = val
			}
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

	if injected && typeTarget != "" && owner != nil {
		owner.Relations = append(owner.Relations, facts.Relation{Kind: facts.RelInjects, Target: typeTarget})
	}
	// A field initializer may contain a constructor call (`= new Foo()`).
	w.walkForCalls(node)
}

// handleConstructorInjection emits RelInjects edges from `owner` to each parameter
// type of an injectable constructor: a sole constructor, a constructor annotated
// @Autowired/@Inject, or (Lombok) when the class carries @RequiredArgsConstructor /
// @AllArgsConstructor.
func (w *astWalker) handleConstructorInjection(decl, body *sitter.Node, owner *facts.Fact, classAnns []javaAnnotation) {
	lombokInject := hasAnnotation(classAnns, "RequiredArgsConstructor", "AllArgsConstructor")

	// record_declaration parameters are constructor parameters too.
	if kindOf(decl) == "record_declaration" {
		if params := decl.ChildByFieldName("parameters"); params != nil && lombokInject {
			w.injectParams(params, owner)
		}
	}

	if lombokInject {
		// Inject each `private final` field's type.
		if body != nil {
			for i := uint(0); i < uint(body.ChildCount()); i++ {
				c := body.Child(i)
				if kindOf(c) != "field_declaration" {
					continue
				}
				mods := findChildByKind(c, "modifiers")
				if mods == nil || !strings.Contains(nodeText(mods, w.src), "final") {
					continue
				}
				if t := w.targetForType(c.ChildByFieldName("type")); t != "" && owner != nil {
					owner.Relations = append(owner.Relations, facts.Relation{Kind: facts.RelInjects, Target: t})
				}
			}
		}
	}

	if body == nil {
		return
	}
	var ctors []*sitter.Node
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		if c := body.Child(i); kindOf(c) == "constructor_declaration" {
			ctors = append(ctors, c)
		}
	}
	for _, ctor := range ctors {
		mods := findChildByKind(ctor, "modifiers")
		annotated := false
		if mods != nil {
			annotated = hasAnnotation(parseAnnotations(mods, w.src), "Autowired", "Inject")
		}
		if annotated || (len(ctors) == 1 && !lombokInject) {
			if params := ctor.ChildByFieldName("parameters"); params != nil {
				w.injectParams(params, owner)
			}
		}
	}
}

func (w *astWalker) injectParams(params *sitter.Node, owner *facts.Fact) {
	if owner == nil {
		return
	}
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		p := params.Child(i)
		if kindOf(p) != "formal_parameter" {
			continue
		}
		if t := w.targetForType(p.ChildByFieldName("type")); t != "" {
			owner.Relations = append(owner.Relations, facts.Relation{Kind: facts.RelInjects, Target: t})
		}
	}
}

// walkForCalls recursively scans a subtree for object_creation_expression (→
// RelInstantiates) and method_invocation (→ RelCalls for resolvable same-class
// calls), attributing each to the current owner. Nested type declarations are
// dispatched to their own handlers so their calls are attributed correctly.
func (w *astWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	kind := kindOf(node)

	// A lambda is a deferred scope: its body runs when invoked, NOT per-iteration of
	// the enclosing loops — so reset the loop depth for its subtree (e.g. a Runnable
	// or listener defined inside a loop). A stream iterator's OWN lambda is handled
	// in the method_invocation branch (its body walks at +1).
	if w.metrics != nil && kind == "lambda_expression" {
		saved, savedScaling, savedRepeat := w.loopDepth, w.scalingDepth, w.repeatDepth
		w.loopDepth, w.scalingDepth, w.repeatDepth = 0, 0, 0
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkForCalls(node.Child(i))
		}
		w.loopDepth, w.scalingDepth, w.repeatDepth = saved, savedScaling, savedRepeat
		return
	}

	// Complexity metrics: count decision points so the single body walk doubles as
	// the cyclomatic pass.
	if w.metrics != nil {
		switch kind {
		case "if_statement", "ternary_expression", "switch_label", "catch_clause":
			w.metrics.decisions++
		case "binary_expression":
			if javaBooleanOp(node) {
				w.metrics.decisions++
			}
		}
	}

	switch kind {
	case "class_declaration":
		w.handleClassLike(node, facts.SymbolClass)
		return
	case "interface_declaration":
		w.handleClassLike(node, facts.SymbolInterface)
		return
	case "enum_declaration":
		w.handleClassLike(node, facts.SymbolEnum)
		return
	case "record_declaration":
		w.handleClassLike(node, facts.SymbolClass)
		return
	case "for_statement", "enhanced_for_statement", "while_statement", "do_statement":
		// Syntactic loops: everything in the body runs per iteration. A constant-count
		// loop raises loop_depth but not scaling_loop_depth (the Big-O exponent); an
		// infinite loop is discounted from the exponent but still repeats.
		class := javaSyntacticLoopClass(node, w.src)
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
		for i := uint(0); i < uint(node.ChildCount()); i++ {
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
	case "object_creation_expression":
		if t := w.targetForType(node.ChildByFieldName("type")); t != "" {
			if owner := w.currentOwner(); owner != nil {
				owner.Relations = append(owner.Relations, facts.Relation{Kind: facts.RelInstantiates, Target: t})
			}
		}
	case "method_invocation":
		w.handleInvocation(node)
		// A Stream/Collection iterator with a lambda (items.forEach(x -> …)) is a
		// loop: its lambda body runs per element, but the receiver/other args once.
		if w.metrics != nil {
			if lambda := javaStreamLambda(node, w.src); lambda != nil {
				// A `List.of(a, b).forEach(...)` over a literal receiver iterates a fixed
				// count — it raises loop_depth but not the scaling depth. The depth bump
				// happens at the lambda body inside walkJavaLambdaSubtree; here we only
				// record the maxes.
				bounded := javaStreamReceiverBounded(node, w.src)
				w.metrics.loopCount++
				w.metrics.decisions++
				if w.loopDepth+1 > w.metrics.loopDepth {
					w.metrics.loopDepth = w.loopDepth + 1
				}
				if !bounded && w.scalingDepth+1 > w.metrics.scalingLoopDepth {
					w.metrics.scalingLoopDepth = w.scalingDepth + 1
				}
				for i := uint(0); i < uint(node.ChildCount()); i++ {
					if c := node.Child(i); javaByteContains(c, lambda) {
						w.walkJavaLambdaSubtree(c, lambda, bounded)
					} else {
						w.walkForCalls(c)
					}
				}
				return
			}
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkForCalls(node.Child(i))
	}
}

func (w *astWalker) handleInvocation(node *sitter.Node) {
	owner := w.currentOwner()
	if owner == nil {
		return
	}
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, w.src)
	obj := node.ChildByFieldName("object")

	// HTTP client call site (RestTemplate). Emitted independently of the call-graph
	// edge below; guarded by a path-like literal argument so non-HTTP same-named
	// calls (map.put, list.delete) don't match.
	w.detectRestTemplateCall(node, name)

	// Resolve bare `foo()` and `this.foo()` calls against the enclosing class's
	// own methods. Calls on other receivers are left unresolved (the receiver's
	// type is not tracked), matching the Kotlin extractor's conservative model.
	isThis := obj != nil && nodeText(obj, w.src) == "this"
	if obj != nil && w.metrics != nil && name == w.selfShort && nodeText(obj, w.src) == "super" {
		// super.<self>() — an override delegating to its supertype. Note it so an
		// arity-matched bare <self>(…) call is read as overload delegation, not recursion.
		w.metrics.sawSuperSelf = true
	}
	if obj == nil || isThis {
		if methods := w.currentMethods(); methods[name] {
			target := w.dir + "." + w.enclosingType() + "." + name
			owner.Relations = append(owner.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: target,
			})
			w.recordCallMetrics(target, javaArgCount(node))
		}
	} else if w.metrics != nil && w.loopDepth > 0 && !javaCheapMethods[name] {
		// Method call on a non-this receiver inside a loop (repo.findById(), …). No
		// graph edge today, but its name feeds the perf metric so the performance
		// analyzer can flag per-iteration JPA/JDBC/network I/O.
		tgt := name
		if recv := nodeText(obj, w.src); recv != "" {
			tgt = recv + "." + name
		}
		w.recordInLoop(tgt)
	}
}

// targetForType returns a relation target for a `_type` node: the rightmost simple
// name resolved through the import map to an FQN, a same-package FQN when not
// imported, or the written FQN when the reference is already qualified. Returns ""
// for primitive/void/unresolvable types.
func (w *astWalker) targetForType(typeNode *sitter.Node) string {
	if typeNode == nil {
		return ""
	}
	full := typeFullName(typeNode, w.src)
	if full == "" {
		return ""
	}
	if isPrimitiveType(full) {
		return ""
	}
	simple := full
	if i := strings.LastIndex(full, "."); i >= 0 {
		// Already qualified in source — use as written.
		return full
	}
	if fqn, ok := w.importMap[simple]; ok {
		return fqn
	}
	if javaLangTypes[simple] {
		return ""
	}
	if w.pkg != "" {
		return w.pkg + "." + simple
	}
	return simple
}

// supertypeTargets returns canonicalization targets (FQNs) for a type's superclass
// and implemented/extended interfaces.
func (w *astWalker) supertypeTargets(node *sitter.Node) []string {
	var out []string
	for _, n := range w.supertypeNodes(node) {
		if t := w.targetForType(n); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// supertypeSimpleNames returns the simple names of a type's supertypes (used by
// component classification, e.g. detecting Spring Data repository interfaces).
func (w *astWalker) supertypeSimpleNames(node *sitter.Node) []string {
	var out []string
	for _, n := range w.supertypeNodes(node) {
		if s := lastTypeComponent(typeFullName(n, w.src)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (w *astWalker) supertypeNodes(node *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	if sc := node.ChildByFieldName("superclass"); sc != nil {
		out = append(out, firstTypeChild(sc))
	}
	// `interfaces` field (class/enum/record) or `extends_interfaces` child (interface).
	if iface := node.ChildByFieldName("interfaces"); iface != nil {
		out = append(out, typeListChildren(iface)...)
	}
	if ext := findChildByKind(node, "extends_interfaces"); ext != nil {
		out = append(out, typeListChildren(ext)...)
	}
	// Filter nils.
	kept := out[:0]
	for _, n := range out {
		if n != nil {
			kept = append(kept, n)
		}
	}
	return kept
}

// --- tree-sitter / type helpers ---

func classBody(node *sitter.Node) *sitter.Node {
	if b := node.ChildByFieldName("body"); b != nil {
		return b
	}
	return nil
}

// typeListChildren returns the concrete `_type` children of a super_interfaces /
// extends_interfaces node, which wrap a single type_list.
func typeListChildren(node *sitter.Node) []*sitter.Node {
	tl := findChildByKind(node, "type_list")
	if tl == nil {
		return nil
	}
	var out []*sitter.Node
	for i := uint(0); i < uint(tl.ChildCount()); i++ {
		c := tl.Child(i)
		if c.IsNamed() && kindOf(c) != "annotation" && kindOf(c) != "marker_annotation" {
			out = append(out, c)
		}
	}
	return out
}

// firstTypeChild returns the first named, non-annotation child of a wrapper node
// (e.g. the `_type` under a superclass node).
func firstTypeChild(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.IsNamed() && kindOf(c) != "annotation" && kindOf(c) != "marker_annotation" {
			return c
		}
	}
	return nil
}

// typeFullName returns the source-written dotted name of a `_type` node, stripping
// generic arguments and array dimensions. For `java.util.List<String>` it returns
// "java.util.List"; for `Map<K,V>` it returns "Map".
func typeFullName(node *sitter.Node, src []byte) string {
	if node == nil {
		return ""
	}
	switch kindOf(node) {
	case "type_identifier", "scoped_type_identifier":
		return nodeText(node, src)
	case "generic_type":
		// First named child is the base type (type_identifier or scoped_type_identifier).
		if base := firstNamedChild(node); base != nil {
			return typeFullName(base, src)
		}
	case "annotated_type":
		// The type follows the annotations.
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			c := node.Child(i)
			if c.IsNamed() && kindOf(c) != "annotation" && kindOf(c) != "marker_annotation" {
				return typeFullName(c, src)
			}
		}
	case "array_type":
		if el := node.ChildByFieldName("element"); el != nil {
			return typeFullName(el, src)
		}
	}
	// Primitive / void / fallback: take the raw text, stripped of generics/arrays.
	t := nodeText(node, src)
	if i := strings.IndexAny(t, "<[ "); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

func lastTypeComponent(full string) string {
	if i := strings.LastIndex(full, "."); i >= 0 {
		return full[i+1:]
	}
	return full
}

func collectMethodNames(body *sitter.Node, src []byte) map[string]bool {
	methods := make(map[string]bool)
	if body == nil {
		return methods
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if kindOf(c) != "method_declaration" {
			continue
		}
		if nameNode := c.ChildByFieldName("name"); nameNode != nil {
			methods[nodeText(nameNode, src)] = true
		}
	}
	return methods
}

// isScreamingSnake reports whether a name is an UPPER_SNAKE_CASE constant
// identifier (the convention for table/column name constants).
func isScreamingSnake(s string) bool {
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

// stringLiteralValue returns the unquoted contents of a string_literal node.
func stringLiteralValue(node *sitter.Node, src []byte) (string, bool) {
	if node == nil || kindOf(node) != "string_literal" {
		return "", false
	}
	return strings.Trim(nodeText(node, src), `"`), true
}

func isPrimitiveType(name string) bool {
	switch name {
	case "void", "boolean", "byte", "short", "int", "long", "char", "float", "double", "var":
		return true
	}
	return false
}

// javaLangTypes are implicitly imported java.lang types that appear as bare names
// without an import statement; resolving them would create dangling edges.
var javaLangTypes = map[string]bool{
	"Object": true, "String": true, "Integer": true, "Long": true, "Short": true,
	"Byte": true, "Character": true, "Boolean": true, "Float": true, "Double": true,
	"Number": true, "Math": true, "System": true, "Thread": true, "Runnable": true,
	"Exception": true, "RuntimeException": true, "Throwable": true, "Error": true,
	"IllegalArgumentException": true, "IllegalStateException": true,
	"NullPointerException": true, "UnsupportedOperationException": true,
	"Class": true, "Enum": true, "Iterable": true, "Comparable": true, "CharSequence": true,
	"StringBuilder": true, "StringBuffer": true, "Void": true, "Override": true,
	"Deprecated": true, "SuppressWarnings": true, "FunctionalInterface": true,
	"AutoCloseable": true, "Cloneable": true, "ClassLoader": true, "Process": true,
}

func findChildByKind(node *sitter.Node, kind string) *sitter.Node {
	if node == nil {
		return nil
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if kindOf(c) == kind {
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
		c := node.Child(i)
		if c.IsNamed() {
			return c
		}
	}
	return nil
}

func nodeText(node *sitter.Node, src []byte) string { return tsutil.Text(node, src) }
