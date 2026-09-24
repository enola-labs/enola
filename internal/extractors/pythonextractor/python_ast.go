package pythonextractor

import (
	"path/filepath"
	"strings"
	"unicode"

	"github.com/enola-labs/enola/internal/extractors/tsutil"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

// extractFileAST parses a Python file with tree-sitter and emits architectural
// facts. It is a superset of extractFile: every symbol / import / route / storage
// fact is preserved, and RelCalls / RelInstantiates edges are added when call
// sites are observed inside function bodies.
//
// It also returns the file's FastAPI router wiring, which extractPython feeds to
// composeRouterPrefixes once every file is known — mount prefixes routinely cross
// module boundaries, so they cannot be folded per file.
func extractFileAST(src []byte, relFile string, isDjango, isFlask, isFastAPI bool, idx *pySymbolIndex) ([]facts.Fact, pyRouterTopology) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(python.Language())); err != nil {
		return nil, pyRouterTopology{}
	}

	tree := parser.Parse(src, nil)
	defer tree.Close()

	module := strings.TrimSuffix(relFile, ".py")
	dir := factpath.Dir(relFile)

	w := &pyWalker{
		src:       src,
		relFile:   relFile,
		module:    module,
		dir:       dir,
		isDjango:  isDjango,
		isFlask:   isFlask,
		isFastAPI: isFastAPI,
		idx:       idx,
	}
	w.walkModule(tree.RootNode())

	// Collected after the walk so the import map is fully populated: a mount
	// argument is routinely a router imported from another module.
	topo := collectRouterTopology(tree.RootNode(), src, relFile, module, w.importMap)
	topo.routes = w.routeRefs
	topo.paths = w.pathRefs
	topo.consts = collectConsts(tree.RootNode(), src, module, w.importMap)
	return w.out, topo
}

// extractFileIndexed is extractFileAST for the extractor's own path: it parses the
// file ONCE and returns, besides the facts and the router topology, the index this
// file contributes and the implementor lookups that need the merged one.
//
// extractFileAST above keeps taking a ready-made index, because a single-file test
// supplies one directly and has no merge step to defer anything to.
func extractFileIndexed(src []byte, relFile string, isDjango, isFlask, isFastAPI bool) ([]facts.Fact, pyRouterTopology, *pySymbolIndex, []pyImplCall) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(python.Language())); err != nil {
		return nil, pyRouterTopology{}, nil, nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	root := tree.RootNode()

	module := strings.TrimSuffix(relFile, ".py")
	local := &pySymbolIndex{classes: make(map[string]*pyClassInfo), moduleDefs: make(map[string]map[string]bool)}
	indexTree(root, src, relFile, local)

	w := &pyWalker{
		src:               src,
		relFile:           relFile,
		module:            module,
		dir:               factpath.Dir(relFile),
		isDjango:          isDjango,
		isFlask:           isFlask,
		isFastAPI:         isFastAPI,
		idx:               local,
		deferImplementors: true,
	}
	w.walkModule(root)

	topo := collectRouterTopology(root, src, relFile, module, w.importMap)
	topo.routes = w.routeRefs
	topo.paths = w.pathRefs
	topo.consts = collectConsts(root, src, module, w.importMap)
	return w.out, topo, local, w.pendingImpl
}

type pyWalker struct {
	src       []byte
	relFile   string
	module    string
	dir       string
	isDjango  bool
	isFlask   bool
	isFastAPI bool

	out []facts.Fact

	// typeStack holds enclosing class names so methods get qualified names.
	typeStack []string

	// ownerStack: top element is the index into w.out of the fact that receives
	// RelCalls / RelInstantiates discovered while walking its body. Indices are
	// used instead of pointers because appending to w.out can reallocate the
	// backing array, invalidating any previously captured pointer.
	ownerStack []int

	// importMap maps a local name to its canonical fact target (empty = external).
	importMap map[string]string

	// importsModal records whether the file imports Modal. Modal registers remote
	// functions with @app.function()/@app.cls(), decorator names far too generic to
	// match on their own — "function" would swallow any @x.function-decorated symbol
	// in any codebase. The import is the discriminator. Set during the walk, so it is
	// reliable for the module-level imports that precede the definitions it guards.
	importsModal bool

	// emittedImports holds the start byte of every import statement that has already
	// produced a dependency fact. A method body is registered TWICE — once by
	// handleFunction, once by walkNestedScope reached through the class body's call
	// walk — and each pass needs the importMap binding, but the fact must be emitted
	// only once or the import is double-counted.
	emittedImports map[uint]bool

	// importDeferred is set while registering imports that do NOT run when the
	// module is imported: a function/class-body (lazy) import, or a
	// `if TYPE_CHECKING:` block, which never executes at runtime at all. Both are
	// off the import path, which is the distinction an import-closure walk needs
	// and the one indentation cannot draw — a module-level `try: import x` is
	// indented too, yet runs. Recorded as Props["deferred"], set only when true.
	importDeferred bool

	// importFallback is set while walking an except_clause: its imports are the
	// fallback arm of the try/except ImportError dual-import idiom and must not
	// clobber the try-branch binding (the relative/canonical form resolves to a
	// real path at walk time; the bare fallback would be dropped as external by
	// resolveCallTargets, deleting the edge).
	importFallback bool

	// methodSets[i] is the set of methods declared directly in typeStack[i],
	// used to resolve bare same-class calls.
	methodSets []map[string]bool

	// idx is the symbol index the walk resolves against, nil when called from tests
	// that do not need cross-file resolution. All lookups must nil-check.
	//
	// On the extractor's own path this is the CURRENT FILE's index, not the merged
	// one: the two lookups made during the walk (resolveCall and the value-reference
	// check) only ever ask about w.module, which no other file contributes to. The
	// one lookup that is genuinely cross-file is deferred instead, below.
	idx *pySymbolIndex

	// deferImplementors records implementor-call lookups rather than answering them,
	// because they need the merged index. Answering them during the walk is what
	// forced a whole separate indexing pass, and with it a second parse of every
	// file in the repository.
	deferImplementors bool
	pendingImpl       []pyImplCall

	// localTypes maps a variable name in the current function scope to its
	// canonical qualified type. Reset at the entry of every handleFunction call.
	localTypes map[string]string

	// localBound is the set of names bound in the current function's own scope
	// (params + assigned/iterated/aliased names). Guards bare-identifier call
	// and value-reference resolution against shadowing. Reset per handleFunction call.
	localBound map[string]bool

	// Per-function complexity state, set up by handleFunction around walkForCalls.
	// metrics is nil outside a function body walk. loopDepth is the current loop
	// nesting depth; selfName is the enclosing function's qualified name (for
	// direct-recursion detection).
	metrics      *pyBodyMetrics
	loopDepth    int
	scalingDepth int // current nesting counting only input-scaling (unbounded) loops
	// repeatDepth counts the enclosing loops that run a non-constant number of times.
	// It differs from scalingDepth for `while True:`, which adds no factor of n but whose
	// body still runs many times — so a query inside it is still an N+1 candidate.
	repeatDepth int
	selfName    string

	// funcScope is the qualified name of the OUTERMOST enclosing function whose
	// body is being walked ("" at module/class level). Unlike selfName it is
	// saved and restored, and it survives walkNestedScope — so a decorator on a
	// def nested in a router factory still reports the factory as its scope.
	// Used to key a function-local router variable to its factory function.
	funcScope string

	// routeRefs ties each emitted route fact (by index into w.out) to the router
	// variable it was registered on, for composeRouterPrefixes.
	routeRefs []pyRouteRef

	// pathRefs holds the routes emitted with a computed path (a constant, an
	// f-string), for resolveRoutePaths.
	pathRefs []pyPathRef

	// routeDecorators is the set of decorator start byte offsets already turned
	// into route facts, so a decorator reached by two walks emits once. A class
	// body is walked twice by design (handleClass: walkBody for owners, then
	// walkForCalls for class-level expressions), which routes a method's
	// decorators through both handleDecoratedDefinition and walkNestedScope.
	// Offsets are unique within a file, and a pyWalker is per-file.
	routeDecorators map[uint]bool

	// fileRefs accumulates RelCalls edges made in file-scope (module-level) code
	// and by decorators — references with no enclosing symbol owner. They are
	// emitted as a single KindFileRef fact per file, which the dead-code detector
	// folds in so top-level/decorator use marks a production symbol used.
	fileRefs []facts.Relation
}

// pyBodyMetrics accumulates per-function complexity signals during the single
// walkForCalls body traversal — mirrors the Go extractor's bodyMetrics.
type pyBodyMetrics struct {
	loopDepth          int             // max loop nesting depth
	scalingLoopDepth   int             // max nesting counting only unbounded (input-scaling) loops
	loopCount          int             // number of loop/comprehension constructs
	decisions          int             // decision points (cyclomatic = 1 + decisions)
	callsInLoop        []string        // distinct call targets invoked at loop depth >= 1
	inLoopSeen         map[string]bool // dedup set for callsInLoop
	callsInScalingLoop []string        // distinct call targets invoked at scaling (unbounded) depth >= 1
	inScalingSeen      map[string]bool // dedup set for callsInScalingLoop
	recursive          bool            // body directly calls the enclosing function
	ioDirect           bool            // body directly invokes a network/file/DB I/O primitive
}

// recordCallMetrics notes a resolved call target against the current function's
// complexity metrics: flags direct recursion and records calls made inside loops.
func (w *pyWalker) recordCallMetrics(target string) {
	if w.metrics == nil || target == "" {
		return
	}
	if target == w.selfName {
		w.metrics.recursive = true
	}
	if w.loopDepth > 0 {
		if w.metrics.inLoopSeen == nil {
			w.metrics.inLoopSeen = make(map[string]bool)
		}
		if !w.metrics.inLoopSeen[target] {
			w.metrics.inLoopSeen[target] = true
			w.metrics.callsInLoop = append(w.metrics.callsInLoop, target)
		}
	}
	// A call inside a loop that repeats a non-constant number of times is an N+1
	// candidate. Only a genuinely constant loop (literal collection / range(<const>))
	// excludes its calls; `while True` repeats, so its calls stay candidates even though
	// its depth is discounted from the Big-O exponent.
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

func (w *pyWalker) pushOwner(idx int) { w.ownerStack = append(w.ownerStack, idx) }
func (w *pyWalker) popOwner() {
	if len(w.ownerStack) > 0 {
		w.ownerStack = w.ownerStack[:len(w.ownerStack)-1]
	}
}
func (w *pyWalker) currentOwner() *facts.Fact {
	if len(w.ownerStack) == 0 {
		return nil
	}
	return &w.out[w.ownerStack[len(w.ownerStack)-1]]
}

// currentOwnerIdx is currentOwner as an index into w.out, for edges resolved after
// the walk: a *Fact would dangle as soon as a later append reallocates w.out.
func (w *pyWalker) currentOwnerIdx() int {
	if len(w.ownerStack) == 0 {
		return -1
	}
	return w.ownerStack[len(w.ownerStack)-1]
}

func (w *pyWalker) enclosingType() string { return strings.Join(w.typeStack, ".") }

func (w *pyWalker) qualify(name string) string {
	if t := w.enclosingType(); t != "" {
		return t + "." + name
	}
	return name
}

func (w *pyWalker) pushType(name string, methods map[string]bool) {
	w.typeStack = append(w.typeStack, name)
	w.methodSets = append(w.methodSets, methods)
}

func (w *pyWalker) popType() {
	if len(w.typeStack) > 0 {
		w.typeStack = w.typeStack[:len(w.typeStack)-1]
	}
	if len(w.methodSets) > 0 {
		w.methodSets = w.methodSets[:len(w.methodSets)-1]
	}
}

func (w *pyWalker) currentMethods() map[string]bool {
	if len(w.methodSets) == 0 {
		return nil
	}
	return w.methodSets[len(w.methodSets)-1]
}

// walkModule iterates the top-level statements of a module node.
func (w *pyWalker) walkModule(root *sitter.Node) {
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		child := root.Child(i)
		w.walkStatement(child)
		// Collect module-scope call edges (e.g. `app = cached_app(...)`), which have
		// no enclosing symbol owner and are otherwise invisible to the dead-code
		// detector. Nested class/function bodies are skipped — they own their calls.
		w.walkTopLevelCalls(child)
	}
	if len(w.fileRefs) > 0 {
		w.out = append(w.out, facts.Fact{
			Kind:      facts.KindFileRef,
			Name:      w.relFile,
			File:      w.relFile,
			Line:      1,
			Props:     map[string]any{"language": "python"},
			Relations: w.fileRefs,
		})
	}
}

// walkTopLevelCalls scans a module-level statement subtree for call sites,
// recording each as a file-scope RelCalls edge. It descends through compound
// statements (if/for/with/try blocks) but stops at nested class/function
// definitions, whose calls are attributed to their own symbol owner, and at
// import statements, which are not calls.
func (w *pyWalker) walkTopLevelCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "class_definition", "function_definition", "decorated_definition",
		"import_statement", "import_from_statement":
		return
	case "call":
		if fn := node.ChildByFieldName("function"); fn != nil {
			w.emitFileRefCall(fn)
		}
		w.emitCallRoute(node)
		if args := node.ChildByFieldName("arguments"); args != nil {
			w.fileRefs = append(w.fileRefs, w.argRefRelations(args)...)
		}
	case "assignment":
		// A module-level assignment whose RHS is a bare def name installs that
		// symbol as a value (the click monkeypatch idiom `click.echo = handler`,
		// dispatch-table entries, alias exports). Fold the RHS value-ref so the
		// referenced def is not mis-flagged dead. resolveCall (via emitFileRefCall)
		// gates on moduleDefs, so only real defs fold — a plain identifier that
		// names a variable resolves to nothing. Nested calls in the RHS are still
		// caught by the generic recursion below.
		if rhs := node.ChildByFieldName("right"); rhs != nil && kindOf(rhs) == "identifier" {
			w.emitFileRefCall(rhs)
		}
	case "string":
		if rel, ok := w.stringRefRelation(node); ok {
			w.fileRefs = append(w.fileRefs, rel)
		}
	case "dictionary", "list", "set", "tuple":
		w.emitCollectionValueRefs(node)
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkTopLevelCalls(node.Child(i))
	}
}

// emitFileRefCall resolves a module-level callee node and records a file-scope
// reference. Mirrors emitCallEdge but without an owner, self/cls handling, or
// complexity metrics (none apply at module scope). KindFileRef carries only
// RelCalls, so capitalized constructors are folded in as calls too — short-name
// matching downstream still marks the class used.
func (w *pyWalker) emitFileRefCall(fn *sitter.Node) {
	switch kindOf(fn) {
	case "identifier":
		name := pyText(fn, w.src)
		if pyBuiltins[name] {
			return
		}
		if pyCapitalized(name) {
			w.fileRefs = append(w.fileRefs, facts.Relation{Kind: facts.RelCalls, Target: name})
			return
		}
		if target := w.resolveCall(name); target != "" {
			w.fileRefs = append(w.fileRefs, facts.Relation{Kind: facts.RelCalls, Target: target})
		}
	case "attribute":
		objNode := fn.ChildByFieldName("object")
		attrNode := fn.ChildByFieldName("attribute")
		if objNode == nil || attrNode == nil {
			return
		}
		obj := pyText(objNode, w.src)
		attr := pyText(attrNode, w.src)
		if qualType := w.resolveVarType(obj); qualType != "" {
			w.fileRefs = append(w.fileRefs, facts.Relation{Kind: facts.RelCalls, Target: qualType + "." + attr})
		}
	}
}

// emitDecoratorRef records a use of a decorator function so `@my_decorator`
// (which never appears as a call node) marks the decorator used. The decorator
// root is resolved via the import map (absolute imports) or same-module
// fallback; unresolved builtins (`@property`, `@staticmethod`) are skipped.
func (w *pyWalker) emitDecoratorRef(dec string) {
	if dec == "" {
		return
	}
	if i := strings.IndexByte(dec, '.'); i >= 0 {
		root := dec[:i]
		tail := dec[i+1:]
		if qt := w.resolveVarType(root); qt != "" {
			w.fileRefs = append(w.fileRefs, facts.Relation{Kind: facts.RelCalls, Target: qt + "." + tail})
		}
		return
	}
	if pyBuiltins[dec] {
		return
	}
	if target := w.resolveCall(dec); target != "" {
		w.fileRefs = append(w.fileRefs, facts.Relation{Kind: facts.RelCalls, Target: target})
	}
}

func (w *pyWalker) walkStatement(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "import_statement":
		w.handleImport(node)
	case "import_from_statement":
		w.handleFromImport(node)
	case "class_definition":
		w.handleClass(node, nil)
	case "function_definition":
		w.handleFunction(node, nil)
	case "decorated_definition":
		w.handleDecoratedDefinition(node)
	case "expression_statement":
		// __tablename__ = "foo" (SQLAlchemy) lives here at class body level.
		// urlpatterns = [...] (Django) lives at module level.
		w.handleExprStatement(node)
	case "assignment":
		// tree-sitter may parse assignments as "assignment" nodes at module level.
		w.handleAssignment(node)
	case "block":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.walkStatement(node.Child(i))
		}
	case "try_statement", "if_statement":
		// Module-level compound statements are module scope at runtime: the
		// try/except ImportError dual-import idiom, `if __name__ == "__main__":`
		// imports, and `if TYPE_CHECKING:` type-level imports all live here.
		// registerBodyImports descends the whole subtree registering those
		// imports (handling the except-branch fallback) and stops at nested
		// def/class nodes — deliberately: a def/class guarded by a conditional is
		// almost always an intentional shim (a macOS-only fallback, a
		// TYPE_CHECKING typing stub, an ImportError alternative) whose name is
		// bound by a sibling branch, so emitting a symbol for it only manufactures
		// a dead-code false positive. Module-level *calls* and assignment value
		// refs in these blocks are still collected by walkTopLevelCalls, which
		// descends through compound statements.
		// `if TYPE_CHECKING:` is the one module-level conditional that never runs:
		// its imports exist for the type checker only, so they are off the import
		// path even though every other module-level if/try is on it.
		prevDeferred := w.importDeferred
		if isTypeCheckingGuard(node, w.src) {
			w.importDeferred = true
		}
		w.registerBodyImports(node)
		w.importDeferred = prevDeferred
		if !isTypeCheckingGuard(node, w.src) {
			w.walkGuardedRoutes(node)
		}
	}
}

// isTypeCheckingGuard reports whether an if_statement is guarded by
// typing.TYPE_CHECKING, whose body is never executed at runtime.
func isTypeCheckingGuard(node *sitter.Node, src []byte) bool {
	if kindOf(node) != "if_statement" {
		return false
	}
	cond := node.ChildByFieldName("condition")
	if cond == nil {
		return false
	}
	return strings.Contains(pyText(cond, src), "TYPE_CHECKING")
}

// handleImport handles `import foo.bar` — emits KindDependency + RelImports.
func (w *pyWalker) handleImport(node *sitter.Node) {
	emit := w.claimImport(node)
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if kindOf(c) == "dotted_name" || kindOf(c) == "aliased_import" {
			var name, alias string
			if kindOf(c) == "aliased_import" {
				nameNode := c.ChildByFieldName("name")
				aliasNode := c.ChildByFieldName("alias")
				if nameNode == nil {
					continue
				}
				name = pyText(c.ChildByFieldName("name"), w.src)
				if aliasNode != nil {
					alias = pyText(aliasNode, w.src)
				}
			} else {
				name = pyText(c, w.src)
			}
			w.noteModalImport(name)
			target := w.module + " -> " + name
			if emit {
				w.out = append(w.out, facts.Fact{
					Kind:  facts.KindDependency,
					Name:  target,
					File:  w.relFile,
					Line:  int(node.StartPosition().Row) + 1,
					Props: w.importProps(nil),
					Relations: []facts.Relation{
						{Kind: facts.RelImports, Target: name},
					},
				})
			}
			local := alias
			if local == "" {
				if dot := strings.LastIndex(name, "."); dot >= 0 {
					local = name[dot+1:]
				} else {
					local = name
				}
			}
			// Record the dotted module path (e.g. "a.b.c") so attribute calls on the
			// bound name emit an edge. resolveCallTargets (post-pass) rewrites internal
			// targets to slash symbol names and drops genuinely-external ones.
			w.setImport(local, name)
		}
	}
}

// handleFromImport handles `from foo.bar import Baz, Qux`.
func (w *pyWalker) handleFromImport(node *sitter.Node) {
	moduleNode := node.ChildByFieldName("module_name")
	if moduleNode == nil {
		return
	}
	moduleName := pyText(moduleNode, w.src)
	w.noteModalImport(moduleName)

	// Determine if this is an intra-project import (relative or same-tree dotted).
	isRelative := strings.HasPrefix(moduleName, ".") ||
		strings.HasPrefix(pyText(node, w.src), "from .")

	target := w.module + " -> " + moduleName
	depProps := w.importProps(map[string]any{"from": true})
	if w.claimImport(node) {
		w.out = append(w.out, facts.Fact{
			Kind:  facts.KindDependency,
			Name:  target,
			File:  w.relFile,
			Line:  int(node.StartPosition().Row) + 1,
			Props: depProps,
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: moduleName},
			},
		})
	}

	// For __init__.py, record the imported short names so the dead-code tool can
	// treat them as re-exported (part of the package's public surface) and not
	// flag them as orphans. Kept only here to avoid bloating every from-import.
	isInit := w.relFile == "__init__.py" || strings.HasSuffix(w.relFile, "/__init__.py")
	var reexported []string
	bindings := make(map[string]string)

	// Map each imported name to a resolvable target or "" (external).
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		if kindOf(c) != "dotted_name" && kindOf(c) != "identifier" && kindOf(c) != "aliased_import" {
			continue
		}
		var localName, importedName string
		if kindOf(c) == "aliased_import" {
			n := c.ChildByFieldName("name")
			a := c.ChildByFieldName("alias")
			if n == nil {
				continue
			}
			importedName = pyText(n, w.src)
			if a != nil {
				localName = pyText(a, w.src)
			} else {
				localName = importedName
			}
		} else {
			importedName = pyText(c, w.src)
			localName = importedName
		}
		if localName != "" && importedName != "" && importedName != "*" {
			bindings[localName] = importedName
		}

		if isInit && importedName != "" && importedName != "*" {
			reexported = append(reexported, importedName)
		}

		if isRelative {
			// Relative import → resolve to a local module path.
			base := moduleName
			if strings.HasPrefix(base, ".") {
				base = w.dir + "/" + strings.TrimLeft(base, ".")
			}
			w.setImport(localName, base+"."+importedName)
		} else {
			// Absolute import (e.g. `from airflow.models import DAG`): record the
			// dotted module path so calls to this name emit an edge. resolveCallTargets
			// (post-pass) rewrites internal targets to slash symbol names and drops
			// genuinely-external ones. Relative imports are handled above.
			w.setImport(localName, moduleName+"."+importedName)
		}
	}

	if len(bindings) > 0 {
		depProps["bindings"] = bindings
	}
	if len(reexported) > 0 {
		depProps["reexports"] = reexported
	}
}

// noteModalImport records that this file imports Modal, gating the generic
// @app.function()/@app.cls() decorators (see pyWalker.importsModal).
func (w *pyWalker) noteModalImport(module string) {
	if module == "modal" || strings.HasPrefix(module, "modal.") {
		w.importsModal = true
	}
}

func (w *pyWalker) setImport(local, target string) {
	if local == "" || local == "*" {
		return
	}
	if w.importMap == nil {
		w.importMap = make(map[string]string)
	}
	if w.importFallback {
		if _, exists := w.importMap[local]; exists {
			return
		}
	}
	w.importMap[local] = target
}

// handleDecoratedDefinition unwraps `@decorator\ndef/class ...` nodes.
func (w *pyWalker) handleDecoratedDefinition(node *sitter.Node) {
	var decorators []string
	var pendingApiViewMethods []string
	// pendingRouteIndices holds w.out indices of route facts emitted from
	// decorators before we see the handler name. Indices are used (not pointers)
	// because subsequent appends to w.out may reallocate its backing array.
	var pendingRouteIndices []int
	// isRouteHandler is set when a route-method decorator (@r.get/post/…) is present
	// in any form (literal, path= keyword, or computed path). The handler is
	// dispatched by the framework, so it is tagged as an entry point.
	var isRouteHandler bool

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c := node.Child(i)
		switch kindOf(c) {
		case "decorator":
			text := pyText(c, w.src)
			// Walk decorator-call arguments for nested calls / value refs, e.g.
			// @router.get(dependencies=[Depends(requires_access_asset(method="GET"))]).
			// These expressions are evaluated at import time but live inside the
			// signature node, so no other pass reaches them. Done before the route/
			// apiview branches below (which `continue`) so it runs for every decorator.
			if call := firstChildOfKind(c, "call"); call != nil {
				if args := call.ChildByFieldName("arguments"); args != nil {
					w.walkTopLevelCalls(args)
					// walkTopLevelCalls finds nested CALLS; a function handed to a
					// decorator as a VALUE (@override_run_tasks(run_tasks_distributed),
					// @register(handler)) is a bare identifier and slips past it. That is
					// a real use — the decorator stores the function and the framework
					// invokes it — so without this the referenced function reads as dead.
					w.fileRefs = append(w.fileRefs, w.argRefRelations(args)...)
				}
			}
			// A route-method decorator in any path form (literal, path= keyword, or a
			// computed expression the route regex below cannot parse) marks this a
			// framework-dispatched handler. Covers @r.get / @app.route and Flask-AppBuilder
			// @expose (which has no receiver dot, so routeMethodRe alone misses it).
			isRoute, emitted := w.emitDecoratorRoute(c, text, &pendingRouteIndices)
			if isRoute {
				isRouteHandler = true
			}
			if emitted {
				continue
			}
			// DRF @api_view(['GET','POST']).
			if m := apiViewRe.FindStringSubmatch(text); m != nil {
				pendingApiViewMethods = append(pendingApiViewMethods, httpMethodWordRe.FindAllString(m[1], -1)...)
				continue
			}
			// Generic decorator name capture.
			if m := decoratorRe.FindStringSubmatch(text); m != nil {
				decorators = append(decorators, m[1])
			}

		case "function_definition":
			// @overload stubs are type-checker-only annotations with no runtime
			// body — skip them to avoid duplicate symbol facts.
			if hasDecorator(decorators, "overload") {
				continue
			}
			fnIdx := len(w.out)
			w.handleFunction(c, decorators)
			// Tag a route handler as an entry point (framework-dispatched, no in-code
			// caller). Covers computed/keyword paths the route-fact regex can't parse.
			if isRouteHandler && fnIdx < len(w.out) && w.out[fnIdx].Kind == facts.KindSymbol {
				w.out[fnIdx].SetProp("web_component", "route_handler")
			}
			handlerName := w.module + "." + w.qualify(pyFuncName(c, w.src))
			// Back-fill handler into pending FastAPI route facts.
			for _, idx := range pendingRouteIndices {
				w.out[idx].SetProp("handler", handlerName)
			}
			// A framework-registration decorator (@compiles, @x.register, @sig.connect,
			// @event.listens_for, Flask hooks) dispatches the function — mark it used.
			for _, dec := range decorators {
				if registrationDecorators[lastComponent(dec)] {
					w.fileRefs = append(w.fileRefs, facts.Relation{Kind: facts.RelCalls, Target: handlerName})
					break
				}
			}
			// @api_view routes — emit after we know the handler name.
			if len(pendingApiViewMethods) > 0 {
				for _, meth := range pendingApiViewMethods {
					w.out = append(w.out, facts.Fact{
						Kind: facts.KindRoute,
						Name: meth + " (view) " + handlerName,
						File: w.relFile,
						Line: int(c.StartPosition().Row) + 1,
						Props: map[string]any{
							facts.PropRole: facts.RoleServer,
							"method":       meth,
							"framework":    "django",
							"handler":      handlerName,
							"language":     "python",
						},
					})
				}
			}

		case "class_definition":
			w.handleClass(c, decorators)
		}
	}

	// Record decorator applications as file-scope uses so decorator helpers
	// (`@provide_session`, custom decorators) are not flagged dead.
	for _, dec := range decorators {
		w.emitDecoratorRef(dec)
	}
}

// emitDecoratorRoute emits the server-route facts carried by a single decorator
// node, if it is a route decorator in a form the path regexes can parse.
//
// isRoute reports whether the decorator is a route-method decorator in ANY form
// (literal, path= keyword, or a computed path the regexes cannot read) — the
// caller uses it to tag the handler as a framework-dispatched entry point.
// emitted reports whether route facts were actually appended.
//
// Shared by handleDecoratedDefinition (module- and class-level defs) and
// walkNestedScope (defs nested in a function body), so the two paths cannot
// drift in which decorator forms they recognize.
func (w *pyWalker) emitDecoratorRoute(c *sitter.Node, text string, pending *[]int) (isRoute, emitted bool) {
	// Covers @r.get / @app.route and Flask-AppBuilder @expose (which has no
	// receiver dot, so routeMethodRe alone misses it).
	isRoute = routeMethodRe.MatchString(text) || exposeDecoratorRe.MatchString(text)
	if w.routeDecorators == nil {
		w.routeDecorators = map[uint]bool{}
	}
	if w.routeDecorators[c.StartByte()] {
		return isRoute, true // already emitted by an earlier walk of this decorator
	}
	// FastAPI/Starlette verb decorator (@r.get) or Flask @app.route / @bp.route.
	if m := routeDecoratorRe.FindStringSubmatch(text); m != nil {
		// Flask @X.route takes its verbs from methods=[...] (default GET) and is
		// labelled flask by the idiom itself; the verb shorthand (@r.get) is shared
		// by FastAPI and Flask 2.0, so its label comes from project detection.
		// m[1] is the receiver the route was registered on (the `router` in
		// `@router.get`); emitRouteAt ties the facts to it so composeRouterPrefixes
		// can fold on the prefix it is mounted at.
		methods, framework, websocket := w.decoratorVerbs(m[2], text)
		w.emitRouteAt([]pyPathPart{{lit: m[3]}}, methods, framework, websocket, m[1], int(c.StartPosition().Row)+1, pending)
		w.routeDecorators[c.StartByte()] = true
		return isRoute, true
	}
	// Flask-AppBuilder @expose("/path", methods=[...]).
	if m := exposeDecoratorRe.FindStringSubmatch(text); m != nil {
		w.emitRoutes(m[1], routeMethods(text), "flask", int(c.StartPosition().Row)+1, pending)
		w.routeDecorators[c.StartByte()] = true
		return isRoute, true
	}
	// A path the literal regex cannot read: a constant, an f-string, a `+`
	// concatenation. Emitted pending, resolved repo-wide by resolveRoutePaths.
	if isRoute && w.emitComputedDecoratorRoute(c, pending) {
		w.routeDecorators[c.StartByte()] = true
		return isRoute, true
	}
	return isRoute, false
}

// emitRoutes appends one KindRoute fact per HTTP method for a server route.
// Name is the bare path (like every other extractor) so the cross-repo linker,
// which treats route Name as the path, can match it; multiple methods on one path
// produce same-Name facts disambiguated by the method prop (the linker indexes by
// (path, method)). Blueprint url_prefix / FAB route_base folding is GAP-PY-06.
// Each new fact's index is appended to *pending so handleDecoratedDefinition can
// back-fill the handler prop once the function name is known.
func (w *pyWalker) emitRoutes(path string, methods []string, framework string, line int, pending *[]int) {
	for _, method := range methods {
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindRoute,
			Name: path,
			File: w.relFile,
			Line: line,
			Props: map[string]any{
				facts.PropRole: facts.RoleServer,
				"method":       method,
				"path":         path,
				"framework":    framework,
				"language":     "python",
			},
		})
		*pending = append(*pending, len(w.out)-1)
	}
}

// verbShorthandFramework picks the framework prop for a bare verb decorator
// (@r.get), shared by FastAPI and Flask 2.0. FastAPI wins when present (preserving
// the historical default); a Flask-only project relabels to flask.
func (w *pyWalker) verbShorthandFramework() string {
	switch {
	case w.isFastAPI:
		return "fastapi"
	case w.isFlask:
		return "flask"
	default:
		return "fastapi"
	}
}

// routeMethods reads the HTTP verbs from a Flask route/expose decorator's
// methods=[...] kwarg, defaulting to GET when absent (Flask's own default).
func routeMethods(decoratorText string) []string {
	if m := routeMethodsListRe.FindStringSubmatch(decoratorText); m != nil {
		if verbs := httpMethodWordRe.FindAllString(m[1], -1); len(verbs) > 0 {
			return verbs
		}
	}
	return []string{"GET"}
}

// handleClass emits a KindSymbol fact for a class and walks its body.
func (w *pyWalker) handleClass(node *sitter.Node, decorators []string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := pyText(nameNode, w.src)
	qualName := w.module + "." + w.qualify(name)

	props := map[string]any{
		"symbol_kind": facts.SymbolClass,
		"language":    "python",
		// Python has no access keyword; the leading-underscore convention marks
		// a name as private/internal. Lets the dead-code visibility filter work.
		"exported": !strings.HasPrefix(name, "_"),
	}
	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}}

	// Superclasses.
	var bases []string
	abstract := false
	if args := node.ChildByFieldName("superclasses"); args != nil {
		for i := uint(0); i < uint(args.ChildCount()); i++ {
			c := args.Child(i)
			switch kindOf(c) {
			case "identifier":
				base := pyText(c, w.src)
				bases = append(bases, base)
				rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: base})
			case "attribute":
				base := pyText(c, w.src)
				bases = append(bases, base)
				rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: base})
			case "subscript":
				// Generic base: CRUDBase[ModelType, IdType] — strip the type params.
				valueNode := c.ChildByFieldName("value")
				if valueNode != nil {
					base := pyText(valueNode, w.src)
					bases = append(bases, base)
					rels = append(rels, facts.Relation{Kind: facts.RelImplements, Target: base})
				}
			case "keyword_argument":
				// metaclass=ABCMeta makes the class abstract.
				nameNode := c.ChildByFieldName("name")
				valueNode := c.ChildByFieldName("value")
				if nameNode != nil && valueNode != nil &&
					pyText(nameNode, w.src) == "metaclass" &&
					lastComponent(pyText(valueNode, w.src)) == "ABCMeta" {
					abstract = true
				}
			}
		}
	}
	// A class is abstract if it inherits ABC/ABCMeta/Protocol, uses metaclass=ABCMeta,
	// declares any @abstractmethod, or follows the idiomatic duck-typed abstract
	// pattern (a method whose whole body is `raise NotImplementedError`). Recorded so
	// package-metrics abstractness (A) is meaningful for Python (which has no
	// interface keyword and where formal ABCs are the exception, not the rule).
	// A class is also classified as an enum (concrete value enumeration, excluded
	// from N) or a data holder (DTO/schema/record — Pydantic/NamedTuple/TypedDict,
	// concrete by design) so package-metrics does not mislabel such packages.
	enum := false
	dataHolder := false
	for _, base := range bases {
		last := lastComponent(base)
		if pyAbstractBases[last] {
			abstract = true
		}
		if pyEnumBases[last] {
			enum = true
		}
		if isDataHolderBase(last) {
			dataHolder = true
		}
	}
	if !abstract && bodyHasAbstractMethod(node.ChildByFieldName("body"), w.src) {
		abstract = true
	}
	if !abstract && bodyHasNotImplementedMethod(node.ChildByFieldName("body"), w.src) {
		abstract = true
	}
	if abstract {
		props["abstract"] = true
	}
	if enum {
		props["enum"] = true
	}
	if dataHolder {
		props["data_class"] = true
	}

	for _, dec := range decorators {
		applyDecoratorProps(props, dec, w.importsModal)
	}

	// Django classification.
	if w.isDjango {
		for _, base := range bases {
			last := lastComponent(base)
			if djangoModelBases[last] {
				props["framework"] = "django"
				tableName := camelToSnake(name)
				w.out = append(w.out, facts.Fact{
					Kind: facts.KindStorage,
					Name: tableName,
					File: w.relFile,
					Line: int(node.StartPosition().Row) + 1,
					Props: map[string]any{
						"storage_kind": "table",
						"framework":    "django",
						"class":        qualName,
					},
				})
				break
			}
			if djangoCBVBases[last] {
				props["django_component"] = "view"
				props["framework"] = "django"
				break
			}
			if djangoSerializerBases[last] {
				props["django_component"] = "serializer"
				props["framework"] = "django"
				break
			}
		}
	}

	f := facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      qualName,
		File:      w.relFile,
		Line:      int(node.StartPosition().Row) + 1,
		Props:     props,
		Relations: rels,
	}

	w.out = append(w.out, f)
	w.pushOwner(len(w.out) - 1)

	bodyNode := node.ChildByFieldName("body")
	w.pushType(name, collectPyMethodNames(bodyNode, w.src))
	if bodyNode != nil {
		w.walkBody(bodyNode)
		// Walk class-body statements for call / value-reference edges (attrs/pydantic/
		// SQLAlchemy field defaults, `x = Depends(dep)` class attrs). These run at
		// class-definition time and are attributed to the class (the current owner);
		// walkForCalls stops at nested defs, so methods keep their own owners. Metrics
		// are nil here, so complexity is unaffected.
		w.walkForCalls(bodyNode)
	}
	w.popType()
	w.popOwner()
}

// handleFunction emits a KindSymbol fact for a function/method.
func (w *pyWalker) handleFunction(node *sitter.Node, decorators []string) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := pyText(nameNode, w.src)
	qualName := w.module + "." + w.qualify(name)

	// Determine if this is a method (inside a class) or a top-level function.
	symbolKind := facts.SymbolFunc
	if len(w.typeStack) > 0 {
		symbolKind = facts.SymbolMethod
	}

	props := map[string]any{
		"symbol_kind": symbolKind,
		"language":    "python",
		// Leading-underscore convention marks a name as private/internal.
		"exported": !strings.HasPrefix(name, "_"),
	}
	if len(w.typeStack) > 0 {
		props["receiver"] = w.typeStack[len(w.typeStack)-1]
	}

	// async keyword: look for it as a sibling before the `def` keyword.
	fullText := pyText(node, w.src)
	if strings.HasPrefix(strings.TrimSpace(fullText), "async ") {
		props["async"] = true
	}

	// Return type.
	if retNode := node.ChildByFieldName("return_type"); retNode != nil {
		rt := strings.TrimSpace(pyText(retNode, w.src))
		if strings.HasPrefix(rt, "->") {
			rt = strings.TrimSpace(rt[2:])
		}
		if rt != "" {
			props["return_type"] = rt
		}
	}

	for _, dec := range decorators {
		applyDecoratorProps(props, dec, w.importsModal)
	}

	rels := []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}}

	f := facts.Fact{
		Kind:      facts.KindSymbol,
		Name:      qualName,
		File:      w.relFile,
		Line:      int(node.StartPosition().Row) + 1,
		Props:     props,
		Relations: rels,
	}

	w.out = append(w.out, f)
	w.pushOwner(len(w.out) - 1)
	if bodyNode := node.ChildByFieldName("body"); bodyNode != nil {
		// Register function-local (lazy) imports before resolving calls, so a call
		// through a name imported inside the body (a common circular-import
		// workaround) resolves to an edge. Must run before collectParamTypes/
		// collectLocalTypes so those see the local imports too.
		prevDeferred := w.importDeferred
		w.importDeferred = true
		w.registerBodyImports(bodyNode)
		w.importDeferred = prevDeferred
		w.localTypes = collectParamTypes(node.ChildByFieldName("parameters"), w.src, w.importMap, w.module)
		for k, v := range collectLocalTypes(bodyNode, w.src, w.importMap, w.module) {
			w.localTypes[k] = v
		}
		w.localBound = collectLocalBoundNames(node.ChildByFieldName("parameters"), bodyNode, w.src)
		// Set up per-function complexity tracking for this body walk. The props
		// map is shared by reference with the fact in w.out, so writing to it
		// after the walk updates the emitted fact.
		w.metrics = &pyBodyMetrics{}
		w.loopDepth = 0
		w.scalingDepth = 0
		w.selfName = qualName
		// Only the outermost function establishes the router scope: a router
		// variable is local to the factory that builds it, and collectRouterTopology
		// keys assignments the same way.
		savedFuncScope := w.funcScope
		if w.funcScope == "" {
			w.funcScope = qualName
		}
		w.walkForCalls(bodyNode)
		w.funcScope = savedFuncScope
		props["cyclomatic"] = 1 + w.metrics.decisions
		if w.metrics.loopDepth > 0 {
			props["loop_depth"] = w.metrics.loopDepth
			// Always emit the scaling depth (bounded loops discounted) alongside — even
			// when it is 0 — so the consumer can tell "all loops are bounded" (discount to
			// O(1)) from "no signal emitted" and fall back correctly.
			props["scaling_loop_depth"] = w.metrics.scalingLoopDepth
		}
		if w.metrics.loopCount > 0 {
			props["loop_count"] = w.metrics.loopCount
		}
		if len(w.metrics.callsInLoop) > 0 {
			props["calls_in_loop"] = w.metrics.callsInLoop
			// Always emit the N+1 subset alongside — even when EMPTY — for the same
			// reason: an omitted key means "signal absent" and makes the consumer fall
			// back to the unfiltered calls_in_loop, defeating the discount in exactly the
			// case it exists for (every in-loop call sitting inside a constant loop).
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
		w.selfName = ""
		w.localTypes = nil
		w.localBound = nil
	}
	// Walk parameter-default expressions (e.g. `body = Depends(parse_login_body)`)
	// for call and value-reference edges. Metrics are already finalized/nil here, so
	// this adds edges without perturbing complexity.
	if params := node.ChildByFieldName("parameters"); params != nil {
		w.walkForCalls(params)
	}
	w.popOwner()
}

// registerBodyImports scans a function body for import statements (including those
// nested in if/try/with blocks — the common lazy-import pattern) and registers them
// into importMap so calls through the imported names resolve. Nested function/class
// bodies are skipped; each registers its own.
func (w *pyWalker) registerBodyImports(node *sitter.Node) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "function_definition", "class_definition", "decorated_definition":
		return
	case "import_statement":
		w.handleImport(node)
		return
	case "import_from_statement":
		w.handleFromImport(node)
		return
	case "except_clause":
		// Same fallback rule as module scope: the except arm of a lazy
		// try/except ImportError dual-import must not clobber the try binding.
		prev := w.importFallback
		w.importFallback = true
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.registerBodyImports(node.Child(i))
		}
		w.importFallback = prev
		return
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.registerBodyImports(node.Child(i))
	}
}

// argRefRelations returns reference edges for functions/classes passed by name as
// call arguments (positional or keyword value), e.g. Depends(get_user),
// add_command(cmd), register_error_handler(404, views.not_found). Only names that
// resolve to an internal target emit an edge. This captures value-passed callables
// that are never invoked at the call site and so have no callee edge.
func (w *pyWalker) argRefRelations(args *sitter.Node) []facts.Relation {
	var rels []facts.Relation
	for i := uint(0); i < uint(args.ChildCount()); i++ {
		c := args.Child(i)
		var val *sitter.Node
		switch kindOf(c) {
		case "identifier", "attribute":
			val = c
		case "keyword_argument":
			val = c.ChildByFieldName("value")
		}
		if val == nil {
			continue
		}
		if rel, ok := w.valueRefRelation(val); ok {
			rels = append(rels, rel)
		}
	}
	return rels
}

// valueRefRelation resolves a node used in a VALUE position (call argument, dict
// value, collection element) to a reference edge. A bare identifier resolves via
// the strict valueRefTarget (imported / same-module def / same-class, param-guarded);
// a capitalized identifier is an instantiation (bare short name); a simple obj.attr
// resolves via resolveVarType. Returns ok=false when nothing internal resolves.
func (w *pyWalker) valueRefRelation(node *sitter.Node) (facts.Relation, bool) {
	switch kindOf(node) {
	case "identifier":
		name := pyText(node, w.src)
		if pyBuiltins[name] {
			return facts.Relation{}, false
		}
		if pyCapitalized(name) {
			return facts.Relation{Kind: facts.RelInstantiates, Target: name}, true
		}
		if target := w.valueRefTarget(name); target != "" {
			return facts.Relation{Kind: facts.RelCalls, Target: target}, true
		}
	case "attribute":
		obj := node.ChildByFieldName("object")
		attr := node.ChildByFieldName("attribute")
		if obj == nil || attr == nil || kindOf(obj) != "identifier" {
			return facts.Relation{}, false
		}
		if qt := w.resolveVarType(pyText(obj, w.src)); qt != "" {
			return facts.Relation{Kind: facts.RelCalls, Target: qt + "." + pyText(attr, w.src)}, true
		}
	}
	return facts.Relation{}, false
}

// emitValueRef records a value-reference edge for node against the current owner
// (function/class body) or, at module scope, the file-scope refs.
func (w *pyWalker) emitValueRef(node *sitter.Node) {
	rel, ok := w.valueRefRelation(node)
	if !ok {
		return
	}
	if owner := w.currentOwner(); owner != nil {
		owner.Relations = append(owner.Relations, rel)
	} else {
		w.fileRefs = append(w.fileRefs, rel)
	}
}

// emitCollectionValueRefs records value-reference edges for the values of a dict
// literal or the elements of a list/set/tuple — dispatch tables / registries such
// as {"ds": ds_filter} or [handler_a, handler_b].
func (w *pyWalker) emitCollectionValueRefs(node *sitter.Node) {
	switch kindOf(node) {
	case "dictionary":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			if p := node.Child(i); kindOf(p) == "pair" {
				if v := p.ChildByFieldName("value"); v != nil {
					w.emitValueRef(v)
				}
			}
		}
	case "list", "set", "tuple":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			w.emitValueRef(node.Child(i))
		}
	}
}

// valueRefTarget resolves a bare name used as a VALUE (a call argument, not a
// callee) to an internal target. Unlike resolveCall it deliberately omits the
// same-module bare fallback: a value-position name is usually a parameter or local
// (e.g. get_user(user_id)), and crediting a same-module symbol of that name would
// be wrong. Only imported names and same-class methods — which are unambiguously
// real symbol references — resolve.
func (w *pyWalker) valueRefTarget(name string) string {
	// A name bound in the enclosing function's own scope — a param or a local
	// assigned/iterated/aliased name — shadows a same-class method or same-module def
	// (e.g. self.x = x in __init__ passing the param, not the same-named property).
	if w.localBound[name] {
		return ""
	}
	if methods := w.currentMethods(); methods[name] {
		return w.module + "." + w.enclosingType() + "." + name
	}
	if t, ok := w.importMap[name]; ok && t != "" {
		return t
	}
	// Same-module top-level def (function or class) referenced by name.
	if w.idx != nil && w.idx.moduleDefs[w.module][name] {
		return w.module + "." + name
	}
	return ""
}

// stringRefRelation returns a reference edge for a string literal that names an
// internal symbol by dotted path (e.g. lazy_load_command("airflow.cli.commands.x.y")
// or a provider "class-name": "airflow.providers….short_circuit_task"). Only plain
// strings whose content is an identifier-dotted path of ≥3 segments qualify;
// f-strings (which have interpolation children, not a single string_content) and
// hyphenated/spaced strings are skipped. resolveCallTargets resolves the dotted
// target to a slash symbol and drops non-internal ones.
func (w *pyWalker) stringRefRelation(node *sitter.Node) (facts.Relation, bool) {
	content := firstChildOfKind(node, "string_content")
	if content == nil {
		return facts.Relation{}, false
	}
	s := pyText(content, w.src)
	if !dottedPathRe.MatchString(s) {
		return facts.Relation{}, false
	}
	return facts.Relation{Kind: facts.RelCalls, Target: s}, true
}

// firstChildOfKind returns the first direct child of node with the given kind.
func firstChildOfKind(node *sitter.Node, kind string) *sitter.Node {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); kindOf(c) == kind {
			return c
		}
	}
	return nil
}

// handleExprStatement checks for SQLAlchemy __tablename__ assignments and
// Django urlpatterns at module/class level.
func (w *pyWalker) handleExprStatement(node *sitter.Node) {
	text := pyText(node, w.src)
	if m := tableNameRe.FindStringSubmatch(text); m != nil {
		// Find the enclosing class name for the storage fact.
		className := ""
		if len(w.typeStack) > 0 {
			className = w.module + "." + w.enclosingType()
		}
		sf := facts.Fact{
			Kind: facts.KindStorage,
			Name: m[1],
			File: w.relFile,
			Line: int(node.StartPosition().Row) + 1,
			Props: map[string]any{
				"storage_kind": "table",
				"framework":    "sqlalchemy",
			},
		}
		if className != "" {
			sf.SetProp("class", className)
			sf.Relations = []facts.Relation{{Kind: facts.RelDeclares, Target: w.dir}}
		}
		w.out = append(w.out, sf)
		return
	}
	// Django urls.py: urlpatterns = [...].
	if w.isDjango && filepath.Base(w.relFile) == "urls.py" {
		for _, m := range urlPathRe.FindAllStringSubmatch(text, -1) {
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindRoute,
				Name: "* " + m[1],
				File: w.relFile,
				Line: int(node.StartPosition().Row) + 1,
				Props: map[string]any{
					facts.PropRole: facts.RoleServer,
					"path":         m[1],
					"handler":      m[2],
					"framework":    "django",
				},
			})
		}
	}
}

// handleAssignment handles module-level assignment statements (tree-sitter
// sometimes emits these as "assignment" nodes rather than "expression_statement").
func (w *pyWalker) handleAssignment(node *sitter.Node) {
	text := pyText(node, w.src)
	// Django urls.py: urlpatterns = [...].
	if w.isDjango && filepath.Base(w.relFile) == "urls.py" {
		for _, m := range urlPathRe.FindAllStringSubmatch(text, -1) {
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindRoute,
				Name: "* " + m[1],
				File: w.relFile,
				Line: int(node.StartPosition().Row) + 1,
				Props: map[string]any{
					facts.PropRole: facts.RoleServer,
					"path":         m[1],
					"handler":      m[2],
					"framework":    "django",
				},
			})
		}
	}
}

// walkBody walks a class body, dispatching each statement.
func (w *pyWalker) walkBody(body *sitter.Node) {
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		w.walkStatement(body.Child(i))
	}
}

// walkForCalls recursively scans a function body for call nodes and emits
// RelCalls / RelInstantiates on the current owner.
func (w *pyWalker) walkForCalls(node *sitter.Node) {
	if node == nil {
		return
	}
	kind := kindOf(node)
	if kind == "call" {
		w.emitCallRoute(node)
		if fn := node.ChildByFieldName("function"); fn != nil {
			w.emitCallEdge(fn)
			// Tag the body io_direct when it directly invokes a DB/network/file primitive
			// (session.execute, requests.get, open(...)); computePyPerformsIO then
			// propagates it transitively into performs_io across the call graph.
			if w.metrics != nil && !w.metrics.ioDirect && pyIsIODirectCall(fn, w.src) {
				w.metrics.ioDirect = true
			}
		}
		if args := node.ChildByFieldName("arguments"); args != nil {
			if owner := w.currentOwner(); owner != nil {
				owner.Relations = append(owner.Relations, w.argRefRelations(args)...)
			} else {
				w.fileRefs = append(w.fileRefs, w.argRefRelations(args)...)
			}
		}
	}
	if kind == "string" {
		if rel, ok := w.stringRefRelation(node); ok {
			if owner := w.currentOwner(); owner != nil {
				owner.Relations = append(owner.Relations, rel)
			} else {
				w.fileRefs = append(w.fileRefs, rel)
			}
		}
	}
	if kind == "dictionary" || kind == "list" || kind == "set" || kind == "tuple" {
		w.emitCollectionValueRefs(node)
	}
	if kind == "assignment" {
		for _, ident := range collectRefValueIdents(node.ChildByFieldName("right")) {
			w.emitValueRef(ident)
		}
	}
	if kind == "return_statement" {
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			for _, ident := range collectRefValueIdents(node.Child(i)) {
				w.emitValueRef(ident)
			}
		}
	}

	// Complexity metrics: count decision points so the single body walk doubles
	// as the cyclomatic/loop pass (mirrors the Go extractor).
	if w.metrics != nil {
		switch kind {
		case "if_statement", "elif_clause", "conditional_expression",
			"except_clause", "case_clause", "boolean_operator":
			w.metrics.decisions++
		}
	}

	// Nested class/function definitions get no symbol of their own, so walk them
	// in a shadowed scope that credits their references to the enclosing symbol
	// (metrics suppressed) instead of dropping them — see walkNestedScope.
	switch kind {
	case "class_definition", "function_definition", "decorated_definition":
		w.walkNestedScope(node)
		return
	}

	// Comprehensions and generator expressions carry an implicit loop, but the
	// first for-clause's iterable is evaluated once (not per-iteration), so they
	// need special handling — see walkComprehension.
	switch kind {
	case "list_comprehension", "dictionary_comprehension", "set_comprehension", "generator_expression":
		w.walkComprehension(node)
		return
	}

	// Statement loops raise the nesting depth for calls within their body. A loop over
	// a literal/constant collection or range(<const>), or an infinite while(true)
	// event/retry loop, runs a fixed number of times — it does not scale in "n", so it
	// raises loop_depth (still a real loop) but not scaling_loop_depth (the Big-O exponent).
	// Only the CONSTANT loop also stops its calls being N+1 candidates: `while True`
	// repeats, so it raises repeatDepth.
	isLoop := kind == "for_statement" || kind == "while_statement"
	bounded, repeats := false, false
	if isLoop {
		bounded = pyLoopBounded(node, w.src)
		repeats = pyLoopRepeats(node, w.src)
		if w.metrics != nil {
			w.metrics.loopCount++
			w.metrics.decisions++
			if w.loopDepth+1 > w.metrics.loopDepth {
				w.metrics.loopDepth = w.loopDepth + 1
			}
			if !bounded && w.scalingDepth+1 > w.metrics.scalingLoopDepth {
				w.metrics.scalingLoopDepth = w.scalingDepth + 1
			}
		}
		w.loopDepth++
		if !bounded {
			w.scalingDepth++
		}
		if repeats {
			w.repeatDepth++
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		w.walkForCalls(node.Child(i))
	}

	if isLoop {
		w.loopDepth--
		if !bounded {
			w.scalingDepth--
		}
		if repeats {
			w.repeatDepth--
		}
	}
}

// walkNestedScope walks a function/class definition nested inside a function
// body, crediting its references (calls, decorators, value/string refs) to the
// enclosing symbol. Nested defs get no symbol of their own, so without this
// walk their calls are attributed to nobody and every helper reached only from
// a closure reads as dead — the FastAPI router-factory pattern (`def
// get_x_router(): @router.post(...) async def handler(): helper()`) being the
// canonical false positive. Mirrors how lambdas have always been walked, with
// two scope adjustments:
//   - metrics are suppressed: a closure's branches and loops are not the
//     enclosing function's complexity, and its calls must not seed the
//     enclosing function's N+1 candidates (every metrics write and
//     recordCallMetrics is nil-guarded);
//   - the nested scope's bindings (its name, params, and body locals) extend
//     the enclosing bound-name set, so bare calls through them cannot
//     fabricate edges to same-named module-level defs.
//
// Decorators on the nested def are walked with the ENCLOSING scope's bindings
// (Python evaluates them there), before the nested name shadows anything.
func (w *pyWalker) walkNestedScope(node *sitter.Node) {
	// Unwrap decorated_definition and walk its decorators in the current scope:
	// a decorator applied inside a function body is a real reference (a bare
	// @retry_on_exception as much as a @log_usage(...) call).
	def := node
	if kindOf(node) == "decorated_definition" {
		if d := node.ChildByFieldName("definition"); d != nil {
			def = d
		}
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			c := node.Child(i)
			if kindOf(c) != "decorator" {
				continue
			}
			if call := firstChildOfKind(c, "call"); call != nil {
				w.walkForCalls(call)
			} else if ident := firstChildOfKind(c, "identifier"); ident != nil {
				w.emitValueRef(ident)
			}
			// A route decorator inside a function body is the FastAPI router-factory
			// pattern (`def get_x_router(): router = APIRouter(); @router.post("/")
			// async def handler(): ...`), which module-level walkStatement never
			// reaches. Emit its routes here, after the reference walk above so the
			// edges that walk produces are unchanged. The nested def gets no symbol
			// of its own, so the route carries no handler prop and the handler needs
			// no route_handler entry-point tag (nothing can read it as dead).
			// Mounted prefixes are folded on afterwards by composeRouterPrefixes.
			var nestedRoutes []int
			w.emitDecoratorRoute(c, pyText(c, w.src), &nestedRoutes)
		}
		if def == node {
			return // malformed decorated_definition with no definition field
		}
	}

	savedMetrics := w.metrics
	savedBound := w.localBound
	savedTypes := w.localTypes
	savedLoop, savedScaling, savedRepeat := w.loopDepth, w.scalingDepth, w.repeatDepth
	w.metrics = nil
	w.loopDepth, w.scalingDepth, w.repeatDepth = 0, 0, 0

	bound := make(map[string]bool, len(savedBound)+8)
	for k := range savedBound {
		bound[k] = true
	}
	pyBindTargets(def.ChildByFieldName("name"), w.src, bound)
	body := def.ChildByFieldName("body")
	if kindOf(def) == "function_definition" {
		params := def.ChildByFieldName("parameters")
		pyParamBoundNames(params, w.src, bound)
		if body != nil {
			walkLocalBoundNames(body, w.src, bound)
			// Function-local (lazy) imports resolve for this subtree, matching
			// registerBodyImports' treatment of top-level function bodies — including
			// their deferral, since this is a nested function body by construction.
			// Without the flag a method-local import is emitted once here and once
			// from handleFunction, and the two facts disagree about `deferred`.
			prevDeferred := w.importDeferred
			w.importDeferred = true
			w.registerBodyImports(body)
			w.importDeferred = prevDeferred
		}
		types := make(map[string]string, len(savedTypes)+4)
		for k, v := range savedTypes {
			types[k] = v
		}
		for k, v := range collectParamTypes(params, w.src, w.importMap, w.module) {
			types[k] = v
		}
		if body != nil {
			for k, v := range collectLocalTypes(body, w.src, w.importMap, w.module) {
				types[k] = v
			}
		}
		w.localTypes = types
	}
	w.localBound = bound

	// Walk the definition subtree: parameter defaults and the body. Deeper
	// nested defs re-enter walkNestedScope with a further-extended scope.
	for i := uint(0); i < uint(def.ChildCount()); i++ {
		w.walkForCalls(def.Child(i))
	}

	w.metrics = savedMetrics
	w.localBound = savedBound
	w.localTypes = savedTypes
	w.loopDepth, w.scalingDepth, w.repeatDepth = savedLoop, savedScaling, savedRepeat
}

// walkComprehension handles list/set/dict comprehensions and generator
// expressions. They carry an implicit loop (counted as one nesting level), but
// the iterable of the FIRST for-clause is evaluated exactly once — so a call
// there (e.g. `[x for x in session.query(...)]`) must NOT be counted as in-loop,
// otherwise it reads as a false N+1. The element expression, if-conditions, and
// any subsequent for-clauses do run per-iteration and are walked at inner depth.
//
// Simplification: a comprehension counts as a single loop level even when it has
// multiple for-clauses (which are really nested); this keeps depth attribution
// conservative rather than over-counting.
func (w *pyWalker) walkComprehension(node *sitter.Node) {
	// Identify the first for-clause's iterable (its "right" field).
	var firstIter *sitter.Node
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if c := node.Child(i); kindOf(c) == "for_in_clause" {
			firstIter = c.ChildByFieldName("right")
			break
		}
	}
	// A comprehension over a literal/constant iterable (`[f(x) for x in (A, B, C)]`) is
	// bounded, so it does not add a scaling loop level.
	bounded := firstIter != nil && pyIterableBounded(firstIter, w.src)

	if w.metrics != nil {
		w.metrics.loopCount++
		w.metrics.decisions++
		if w.loopDepth+1 > w.metrics.loopDepth {
			w.metrics.loopDepth = w.loopDepth + 1
		}
		if !bounded && w.scalingDepth+1 > w.metrics.scalingLoopDepth {
			w.metrics.scalingLoopDepth = w.scalingDepth + 1
		}
	}

	if firstIter != nil {
		w.walkForCalls(firstIter) // evaluated once → stays at outer depth
	}

	// A comprehension's iterable is either constant or input-scaling — never infinite —
	// so repeating and scaling coincide here.
	w.loopDepth++
	if !bounded {
		w.scalingDepth++
		w.repeatDepth++
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		child := node.Child(i)
		if kindOf(child) == "for_in_clause" {
			// Walk the clause's children at inner depth, skipping the already
			// handled first iterable (matched by byte range).
			for j := uint(0); j < uint(child.ChildCount()); j++ {
				cc := child.Child(j)
				if firstIter != nil && cc.StartByte() == firstIter.StartByte() && cc.EndByte() == firstIter.EndByte() {
					continue
				}
				w.walkForCalls(cc)
			}
			continue
		}
		w.walkForCalls(child)
	}
	w.loopDepth--
	if !bounded {
		w.scalingDepth--
		w.repeatDepth--
	}
}

// --- Complexity: bounded-loop and direct-I/O classification ---

// pyLoopBounded reports whether a for/while statement's trip count is independent of the
// input size, so it contributes no factor of n to Big-O: a for-loop over a literal
// collection or range(<int literal>), or an infinite while(true)/while(1) event/retry
// loop (driven by external events, not data).
//
// It does NOT mean the loop runs a constant number of times — see pyLoopRepeats.
func pyLoopBounded(node *sitter.Node, src []byte) bool {
	return pyLoopConstant(node, src) || pyLoopInfinite(node, src)
}

// pyLoopRepeats reports whether the loop body runs a non-constant number of times, so a
// call inside it is an N+1 candidate. Every loop repeats except a genuinely constant one:
// `while True: row = fetch(id)` is a retry / chain walk that queries once per iteration,
// even though its depth is discounted from the Big-O exponent. Scaling and repeating are
// not the same property.
func pyLoopRepeats(node *sitter.Node, src []byte) bool {
	return !pyLoopConstant(node, src)
}

// pyLoopConstant reports whether a for-loop iterates a compile-time-fixed number of
// times. A while-loop never qualifies: `while True` repeats indefinitely.
func pyLoopConstant(node *sitter.Node, src []byte) bool {
	if kindOf(node) != "for_statement" {
		return false
	}
	it := node.ChildByFieldName("right")
	return it != nil && pyIterableBounded(it, src)
}

// pyLoopInfinite reports whether a while-loop is the `while True:` / `while 1:` form,
// exited by break/return rather than by exhausting the input.
func pyLoopInfinite(node *sitter.Node, src []byte) bool {
	if kindOf(node) != "while_statement" {
		return false
	}
	cond := node.ChildByFieldName("condition")
	if cond == nil {
		return false
	}
	switch kindOf(cond) {
	case "true":
		return true
	case "integer":
		return pyText(cond, src) != "0"
	}
	return false
}

// pyIterableBounded reports whether an iterable expression has a compile-time-fixed
// length: a list/tuple/set/dict literal, or range(...) whose arguments are all integer
// literals. Anything data-derived (a variable, range(len(x)), a call result) is unbounded.
func pyIterableBounded(it *sitter.Node, src []byte) bool {
	switch kindOf(it) {
	case "list", "tuple", "set", "dictionary":
		return true
	case "call":
		fn := it.ChildByFieldName("function")
		if fn == nil || pyText(fn, src) != "range" {
			return false
		}
		args := it.ChildByFieldName("arguments")
		if args == nil {
			return false
		}
		sawArg := false
		for i := uint(0); i < uint(args.ChildCount()); i++ {
			c := args.Child(i)
			switch kindOf(c) {
			case "(", ")", ",":
				continue
			}
			sawArg = true
			if kindOf(c) != "integer" {
				return false
			}
		}
		return sawArg
	}
	return false
}

// pyIODirectMethods are Python method names that are unambiguously a DB/session
// round-trip (SQLAlchemy / DBAPI). Ambiguous verbs (get/save/update/merge) are
// intentionally excluded — they collide with in-memory helpers — and covered by
// receiver matching (pyIOReceivers) instead.
var pyIODirectMethods = map[string]bool{
	"execute": true, "executemany": true, "scalars": true, "scalar": true,
	"fetchone": true, "fetchall": true, "fetchmany": true,
	"commit": true, "flush": true,
	"bulk_create": true, "bulk_update": true, "bulk_save_objects": true,
	"bulk_insert_mappings": true, "add_all": true,
	"get_or_create": true, "update_or_create": true, "urlopen": true,
}

// pyIOReceivers are module/object roots whose calls are network I/O regardless of method.
var pyIOReceivers = map[string]bool{
	"requests": true, "httpx": true, "aiohttp": true, "urllib": true, "urllib2": true,
}

// pyIsIODirectCall reports whether a call's function node names a direct network/file/DB
// I/O primitive — a builtin open()/urlopen(), an unambiguous DB method (session.execute,
// cursor.fetchall), or a call on a known HTTP-client module (requests.get, httpx.post).
func pyIsIODirectCall(fn *sitter.Node, src []byte) bool {
	switch kindOf(fn) {
	case "identifier":
		name := pyText(fn, src)
		return name == "open" || name == "urlopen"
	case "attribute":
		if attr := fn.ChildByFieldName("attribute"); attr != nil && pyIODirectMethods[pyText(attr, src)] {
			return true
		}
		obj := fn.ChildByFieldName("object")
		if obj == nil {
			return false
		}
		root := pyText(obj, src)
		if i := strings.IndexByte(root, '.'); i >= 0 {
			root = root[:i]
		}
		return pyIOReceivers[root]
	}
	return false
}

// emitCallEdge resolves the callee node and appends a relation to the current owner.
func (w *pyWalker) emitCallEdge(fn *sitter.Node) {
	owner := w.currentOwner()
	if owner == nil {
		return
	}

	switch kindOf(fn) {
	case "identifier":
		name := pyText(fn, w.src)
		if pyBuiltins[name] {
			return
		}
		if w.localBound[name] {
			return // shadowed by a param/local — not the module-level def of this name
		}
		if pyCapitalized(name) {
			owner.Relations = append(owner.Relations, facts.Relation{
				Kind:   facts.RelInstantiates,
				Target: name,
			})
			return
		}
		if target := w.resolveCall(name); target != "" {
			owner.Relations = append(owner.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: target,
			})
			w.recordCallMetrics(target)
		}

	case "attribute":
		objNode := fn.ChildByFieldName("object")
		attrNode := fn.ChildByFieldName("attribute")
		if objNode == nil || attrNode == nil {
			return
		}
		obj := pyText(objNode, w.src)
		attr := pyText(attrNode, w.src)
		if obj == "self" || obj == "cls" {
			if methods := w.currentMethods(); methods[attr] {
				target := w.module + "." + w.enclosingType() + "." + attr
				owner.Relations = append(owner.Relations, facts.Relation{
					Kind:   facts.RelCalls,
					Target: target,
				})
				w.recordCallMetrics(target)
			}
			return
		}
		// Resolve the receiver to a qualified type via localTypes or importMap.
		qualType := w.resolveVarType(obj)
		if qualType == "" {
			return
		}
		target := qualType + "." + attr
		owner.Relations = append(owner.Relations, facts.Relation{
			Kind:   facts.RelCalls,
			Target: target,
		})
		w.recordCallMetrics(target)
		w.emitImplementorCalls(w.currentOwnerIdx(), attr, qualType)
	}
}

// resolveVarType returns the canonical qualified type for a local variable name,
// checking localTypes first then importMap (for class-level references).
// Returns "" when the type cannot be statically determined.
func (w *pyWalker) resolveVarType(obj string) string {
	if w.localTypes != nil {
		if t, ok := w.localTypes[obj]; ok {
			return t
		}
	}
	if w.importMap != nil {
		if t, ok := w.importMap[obj]; ok && t != "" {
			return t
		}
	}
	return ""
}

// emitImplementorCalls emits additional RelCalls edges to all concrete classes
// that implement qualType, when a matching method exists on each implementor.
func (w *pyWalker) emitImplementorCalls(ownerIdx int, methodName, qualType string) {
	if ownerIdx < 0 {
		return
	}
	if w.deferImplementors {
		w.pendingImpl = append(w.pendingImpl, pyImplCall{owner: ownerIdx, method: methodName, qualType: qualType})
		return
	}
	if w.idx == nil {
		return
	}
	addImplementorCalls(&w.out[ownerIdx], methodName, qualType, w.idx)
}

// pyImplCall is one implementor lookup held back until the merged index exists.
type pyImplCall struct {
	owner    int // index into the file's fact slice
	method   string
	qualType string
}

// applyPendingImplementors answers the held-back lookups against the merged index.
func applyPendingImplementors(ff []facts.Fact, pending []pyImplCall, idx *pySymbolIndex) {
	if idx == nil {
		return
	}
	for _, p := range pending {
		if p.owner < 0 || p.owner >= len(ff) {
			continue
		}
		addImplementorCalls(&ff[p.owner], p.method, p.qualType, idx)
	}
}

// addImplementorCalls appends a RelCalls edge to every concrete implementor of
// qualType that declares methodName.
func addImplementorCalls(owner *facts.Fact, methodName, qualType string, idx *pySymbolIndex) {
	bare := lastComponent(qualType)
	seen := make(map[string]bool)
	for _, key := range []string{bare, qualType} {
		for _, concreteQual := range idx.implMap[key] {
			if seen[concreteQual] {
				continue
			}
			seen[concreteQual] = true
			info, ok := idx.classes[concreteQual]
			if !ok || !info.methods[methodName] {
				continue
			}
			owner.Relations = append(owner.Relations, facts.Relation{
				Kind:   facts.RelCalls,
				Target: concreteQual + "." + methodName,
			})
		}
	}
}

// resolveCall maps a bare call name to a canonical fact target.
func (w *pyWalker) resolveCall(name string) string {
	// A bare name that shadows a param/local/loop-var is that local binding, not a
	// same-class method or module-level def (e.g. def wrapper(cb): cb()), so this must
	// gate every branch below, not just the same-module fallback.
	if w.localBound[name] {
		return ""
	}
	// Same-class method.
	if methods := w.currentMethods(); methods[name] {
		return w.module + "." + w.enclosingType() + "." + name
	}
	// Imported name.
	if target, ok := w.importMap[name]; ok {
		return target // "" means external → no edge
	}
	// Same-module top-level function. When an index is available, resolve only names
	// that are actually module-level defs, so callable locals/params/loop vars don't
	// fabricate edges. Without an index (single-file extraction) fall back to
	// best-effort; production always supplies one.
	if w.idx != nil {
		if w.idx.moduleDefs[w.module][name] {
			return w.module + "." + name
		}
		return ""
	}
	return w.module + "." + name
}

// pyBindTargets recursively binds an assignment/parameter target node into out:
// identifier -> itself; unpacking forms -> recurse into elements. attribute
// (self.x) and subscript (d[k]) targets are not name bindings and are skipped.
func pyBindTargets(node *sitter.Node, src []byte, out map[string]bool) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "identifier":
		out[pyText(node, src)] = true
	case "pattern_list", "tuple_pattern", "list_pattern", "list_splat_pattern", "dictionary_splat_pattern":
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			pyBindTargets(node.Child(i), src, out)
		}
	}
}

// pyParamBoundNames adds every name a `parameters` node binds to out, covering
// all parameter shapes (x, x=1, x: T, x: T = 1, *args, **kwargs).
func pyParamBoundNames(params *sitter.Node, src []byte, out map[string]bool) {
	if params == nil {
		return
	}
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		c := params.Child(i)
		switch kindOf(c) {
		case "identifier", "list_splat_pattern", "dictionary_splat_pattern":
			pyBindTargets(c, src, out)
		case "default_parameter", "typed_default_parameter":
			pyBindTargets(c.ChildByFieldName("name"), src, out)
		case "typed_parameter":
			for j := uint(0); j < uint(c.ChildCount()); j++ {
				pyBindTargets(c.Child(j), src, out)
			}
		}
	}
}

// collectLocalBoundNames returns every name bound in a function's own scope:
// its parameters plus every name assigned, iterated, or aliased in its body.
// Used to guard bare-identifier call resolution — a name bound here refers to
// a local value, not a same-module def of the same name.
func collectLocalBoundNames(params, body *sitter.Node, src []byte) map[string]bool {
	bound := make(map[string]bool)
	pyParamBoundNames(params, src, bound)
	walkLocalBoundNames(body, src, bound)
	return bound
}

// walkLocalBoundNames walks a function body collecting bound names, stopping
// at nested function/class/lambda scopes (their bindings belong to them).
func walkLocalBoundNames(node *sitter.Node, src []byte, bound map[string]bool) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "function_definition", "class_definition":
		// The nested def's NAME is bound in this scope (its params/locals are
		// not — they belong to the nested scope, which walkNestedScope rebinds).
		// Without this, a closure named like a module-level def makes bare calls
		// resolve to the module symbol — a fabricated edge that can rescue
		// genuinely dead code.
		pyBindTargets(node.ChildByFieldName("name"), src, bound)
		return
	case "decorated_definition":
		if d := node.ChildByFieldName("definition"); d != nil {
			pyBindTargets(d.ChildByFieldName("name"), src, bound)
		}
		return
	case "lambda":
		return
	case "assignment", "augmented_assignment":
		pyBindTargets(node.ChildByFieldName("left"), src, bound)
	case "for_statement":
		pyBindTargets(node.ChildByFieldName("left"), src, bound)
	case "named_expression":
		pyBindTargets(node.ChildByFieldName("name"), src, bound)
	case "with_item":
		if v := node.ChildByFieldName("value"); v != nil && kindOf(v) == "as_pattern" {
			pyBindTargets(v.ChildByFieldName("alias"), src, bound)
		}
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		walkLocalBoundNames(node.Child(i), src, bound)
	}
}

// collectRefValueIdents returns the bare identifier(s) a value expression
// resolves to: itself if it's a plain identifier, or each identifier element
// of a tuple (expression_list), e.g. `cb = handler` / `a, b = f, g`.
func collectRefValueIdents(node *sitter.Node) []*sitter.Node {
	if node == nil {
		return nil
	}
	switch kindOf(node) {
	case "identifier":
		return []*sitter.Node{node}
	case "expression_list":
		var out []*sitter.Node
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			if c := node.Child(i); kindOf(c) == "identifier" {
				out = append(out, c)
			}
		}
		return out
	}
	return nil
}

// collectPyMethodNames returns the set of function names declared directly in a
// class body node.
func collectPyMethodNames(body *sitter.Node, src []byte) map[string]bool {
	methods := make(map[string]bool)
	if body == nil {
		return methods
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		var fn *sitter.Node
		switch kindOf(c) {
		case "function_definition":
			fn = c
		case "decorated_definition":
			for j := uint(0); j < uint(c.ChildCount()); j++ {
				if kindOf(c.Child(j)) == "function_definition" {
					fn = c.Child(j)
					break
				}
			}
		}
		if fn != nil {
			if nameNode := fn.ChildByFieldName("name"); nameNode != nil {
				methods[pyText(nameNode, src)] = true
			}
		}
	}
	return methods
}

// bodyHasAbstractMethod reports whether a class body declares at least one
// method decorated with @abstractmethod (or @abc.abstractmethod), a reliable
// signal that the class is abstract.
func bodyHasAbstractMethod(body *sitter.Node, src []byte) bool {
	if body == nil {
		return false
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if kindOf(c) != "decorated_definition" {
			continue
		}
		for j := uint(0); j < uint(c.ChildCount()); j++ {
			d := c.Child(j)
			if kindOf(d) != "decorator" {
				continue
			}
			if m := decoratorRe.FindStringSubmatch(pyText(d, src)); m != nil {
				if lastComponent(m[1]) == "abstractmethod" {
					return true
				}
			}
		}
	}
	return false
}

// bodyHasNotImplementedMethod reports whether a class body declares at least one
// method whose entire body is `raise NotImplementedError` (optionally preceded by
// a docstring). This is the idiomatic Python "abstract base" pattern for code that
// does not use ABC/@abstractmethod, so treating it as abstract makes package-metrics
// abstractness (A) meaningful for duck-typed hierarchies. Conservative on purpose:
// bare `pass` / `...` bodies are NOT treated as abstract (too many concrete stubs
// use them), only an explicit NotImplementedError raise.
func bodyHasNotImplementedMethod(body *sitter.Node, src []byte) bool {
	if body == nil {
		return false
	}
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		fn := funcDefOf(body.Child(i))
		if fn == nil {
			continue
		}
		if funcBodyOnlyRaisesNotImplemented(fn.ChildByFieldName("body"), src) {
			return true
		}
	}
	return false
}

// funcDefOf returns the function_definition node for a class-body child, unwrapping
// a decorated_definition; returns nil for non-function children.
func funcDefOf(n *sitter.Node) *sitter.Node {
	switch kindOf(n) {
	case "function_definition":
		return n
	case "decorated_definition":
		if d := n.ChildByFieldName("definition"); d != nil && kindOf(d) == "function_definition" {
			return d
		}
	}
	return nil
}

// funcBodyOnlyRaisesNotImplemented reports whether a function body consists solely
// of a `raise NotImplementedError` (with an optional leading docstring). Any other
// statement means the method has a real implementation.
func funcBodyOnlyRaisesNotImplemented(fnBody *sitter.Node, src []byte) bool {
	if fnBody == nil {
		return false
	}
	sawRaise := false
	for i := uint(0); i < uint(fnBody.ChildCount()); i++ {
		c := fnBody.Child(i)
		switch kindOf(c) {
		case "comment":
			continue
		case "expression_statement":
			if stmtIsDocstring(c) { // leading docstring is allowed
				continue
			}
			return false
		case "raise_statement":
			if !strings.Contains(pyText(c, src), "NotImplementedError") {
				return false
			}
			sawRaise = true
		default:
			return false
		}
	}
	return sawRaise
}

// stmtIsDocstring reports whether an expression_statement is a bare string literal
// (a docstring), as opposed to a call, assignment, or other expression. The first
// child holds the statement's expression, so a string there means a docstring.
func stmtIsDocstring(stmt *sitter.Node) bool {
	if stmt.ChildCount() == 0 {
		return false
	}
	return kindOf(stmt.Child(0)) == "string"
}

// hasDecorator reports whether any name in decorators has last as its
// last dot-separated component (e.g. "overload" matches both "overload"
// and "typing.overload").
func hasDecorator(decorators []string, last string) bool {
	for _, d := range decorators {
		if lastComponent(d) == last {
			return true
		}
	}
	return false
}

func pyFuncName(node *sitter.Node, src []byte) string {
	if n := node.ChildByFieldName("name"); n != nil {
		return pyText(n, src)
	}
	return ""
}

func pyText(node *sitter.Node, src []byte) string { return tsutil.Text(node, src) }

func pyCapitalized(s string) bool {
	if s == "" {
		return false
	}
	return unicode.IsUpper([]rune(s)[0])
}

// --- Type resolution helpers ---

// resolveTypeNamePy converts a raw annotation string to a canonical qualified name
// using the file's importMap. Returns "" for external/unresolvable/built-in types.
func resolveTypeNamePy(typeStr, module string, importMap map[string]string) string {
	typeStr = strings.TrimSpace(typeStr)
	typeStr = stripGenericWrapper(typeStr)
	if typeStr == "" || pyBuiltinTypes[typeStr] {
		return ""
	}
	if strings.Contains(typeStr, ".") {
		parts := strings.SplitN(typeStr, ".", 2)
		if importMap != nil {
			if t, ok := importMap[parts[0]]; ok && t != "" {
				return t + "." + parts[1]
			}
		}
		// Dotted name not in importMap — treat as external, skip.
		return ""
	}
	if importMap != nil {
		if t, ok := importMap[typeStr]; ok {
			return t // "" means external → caller drops it
		}
	}
	// Not imported and not a built-in — assume same-module class.
	return module + "." + typeStr
}

// stripGenericWrapper strips the outermost generic wrapper from a type string.
// Handles bracket generics (Optional[X], List[X], Union[X, None]) and PEP-604
// union syntax (X | None, X | Y).
func stripGenericWrapper(s string) string {
	// PEP-604 union: "X | Y" — take the first non-None part.
	if strings.Contains(s, "|") && !strings.Contains(s, "[") {
		for _, part := range strings.Split(s, "|") {
			part = strings.TrimSpace(part)
			if part != "None" && part != "" {
				return part
			}
		}
		return ""
	}
	i := strings.Index(s, "[")
	if i < 0 {
		return s
	}
	inner := strings.TrimSpace(s[i+1 : len(s)-1])
	if comma := strings.Index(inner, ","); comma >= 0 {
		part := strings.TrimSpace(inner[:comma])
		if part == "None" {
			part = strings.TrimSpace(inner[comma+1:])
		}
		return part
	}
	return inner
}

// pyBuiltinTypes is the set of Python built-in and standard-library primitive
// type names that should not be resolved to project facts.
var pyBuiltinTypes = map[string]bool{
	"str": true, "int": true, "float": true, "bool": true, "bytes": true,
	"bytearray": true, "list": true, "dict": true, "set": true, "tuple": true,
	"frozenset": true, "type": true, "object": true, "complex": true,
	"None": true, "NoneType": true, "Any": true, "Optional": true, "Union": true,
	"List": true, "Dict": true, "Set": true, "Tuple": true, "FrozenSet": true,
	"Type": true, "Callable": true, "ClassVar": true, "Final": true,
	"Literal": true, "TypeVar": true, "Generic": true, "NamedTuple": true,
	"TypedDict": true, "Iterator": true, "Generator": true, "Iterable": true,
	"AsyncIterator": true, "AsyncGenerator": true, "AsyncIterable": true,
	"Sequence": true, "MutableSequence": true, "Mapping": true,
	"MutableMapping": true, "IO": true, "TextIO": true, "BinaryIO": true,
	"Pattern": true, "Match": true, "AnyStr": true, "Text": true,
	"Awaitable": true, "Coroutine": true, "T": true,
}

// collectParamTypes scans a function parameter list and returns a map of
// param name → qualified type for all typed parameters (skipping self/cls).
func collectParamTypes(params *sitter.Node, src []byte, importMap map[string]string, module string) map[string]string {
	result := make(map[string]string)
	if params == nil {
		return result
	}
	for i := uint(0); i < uint(params.ChildCount()); i++ {
		c := params.Child(i)
		var paramName string
		var typeNode *sitter.Node
		switch kindOf(c) {
		case "typed_parameter":
			// The identifier is the first child; type is a named field.
			if first := c.Child(0); first != nil && kindOf(first) == "identifier" {
				paramName = pyText(first, src)
			}
			typeNode = c.ChildByFieldName("type")
		case "typed_default_parameter":
			if n := c.ChildByFieldName("name"); n != nil {
				paramName = pyText(n, src)
			}
			typeNode = c.ChildByFieldName("type")
		}
		if paramName == "" || paramName == "self" || paramName == "cls" || typeNode == nil {
			continue
		}
		if resolved := resolveTypeNamePy(pyText(typeNode, src), module, importMap); resolved != "" {
			result[paramName] = resolved
		}
	}
	return result
}

// collectLocalTypes scans a function body for type-inferable assignments and
// returns a map of local variable name → qualified type.
func collectLocalTypes(body *sitter.Node, src []byte, importMap map[string]string, module string) map[string]string {
	result := make(map[string]string)
	collectLocalTypesNode(body, src, importMap, module, result)
	return result
}

func collectLocalTypesNode(node *sitter.Node, src []byte, importMap map[string]string, module string, result map[string]string) {
	if node == nil {
		return
	}
	switch kindOf(node) {
	case "function_definition", "class_definition", "decorated_definition":
		return // do not cross scope boundaries
	case "annotated_assignment":
		// x: Type  or  x: Type = value
		leftNode := node.ChildByFieldName("left")
		annotNode := node.ChildByFieldName("annotation")
		if leftNode != nil && kindOf(leftNode) == "identifier" && annotNode != nil {
			name := pyText(leftNode, src)
			if resolved := resolveTypeNamePy(pyText(annotNode, src), module, importMap); resolved != "" {
				result[name] = resolved
			}
		}
		return
	case "assignment":
		// x = MyClass()
		leftNode := node.ChildByFieldName("left")
		rightNode := node.ChildByFieldName("right")
		if leftNode != nil && kindOf(leftNode) == "identifier" && rightNode != nil {
			if kindOf(rightNode) == "call" {
				if fnNode := rightNode.ChildByFieldName("function"); fnNode != nil && kindOf(fnNode) == "identifier" {
					ctorName := pyText(fnNode, src)
					if pyCapitalized(ctorName) && !pyBuiltins[ctorName] {
						if resolved := resolveTypeNamePy(ctorName, module, importMap); resolved != "" {
							result[pyText(leftNode, src)] = resolved
						}
					}
				}
			}
		}
		return
	default:
		for i := uint(0); i < uint(node.ChildCount()); i++ {
			collectLocalTypesNode(node.Child(i), src, importMap, module, result)
		}
	}
}

// --- Multi-file symbol index ---

// pyClassInfo records what was learned about a class during the index pass.
type pyClassInfo struct {
	qualName   string
	bases      []string
	isAbstract bool
	methods    map[string]bool
}

// pySymbolIndex is the global symbol table built by the index pass.
type pySymbolIndex struct {
	classes map[string]*pyClassInfo // "module.ClassName" → info
	implMap map[string][]string     // base name → concrete implementor qual names
	// moduleDefs maps a module path → set of its top-level def short names (functions
	// and classes). Used to safely credit a same-module symbol passed by name as a
	// value (a param/local of the same name is never in this set).
	moduleDefs map[string]map[string]bool
}

// buildFileIndex scans src for class declarations and populates idx.
// It does not emit any facts — it is read-only over the AST.
func buildFileIndex(src []byte, relFile string, idx *pySymbolIndex) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(python.Language())); err != nil {
		return
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	indexTree(tree.RootNode(), src, relFile, idx)
}

// indexTree is buildFileIndex over a tree the caller already parsed, so the
// extractor contributes to the index from the same parse that emits the facts.
func indexTree(root *sitter.Node, src []byte, relFile string, idx *pySymbolIndex) {
	module := strings.TrimSuffix(relFile, ".py")
	for i := uint(0); i < uint(root.ChildCount()); i++ {
		node := root.Child(i)
		switch kindOf(node) {
		case "class_definition":
			indexClass(node, src, module, idx)
			indexModuleDef(node, src, module, idx)
		case "function_definition":
			indexModuleDef(node, src, module, idx)
		case "decorated_definition":
			for j := uint(0); j < uint(node.ChildCount()); j++ {
				inner := node.Child(j)
				if kindOf(inner) == "class_definition" {
					indexClass(inner, src, module, idx)
					indexModuleDef(inner, src, module, idx)
					break
				}
				if kindOf(inner) == "function_definition" {
					indexModuleDef(inner, src, module, idx)
					break
				}
			}
		}
		// Conditional (try/if) blocks are deliberately NOT descended: a def/class
		// guarded by a conditional is a shim we do not emit a symbol for (see
		// walkStatement), so indexing its name would resolve a same-module call to
		// a symbol that never exists — a fabricated edge.
	}
}

// indexModuleDef records a top-level function/class short name under its module in
// idx.moduleDefs, so a same-module reference by name can be resolved safely.
func indexModuleDef(node *sitter.Node, src []byte, module string, idx *pySymbolIndex) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil || idx.moduleDefs == nil {
		return
	}
	set := idx.moduleDefs[module]
	if set == nil {
		set = make(map[string]bool)
		idx.moduleDefs[module] = set
	}
	set[pyText(nameNode, src)] = true
}

// indexClass records a class from the index pass into idx.
func indexClass(node *sitter.Node, src []byte, module string, idx *pySymbolIndex) {
	nameNode := node.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := pyText(nameNode, src)
	qualName := module + "." + name

	var bases []string
	isAbstract := false

	if args := node.ChildByFieldName("superclasses"); args != nil {
		for i := uint(0); i < uint(args.ChildCount()); i++ {
			c := args.Child(i)
			var base string
			switch kindOf(c) {
			case "identifier":
				base = pyText(c, src)
			case "attribute":
				base = pyText(c, src)
			case "subscript":
				if valueNode := c.ChildByFieldName("value"); valueNode != nil {
					base = pyText(valueNode, src)
				}
			}
			if base != "" {
				last := lastComponent(base)
				if last == "ABC" || last == "ABCMeta" || last == "Protocol" {
					isAbstract = true
				}
				bases = append(bases, base)
			}
		}
	}

	bodyNode := node.ChildByFieldName("body")
	methods := collectPyMethodNames(bodyNode, src)

	if !isAbstract && bodyNode != nil && hasAbstractMethod(bodyNode, src) {
		isAbstract = true
	}

	idx.classes[qualName] = &pyClassInfo{
		qualName:   qualName,
		bases:      bases,
		isAbstract: isAbstract,
		methods:    methods,
	}
}

// hasAbstractMethod reports whether any method in a class body has @abstractmethod.
func hasAbstractMethod(body *sitter.Node, src []byte) bool {
	for i := uint(0); i < uint(body.ChildCount()); i++ {
		c := body.Child(i)
		if kindOf(c) != "decorated_definition" {
			continue
		}
		for j := uint(0); j < uint(c.ChildCount()); j++ {
			d := c.Child(j)
			if kindOf(d) == "decorator" {
				text := pyText(d, src)
				if m := decoratorRe.FindStringSubmatch(text); m != nil {
					if lastComponent(m[1]) == "abstractmethod" {
						return true
					}
				}
			}
		}
	}
	return false
}

// finalizeImplMap populates idx.implMap by inverting the implements edges:
// for each concrete class C that lists base B, C is appended to implMap[B].
func finalizeImplMap(idx *pySymbolIndex) {
	idx.implMap = make(map[string][]string)
	for qualName, info := range idx.classes {
		if info.isAbstract {
			continue
		}
		for _, base := range info.bases {
			idx.implMap[base] = append(idx.implMap[base], qualName)
		}
	}
}

// pyBuiltins are Python built-in functions that appear as bare calls without
// an import and have no local fact — resolving them would produce phantom edges.
var pyBuiltins = map[string]bool{
	"print": true, "len": true, "range": true, "enumerate": true, "zip": true,
	"map": true, "filter": true, "sorted": true, "reversed": true, "list": true,
	"dict": true, "set": true, "tuple": true, "str": true, "int": true,
	"float": true, "bool": true, "bytes": true, "type": true, "isinstance": true,
	"issubclass": true, "hasattr": true, "getattr": true, "setattr": true,
	"delattr": true, "callable": true, "repr": true, "hash": true, "id": true,
	"abs": true, "round": true, "min": true, "max": true, "sum": true,
	"any": true, "all": true, "next": true, "iter": true, "open": true,
	"super": true, "object": true, "property": true, "staticmethod": true,
	"classmethod": true, "vars": true, "dir": true, "globals": true,
	"locals": true, "exec": true, "eval": true, "compile": true,
	"input": true, "format": true, "chr": true, "ord": true, "hex": true,
	"oct": true, "bin": true, "pow": true, "divmod": true, "slice": true,
	"NotImplemented": true, "Exception": true, "ValueError": true,
	"TypeError": true, "KeyError": true, "IndexError": true,
	"AttributeError": true, "RuntimeError": true, "StopIteration": true,
	"GeneratorExit": true, "SystemExit": true, "KeyboardInterrupt": true,
}

// importProps builds the Props for an import dependency fact, carrying `deferred`
// only when the import does not run at module-import time. Following `from`, the
// prop is written only when true: absence means the import IS on the import path,
// which is the overwhelming majority.
func (w *pyWalker) importProps(extra map[string]any) map[string]any {
	p := map[string]any{"language": "python"}
	for k, v := range extra {
		p[k] = v
	}
	if w.importDeferred {
		p["deferred"] = true
	}
	return p
}

// claimImport reports whether this import statement should emit a dependency fact,
// recording it so a second registration pass over the same body does not emit a
// duplicate. Both passes still register the importMap binding, which is what they
// are each there for; only the fact is once-per-statement.
func (w *pyWalker) claimImport(node *sitter.Node) bool {
	if node == nil {
		return true
	}
	if w.emittedImports == nil {
		w.emittedImports = map[uint]bool{}
	}
	start := uint(node.StartByte())
	if w.emittedImports[start] {
		return false
	}
	w.emittedImports[start] = true
	return true
}
