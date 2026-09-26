package engine

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/pkg/plugin"
)

// v276: Scala reads capture-checking syntax. The grammar moves from
// tree-sitter-scala v0.24.1 to v0.26.2, vendored and regenerated at ABI 14 the
// way the Dart grammar is, because v0.25.0+ ship ABI 15. v0.24.1 had no
// capture sets: on `() ->{this} Unit`, `T^{C}` or `class Box[T, C^]` its error
// recovery re-synced past the set, so methods vanished, a class's members were
// hoisted to the enclosing object and whole objects' members to package scope,
// all under `parse_errors: 0`. Measured on a capture-checked Scala 3 codebase
// of 318 files: 33 parsed with errors before and none after, 94 symbols move
// to their correct owner and 19 that were dropped reappear, and the dependency
// facts are unchanged.
// v275: Python router mounts follow the application layout FastAPI documents.
// A relative from-import binds a submodule (`from .internal import admin`) to its
// module path, so `admin.router` resolves to its group and its include_router
// prefix applies; dots past the first climb packages (`from ..deps import f`
// had resolved one package too shallow, leaving the reference dangling). A
// computed include_router or APIRouter prefix (`settings.API_V1_STR`,
// `API + "/v1"`) resolves through the v274 constant table, including a class
// default read through a module-level instance (`settings = Settings()`). A
// module-level `router = build_router()` names its factory's router, and a
// router defined in a package __init__ resolves from `pkg.router`, including one
// imported relatively (`from . import explore`). A router built from an APIRouter
// subclass the repository declares (`UserAPIRouter(prefix=...)`) is a router. A
// route registered on "" is named "/" when no mount composes onto it.
// v274: Python reads the FastAPI/Starlette route forms beyond `@router.get("/x")`:
// @x.api_route (verbs from methods=, default GET), @x.websocket/@x.websocket_route
// (GET, protocol=websocket), add_api_route/add_route and their websocket forms
// (module level and in controller methods, `self.router` keyed to its class), and
// route-decorated defs under a module-level if/try. A path held in a constant, an
// f-string or a `+` concatenation resolves repo-wide against module- and class-level
// string constants, through imports; a route whose path does not resolve is dropped,
// never emitted half-known. The engine also keeps a build/, tmp/ or dist/ directory
// that holds an __init__.py: it is a Python package, not build output.
// v273: the hierarchical rule crosses call boundaries. Go records, per in-loop call,
// whether the callee is handed an element of the caller's loop (calls_on_loop_element)
// and, per function, whether its own loops walk something a caller passed it
// (loops_over_param). `for _, x := range xs { g(x) }` where g loops over what it
// received visits each element of each x once across the whole nest, so the callee is
// finishing the caller's walk rather than running one per element, and its depth no
// longer compounds. Both halves are required: passing the element proves nothing when
// the callee loops over package state, and looping over a parameter proves nothing
// when the caller passes something else.
//
// A value computed from the element counts as the element (`rel := norm(f); use(rel)`),
// or the rule would only ever fire on a bare loop variable, which is the minority of
// call sites. That taint also reaches the LOCAL hierarchical rule, which is why nested
// loops over derived collections stopped counting too.
//
// On this repository the high tier fell from 250 to 103. A second labeled sample, ten
// of the 170 findings that stopped being high, was read to check the direction: all
// ten were over-counts (internal/perf/testdata/high_tier_labels.jsonl, sample
// "removed"). The two paths that compute effective depth had to be changed together;
// they answer the same question, once memoised per name and once recomputed per fact.
//
// v272: Go's scaling_loop_depth gains three bounded-loop rules, so nesting that
// cannot grow with the input stops entering the Big-O exponent. A loop with a literal
// trip count (`for d := 1; d <= 10; d++`), a loop that advances an index its enclosing
// loop already advances (one scan of a buffer, not a scan per byte), and a range over
// a collection rebuilt from a composite literal on every iteration (at most a couple
// of entries, however large the graph) are each proved constant syntactically.
//
// The hierarchical discount that already existed had to be split from the new ones:
// reaching a collection through an outer loop's variable cancels a factor only when
// the outer loop contributed one, so under a constant outer it must not apply, or a
// plain walk reads as O(1). A hierarchical loop still passes the hierarchy on.
//
// Measured on this repository: a hand-labeled sample of 35 of its 250 high findings
// held 3 that scale and 32 that count a loop which cannot
// (internal/perf/testdata/high_tier_labels.jsonl). Every rule fails closed, so a loop
// it cannot prove bounded keeps counting.
//
// v271: Go records a function used as a VALUE. analyzeBody walked bodies for CallExpr
// only, so `"go.mod": readGoMod` in a dispatch table and `rc.lock(f, "yarn.lock",
// yarnLock)` as an argument left the callee with no incoming edge; package-level var
// and const initializers were not walked at all, so `var repoRoot = findRepoRoot()`
// was invisible too. Both read as dead code at HIGH confidence — the tier documented
// as the safest to delete. On enola itself, 23 of 27 first-party high-confidence
// findings were one of these two shapes, every one false. Package-level references
// hang on the package fact, because the table holding them is usually an unexported
// var with no symbol of its own. Resolution stays narrow (a bare identifier naming
// this package's own top-level function) and a redeclared name suppresses the edge,
// so a shadow costs a reference rather than inventing one.
// cacheVersion is mixed into every cache key. Bump it whenever the fact schema or
// an extractor's output format changes in a way that invalidates stored facts.
// v2: Swift URLSession extractor precision (file-URL exclusion, interpolation fix).
// v3: Python route facts use method/role/bare-path Name (was http_method, verb-in-name).
// v4: Java HTTP client detection (RestTemplate call sites + @FeignClient interfaces).
// v5: PHP HTTP client detection + Laravel/Symfony route DSLs (attributes, YAML/XML config).
// v6: Ruby bare-constant references emitted as RelCalls edges (dead-code precision).
// v7: Ruby skips builtin-constant edges (god-class noise) + serializer attribute/include_ folding.
// v8: C/C++ file-scope registration-macro args (module_init/EXPORT_SYMBOL/DEVICE_ATTR) emitted as module-fact call edges.
// v9: C/C++ function-pointer field assignments (obj->cb = fn) + macro-body call references (IDENT( inside #define) emitted as call edges.
// v10: C/C++ qualifier-prefixed registration macros (static DEFINE_*_PM_OPS(name, suspend, resume)) record their function-name args as call edges.
// v11: C/C++ in-body compound-literal designated initializers (cfg = (struct X){ .cb = fn }) record their function-pointer fields as call edges.
// v12: C/C++ macro-body scan also captures value-position function pointers (.field = fn / = &fn inside #define), e.g. ops tables defined via a macro.
// v13: C/C++ in-extractor macro expansion of file-scope invocations recovers token-pasted callbacks (CONFIGFS_ATTR/DEVICE_ATTR_RO -> name##_show).
// v14: C/C++ static single-arg DEVICE_ATTR/BUS_ATTR expansion, all-ident scan of expanded macros (DEFINE_SHOW_ATTRIBUTE), capitalized C callee resolution.
// v15: C/C++ salvage function-pointer refs from file-scope ERROR regions (macro-opened structs like MACHINE_START/DT_MACHINE_START ... MACHINE_END).
// v16: extend that salvage to file-scope assignment_expression/field_expression fragments (machine_desc blocks parse that way when surrounded by other code).
// v17: full-tree salvage of `.field = fn` macro-struct debris (machine_desc) regardless of where tree-sitter scatters it (skips function bodies).
// v18: Ruby records custom class-body macro names + resolves self/self.class receivers as call edges (dead-code precision). KindTestRef facts index outbound refs from spec/test files.
// v19: Ruby captures call edges outside method bodies — class/module-body qualified & argument-position calls attach to the class fact; top-level/file-scope calls (fixtures, after_initialize blocks) attach to a new KindFileRef fact folded by the orphan collector.
// v20: Ruby call capture outside method bodies generalized to a per-scope pass (whole class/module body + top-level program), so assignment RHS and all non-`call` statements — e.g. `x = GlobalSetting.foo`, `CONST = { Proc.new { Group.bar } }` — are covered, not just bare call statements.
// v21: Ruby records the static prefix of interpolated symbols (`:"report_#{type}"` -> "report_") as a KindFileRef prop, so the dead-code detector treats dynamically dispatched (public_send/send) same-prefix methods as used.
// v22: Ruby records `super` as a call to the same-named ancestor method, and literal-symbol dispatch args (`obj.try(:foo)`, `send(:bar)`, `respond_to?(:baz)`) as calls to the named method.
// v23: Ruby captures no-arg calls on a chained receiver (ActiveRecord scope/class-method chains `Model.scope.final`, `assoc.class_method`, `x.class.method`; cheap attribute reads skipped) and indexes .rake/Rakefile files.
// v24: Ruby walks method default-parameter values for calls (`def f(x = self.class.foo)`) and records single-level predicate/bang calls (`viewer.rich?`, `x.save!`) which — unlike plain attribute reads — are unambiguously method invocations.
// v25: Ruby folds `delegate :a, :b, ..., to: X` method names as calls, and records calls on a bare-method (non-local identifier) receiver when the method name is scope-like (`some_relation.pluck_job_id`).
// v26: Ruby records scope-like (underscored) method calls on ANY identifier receiver, including local relation variables (`items = ...; items.preload_relations`).
// v27: Ruby resolves underscored calls on @ivar/@@cvar/$gvar receivers (`@klass.bo_search_fields`) and indexes view templates (ERB/Slim/HAML) for embedded Ruby calls (helpers/class methods), emitting KindFileRef references.
// v28: Ruby records method calls on a `klass`/`clazz`/`klazz` (or @klass) receiver as class-method dispatch (`klass.inline`), regardless of the method name.
// v29: Ruby extends the interpolated-prefix dispatch heuristic to strings (`"present_#{idx}"`), not just symbols, so send()-by-computed-string-name marks same-prefix methods used.
// v30: interpolated-string prefixes are now gated on dispatcher-proximity (committed only when the enclosing scope also calls send/public_send/…), so cache/Redis-key strings (`"fetch_#{id}"`) no longer hide genuine orphans; interpolated symbols remain unconditional.
// v31: Ruby block parameters (`each do |user| … end`) are now treated as locals, so a bare block var whose name matches an association no longer records a spurious in-loop call (N+1 false positive); and find_in_batches/in_batches are no longer counted as element loops (their block yields a batch, so the inner .each/.map is the real per-element loop) — fixing O(n²) mislabels of single-pass batch scans.
// v32: Ruby no longer flags `super` (climbs the inheritance chain, terminates) or a same-named call on an explicit non-self receiver (SimpleDelegator/decorator, `@delegate.render`/`new.call`) as self-recursion — both set recursive_self spuriously, the dominant recursion false positive.
// v33: Ruby recursion is now gated to same-object self dispatch — `self.class.foo` (instance method calling its sibling class method) and `obj.try(:foo)` (dispatch to a different object) no longer set recursive_self; receiverless calls, `self.foo`, and `Const.foo` matching the method's own full name still do.
// v34: Ruby constant-bounded iterators (`6.times`, `[…].each`, `%w[…]`, ALL-CAPS `CONST.each`) no longer add scaling loop_depth — they run a fixed number of times, so they no longer inflate a genuine O(n) into a false O(n²)/O(n³).
// v35: constant-bounded-loop detection now unwraps trailing size-preserving chain methods (`[a,b].compact.all?`, `%w[…].map.each`), so a bounded literal/constant behind `.compact`/`.uniq`/`.map`/… is still recognized as bounded.
// v36: Ruby module symbols now carry an `abstract` bool prop — true for mixins (modules that define instance methods) and ActiveSupport::Concerns, false for namespace/utility modules — so package-metrics abstractness (A) no longer counts Rails namespaces as abstractions. Bare-constant coupling resolution is also namespace-aware now.
// v37: Swift resolves modules at the SPM/XcodeGen *target* level instead of by leaf directory — it parses project.yml (and its include: files) so each product target's files form one module (Sources/<Name>), and routes SPM package sources into their target module; symbol names, module facts, and inter-target dependency edges all change accordingly.
// v38: Swift XcodeGen targets sharing one primary source root (e.g. the app plus its SwiftUI-preview and unit-test host targets) now collapse to a single module — the first target by sorted name owns the identity, shadow targets emit no duplicate module fact.
// v39: Swift emits SymbolMethod (not SymbolFunc) for functions declared inside a type, and records member-call edges for any receiver — self?.method() cross-extension/closure dispatch, and lowercase/property-chain receivers (coordinator?.foo(), delegate?.bar()) — resolved against a project-wide method index in a serial post-pass (unique→qualified, ambiguous→bare short name, unmatched→dropped). Also credits the method in Type.foo() and suppresses the phantom `defer` call edge. Fixes coordinator-pattern dead-code false positives.
// v40: Swift captures top-level/file-scope calls (bare `foo()` and `let x = foo()` in #!/usr/bin/swift scripts) as a KindFileRef fact so file-scope-invoked functions aren't flagged dead, and emits call edges for custom-operator usage (infix/prefix `custom_operator`, e.g. `a <- b`) resolved against operator overloads now added to the method index. (Standard-token operators like +/+=/^ are intentionally not tracked to avoid fan-in flooding.)
// v41: Swift custom-operator usage now excludes stdlib operators that the scanner emits as `custom_operator` tokens (multi-char `<=`, `>=`, `??`, `..<`, …) — only genuinely user-defined operators (`<-`) get usage edges, so comparison overloads (Time.<=/>=) no longer collect spurious fan-in / false recursion.
// v42: Swift resolves member calls to top-level functions (funcIndex fallback in resolveMethodCalls) so methods of a type whose body tree-sitter fails to parse — flattened to top-level functions, e.g. ImageUploadModel with a tuple-metatype `(T,U).self` — are no longer seen as dead; and property initializer/computed-getter calls now attach to the property as owner (essential inside `extension` blocks, which push no type owner), so a helper called only from an extension property is no longer flagged dead.
// v43: the v42 property-owner change is now scoped to the ownerless (extension) case only — class/struct property init edges stay attributed to the enclosing type as before, avoiding a broad re-attribution of the coupling graph while still fixing extension-property call capture.
// v44: Swift constant-bounded loops (`for i in 0..<10`, literal-bound `stride(...)`, and iterator closures over an array/dictionary literal or ALL-CAPS constant like `STOP_CHARS.forEach`) no longer add scaling loop_depth — they run a fixed number of times, so they stop inflating a genuine O(n) into a false O(n²)/O(n³) (Swift parity with Ruby v34/v35). Also: computed-property getters and willSet/didSet observers now emit complexity metrics (cyclomatic/loop_depth/loop_count/calls_in_loop/recursive_self), so a loop or per-iteration I/O inside `var x: [T] { … }` or `didSet { for … }` is visible to analyze_performance.
// v45: Swift subscript access (`dict[key]`, `parameters["x"] = 1`) is no longer mistaken for a function call — the tree-sitter grammar models it as a call_expression whose `[...]` is a call_suffix, so a subscript on a local/property whose name collides with a method (`parameters["x"]` inside `func parameters()`) was recorded as a self-call, producing a phantom RelCalls edge and a false `recursive_self` flag. Subscript call-expressions are now detected by their `[` call-suffix delimiter and skipped (their receiver/key are still walked for real calls), removing false recursion findings and phantom call-graph edges.
// v46: Swift `recursive_self` is now argument-label aware — a call that shares the enclosing function's bare name is flagged as recursion only when its argument labels match the function's parameter labels. This stops a call to a DIFFERENT overload/override/stdlib method of the same name being read as self-recursion: an `override func setSelected(_:animated:)` calling `super.setSelected(_:animated:)`, a `decode(key:)` extension calling stdlib `decode(_:forKey:)`, or `loadMore(completion:)` delegating to a sibling `loadMore(service:)`. Call edges are unchanged (dead-code/coupling unaffected); only the recursion signal is refined.
// v47: Swift methods now carry an `io_direct` prop when their body invokes a network/file I/O primitive (URLSession/dataTask/.data(for:), Alamofire request/download/upload, Data(contentsOf:)/String(contentsOf:)), and a transitive `performs_io` prop computed by a serial closure that propagates io_direct up the call graph — crossing ambiguous kept-bare member-call edges by expanding them through the methodIndex candidate sets (bounded), without adding edges to the shared graph. Lets the enterprise analyzer flag a genuine per-iteration network N+1 (a loop calling a method that transitively hits the network) that was previously invisible because the I/O sat behind wrapper layers and ambiguous edges.
// v48: Swift resolves inherited-method calls — a subclass (or protocol conformer) calling a base-class / protocol-extension method used to leave a dangling edge (`dir.runRequest`) because the callee isn't in the enclosing type's own method set. A serial post-pass now rewrites such dangling call targets to the declaring ancestor's method fact (`dir.DataModel.runRequest`) by walking the caller type's supertype chain (nearest-first), so class/protocol hierarchies are traversable for impact_analysis, dead-code, coupling, and the performs_io closure. Only dangling targets whose short name an ancestor declares are rewritten; already-resolved edges are untouched.
// v49: Swift models XcodeGen test-bundle targets (bundle.unit-test/bundle.ui-testing) as one module each, so a test bundle's files (e.g. Tests/Core/**) collapse into a single module instead of exploding into per-leaf-directory modules; and every module fact now carries a normalized `module_role` prop (production/test/tooling/unknown) — derived from the XcodeGen target type, the SPM target vs testTarget call, or a path heuristic for leaf-directory fallback — so package-metrics and other analyses can measure the production population without re-parsing manifests.
// v50: the `module_role` prop is now emitted by the Ruby extractor too (packwerk packages → production; leaf-directory modules → path heuristic), and the path heuristic was hoisted to facts.ModuleRoleForPath and broadened to common cross-language conventions (spec/test/tests + scripts/bin/fastlane/ci_scripts), so Ruby build-tooling modules (fastlane/, Scripts/) are classified as tooling rather than defaulting to the production population.
// v51: Swift no longer emits type-reference-derived module→module dependency edges for files that belong to a resolved SPM/XcodeGen target — those files' cross-module deps are captured completely by their `import X` statements plus the declared target graph, whereas the type-reference pass resolved bare short names through a collision-prone global index (Swift namespaces nested types, so names like Event/State/Coordinator recur across targets) and fabricated impossible back-edges (a Foundation-level target "importing" a feature target) that produced a false module cycle. For loose Swift projects (leaf-directory fallback, no target graph) the pass still runs but now skips any type name defined in more than one module. Fixes the false Swift dependency cycle.
// v52: Kotlin emits SymbolMethod (not SymbolFunc) for functions declared inside a class/object (parity with Go/Java), keeping member functions out of the high-confidence orphan bucket; records a short-name RelCalls edge for every navigation expression (receiver method call `repo.getUser()` and property/field access `slot.uniqueId`), which the short-name-matching dead-code detector needs to see live members as used; and tags `override` methods and Dagger/Hilt `@Provides`/`@Binds` methods with `override`/`di_provider` props so framework/DI entry points are excluded from orphan reporting. Fixes the mass Kotlin/Android dead-code false positives (thousands of live Retrofit/lifecycle/interface members reported as high-confidence orphans).
// v53: Kotlin base-package detection now matches Groovy build scripts (`namespace 'x'`, single quotes) in addition to Kotlin-DSL (`namespace = "x"`). A double-quote-only regex left the base package empty for `.gradle` (Groovy) projects, so every in-repo import resolved as external and bare calls to imported top-level/extension functions (`formatPrettyDate(...)`, `setMargin(...)`) emitted no call edge — reporting live utilities as high-confidence orphans.
// v54: Kotlin now walks calls that live OUTSIDE a function body — function default-parameter values (`fun f(x = helper())`) and supertype constructor-delegation arguments (`class NpeId(id) : Enrichment(npeEntity(id))`, and the object equivalent). These were previously skipped, so a helper/factory referenced only from a default value or a base-class initializer was mis-reported as a high-confidence orphan (e.g. Snowplow entity builders, Compose default-arg providers).
// v55: Kotlin captures callable references (`::foo`, `Type::foo`, e.g. `onClick = ::doNothing`, `.map(::helper)`) as short-name RelCalls edges. A function referenced only as a method reference (never called directly) was previously mis-reported as a high-confidence orphan.
// v56: Kotlin/Java perf-fact precision — (1) recursion (`recursive_self`) is now argument-count aware: a call sharing the enclosing function's name is only flagged recursion when its arg count matches the parameter count, so a call to a same-named overload (`updateItem(x)` → `updateItem(i, x)`, `onChangeStarted(2)` override → `onChangeStarted(3)`) is no longer read as self-recursion — the dominant Kotlin/Android recursion false positive (Conductor `onChange*` lifecycle). (2) Kotlin RxJava/coroutine-Flow chains no longer inflate loop_depth: in a reactive function (reactive return type Single/Observable/Maybe/Flowable/Completable/Flow, or body reactive operators subscribeOn/observeOn/applySchedulers/andThen/.subscribe/flowOn/.collect/…), the ambiguous operators (map/flatMap/filter/fold/reduce/onEach) are stream transforms, not per-element collection loops, so a `Single.flatMap { … .map { } }` is no longer a false O(n²)/O(n³). Fixes the analyze_performance false positives on this RxJava-heavy codebase.
// v57: Kotlin/Java recursion also clears the arity-matched case where an `override` delegates to a same-name, same-arity overload declared in a parent (invisible here): a body that calls `super.<self>()` marks the sibling `<self>(…)` call as delegation, not recursion (fixes the residual Conductor `onChangeEnded` false positives). And Kotlin methods now carry `io_direct`/`performs_io` when annotated as a Retrofit endpoint (@GET/@POST/@PUT/@DELETE/@PATCH/@HEAD/@OPTIONS/@HTTP) or a Room DAO op (@Query/@Insert/@Update/@Upsert/@Delete/@RawQuery) — a precise per-method I/O identity that lets analyze_performance flag a per-iteration call to a real network/DB method as a genuine N+1 (ranked high) without relying on the cross-language keyword guess.
// v58: Kotlin/Java multi-module import resolution — imports are now resolved via a
// cross-language declared-package→directory index (built from every non-test .kt AND
// .java file) instead of assuming a single global source root. In a multi-module
// Gradle project where several modules root packages at the same prefix (app/, api/,
// business/ all under de.foo.*), the old Kotlin resolver mapped every internal import
// under the single most common source root (the app module), collapsing all
// cross-module afferent coupling onto the app package (bogus Ca god-package) and
// starving library modules of Ca (falsely "useless" in package-metrics). Now an
// import resolves to the module that actually declares the package. The Java extractor
// seeds its FQN resolver with the same cross-language index so Java→Kotlin imports no
// longer drop. Kotlin & Java module facts now carry `module_role` (test for
// src/test & src/androidTest, else production) so package-metrics excludes test source
// sets; and Kotlin `sealed` classes are marked `abstract` so abstractness (A) counts
// them (they are non-instantiable), fixing inflated Distance / false "rigid" findings.
// v59: the package index now prefers a main source set (…/src/main/…) over a Gradle
// build-variant source set (src/debug, src/release, src/staging) when both declare the
// same package. An Android app's src/main and src/debug both declare the root
// application package; the v58 lexicographic tie-break wrongly mapped it to src/debug
// ('d' < 'm'), misrouting the whole app's afferent coupling onto the debug variant
// (a bogus Ca god-package). Imports of the root package now resolve to the main module.
// v60: package-metrics precision — (1) module_role now sub-token-matches compound test
// module names (split each path segment on -/_ and match an exact `test`/`tests` token),
// so Gradle test-automation modules that compile as src/main (release-tests, ui-test-utils,
// test-lab) are classified test rather than leaking into the production population, without
// misfiring on latest/contest/abtest. (2) Dagger/Hilt DI infrastructure is now tagged:
// @Component/@Subcomponent interfaces get `di_component`, @Module classes get `di_module`
// (Java & Kotlin); a Dagger @Component interface is no longer mislabeled a Spring component
// (disambiguated by interface-vs-class). Lets package-metrics exclude DI wiring from
// abstractness/type counts (a Dagger component package was falsely "useless").
// v61: TS/JS extractor now emits file-scope reference facts (KindFileRef) for JSX
// component rendering (<Foo/>), imported-identifier values (route configs like
// `{ component: Foo }`), namespace member access (`ns.foo`), require()-bound names,
// and `export … from` re-exports — plus require()/dynamic-import() dependency edges.
// Fixes massive dead-code false positives on React/CommonJS codebases, where a
// component used only via JSX or a route table previously had no incoming edge.
// v62: the TS/JS file-scope reference pass now also records same-module use
// positions — a bare call callee and identifier call arguments — so a function used
// only at module scope (`startSession()` at file top level) or passed as a value to
// an HOC (`connect(mapStateToProps)`) is no longer falsely reported dead.
// v63: a default import now also references the target module's default-export symbol
// (resolved via the known-files set + fileSymbolName), so an anonymous folder-index
// default like `export default connect(...)(X)` — named "<Folder>Index" — is no longer
// falsely reported dead when imported by the component's own name.
// v64: a `this.<member>` reference inside a class method now records a use of that
// member, so a React class-component event handler bound as a prop value
// (onClick={this.handleClick}) — never called by name — is no longer falsely dead.
// v65: the TypeScript extractor now skips minified/bundled files (any line longer
// than ~2000 chars), so checked-in vendor bundles emit no facts — invalidates
// caches that still hold the obfuscated symbols.
// v66: the TypeScript extractor now emits io_direct (body calls a network/file
// primitive or a network-module import binding) and a transitively-propagated
// performs_io prop, so cached TS facts must be re-extracted to carry them.
// v67: tightened TS io_direct — only DEFAULT/NAMESPACE network-module imports are
// I/O bindings (not named imports), and types/utils submodules are excluded, so pure
// helpers (e.g. `resolved` from network/types) no longer mislabel callers.
// v68: the TypeScript extractor now sets abstract:true on `abstract class`
// declarations (was previously indistinguishable from a concrete class), so
// package-metrics abstractness for TS must re-extract to pick up the flag.
// v69: HTTP-client detection expanded — TS options-object/openapi-fetch clients,
// Swift endpoint-enum (APIEndpoint/TargetType) clients, Rails draw(:pkg) routes now
// carry their /api/vN scope prefix, and Swift URLSession skips test/fixture sources.
// v70: Swift endpoint extractor resolves the version prefix — repo-wide default
// (protocol-extension urlPrefixComponent), single-value/switch-default overrides,
// and version-constant interpolation — so prefix-less endpoints match backend routes.
// v71: Swift endpoint extractor resolves stored-method endpoint structs (path/prefix
// computed, `method` a stored property) by reading the HTTP verb from each
// instantiation site's `method:` argument, emitting one client route per (path, verb).
// v72: Swift extractor also resolves request-wrapper endpoints (path supplied at the
// call site's `urlPathComponent:` arg, verb from `method:`/`httpMethod:` or a type
// default); Ruby extractor fixes nested Rails resource paths — a singular `resource`
// gets no `:id`, and children of a plural `resources` nest under `:<singular>_id`.
// v73: new gRPC extractor emits a server-role KindRoute per proto RPC (Name
// "/pkg.Service/Method", method POST) plus service/rpc/message symbols; the TS
// extractor detects gRPC-web client call sites as client-role routes
// (source "ts-grpc-client"), so gRPC flows through the cross-repo linker and
// unused-routes like HTTP.
// v74: Go extractor detects gRPC client call sites (NewXxxClient(...) +
// client.Method(...)) and emits client-role routes (source "go-grpc-client"),
// resolving the wire path from the generated concrete client's Invoke/NewStream
// literal. Documentation-only bump — goextractor is not a FileOwner, so its
// facts are never cached; recorded for changelog continuity.
// v75: broadened gRPC client detection — Go now resolves struct-field-injected
// clients (via the field-type map) and connect-go (procedure-const paths); the
// TypeScript extractor detects connect-es createClient/createPromiseClient(...)
// call sites. Bump required because the TS extractor is a FileOwner (cached).
// v76: classic grpc-web clients recognized (TS extractor derives service+methods
// from MethodDescriptor/rpcCall path literals); Go extractor resolves gRPC
// clients held in package-level vars. Bump required for the TS (FileOwner) change.
// v77: Ruby extractor detects Gemfile-less repos via a loose .rb/shebang scan and
// indexes extensionless Ruby executables. Bump so snapshots that cached an empty
// (undetected) Ruby result re-extract instead of serving stale zero facts.
// v78: Python extractor now emits call edges for ABSOLUTE intra-project imports
// (previously only relative imports resolved, so functions reached via
// `from pkg.mod import fn` had no incoming edge and read as dead code). A post-pass
// (resolveCallTargets) rewrites the dotted targets to canonical slash symbol names
// and drops stdlib/third-party edges; the extractor also emits KindFileRef edges for
// module-level (top-level) calls and records decorator applications as uses. Bump so
// cached Python snapshots re-extract with the new edges.
// v79: Python extractor tracks more reference mechanisms — function-local (lazy)
// imports are registered so calls through them resolve; functions/classes passed by
// name as call arguments (Depends(fn), add_command(cmd)) emit reference edges;
// parameter-default and decorator-call-argument expressions are walked (FastAPI
// Depends(...) in signatures and route-decorator dependencies); and click/Typer
// @command/@group functions are tagged cli_command. Reduces Python dead-code false
// positives; bump so cached snapshots re-extract.
// v80: Python extractor tracks three more reference mechanisms — FastAPI route
// decorators declared with the path= keyword (and empty paths) now emit route facts
// (so their handlers are rescued); pyproject.toml entry-points / console-scripts emit
// reference edges to the registered module:function; and dotted-path string literals
// (>=3 identifier segments) that name an internal symbol (lazy_load_command targets,
// provider "class-name" metadata) emit reference edges. Reduces Python dead-code
// false positives; bump so cached snapshots re-extract.
// v81: Python extractor closes three more reference gaps — class-body statements are
// walked for calls/value-refs (attrs/pydantic/SQLAlchemy field wiring like
// factory=_helper(...) and default=Factory(fn)); same-module functions/classes passed
// by name as a value are credited (via a per-module top-level-def index, excluding
// shadowing params); and FastAPI route handlers declared with a non-literal/computed
// path are tagged web_component=route_handler (framework entry points). Reduces Python
// dead-code false positives; bump so cached snapshots re-extract.
// v82: Python extractor closes the last registration gaps — functions decorated with a
// framework-registration decorator (@compiles, @x.register singledispatch, @sig.connect,
// @event.listens_for, Flask hooks) are marked used via a self file-ref edge; value
// references now also resolve attribute args (register_error_handler(404, m.handler))
// and dict/list/set/tuple values (dispatch tables like {"ds": ds_filter}). Reduces
// Python dead-code false positives; bump so cached snapshots re-extract.
// v83: complexity signals gain a new prop scaling_loop_depth (loop nesting counting only
// input-scaling loops — literal/constant/range(<const>) iterables and infinite
// while(true)/for{} event loops are discounted), emitted by the Python, TypeScript, and Go
// extractors; the Python extractor also now emits io_direct/performs_io (transitive
// DB/network/file I/O). Consumed by the enterprise performance analyzer to deflate the
// O(n^k) tail and tighten call-in-loop precision; bump so cached snapshots re-extract.
// v84: complexity signals gain calls_in_scaling_loop — the subset of calls_in_loop made
// inside an input-scaling (unbounded) loop — emitted by the Python, TypeScript, and Go
// extractors. Lets the performance analyzer treat a call in a bounded loop (literal /
// range(<const>) / while(true)) as a fixed count, not an N+1; bump so cached snapshots
// re-extract with the new signal.
// v85: Python extractor emits two new structural props so package-metrics stops
// mislabeling idiomatic-Python packages. (1) `enum` on Enum/IntEnum/StrEnum/Flag/IntFlag
// subclasses (excluded from N like Kotlin enums) and `data_class` on DTO/schema/record
// classes — @dataclass/@attrs-decorated, Pydantic BaseModel, NamedTuple, TypedDict —
// so DTO/model packages (e.g. OpenAPI-generated Pydantic "datamodels") are no longer
// flagged "rigid — extract interfaces". data_class covers Pydantic BaseModel, NamedTuple,
// and TypedDict subclasses plus @dataclass/@attrs-decorated classes. (2) `abstract` now
// also covers the duck-typed abstract pattern (a method whose whole body is
// `raise NotImplementedError`), so abstractness (A) is meaningful for Python base classes
// that don't use ABC. Bump so cached Python snapshots re-extract with the new props.
// v86: Python data_class detection broadened to Pydantic RootModel/GenericModel/BaseSettings
// and any "*BaseModel" subclass — covers project-local Pydantic bases (StrictBaseModel,
// <App>BaseModel) used by hand-written schema packages, which v85 missed because BaseModel
// wasn't a direct base (so those datamodels packages were still flagged "rigid"). Bump so
// snapshots cached under v85 re-extract with the widened detection.
// v87: Python resolveCall no longer fabricates same-module RelCalls edges for callable
// parameters, locals, and loop variables — it now resolves a bare callee only when the name
// is a known module-level def and not shadowed by a parameter (mirrors valueRefTarget). Drops
// spurious call edges; bump so cached Python snapshots re-extract with the tightened resolver.
// v88: Python extractor now emits gRPC client-role routes (source=python-grpc-client) for
// stub.Method(...) call sites, detected from source. New facts, so cached Python snapshots must
// re-extract to pick them up.
// v89: Swift HTTP-client extractor widens method inference (symmetric scan window + enum/
// .rawValue/Alamofire .method forms) and tags calls to hardcoded absolute hosts with
// external=true + host. Changes route methods and props, so cached Swift snapshots must
// re-extract.
// v90: Kotlin Retrofit extractor tags absolute-URL annotations external=true + host; Ruby
// route extractor adds `match ... via:` verbs and reads `scope`/`namespace path:` keyword
// prefixes. New/changed route facts, so cached Kotlin and Ruby snapshots must re-extract.
// v91: Ruby route extractor emits both PATCH and PUT for the resources/resource update
// action (Rails routes both verbs to update), so a client calling PUT resolves. New route
// facts, so cached Ruby snapshots must re-extract.
// v92: Ruby route extractor handles symbol path args (`get :cities_by_zip`), a bare-symbol
// `scope :users` path prefix, and the `resource(s) ..., path:` segment override. New/
// corrected route paths, so cached Ruby snapshots must re-extract.
// v93: Swift endpoint extractor's switchReturns now collects case labels that wrap across
// multiple lines, so a `case .a,\n .b: return .post` maps every label (not just the first)
// — correcting HTTP methods that previously defaulted to GET. Cached Swift snapshots must
// re-extract.
// v94: Swift endpoint extractor reads a single-value method property (`var method:
// HTTPMethod { return .post }`, no switch) and applies its lone verb to every case, instead
// of defaulting to GET. Cached Swift snapshots must re-extract.
// v95: Ruby extractor tags synthetic coupling edges with a coupling_kind prop (so the cycles
// explainer can exclude ActiveRecord associations) and adds common Rails framework constants
// (I18n, Rails, Logger, ...) to the builtin-const ignore list. Cached Ruby snapshots must
// re-extract to pick up the new edge props and suppressed references.
// v96: Python value-ref resolution now records assignment-RHS and return-statement bare-name
// references (cb = handler; return cb / return handler) as RelCalls edges, not just call
// args/decorator args/collection literals; and its shadow guard (previously param-only, shared
// with resolveCall's v87 fix) now covers any name bound in the enclosing function's own scope —
// assigned/iterated/aliased locals, not just parameters — so a loop var or local reusing a
// same-named top-level def's name no longer fabricates a same-module edge. Cached Python
// snapshots must re-extract.
// v97: the Ruby ignore/test globs are directory-scoped ("**/spec/**/*_spec.rb" rather than
// "**/*_spec.rb"), so a production file whose basename merely ends in the token _test/_spec
// (a job named cache_warmup_ab_test.rb) is indexed as source instead of being ignored and
// misrouted to reference-only test-ref extraction. The glob matcher gained the
// "<prefix>/**/<fileglob>" form to express it. Cached Ruby snapshots must re-extract: the
// file set reaching the extractor changes.
// v98: the Kotlin extractor learned bounded-loop discounting and now emits
// scaling_loop_depth (and calls_in_scaling_loop) alongside loop_depth, joining the
// Go/Python/TypeScript convention. A constant-count loop — a literal integer range
// (0..2, 0 until 3, 2 downTo 0, 0..10 step 2), a collection-literal receiver
// (listOf(a, b).forEach), an ALL_CAPS data constant, or a size-preserving chain over
// either — no longer inflates the Big-O exponent, and neither does an infinite
// while (true) / do-while (true). Previously perf.scalingDepth() silently substituted the
// raw loop_depth for Kotlin, so `for (ring in 1..6) { for (i in 0 until sides) }` reported
// O(n2) at full confidence. Cached Kotlin snapshots must re-extract.
// v99: calls_in_scaling_loop now means "calls inside a loop that repeats a NON-CONSTANT
// number of times", not "calls inside an input-scaling loop", and is emitted by the Go,
// Python, TypeScript and Kotlin extractors whenever calls_in_loop is — even when empty.
// Two bugs, one cause: `for {}` / `while (true)` were classed bounded, which is right for
// the Big-O exponent (they add no factor of n) but wrong for N+1 detection (a parent-chain
// walk runs one query per level), and an omitted empty subset was read by
// perf.scalingLoopCalls() as "extractor never computed it", falling back to the unfiltered
// calls_in_loop. The second bug masked the first: fixing only the emptiness would have
// deleted true-positive N+1 findings on infinite loops. Cached Go/Python/TypeScript/Kotlin
// snapshots must re-extract.
// v100: the Go extractor implements plugin.TestRefExtractor and config.Default().TestGlobs
// gained "**/*_test.go", so a production function whose only caller is its own _test.go is
// no longer reported as high-confidence dead code. Both gates were needed: the glob alone
// is a no-op (runTestRefExtractors skips non-implementers), and the interface alone never
// sees the files (walkRepo collects an ignored file only when it matches a test glob). Go
// was the sharpest case of GAP-XL-02 — _test.go is ignored under BOTH shipped configs and a
// plain function's orphan tier is `high`. Test refs are resolved with the production
// resolvers (flattenSelector/collectLocalTypes/resolveChain), so a reference from a test is
// spelled exactly as one from production and inherits goBuiltins filtering: a package-level
// `min` shadowing the Go 1.21 builtin is still not credited, from either side. The file set
// reaching the extractors changes, so cached snapshots must re-extract.
// v101: the Go HTTP-client extractor recovers the host that `baseURL + "/path"`
// concatenation discards. It resolves the base identifier through a package-scoped
// string-literal index (const/var/assignment/composite-field bindings) and, when
// every binding is an absolute http(s) URL, tags the route external=true (plus a
// host prop when the bindings agree on one host). Before this, a service calling
// only third-party APIs via base-URL concats accumulated phantom "unresolved
// internal edges" and was misclassified
// coverage_gap. A config-injected base (options.BaseURL, no literal binding) stays
// an internal client route. Route facts gain external/host props, so cached Go
// snapshots must re-extract. (The paired linker change — matching a client route
// before bucketing it external, so a hardcoded internal host still resolves — runs
// post-extraction and needs no cache bump; it is covered by TestGolden here.)
// v102: the Swift extractor folds a typealias's aliased type into the alias fact
// as a RelInstantiates edge. `typealias FooViewModel = FooEditorState` used to
// leave FooEditorState with no incoming edge, so a type reached only through its
// alias name was mis-reported as unreferenced dead code (GAP-SW-09). handleTypeAlias
// now emits the edge (guarded like handleInit — system types and function/tuple/
// optional RHS shapes, which have no simple type name, emit nothing). Swift symbol
// facts gain a relation, so cached Swift snapshots must re-extract.
// v103: the TypeScript extractor implements plugin.TestRefExtractor, and
// config.Default().TestGlobs gains the four *.test.ts(x)/*.spec.ts(x) globs, so a
// production symbol whose only caller is its co-located test keeps an incoming
// edge and is no longer reported dead (GAP-XL-02 TS half — the last language
// affected under config.Default(), after Go at v100). ExtractTestRefs reuses the
// file-ref walk's production resolvers so targets are fully qualified (no bare-name
// over-crediting via orphans' lastSeg fold). TS test files now emit test_ref facts,
// so cached TS snapshots must re-extract; the bump is required because the TS
// extractor is a FileOwner (cached).
// v104: the Java extractor learned bounded-loop discounting and now emits
// scaling_loop_depth (and calls_in_scaling_loop) alongside loop_depth, joining the
// Go/Python/TypeScript/Kotlin convention (GAP-JV-01, the Java third of GAP-XL-01). A
// constant-count loop — a literal-bounded for (for (int i = 0; i < 3; i++)), a for-each
// over a collection literal (List.of(...)) or an ALL_CAPS constant, or a stream iterator
// over such a receiver — no longer inflates the Big-O exponent; an infinite for (;;) /
// while (true) is likewise discounted from the exponent but keeps its per-iteration calls
// as N+1 candidates (three-valued classification: constant / infinite / scaling — see
// v99). Previously perf.scalingDepth() silently substituted the raw loop_depth for Java,
// so a fixed-count for reported O(n2)/O(n3) at full confidence. Cached Java snapshots must
// re-extract. C/C++ and PHP remain undiscounted (GAP-XL-01 stays open for them).
// v105: the C/C++ extractor learned bounded-loop discounting and now emits
// scaling_loop_depth (and calls_in_scaling_loop) alongside loop_depth, joining the
// convention (GAP-CP-01). A constant-count loop — a literal-bounded for
// (for (int i = 0; i < 3; i++)) or a range-for over a braced init-list ({1, 2, 3}) —
// no longer inflates the Big-O exponent; an infinite for (;;) / while (true) / while (1)
// is discounted from the exponent but keeps its per-iteration calls as N+1 candidates
// (three-valued classification, cache.go v99). STL algorithms (std::for_each over
// [begin, end)) scale with the container and count toward scaling_loop_depth. C/C++ was
// the worst-affected language — fixed-size array iteration is idiomatic. Cached C/C++
// snapshots must re-extract.
// v106: the PHP extractor learned bounded-loop discounting and now emits
// scaling_loop_depth (and calls_in_scaling_loop) alongside loop_depth, joining the
// convention (GAP-PH-01, the last of GAP-XL-01). A constant-count loop — a
// literal-bounded for ($i = 0; $i < 3; $i++) or a foreach over an array literal
// ([1, 2, 3]) / ALL_CAPS constant — no longer inflates the Big-O exponent; an infinite
// while (true) / for (;;) is discounted from the exponent but keeps its per-iteration
// calls as N+1 candidates. Cached PHP snapshots must re-extract. With this, all seven
// AST extractors that discount bounded loops (Go, Python, TS, Kotlin, Java, C/C++, PHP)
// plus Ruby/Swift (inline) are complete — GAP-XL-01 is fully closed.
// v107: the Swift extractor now emits the `override` prop on a method declared with the
// `override` modifier, mirroring the Kotlin extractor (kotlin_ast.go). An override is
// dispatched polymorphically through its supertype — UIKit/SwiftUI lifecycle callbacks
// (viewDidLoad, viewWillAppear, …) are invoked by the framework, never by the override's
// own literal name — so the enterprise dead-code detector (orphans.go, which already
// consumes the prop and documented it as Kotlin/Swift) now excludes them as framework
// entry points instead of flagging them as orphans. Cached Swift snapshots must
// re-extract (GAP-SW-01).
// v108: the Swift extractor now emits `conditional: true` on every symbol declared
// inside a #if/#elseif/#else conditional-compilation branch. Tree-sitter walks both
// branches (the compile-time condition is not evaluated), so a type declared once per
// branch yields two same-name symbol facts; tagging them lets the counting consumers
// (exported-surface, complexity-outliers, package-metrics, via facts.CanonicalSymbols)
// collapse the duplicate while both declarations stay individually queryable. Cached
// Swift snapshots must re-extract (GAP-SW-10).
// v109: the Python extractor now detects Flask routes. Classic @app.route / @bp.route
// (verbs read from methods=[...], default GET) and Flask-AppBuilder @expose — which has
// no receiver dot and was invisible to the FastAPI verb regex — now emit KindRoute facts
// tagged framework="flask". The @app.get verb shorthand is labeled from project detection
// (detectFlask/detectFastAPI) instead of a hardcoded "fastapi", so a Flask 2.0 app is no
// longer mislabeled. Cached Python snapshots must re-extract (GAP-PY-01).
// v110: the Java extractor now emits `io_direct`/`performs_io` on methods that are
// genuine DB/network round-trips — every method of a @FeignClient interface or a
// Spring Data repository interface (extends JpaRepository/CrudRepository/…), any
// @Query/@Modifying/@Procedure method, and Room ops inside a @Dao interface. This
// populates the performance analyzer's isExpensiveJvmCall I/O index, which was empty on
// pure-Java Spring/JPA repos (v57 wired the prop for Kotlin only), so a per-iteration
// repository call now ranks a confirmed N+1 `high` instead of a keyword guess. The
// seed is deliberately type-level and never keys off a bare HTTP verb (@GET/@GetMapping),
// which on server-side Java is an inbound JAX-RS/Spring handler, not I/O; and there is
// no transitive fixpoint (performs_io == io_direct), matching the Kotlin design. Cached
// Java snapshots must re-extract (GAP-JV-02).
// v111: the Go extractor now emits `http_handler: true` on any func or method whose
// signature is exactly func(http.ResponseWriter, *http.Request) — net/http's handler
// contract, decidable from the AST. It exists so a route can be bound to the symbol that
// actually serves it: goextractor renders a route's `handler` prop from the REGISTRATION
// site, so it is the receiver VARIABLE chain ("h.fooHandler.DoThing")
// while the symbol is named by its receiver TYPE (".../foo.HandlerV2.DoThing").
// The two key spaces are disjoint — in a real service the handler props barely
// intersected the symbol names at all — so every consumer keyed on the handler (the
// enterprise route-handler escalation, orphans' handler rescue) matched nothing, and
// almost no routes carried a handled_by edge. The method name is all the two sides share, and
// it is ambiguous: a service can have several same-named methods (the handler, a service, a mock, and
// a null-object stub in the WIRING package where the routes are registered), so a
// name-only or package-scoped binder resolves the route to the stub — and a wrong
// handled_by edge feeds impact_analysis and find_path. The signature is the structural
// discriminator that rejects the stub by construction. Consumed by the new engine pass
// bindHTTPHandlers (post-extraction, outside the cache). Cached Go snapshots must
// re-extract (new/18).
// v112: the TypeScript extractor now emits `storage` facts for the three mainstream TS
// ORMs — a TypeORM `@Entity()` class (the extractor's first decorator support; it read
// none before), a Drizzle `pgTable("orders", …)` const, and every `model` in a Prisma
// `schema.prisma` (read off-glob, as package.json and tsconfig.json already are, since
// the schema is a separate DSL that tree-sitter never sees). Facts are named after the
// declaration with the physical table in a `table` prop, matching Kotlin/Room and
// Java/JPA. TypeScript was the ONLY backend language in enola that modelled no storage
// at all — Go, Java, Kotlin, Python and Ruby all do — so a database-backed Node service
// reported zero tables in explore, impact_analysis, llm_context and --explain.
//
// The same change adds the distinctive ORM query methods (findMany/findUnique/queryRaw/…)
// to the io_direct seed set, so computeTSPerformsIO propagates performs_io through a
// REPOSITORY WRAPPER. A direct in-loop `prisma.post.findMany()` was already caught by the
// analyzer's name list; a wrapper around it invoked no network primitive, so it was never
// io_direct and a per-iteration call to it was invisible. The generic verbs
// (find/save/update) stay OUT of the seed set: frontend TS reuses them for in-memory
// helpers and everything is exported, which is what flooded the high bucket with false
// N+1s and is why the TS detector was narrowed in the first place. Detection is gated on
// the package.json dependency, so a class coincidentally decorated @Entity in a non-ORM
// repo models nothing. Cached TypeScript snapshots must re-extract (GAP-XL-04, new/26).
// v113: Java rescues framework-loaded classes that carry no in-code caller and were
// reported as dead-code orphans. (1) SPI service-file fold — the extractor reads
// META-INF/services/* (JDK ServiceLoader) and META-INF/dubbo/** (Dubbo SPI) files,
// resolves each registered impl FQN to its in-repo canonical type, and emits a
// KindFileRef fact whose RelCalls edges reference those impls (external entries
// dropped) — so a Dubbo/JDK SPI extension registered only in a service file, with no
// @Activate annotation, is no longer a false orphan. (2) @RuleNode — a class annotated
// with a runtime classpath-scanned plugin annotation (ThingsBoard @RuleNode) now
// carries scanned_plugin=true (consumed by the enterprise dead-code detector as an
// entry point). Both are new/changed facts, so cached Java snapshots must re-extract
// (GAP-JV-08, new/60).
// v114: adds the Rust extractor — fn/struct/enum/trait/type/const/static symbols;
// impl/trait "implements" edges attached via a post-pass; use-based dependency facts
// classified internal/external/stdlib; cyclomatic complexity; Axum route facts; and
// RelCalls/RelInstantiates edges covering calls inside a macro invocation's token_tree
// (bail!/format!/matches! arguments, and an ordinary call embedded in any attribute
// macro, e.g. thiserror's #[error("{}", helper(x))]), a function passed by name as a
// value (call argument, struct field, &f, nested inside a macro argument like
// vec![Box::new(f)], or a local fn declared earlier in the same body), serde/clap/merge
// attribute strings and scoped paths (default, skip_serializing_if, value_parser,
// strategy), an unprefixed `use foo::bar;` where foo is a body-less `mod foo;` in the
// same file/mod scope (classified internal instead of external), calls/references made
// in macro content with no enclosing symbol (a macro_rules! template body, or an
// item-level macro invocation standing in for a whole function — recorded on a file_ref
// fact instead of dropped), a scoped associated-function call (Type::new(),
// some_mod::Type::from(x)), Type::Variant (bare value or match pattern), and a bare
// capitalized identifier used as a plain value (unit struct argument/let-binding) — the
// last three all recorded as RelInstantiates for Type, the dominant Rust construction
// idioms beyond a struct literal. Drop::drop and Future::poll are tagged override
// (compiler/runtime-invoked, never called by name), mirroring Kotlin/Swift's `override`
// handling. Rust is a new FileOwner, so its arrival also reshuffles which files count as
// "shared" for every other cached extractor's key in a mixed-language repo. Cached
// snapshots of any repo containing Rust files must re-extract.
// v115: Rust recognizes a #[test]/#[tokio::test]/#[wasm_bindgen_test] fn wherever it
// lives — a plain tests.rs file, an un-gated mod tests {} — not just inside a
// #[cfg(test)] module; it gets no symbol fact, and its calls into production code are
// credited via a file_ref/test_ref fact instead of counted as dead production code.
// Also records a function referenced bare inside an array literal (a `&[f, g]`
// dispatch table) as used, and resolves the schemars crate's
// #[schemars(schema_with = "fn")] attribute string (bare or crate::-qualified)
// like serde's default/skip_serializing_if. Fixes a latent bug where
// self.method() was silently dropped (no edge at all) whenever method wasn't
// a sibling of the immediately enclosing impl block — e.g. a type with
// several impl blocks, or a trait's own default method.
// v116: the C/C++ extractor now credits calls inside a detached top-level
// compound_statement — the body of a function defined by a name-carrying macro
// (SYSCALL_DEFINE0-6, COMPAT_SYSCALL_DEFINE*, and kin) that tree-sitter parses as a
// separate errored call_expression plus a loose `{ … }` block. Previously the body's
// calls had no owning function_definition and were dropped, so a static helper
// reached only from a syscall handler was mis-reported as dead code (find_orphans
// high-confidence false positive). The body's function-position identifiers are now
// hung off the module owner (same mechanism as a #define body); no symbol is emitted
// for the macro-defined handler itself (a syscall is dispatched by table, never
// called by name, so a handler symbol would be a fresh orphan FP). Cached C/C++
// snapshots must re-extract.
// v117: the Python extractor now walks function/class definitions nested inside
// function bodies, crediting their references (calls, decorators on nested defs,
// lazy imports, value/string refs) to the enclosing symbol — as lambdas always
// were. Previously walkForCalls hard-stopped at nested defs, which own no symbol,
// so every helper reached only from a closure was mis-reported dead (find_orphans
// high-confidence FP; the FastAPI router-factory pattern `def get_x_router():
// @router.post async def handler(): helper()` was the dominant bucket — ~80 of 326
// high-confidence orphans across the Python corpus). Metrics stay suppressed in
// nested scopes (a closure's loops are not the enclosing function's complexity and
// must not seed its N+1 candidates), and nested-scope bindings — including the
// nested def's own name, now also bound in the enclosing scope — shadow same-named
// module defs so bare calls cannot fabricate edges. Cached Python snapshots must
// re-extract.
// v118: the Python extractor's registration-decorator set now covers MCP servers —
// FastMCP @mcp.tool / @mcp.resource / @mcp.prompt / @mcp.custom_route, and bare
// re-exported wrappers (@tool, @prompt) that app cores build around FastMCP. These
// decorators register the function as a protocol handler the server dispatches per
// client request, so it has no in-code caller by construction; previously every
// MCP tool/resource/prompt/health-route handler was mis-reported as dead code
// (find_orphans high-confidence FP — the largest Python bucket, 18 of 234
// high-confidence orphans across the Python corpus). Matching functions get the
// existing registration self file-ref edge; functions carrying only wrapper
// decorators (e.g. @log_usage) are untouched, so genuinely unregistered handlers
// still flag. Cached Python snapshots must re-extract.
// v119: the Python extractor now registers imports inside module-level try/if
// compound statements — previously walkStatement had no case for them, so the
// try/except ImportError dual-import idiom, `if __name__ == "__main__":` imports,
// and `if TYPE_CHECKING:` imports registered no binding and emitted no dependency
// fact. Calls through the guarded names were then unresolvable (or fabricated a
// same-module target), so the callees read as dead (find_orphans high-confidence
// FPs — a Python MCP server's entire server-utils surface, ~13 of 103 across the
// Python corpus). Imports inside an except_clause register as fallbacks that never
// clobber the try-branch binding (the relative form resolves to a real path; the
// bare fallback would be dropped as external, deleting the edge). Separately, a
// module-level assignment whose RHS is a bare def name (the
// `click.echo = echo_to_stderr` monkeypatch / dispatch-table / alias-export
// idiom) now folds a value-ref to that def, so a function installed by assignment
// is not mis-flagged dead. Def/class definitions *guarded by a conditional* are
// deliberately NOT emitted as symbols: they are almost always intentional shims
// (a macOS-only fallback, a TYPE_CHECKING typing stub, an ImportError
// alternative) whose name is bound by a sibling branch, so a symbol for one is a
// dead-code false positive. Cached Python snapshots must re-extract.
// v120: the Svelte extractor now scans a SFC's template/markup for identifiers
// referenced only from there — event-handler attributes (on:click={fn},
// onclick={fn}), mustache expressions ({fn(x)}), and bind:/use: directives —
// folding them into a KindFileRef fact, mirroring the TS extractor's JSX file-ref
// pass. Previously extractSvelteSFC fed only the <script> block to the parser and
// discarded the template outright, so any handler/action/binding wired solely from
// markup had zero incoming edges and read as dead (find_orphans medium-confidence
// FP; ~136 of 229 medium orphans across the Svelte corpus). Cached Svelte snapshots
// must re-extract.
//
// v121: the Rust extractor now emits the loop/IO complexity metadata every other
// extractor already emits — loop_depth, scaling_loop_depth, loop_count,
// calls_in_loop, calls_in_scaling_loop (syntactic for/while/loop with a
// constant-trip-vs-scaling distinction), io_direct (direct filesystem/DB/HTTP
// primitives on the callee), performs_io (transitive fixpoint over the call
// graph), and recursive_self. Previously Rust symbols carried only cyclomatic, so
// the performance analyzer produced nothing for Rust regardless of nesting depth
// or I/O. Cached Rust snapshots must re-extract to gain the new props.
//
// v122: the PHP extractor now emits io_direct (direct filesystem/DB/HTTP primitives
// on the callee — file_get_contents/fopen/curl_exec/mysqli_*/wp_remote_*, and
// distinctive DB methods $wpdb->get_results/get_row/get_var/get_col, PDO/mysqli
// fetch_all/query/execute) and performs_io (transitive fixpoint over the call
// graph). Previously PHP emitted loop metadata but no I/O signal, so an in-loop
// call to a DB/HTTP wrapper was not recognized as an N+1. Cached PHP snapshots must
// re-extract to gain the new props.
//
// v123: the C++ extractor now emits io_direct (a narrow, unambiguous set of direct
// file/socket data-transfer primitives on a free/namespaced call — fopen/freopen/
// fread/fwrite/socket/recvfrom/sendto; console/logging fprintf and the ambiguous
// socket verbs bind/connect/send are deliberately excluded to avoid mass FPs) and
// performs_io (transitive fixpoint over the call graph). C++ <fstream> stream I/O
// via member calls is not detectable (dropped upstream). Previously C++ emitted
// loop metadata but no I/O signal, so an in-loop file/socket wrapper call was not
// recognized as an N+1. Cached C/C++ snapshots must re-extract to gain the new props.
//
// v124: the TS extractor now classifies SvelteKit's file/export-name-convention
// entry points as route_handler — `load` in +page.ts/+layout.ts/+page.server.ts/
// +layout.server.ts (under routes/), GET/POST/... in +server.ts (under routes/),
// and handle/handleError/handleFetch in hooks.server.ts — reusing the existing
// web_component=="route_handler" exclusion classifySymbol already applies to
// Next.js App Router handlers. detectSvelteKitRoute only ever ran for .svelte SFCs,
// so these plain-.ts framework entry points got no route fact and no symbol
// classification at all; their exports read as find_orphans false positives
// (11 of 80 remaining medium orphans across the Svelte corpus after v120). No
// enterprise-side change: the route_handler consumer already existed. Cached
// TypeScript/Svelte snapshots must re-extract.
//
// v125: the Go route extractor now composes gorilla/mux (and chi) subrouter
// PathPrefix mounts INTERPROCEDURALLY. Previously it composed prefixes only within
// a single function, so a real backend that creates `apiRouter :=
// router.PathPrefix("/api").Subrouter()` and passes it into a per-file/per-package
// registration function (`func RegisterX(r *mux.Router){ r.HandleFunc("/courses",…) }`)
// stored the bare leaf "/courses" instead of the true runtime "/api/courses". A
// module-wide fixpoint (buildRoutePrefixIndex) now propagates the prefix a router
// argument carries at each call site — including receiver-method registrations
// (`h.RegisterRoutes(sub)`, `app.Handler.RegisterRoutes(sub)`), resolved via the
// shared resolveChain type resolver — to the callee's router parameter, seeding
// extractRoutes so routes are stored at their full mounted path. This corrects
// cross-repo client↔route matching (unused-routes false positives) and route
// facts consumed by impact_analysis/traverse. Cached Go snapshots must re-extract.
//
// v126: three Go-extractor accuracy fixes. (a) Collection-root routes registered
// as `subrouter.HandleFunc("", h)` are no longer dropped by the empty-path guard
// that fired before subrouter-prefix composition — an empty STRING LITERAL is now
// composed to the subrouter prefix (e.g. "/api/settings/courses"), while a dynamic
// (non-literal) path arg still drops; same relaxation in chi/net-http. (b) A struct
// used as a composite literal (`T{}`/`&T{}`) or as another struct's named field
// type now emits a RelInstantiates edge (internal types only), so a struct used
// only as a value/field is no longer a dead-code false positive — matching the
// Swift/Rust convention. (c) scaling_loop_depth now discounts HIERARCHICAL nested
// loops: a loop whose ranged collection is reached through an enclosing loop
// variable (`range pkg.Files`, `range pkgs[i]…`) visits each element once and adds
// no factor of n, so a tree/AST walk no longer reads as O(n²+); all-pairs over an
// independent/same collection still counts. Cached Go snapshots must re-extract.
//
// v127: two Rails route-extractor accuracy fixes. (a) `resources :x, shallow: true`
// nested under a parent resource now emits its MEMBER routes (show/edit/update/
// destroy) at the shallow path — the parent segment and its :parent_id are dropped
// (e.g. /api/v3/replies/:id, not /api/v3/posts/:post_id/replies/:id) — while
// collection routes (index/create/new) stay nested, mirroring Rails. (b) An optional
// route segment `foo(/:bar)` now expands to both concrete paths it serves
// (/foo and /foo/:bar) instead of one literal that matched nothing. (c) The
// hash-rocket route form `get 'path' => 'ctrl#action'` (path as the string hash
// key, not a `to:` keyword) is now extracted — previously the path was skipped
// entirely and the route was never emitted. Cached Ruby snapshots must re-extract.
// v128: four TS HTTP-client extractor precision fixes that cut false cross-repo
// "unresolved edges" from a frontend. (a) The fetch()/makeRequest() matcher gained
// a left word-boundary, so a call whose name merely ENDS in "fetch"
// (router.prefetch(...), query.refetch(...) — navigation/cache primitives) is no
// longer captured as an outbound call. (b) An options-object `url:` is a request
// only when the object carries a real HTTP verb or a request-payload key; a Next.js
// SEO block (openGraph { url, type:'website', siteName }) / JSON-LD, whose only
// signal is a non-verb type:, is no longer mis-read as a call. (c) cleanTSPath now
// requires a leading "/", dropping non-path literals (a lone ",", a fragment of an
// analysis script scanning for "fetch(") that became phantom routes. (d) cleanTSPath
// strips a query-string placeholder fused to the final segment (`${queryParams}` →
// ".../role-distribution{}" → ".../role-distribution"), which a real `?` inside the
// interpolated variable hid from the query strip; own-segment "/{}" path params are
// untouched. Cached TypeScript snapshots must re-extract.
// v129: the TS http-client extractor now resolves a file-local base-URL literal
// interpolated at the head of a call path. A client call written as
// `${this.basePath}/calculate`, where `basePath = '/api/settings/pricing'` is a
// const/class-field/ctor-default "/"-rooted string literal in the same file, is
// reconstructed to its full path ("/api/settings/pricing/calculate") instead of
// collapsing to the single-segment suffix "/calculate" that the cross-repo matcher
// skips — so these real calls resolve to their server route. An identifier bound to
// two different literals in the file is ambiguous and left unstripped; an
// injected/env/absolute base (not a "/"-rooted literal) still falls back to the
// suffix, as before. Cached TypeScript snapshots must re-extract.
// v130: the Rust/Axum route extractor now composes `.nest(prefix, module::router())`
// mount prefixes INTERPROCEDURALLY. A router built by `pub fn router() -> Router` in
// one file (registering bare in-router paths like "/status", "/{id}/data") mounted
// by a parent via `.nest("/api/v1/datasets", routers::datasets::router())` in another
// file is now stored at its true runtime path ("/api/v1/datasets/status"). A crate-
// wide fixpoint resolves each nest to its callee builder (by fn name + module/file
// stem, same-crate preferred) and propagates prefixes from root builders; a router
// mounted at several prefixes emits each route once per prefix, and a root or
// unresolvable mount keeps the bare path so composition never drops a route — the
// Axum analog of the Go gorilla/mux+chi subrouter composition (v125). This fixes
// cross-repo client↔route matching (unused-routes / coverage FPs) and the route
// facts consumed by impact_analysis/traverse. Cached Rust snapshots must re-extract.
// v131: the Swift extractor now sub-divides an XcodeGen application/app-extension
// target into per-directory packages (Go/Ruby-style leaf-directory modules) instead
// of collapsing the whole target to a single module, and runs the type-reference pass
// INTRA-target to synthesize the directory→directory coupling edges that files within
// one Swift module never express via `import`. Framework and SPM targets are kept
// whole (they are import units addressed by name); test bundles still collapse per
// bundle. A monolithic iOS app target now surfaces its internal structure in
// package_metrics (per-directory Ca/Ce/abstractness) instead of appearing as one
// 400+-type package. Cached Swift snapshots must re-extract.
// v132: the Go extractor emits a topic-reference fact (KindStorage,
// storage_kind=topic, messaging=kafka) for the two ways a Kafka topic string reaches
// Go code without a literal at the call site — an envconfig `default:` tag on a
// *Topic-suffixed config field, and env.Get("<KEY>_TOPIC", "<default>"). Both anchor
// on an explicit topic marker, so an in-process event bus (whose Subscribe takes a Go
// event symbol, not a topic string) emits nothing by construction. The cross-repo
// linker gained a matching signal that binds consumer -> producer by the topic name's
// owning-service prefix (the same leading-segment-to-repo resolution the import linker
// uses), making asynchronous coupling visible where no import, call, or route exists;
// a repo's own topic and a topic owned by no loaded repo draw no edge. Go does not
// participate in the extractor cache, so this bump documents the behavior change
// rather than invalidating anything.
// v133: Python FastAPI routers are no longer invisible. Route decorators on a def
// nested in a function body are extracted (the router-factory idiom `def
// get_x_router(): router = APIRouter(); @router.post("/") ...`, which module-level
// statement walking never reached — v117 fixed only its dead-code attribution), and
// a repo-wide fixpoint folds include_router mount prefixes onto the bare decorator
// path, so a route is stored at the path it actually serves ("/api/v1/cognify", not
// "/"). Mirrors the Go (v125) and Axum (v130) prefix composition, keyed by the
// decorator's receiver rather than by line span; an unresolved or ambiguous mount
// keeps the bare path, and Flask Blueprint url_prefix stays unfolded (GAP-PY-06).
// TypeScript module facts gained package_name (the nearest package.json name), which
// the cross-repo import linker reads to recognize a repo's own npm @scope.
// v134: the TypeScript HTTP-client extractor reads a call's verb from that call's
// OWN options-object literal (brace-matched from the second argument) instead of a
// flat 200-byte window scanned forward from the URL. The window let a later call's
// `method:` bleed backwards, so a plain fetch("/a/b") sitting above a POST was
// emitted as POST — a wrong verb on a real path, which the cross-repo linker then
// matches against the wrong server route or fails to match at all. Options passed
// as a variable carry no readable verb and now fall back to GET rather than
// adopting an unrelated one. Mirrors how Pass 3 already scopes its scan with
// enclosingObject.
// v135: "**/testdata/**" joins the default ignore globs, and the two extractors
// that walk the repo themselves rather than consuming the engine's filtered file
// list — OpenAPI (Extract) and gRPC (Detect) — repeat the exclusion in their own
// skipDir, which the globs cannot reach. Go reserves testdata/ for fixtures, and
// a fixture repo is a whole miniature codebase: enola's own testdata/repos/**
// supplied every one of its detected outbound HTTP call sites, manufacturing a
// cross-repo coverage gap for a service that makes no outbound calls at all.
// Fixture-driven tests are unaffected — they root each scan INSIDE the fixture,
// so the scanned-relative paths contain no testdata/ segment to match.
// v136: the Python extractor implements plugin.TestRefExtractor, so a production
// symbol exercised only by a pytest file is no longer mis-reported as dead — on a
// real corpus ~58% of Python symbols with no incoming call edge were imported or
// called from tests. plugin.TestRefExtractor now also receives the production file
// list: Python spells an absolute-import target as a dotted path ("pkg.mod.func")
// and can only rewrite it to a canonical symbol name by checking that path against
// the set of files that exist, which the engine alone knows post-ignore-globs.
// The pass emits ONLY KindTestRef facts — never symbols, modules or routes — so
// re-reading files the ignore globs exclude cannot put a pytest fixture's
// include_router prefix back into the production route graph (the v135 regression).
// The global symbol index is deliberately not rebuilt (Extract's Pass 1 is the
// expensive half of Python extraction), so receiver-typed method calls go
// unresolved; an unresolved target is dropped, never guessed, leaving its symbol
// the dead-code candidate it already was.
// v137: Python call targets imported through a package __init__.py re-export now
// resolve to the module that DEFINES the symbol instead of dangling as a dotted
// string. A package is a directory, so "from pkg.sub import thing" left a target
// "pkg.sub.thing" whose prefix matched no module file (the set holds
// pkg/sub/__init__ and pkg/sub/thing, never pkg/sub) — 674 such targets carrying
// 4,660 call edges in one corpus, including every router factory its API
// composition root mounts, so find_path from that file reached none of them. The
// walker already recorded the data (Props["reexports"] plus the import target);
// resolveCallTargets now indexes it, keyed by full package dir so a lookup is
// exact. Exact module resolution still runs first, making this purely additive; a
// name re-exported from two modules in one package stays dotted rather than bind
// arbitrarily. This is a GRAPH fix, not a dead-code one — the orphans explainer's
// short-name matching already treated these symbols as used.
// v138: two more Python call-target resolution fixes, both about targets that were
// wrong or retained rather than merely unresolved.
//
// (a) Suffix matching and the internal-root gate accepted ANY directory name at
// any depth, so an internal dir sharing a name with a third-party package captured
// its imports — a repo with ".../databases/relational/sqlalchemy" and
// "cognee/alembic" resolved plain `import sqlalchemy` internally, marking those
// dependencies source:internal and keeping ~500 third-party call edges as
// first-party. Both now apply Python's actual rule: a directory is a top-level
// package only if its parent is not itself one (no __init__.py). With no package
// information the rule cannot fire, so callers that do not track __init__.py keep
// the historical permissive behaviour.
//
// (b) resolveAbsolute drops trailing segments on failure, which is right for an
// import but wrong for a call target: "pkg.mod.Cls.method" resolved to pkg/mod and
// then kept only the last segment, yielding "pkg/mod.method" — not a dangling
// target but a WRONG one, pointing at whatever else bore that name. Resolution now
// walks the module/symbol split point leftwards with an EXACT module lookup, so the
// class segment moves into the symbol part. A multi-segment symbol part is only
// accepted if it names a symbol the snapshot actually has; unconfirmed candidates
// stay dotted rather than minting a plausible-but-wrong edge.
// v139: two new signals that let the dead-code detector drop findings it can never
// act on. Facts from a file carrying a codegen banner ("DO NOT EDIT", "generated
// by", "@generated" — matched against the file head, so it works for any generator
// and any language) gain generated=true; a generator emits a whole API surface
// regardless of how much the project calls, so its unreferenced half is guaranteed
// noise. Python functions registered with a framework by decorator (FastAPI
// @app.exception_handler/@app.middleware/@app.on_event/@router.websocket, Modal
// @app.local_entrypoint and — gated on the file importing modal, since the names
// alone are far too generic — @app.function/@app.cls) gain
// framework_registered=true, the same treatment click/Typer commands already got.
// Facts are flagged, never dropped: callers of generated code must still resolve,
// so only the dead-code judgement changes.
// v140: two Python reference gaps, each making live code read as dead.
//
// importableRoots stopped scanning a path at the first package parent, but a
// directory with no __init__.py BREAKS the package chain and starts a new source
// root, so a segment below a package can itself be importable. A non-package
// directory nested in a package puts its own siblings on sys.path, and
// "from corpus import read_source" between two files there is a real internal
// import; classifying those external made a whole analysis tree read as dead. Now
// judged per position, matching buildSuffixIndex — which already did — so a
// like-named third-party directory whose parent IS a package stays excluded (v138).
//
// A function handed to a decorator as a VALUE (@override_run_tasks(run_tasks),
// @register(handler)) is a real use: the decorator stores it and the framework
// invokes it later. The decorator-argument walk looked only for nested CALLS, so a
// bare identifier slipped past and the referenced function had no incoming edge.
// v141: two TypeScript HTTP-client detection gaps, both of which made outbound
// calls vanish from the graph entirely — not counted detected, external OR
// unresolved, so the cross-repo residual under-reported with no sign it had.
//
// The optional type-argument group in every client-call pattern was "<[^>]*>",
// which cannot span a NESTED type argument: on fetch<ApiResponse<Foo>>(…) the inner
// class stops at the first ">", the following "\s*\(" meets a ">" and fails, and
// (RE2 having no recursion) backtracking to the empty alternative then meets "<".
// One level of generics matched, two silently did not — and this hit the paths the
// extractor advertises, openapi-fetch's API.GET<ApiResponse<T>>(…) among them. The
// group is now bounded on "(", which a type argument never contains.
//
// Lowercase verb calls — axios.get('/path'), http.post('/path'), the dominant
// hand-written idiom — matched nothing at all. They were excluded to avoid
// colliding with map.get()/cache.delete(), which is a real hazard; the new
// lowerVerbCall pays for admitting them by requiring a "/"-rooted literal argument,
// a condition cleanTSPath already enforces downstream, so no collection lookup can
// reach it. A lowercase call on an interpolated template (`${base}/x`) is still
// deliberately not matched.
//
// Admitting lowercase verbs immediately required a guard it did not need before:
// a supertest call in an e2e suite — request(app).get('/v2/me') — is byte-identical
// in shape to production axios traffic. Client-route extraction is therefore now
// gated on !facts.IsTestPath, because a test's HTTP traffic is not an architectural
// dependency, and facts.IsTestPath learned the two e2e conventions its .spec/.test
// suffixes could never match (".e2e-spec.ts", Nest's generated form, and ".e2e.ts",
// Playwright's — the hyphen means neither carries the leading dot those entries
// require). Ungated, a NestJS API's own test suite became 500+ client routes,
// promoted the service from isolated to connected, and fabricated a cross-repo
// dependency edge out of test traffic — the paths matched a real server because
// they are the routes under test. Same principle as v-era GAP-XL-15, which keeps
// test_ref facts out of the coupling graph.
// v142: TypeScript gains its first SERVER-side route DSL. Until now every `role` the
// extractor wrote was "client" — server routes existed only for file-based routers
// (Next.js, Nuxt, SvelteKit) — so a decorator-routed backend contributed zero routes,
// every client call against it fell into the unresolved residual, and the backend was
// classified `isolated`, i.e. a leaf.
//
// A class carrying @Controller (NestJS) or @controller (InversifyJS) now emits one
// server route per verb-decorated method, composing the class base path with the
// method sub-path through facts.JoinRoutePath. Both argument forms are read —
// @Controller("/users") and @Controller({path: "/users"}), the latter being the form
// real NestJS code overwhelmingly uses. Emission is gated on the controller
// decorator, so a generic @Get on an ordinary class mints nothing, and the two
// frameworks' verb vocabularies are kept separate so a class cannot mix them.
//
// Two things are deliberately NOT composed into the path, because the decorator does
// not determine them: a `version:` property (NestJS versioning may be header- or
// media-type-based) and the application's global prefix (routinely read from the
// environment). The linker's >=2-segment suffix match resolves the difference.
//
// Gated on facts.IsTestPath like v141's client side: an e2e fixture's controller
// would otherwise mint server routes no production client calls, which is a false
// unused-route finding rather than a discovery.
// v143: the other half of TypeScript's server side — routes registered by CALL
// rather than by decorator. `<recv>.<verb>('/path', handler)` is the shape Express,
// Fastify, Hono and Koa/Oak all share, so one pass covers the family, and both the
// ESM and CommonJS binding forms are read (`const app = require('express')()` is as
// common as the import form, and matching only the latter found zero routes on the
// one real Express server available to measure against).
//
// The shape is also v141's CLIENT shape — axios.get('/x') and router.get('/x') are
// the same text — so the two passes are separated by RECEIVER BINDING, resolved per
// file. A receiver bound to an app or router registers routes; anything else stays a
// client call, unchanged. The client pass skips known server receivers so a single
// call site cannot be emitted twice, once in each direction; measured on the corpus,
// that also corrected two registrations v141 had been reporting as outbound calls.
//
// A sub-router with no visible mount emits NOTHING. Its paths are fragments —
// router.post('/login') in a module mounted at '/webhooks' elsewhere serves
// '/webhooks/login' — so emitting '/login' would be a wrong fact, and a wrong path can
// false-match another repo's route, which is worse than silence. Mounts declared in
// the same file are composed; cross-file mount resolution needs a repo-wide pass and
// is deliberately not attempted. Bare catch-alls (app.get('*')) are skipped for the
// same reason: a SPA fallback is not an endpoint and would match any client path.
// v144: two annotation/chain forms that real code uses and the extractors did not
// read, each found by adding a production repository to the benchmark corpus rather
// than by inspection — which is the point of having one.
//
// Kotlin/Retrofit: `@GET(value = "topics")`. Kotlin permits any single-argument
// annotation to be written with its argument named, and Google's reference Android
// app writes EVERY endpoint that way. Matching only the positional form yielded zero
// client routes there, and a Retrofit interface with no routes contributes no
// mobile-to-backend edges at all — so "which screens break if I change this
// endpoint" answered nothing, silently, on an entire class of Android codebase.
//
// Rust/Axum: `.route("/x", get(handler).layer(mw))`. A non-verb method in the
// MethodRouter chain was treated as a terminator, so per-route middleware — which is
// idiomatic Axum — discarded the verbs beneath it and dropped the route entirely,
// with `get` sitting in plain sight. Non-verb methods are now transparent: the walk
// recurses past them and keeps the chain below. Only ever applied to the second
// argument of `.route(path, …)`, which is a MethodRouter by construction, so any
// method on it is a wrapper around one.
//
// Unchanged, and still deliberate: `.route(path, handler_var)` emits nothing. There
// is no verb to infer, and inventing one would produce a route that could false-match
// another repository's endpoint — worse than the visible gap.
// v145: TypeScript alias resolution picked among OVERLAPPING aliases by Go map
// iteration order, taking the first match rather than the most specific one.
//
// Wrong twice over. tsconfig `paths` resolution is most-specific-first, so a project
// mapping both "@acme/schema" and "@acme/" means the former for "@acme/schema/x" —
// the old code could resolve it to either. And because map iteration is randomized,
// "either" meant a DIFFERENT answer on different runs of the same unchanged tree.
//
// Measured on a 15k-file monorepo that maps one package both to its source and to its
// built output: 2 facts of 163,582 flipped between runs — enough that three
// consecutive `enola check` runs on an untouched tree reported `edges +1/-1`, then
// `+6/-6`, then clean. It was the only repository of 38 that failed byte-level
// reproducibility, and the failure is the expensive kind: a delta tool that invents
// churn is worse than one that is merely incomplete, because invented churn is
// indistinguishable from a real change.
//
// Longest matching prefix now wins, ties broken on the prefix string so the result is
// a total order rather than a less-arbitrary one.
// v146: package.json is now read DIRECTLY FROM DISK by the TypeScript extractor rather
// than from the engine's ignore-glob-filtered file list, so a config that drops JSON as
// data noise can no longer disable the module `package_name` prop.
//
// The bundled mcp-arch.yaml ignores "**/*.json" — a reasonable thing to write, and the
// only config most snapshots actually run under. Under it, collectPackageNames saw no
// package.json at all, so NO module carried package_name, so the cross-repo linker's
// own-@scope guard could not fire. A repo importing a sibling package it publishes
// itself ("@acme/native-darwin-arm64" from the repo that publishes "@acme/sdk") was
// reported as depending on whatever other repo happened to be labelled "acme".
//
// Observed on a real pair: a Rust repo publishing @scope-prefixed platform packages drew
// a fabricated import edge to the Python repo of the same name — an edge pointing the
// wrong way down a dependency that does not exist.
//
// Nothing caught it in either direction. No error was raised, and the golden fixtures
// kept passing because TestGolden builds its engine from config.Default(), which has no
// such glob — so the fixture and the shipped config had been disagreeing for as long as
// the guard existed. This is the same principle the OpenAPI and Symfony-config
// extractors already apply: the globs exist to suppress config/data noise, not to hide
// architecturally meaningful files.
//
// Because the globs no longer apply to this read, the directories that must not be
// descended into are named in the extractor instead: tsSkipDirs (shared with the
// alias-root walk), dot-directories, and testdata — node_modules being the critical one,
// since a dependency's package.json would otherwise read as one the repo publishes.
// v147: the Go extractor no longer reads ENGLISH PROSE as SQL. `reSelectFrom` matched a
// bare `FROM <word>` anywhere in any string literal, and "from the", "from what", "from
// one" and "from in" are ordinary English — so a help string or an error message produced a
// storage fact for a table named `the`, `what`, `one` or `in`. It now requires the literal
// to contain a SELECT before a bare FROM is trusted.
//
// Found four separate times while building an unrelated feature, each time as an
// unexplained `storage: +N` in a diff of a change that touched no storage — which is how it
// became clear this was a defect rather than a curiosity. Every instance came from this
// repository's own text; the phrases are quoted in selectGate.
//
// It is worth a version bump for what it is rather than for how many facts it moves. A
// false FINDING is an opinion a reader weighs and can discard. A false FACT is part of the
// layer every finding, every edge and every diff is computed from, and the graph's central
// claim is that its contents are derived rather than guessed. A regex reading prose as SQL
// is guessing; it is simply a regex doing it rather than a model.
//
// Four rules, each measured against Grafana's 6,184 Go files rather than argued for:
//
//  1. A bare FROM is trusted only when the literal also contains SELECT. Not a clause-word
//     list — WHERE, ORDER, GROUP and LIMIT are ordinary English too, and that variant was
//     measured to admit 844 prose literals, which is worse than the bug.
//  2. JOINed tables are now read, but only inside the FROM CLAUSE. Reading them literal-wide
//     reported a table called `time` from a JSON schema whose SELECT was 50 KB away.
//  3. Literals over 16 KB are not searched. 403 real SELECT+FROM literals in Grafana: median
//     65 B, p99 2.9 KB, largest 6.4 KB, none over 8 KB. A document contains every keyword
//     eventually, so only a bound helps.
//  4. `-- …` comments are stripped first: a comment inside a query is prose surrounded by
//     SQL, and removing it is reading the language correctly rather than guessing.
//
// Net on Grafana: 419 storage facts over 108 distinct names that read as a real schema,
// where the previous code produced `IF`, `as`, `by`, `time` and `was` AND missed five real
// tables in one Postgres catalog query. Residual, measured: ~1% still come from English
// genuinely shaped like SQL ("unable to create table response"), which no rule separates
// without also losing lowercase SQL.
// v148: the TypeScript extractor gains Ember/Glimmer support. Template-tag files
// (.gts/.gjs) are parsed by blanking their <template> blocks in place — newlines
// preserved, so every fact's Line is true to the original file — and their
// template references resolve through the file's own import bindings (strict
// mode makes that exact: an identifier a template renders is either imported or
// local). Classic .hbs templates emit a file_ref carrier with their invocation
// names; the new ember-resolver binder joins those, and @service injections, to
// the declared symbols after extraction, skipping anything ambiguous. Router.map
// declarations become page routes with parent paths composed, and ember-data
// model classes gain a storage companion — the same modelling Nuxt/SvelteKit
// routes and ActiveRecord models already get. Before this, a .gts/.gjs/.hbs file
// produced no facts at all: an Ember app's component architecture was invisible
// while its plain-.ts half made the graph look populated.
// v149: Ember routes become first-class graph citizens, and Ember's test tree
// stops reading as production code. Router.map composition honors resetNamespace
// (the route NAME restarts at the segment while the URL path keeps nesting — on
// one production router this was the difference between 12 and 406 of 510
// routes binding); each router-map route gains a handled_by edge to the route
// class its dot-name resolves to; `<LinkTo @route=…>` names and literal
// `transitionTo`/`replaceWith` arguments become navigation edges to route
// facts, with the implicit `.index` child resolving to its parent. Page-type UI
// routes (Ember, Nuxt, SvelteKit, Next pages, Vue router configs) are excluded
// from cross-repo HTTP server indexing — a browser navigation URL is not a
// served endpoint, and indexing one manufactured false unused-route findings.
// tests/**/*-test.{js,ts,gjs,gts} joins the default ignore + test globs: the
// hyphenated suffix is ember-cli's reserved convention INSIDE tests/, and
// without the entry an app's acceptance suite indexed as production symbols
// (the directory is demanded because a bare *-test.ts also swallows an
// experimentation util named ab-test.ts — the Ruby _test.rb precedent).
// v150: the ember-complete-coverage epic lands in one pass. Container-resolved
// role classes (adapters, serializers, transforms, initializers, routes,
// controllers) gain framework_registered so live singletons stop reading as
// dead code — the Python v139 precedent applied to Ember's container.
// @attr('type') binds models to their app-defined transforms; literal
// {{component "x"}}/(helper …)/(modifier …) forms resolve as typed
// invocations; loading/error substate and index templates find their parent
// routes. V1 addon publishes resolve by chasing the recorded re-export stubs
// (which also resolves barrels everywhere); engines compose this.mount with
// their buildRoutes maps when the mount is unique, and engine templates
// resolve in their own isolated tree. Contextual components join two recorded
// literals — a yield-hash entry and a block-param consumption. Pods and the
// classic templates/components split join the candidate fragments (layouts
// are fragments, never a mode). File-local single-assignment string constants
// fold into name arguments (derivation, not inference), and irreducibly
// dynamic sites are counted with capped samples — visible, never guessed.
// v151: the deterministic-coverage epic, part 1 — GraphQL contracts, React
// Navigation, Sequel. graphql-ruby root-type `field` declarations and client
// gql-tagged/.graphql operations emit route facts named by root field
// (`Query.pageViews`, type=graphql), joined cross-repo by a new directional
// signal on the exact name — the HTTP linker's shape for GraphQL — while
// staying out of HTTP path matching the way gRPC does; schema COPIES on the
// client side (type-definition blocks, codegen inputs) deliberately emit
// nothing. React Navigation screen registrations become page routes handled_by
// their imported components, and literal navigate()/push() calls become
// navigation edges from the enclosing symbol (the Ember router mechanism,
// text-scanned because React Native writes JSX in .js). Sequel::Model classes
// emit the ActiveRecord companion storage shape, the literal dataset argument
// winning as the physical table. Part 2, same version (unreleased): two new
// extractors. hcl reads Terraform with an exact block scanner — blocks become
// symbols addressed as Terraform addresses them, references (prefixed,
// declared-set bare addresses, depends_on lists) become depends_on edges, and
// a module block's literal local source draws the directory dependency, remote
// sources marked external. ansible walks the repository itself (YAML is
// ignore-globbed — the OpenAPI self-walk precedent): plays depend on the roles
// they list, import_role/include_role literal names draw role-to-role edges,
// and .j2 templates are counted without ever being rendered.
// v152: the deterministic-coverage epic, part 3 — the seams the estate probe
// found dark. GraphQL operation documents activate the TypeScript extractor on
// their own (a Swift/Kotlin Apollo repo carries .graphql documents and no
// package.json; the machinery no-ops without TS files and schema copies stay
// inert). Ruby operation-string literals — a quoted literal or heredoc body
// opening with an operation head — emit client-role graphql routes through the
// shared gqlscan grammar, so a Rails service calling a sibling's GraphQL API
// joins the same cross-repo signal as a JS client; Ruby's own block syntax
// (`query { |x| … }`) can never match, and graphql/ trees plus root-type files
// are excluded as server side. Append mode discards a prior state whose
// extractor_version differs from this constant instead of carrying it: the
// retroactive-tagging migration would bulk-claim a stale cross-version union
// under one repo's label, manufacturing facts no repo's source states.
// v153: literal derivation folding. One shared helper (litfold) owns the
// bounded derivation set — a file-local single-assignment constant (a name
// assigned twice folds nothing), a wrapper call's single "/"-rooted string
// argument, and an interpolation-headed template whose tail is a "/"-rooted
// literal. Applied at the HTTP-client scans: fetch/makeRequest with a bare
// identifier argument resolves through the single-assignment store; Ruby
// client calls admit connection.post(build_url("/pageview")); lowercase verb
// calls admit `${base}/path` templates (the base-URL half formerly recorded
// as GAP-TS-06), with cleanTSPath resolving or stripping the base as before.
// Folded routes carry a descriptive `derived` prop naming the form. One step
// only, never evaluation; derived and inline literals join identically.
// v154: the Sequel dataset form actually extracts. `class X <
// Sequel::Model(:customers)` parses as a CALL-form superclass, which the
// superclass reader dropped entirely — so the paren form emitted no storage
// fact, no superclass prop, and no implements edge, and the ruby_sample
// golden pinned the miss while the helper's unit test (fed the string
// directly) passed. The reader now returns call-form superclasses whole;
// consumers strip the arguments wherever the base name alone is meant, so
// implements targets stay clean while sequelModelBase finally receives the
// literal table.
// v155: intent compilation. Declared architectural intent (enola-intent.yaml
// per repo, or a cluster config's intent: block with wholesale reported
// override) compiles into KindIntent facts inside the extraction window, so
// snapshots carry declarations, diffs track them, and receipts fingerprint
// them. A standard explainer named `intent` verdicts declared consumes
// against measured cross-repo edges — unexpected-seam and mis-via at
// confidence 1.0 (set difference between stated and measured), missing-seam
// capped at 0.8 (absence may be an extraction miss), and an override notice
// so a cluster-over-file override is never only in a log; repos without
// declarations are unasked, and a declared seam without its counterparty in
// the graph is skipped, never failed. The layers explainer builds declared
// patterns per declaring repo at confidence 1.0 (outermost-first order,
// exact-or-`prefix/**` globs) and runs its existing violation machinery over
// them; heuristics keep every undeclared repo. Via kinds are contract
// constants the signals and the intent parser share — shared_symbols
// included, which the first vocabulary pass missed and the estate corpus
// found by failing to declare a measured seam. Two matching refinements the
// same shakedown earned: a stripped interpolated base carries a target hint
// (its trailing identifier — `${config.BACKEND_HOST}/mcp` hints "backend"),
// and a single-segment path accepts a hint whose normalized form EQUALS a
// provider label as disambiguation — the source names the host, and two
// loaded repos serving the same short path is exactly what the hint is for.
// Substring hints stay rejected there.
// v156: the unresolved residue triaged into named classes. Bare verbs inside a
// plural resources block nest under the parent member id (Rails serves
// `resources :steps do get :status end` at /steps/:step_id/status); explicit
// on: :collection stays flat. A GraphQL self-match — a frontend querying its
// own repo's schema — counts resolved coverage without drawing an edge, and
// the unmatched-route pass skips GraphQL operations entirely (they are the
// graphql signal's domain). A failed HTTP match whose host-derived target
// hint resolves to no loaded repo classifies external (the source names where
// the call goes, and it is not here); and a repo whose intent declaration
// names exactly one http-client seam gets its remaining unmatched calls
// attributed there — a new `declared` coverage bucket and an
// attributed_by_intent reason, labeled, never resolved into an edge.
// v157: the wiki is a source tree. A markdown page opting in with an
// enola_intent: frontmatter key (namespaced so a wiki's own toolchain ignores
// it) compiles directly into intent facts — seams and layers with an explicit
// intent_owner (the page lives where the decision lives, not inside the repo
// it governs), and CLAIMS: measurable statements (fact-count, seam) the
// intent explainer verdicts every snapshot, failures proof-class. Fact
// provenance is the page itself, so a verdict's evidence cites the decision
// that declared the intent. The body is prose and is never parsed; an
// invalid block fails the snapshot.
// v158: the whole wiki compiles. An enola_intent block gains a `page:`
// declaration — type, status, scope, affects, and typed relations to other
// pages (depends-on, supersedes, superseded-by, part-of, relates-to) — so
// every decision, spec and reference becomes a knowledge node with edges,
// not just the pages that declare seams. The intent explainer verdicts
// dangling relations at capped confidence (a missing target may be deleted
// or merely not opted in), and a store carrying only page intent no longer
// early-returns before claim and relation verdicts run.
// v159: Typhoeus joins the Ruby client matchers — direct verb calls, and a
// request-wrapper derivation for the sink-behind-a-private-method shape
// (`Typhoeus::Request.new("#{base}#{path}", method: :get)` inside
// `def make_request(path:, …)`, rooted path literals threaded from same-file
// call sites; single-sink rule, literal verb required, env-var base hints the
// provider). And `object-storage` joins the via vocabulary: a bucket-mediated
// export/import handoff is declarable intent even though no linker measures
// it yet.
// v160: page declarations gain origin — a closed channel vocabulary (slack,
// langfuse, notion, github, web, repo, other) naming WHERE the declared
// knowledge came from, compiled onto the page node's props so receipts
// fingerprint provenance and any explainer can key on it. An unknown channel
// fails the snapshot with the vocabulary named.
// v161: page declarations gain anchors — repo + repo-relative path pinning a
// page to the code it is about, compiled as anchor intent facts and joined
// against the measured graph (exact file or directory prefix, in both the
// label-prefixed and repo-relative file forms). A miss is a dangling-anchor
// finding at capped confidence only when the repo measures files of that
// kind; an absent repo, or a file kind no measured fact carries (READMEs,
// manifests, docs), is unasked — a 60-repo regression run showed the
// conflation flooding wikis with false dangling findings. Scope/affects
// stay unverdicted props by design: they speak the wiki's vocabulary, and
// the wiki-to-cluster label mapping is the deriving toolchain's side of the
// boundary.
// v162: a hint must name a provider. tsBaseHint now derives nothing from a base
// identifier carrying no URL/host suffix, and nothing at all from a token that
// is not a bare identifier — `${base}`, `${url}`, `${getRootUrl()}` and
// `${Cypress.env('baseUrl')}` named no provider, resolved to no loaded repo,
// and so filed real internal blind spots as expected third-party calls: on a
// three-repo estate 7 of 15 unresolved call sites vanished into `external` and
// a coverage-gap finding stopped being reported. Same rule the Ruby side's
// stripURLVarSuffix already applied, for the same reason. The single-segment
// carve-out now reads target_hint alone rather than serviceHint, whose `api`
// fallback is the client FILE's name: with two repos serving one short path, a
// file named api.ts elected the repo named api, and renaming the file moved the
// dependency.
// v163: exported package-level Go consts and vars emit symbols. A const-only
// file — a shared vocabulary, a sentinel-error set — was parsed yet invisible:
// no fact carried its path, so an anchor to it verdicted dangling as "eluded
// extraction" when the honest answer was that the extractor skipped every
// ValueSpec by construction. Exported-only, package-level only: private
// bindings and function-local declarations are implementation detail, and
// emitting them would drown the symbol set.
// v164: C# is extracted. Types (class/interface/struct/record/enum/delegate),
// their members, `using` directives, base lists, constructor and primary-
// constructor injection, same-type call resolution and the standard complexity /
// io_direct metrics. Three decisions are C#-specific and load-bearing: a
// `partial` type declared across several files merges into ONE symbol rather
// than one per half (7,740 files in the benchmark corpus declare one, so left
// alone it inflates every symbol count and scatters a type's edges); only public
// and protected fields and properties become symbols, since a BCL-scale
// repository's private state would otherwise dominate the fact set; and a bare
// type reference resolves against a project-wide index rather than the file's
// imports, because C# `using` opens a NAMESPACE and so names no type in
// particular — an ambiguous simple name resolves to nothing rather than to a
// guess.
// v165: ASP.NET Core attribute routing. A class-level [Route] composes with a
// method-level [HttpGet("…")] into one route per verb, bound to its action by a
// handled_by edge. Two rules carry it. The template is frequently INHERITED — 40
// of jellyfin's 64 controllers declare no [Route] and take [Route("[controller]")]
// from a shared base in another file — so composition runs after the whole fact
// set exists and walks the resolved inheritance edges, resolving [controller] to
// each subclass's own name. And a controller with no [Route] anywhere in its
// hierarchy emits NOTHING: that is conventional routing, whose template lives in
// Program.cs, and composing from what is visible gave every action the path "/" —
// wrong, and (facts being name-keyed) collapsing a controller's actions onto one
// root node. Measured on jellyfin: 422 routes, exactly one per verb attribute in
// the source, all 388 distinct handlers resolving to real symbols.
// v166: ASP.NET Core minimal APIs. `var api = app.MapGroup("api/orders")` binds a
// prefix to a local variable and `api.MapPut("/cancel", H)` composes against it,
// including nested groups and a fluent call after MapGroup; the handler binds when
// it is a method group, and a lambda carries none rather than a fabricated one.
// Scoped to one body, like the Go extractor's intra-function subrouter
// composition. Two rules keep it honest: a string-literal first argument is the
// discriminator, so MapControllers/MapRazorPages/MapHub — which take no path — are
// excluded structurally rather than by a name list; and a group whose prefix is
// NOT a literal (the MCP C# SDK mounts its whole surface at a caller-supplied
// `pattern`) marks its routes unresolvable and emits nothing, since publishing the
// registration path alone would claim endpoints the library does not serve and
// collapse the `MapPost("")` ones onto "/". Measured: eShop 0 -> 30 routes with
// composed prefixes; the SDK's caller-mounted file contributes 0.
// v167: CSharpExtractor implements plugin.TestRefExtractor. Excluding test
// projects (v166's globs) left a production symbol whose only caller is a test
// with no inbound edge at all, reading as dead; this restores that one signal
// without putting test code back in the graph — one KindTestRef fact per test
// file, carrying RelCalls to the production names it touches and no symbols, so a
// test class still never becomes a dead-code candidate. Targets are emitted AS
// WRITTEN (`OrderService.Find`, `Find`) rather than resolved, because the
// production symbol index lives inside Extract and the orphan detector matches
// both the full target and its last segment — the same shape Ruby emits. A
// framework receiver (Assert, Mock, It, …) disqualifies the bare method name as
// well as the qualified one: filtering only `Assert.Equal` let `Equal` through,
// and production code really does declare Equal, so the harness would have vouched
// for a symbol no test exercises. Measured on jellyfin: 210 test-ref facts, 2,826
// distinct targets, 70% matching a production symbol.
// v168: a C# call on an untracked receiver emits a bare method-name edge. The
// call graph was same-type-and-static only, and a DI-wired .NET application calls
// almost everything through an interface — so a method reached that way had no
// inbound edge at all and read as dead. On jellyfin that was 2,478 of 6,975
// methods; it is now 988, and both halves of every implicit interface
// implementation (the interface member AND the class method serving it) are
// rescued. The name is kept BARE rather than bound even when exactly one type
// declares it: jellyfin has one `Match` method, FileStackRule.Match, while four of
// its five variable-receiver `.Match(` call sites are Regex — binding on
// uniqueness would have pointed them into the video-stack parser, and a wrong edge
// feeds impact_analysis and find_path. Nothing is lost, because the dead-code
// detector matches by short name: measured identical rescue with and without
// binding. A name no type in the repo declares is dropped, which is the majority
// (.ToString(), .Add(), .GetAwaiter()).
// v169: a qualified `Type.Member` reference emits edges to both the member and
// the declaring type, and a member access that is not a call (an enum member read,
// a static field, a constant) emits one at all. Previously only invocations did,
// so `VideoRange.HDR` produced nothing and 84 of jellyfin's 137 enums read as
// isolated while it appeared at 25 call sites. The member edge alone is not
// enough either — the dead-code detector matches by last segment, so it vouches
// for HDR and says nothing about VideoRange, which also left every class reached
// only through static calls unreferenced. The TYPE edge is added in
// resolveCSharpTargets once the receiver has provably resolved, not at the call
// site: a bare type name emitted there is indistinguishable from the bare method
// name a member call produces, and `foo.Order()` would bind to a class named
// Order. Receivers are gated on PascalCase, C#'s type-naming convention. On
// jellyfin: enums 105 -> 8 unreferenced, constants 802 -> 179, classes 848 -> 729.
// v170: a loop's iterable is walked in the ENCLOSING scope, not inside the loop.
// `foreach (x in items.Where(p))` enumerates once — the lambda runs per element of
// `items`, the same n the loop runs, not n per iteration — so walking it inside
// reported O(n²) for O(n) work on the most common C# iteration idiom. A `for`
// initializer runs once for the same reason; its condition and update genuinely
// repeat and stay inside. On jellyfin this moved 26 methods from scaling depth 2
// to 1 and 3 from 3 to 2, and dropped 21 spurious N+1 candidates.
// v171: a C# class that declares state and no behaviour carries data_holder. The
// package-metrics explainer spares "data holder" packages its rigid — extract
// interfaces advice, but recognised only a dedicated construct (Kotlin data class,
// Java/C# record). C# almost never uses one: jellyfin declares 1,552 classes and
// 13 records, while 278 of those classes are property-only carriers — so the
// exemption saw nothing and 12 of that repo's 37 package-metrics findings were
// DTO, constant and attribute packages told to extract interfaces. Constructors
// are not behaviour (a record has one too) and nested types are not counted, so a
// class holding a nested handler stays a data holder.
// v172: MSBuild project files are PARSED rather than read by path. A
// ProjectReference becomes a module depends_on edge, so .NET's dependency unit —
// the assembly — is finally in the graph; before this, cycles and package_metrics
// judged .NET at directory granularity while MSBuild judged it at assembly
// granularity. PackageReference becomes a nuget dependency fact, and
// AssemblyName/TargetFramework/OutputType/IsTestProject/solution become module
// props. Project files of EVERY .NET language are read (.fsproj, .vbproj), which
// is what stops a claimed repository from being an empty one: Giraffe ships a
// .slnx and no .cs, so the extractor matched, claimed it and emitted zero facts.
// v173: Razor is read. A .razor component becomes a partial symbol that merges
// with its .razor.cs code-behind, carrying the markup's references — @onclick
// handlers, @bind targets, @if conditions, component tags, @inject types — so a
// member whose only caller is markup stops reading as dead. MudBlazor reported
// 5,749 orphans of 9,287 symbols before this. A .cshtml view emits a KindFileRef
// instead of a symbol, and ASP.NET tag helpers (asp-for="Prop") are read as
// references, which is how MVC views bind view-model members with no @ transition
// at all. @page becomes a UI route (type=page), never a server route.
// v174: XAML is read. An x:Class document becomes a partial symbol that merges
// with its .xaml.cs code-behind, carrying event handlers (Click="OnSave"), bound
// members ({Binding Path=Title}, {x:Bind}), converters and clr-namespace control
// tags — so a dependency property or view-model member whose only use is a
// binding stops reading as dead. Files reported 4,819 orphans, 15.9% of them
// XAML-only. A document with no x:Class emits a file_ref instead of inventing a
// class. Covers WPF, WinUI/UWP, MAUI and Avalonia (.axaml).
// v175: VB.NET is read, into the SAME fact set as C# so a VB class referencing a
// C# type resolves through one shared type index. Line-oriented: VB terminates
// every construct explicitly and has no maintained tree-sitter grammar. Types,
// members, Imports, Inherits/Implements, Handles clauses and complexity metrics.
// roslyn's VB compiler is 3,652 files and 6,644 of its C# orphans had callers
// only there. Also fixes a CASE-SENSITIVITY hole in the .NET test globs: .NET
// names those directories Test/ and Tests/, which the lowercase-only patterns
// never matched, so roslyn's 4,734 test files were indexed as production code.
// v176: F# is read — modules, types, members and module-level FREE FUNCTIONS,
// which no other .NET language has. dotnet/fsharp parsed 111 files of 10,519
// before this. Indentation-scoped: F# closes a scope by dedenting.
//
// Two fixes it forced. Free functions now enter the bare-call index alongside
// methods, without which every F#-to-F# call was dropped. And a module's
// `language` prop is now the dominant language of the files in it rather than a
// hardcoded "csharp" — the layers explainer gates on that prop, and a directory
// of .vb or .fs sources was claiming to be C#.
// v177: persistence. `storage` was ZERO in all fourteen .NET repos of the corpus,
// bitwarden-server and eShop included, both EF Core products. A DbContext becomes
// a storage fact; the types its DbSet<T> and IEntityTypeConfiguration<T> name
// become entities, with the physical table from ToTable("…"); Dapper's generic
// query methods and IMongoCollection<T> name their row types; a Migration
// subclass is recorded as one.
//
// EF Core entities carry NO ANNOTATION — Java can look for @Entity, a C# entity
// is a plain class — so an entity is named for the SYMBOL that declares it, after
// resolution, rather than for the directory that mentioned it.
// v178: outbound HTTP. role=client routes are emitted for HttpClient verb calls
// and Refit attributes, so a C# service is finally BOTH sides of a cross-repo
// edge rather than a route provider only.
//
// The path is rarely a literal at the call site — eShop holds a base URL in a
// field, builds the path by interpolation into a local, and passes the local — so
// a literal environment is resolved per member body, with type-level fields as
// the fallback. An interpolation hole becomes a path parameter rather than being
// dropped, which is what makes a client's items/{id} match a server template of
// the same shape. An absolute URL keeps only its path: under Aspire the host is a
// service NAME, not a hostname.
// v179: conventional MVC routing. A MapControllerRoute / MapAreaControllerRoute
// registration is READ, and its template — routinely a const field — resolved
// through the same literal environment the client scan builds, with areaName and
// the controller/action defaults substituted in. OrchardCore declares 288 verb
// attributes across 114 controllers and only 7 carry a [Route].
//
// A template still containing {controller}/{action}/{area} after substitution is
// NOT emitted: expanding it needs each controller's area, and a route at a literal
// /{area}/… would be a URL the application never serves. The count left generic is
// logged rather than silently absorbed.
// v180: DI registrations are references. A .NET app calls almost everything
// through an interface, and the only place the implementation is named is
// `services.AddScoped<IFoo, Foo>()` in a startup file — so without reading those,
// Foo has no inbound edge and reads as dead. 441 of bitwarden-server's 1,661
// orphan classes (27%) and 59 of eShop's 202 were named in a registration and
// nowhere else. A fix to the graph rather than to a confidence heuristic.
//
// v181: Scala. A fourth JVM language, and the first whose extractor must assume it
// does NOT own the repository: apache/spark holds 1,355 .java beside 6,275 .scala
// and apache/pekko 582, in the same packages, so jvmsrc's package index now reads
// .scala too and the Java/Kotlin extractors invalidate on it. Symbols, extends/with
// as implements, `new` as instantiates, and imports; call edges and complexity
// metrics are not in this version. Two decisions are load-bearing and measured
// rather than assumed. The grammar is pinned to tree-sitter-scala v0.24.1 — the
// newest ABI-14 release, since v0.25.0+ are ABI 15 and the vendored runtime rejects
// them SILENTLY, the C# failure mode. And the test globs are scoped to the sbt
// source set (`src/test`, two segments) rather than to a directory named `test`,
// because a one-segment glob deletes 183 production files across the benchmark
// corpus, 175 of them zio's own test library whose package is `zio.test`; the
// shared path matcher gained multi-segment directory prefixes for it.
//
// v182: Scala call graph, complexity metrics and the I/O closure. The load-bearing
// decision is what counts as a LOOP, because Scala spells iteration and effect
// sequencing the same way: `for (u <- users) yield load(u)` runs load once per user,
// `for (a <- fetchA; b <- fetchB(a)) yield b` runs fetchB exactly once, and the two
// are the same construct. Counting the second as a loop puts a per-iteration-I/O
// finding on every effectful method in the language. Measured over the corpus (8,119
// production files) rather than assumed: `for … yield` is 60.4% effect-typed and
// `for` without yield only 9.7%, so the `yield` keyword is the discriminator; the
// same split puts flatMap/fold (~49% effect) on the discounted side and
// foreach/map/filter/foldLeft on the scaling side. Ambiguous constructs raise
// loop_depth but not scaling_loop_depth, the discount Go and C# already use for
// constant-trip loops, so a finding is downgraded rather than fabricated or lost.
// Call resolution binds a receiver whose type the SOURCE declares (constructor and
// method parameters), which is what lets performs_io cross the constructor-injection
// boundary every Scala service is built on; an inferred or chained receiver stays a
// bare short name rather than a guessed one. Also classifies sbt's `project/` and
// Gradle's `buildSrc/` as tooling: both are build DEFINITIONS compiled as ordinary
// source, so every JVM extractor read them as production packages.
//
// v183: Scala routes, storage and outbound clients. Play's conf/routes (and its
// included *.routes) is read directly from disk like the OpenAPI and Symfony route
// configs — it has no extension, so no glob would admit it — and a sub-router's
// mount prefix is composed onto its paths UNLESS they already carry it segment-wise,
// which every included routes file in the corpus does and which composing blindly
// turned into /team/team. Pekko/Akka HTTP and http4s route trees are read from the
// AST, one route per verb a path block names.
//
// The recurring lesson is that Scala's DSL names are ordinary method names, so three
// passes are gated on the file importing the framework: `path(...)` produced routes
// from a metrics timer whose API is literally path(result).record(nanos); `get`/`post`
// would make every map lookup an HTTP call; and `topic` is a domain noun — a forum's
// closeTopic/hideTopic became seven phantom Kafka topics, which is not inert, because
// the linker turns a topic into cross-repo coupling by name ownership. SQL string
// literals are deliberately NOT read: the corpus contains a SQL engine whose 198
// literal-bearing files are grammar and planner fixtures, not storage it owns.
//
// Also fixes a non-determinism: Pekko verb selection scanned enclosing source text
// and iterated a Go map, so a route's method varied between runs on identical input.
//
// v184: Scala test references. Excluding test source sets left a production symbol
// whose only caller is a spec with no inbound edge at all, so it read as dead — a
// large class in Scala, where a library's public API is frequently exercised only
// from its own suite. One test_ref fact per file carries the outbound references and
// no symbols, so specs still never become dead-code candidates themselves. Assertion,
// matcher, mocking and spec-structure names are dropped, and a harness receiver
// disqualifies the BARE method name too: filtering only `Assert.equals` lets `equals`
// through, and production code declares `equals`, so the harness would vouch for a
// symbol no test exercises and suppress a real finding. 50-85% of a spec's references
// match a production symbol; the rest are library calls that match nothing.
//
// v185: walk anonymous-class bodies. handleInstanceExpression walked only the
// constructor arguments, so the body of `new: …` (Scala 3, braceless) and
// `new T { … }` was dropped whole — 1,817 such bodies across the corpus, carrying
// 5,673 declarations and 9,637 calls. An anonymous class is where Scala puts
// implementations, so this was the largest single cause of dead-code false
// positives: an extension method whose only call site sat inside such a body had no
// inbound edge at all and was reported at HIGH confidence.
//
// v186: a parenless member reference is an edge. Scala's uniform access principle
// means a parameterless method is invoked WITHOUT parentheses, so `xa.transaction`
// and `stream.union2` reach the grammar as field_expression — the same node as a
// field read — and treating only call_expression as a reference left every such
// method with no inbound edge. The same node also covers a value passed by name
// (`WebHook.Create`) and a method used as a value (`Form.apply`), which are
// references by any reading. The edge is emitted but NOT recorded as an in-loop
// call: a field read inside a loop is not per-iteration work in the sense the N+1
// heuristic means, and admitting every one would bury the real callees in the
// analyzer's evidence. Costs 24-34% more edges, which is what a whole category of
// reference is worth.
//
// v187: a combinator applied to an Option repeats at most once. The combinator NAME
// cannot distinguish iteration from an Option chain — `xs.foreach` and
// `xs.find(p).foreach` differ only in what they are applied to — so the receiver's
// trailing method is the evidence. Demoted to the discounted tier rather than
// dropped: the construct is still repetition-shaped, it just cannot scale with the
// input. This was the only confirmed analyze_performance false positive on the
// corpus; a servlet's `find{…}.foreach{…}` was reported O(n²) at high severity
// though neither combinator can run twice.
//
// v188: a trait is an abstraction only when it declares something abstract, and a
// case class is a data holder. Scala traits carry implementations and the idiom
// leans on it — a mixin with a self-type and a concrete body is the ordinary way to
// compose a service — so counting every trait as abstract read one corpus package of
// sixteen controller traits, whose bodies ARE the REST API and which declare nothing
// abstract between them, as A=1.00 and reported it "useless". The `abstract` prop is
// authoritative for package metrics and can demote as well as promote (the hook Ruby
// uses for namespace modules), so it is now declared explicitly either way. Separately
// a case class carries the data_holder marker, the same signal a Kotlin data class or
// a record gives, which stops "extract interfaces" being advised on a package that is
// mostly value carriers — one corpus package of 92 header types got exactly that.
//
// v189: Scala module facts carry the build module they compile into (jvm_module),
// derived from the path prefix before the first `src/` segment — the layout sbt,
// Mill, Maven and Gradle share, so no build file is parsed. The cycles explainer
// reads it: Scala imposes no package-level acyclicity WITHIN a module, so a cycle
// there is legal and compiles, while sbt and Maven both reject a circular
// dependency BETWEEN modules. Without the prop every Scala cycle was reported as
// something that "can cause initialization issues", which for the common case is
// untrue — the same over-claim the prop already prevents for C# and Rust. A
// single-module repository returns "." rather than "", because callers read "" as
// unattributed and that is the case where attribution matters most.
//
// v190: Dart and Flutter. Symbols, imports, call edges, complexity metrics and the
// io_direct/performs_io closure, plus four framework surfaces — Flutter widget roles,
// navigation routes, outbound HTTP call sites, and local/remote stores. Two decisions
// carry most of the weight. Every framework pass is gated on the FILE'S OWN IMPORTS,
// which Dart makes a language guarantee rather than a heuristic: imports are mandatory
// and there is no ambient namespace, so a file that has not imported go_router cannot
// be calling it — this is what makes it safe to match on names as short as `go`, `get`
// and `collection`, the trap that produced phantom Scala routes and topics. And
// navigation routes carry type "page", so routeindex.IsUIRoute keeps a Flutter screen
// out of the cross-repo server-route index and out of unused-routes: an app's
// `/users/:id` screen is not an endpoint anything can call, and indexing it beside its
// own backend's real `/users/:id` would manufacture an edge in the wrong direction.
// Module facts carry pub_package (registered in facts.CompilationUnitProps) because
// Dart permits circular imports between libraries outright — legal, compiling, and
// common — so a Dart cycle is a coupling signal and never a build-order defect.
// Generated Dart (.g.dart, .freezed.dart, .mocks.dart, .pb*.dart) produces nothing:
// it is the majority of files in a build_runner project and none of it is navigated.
// v191: gin routes in the Go extractor. Registrations (`r.GET`, the generic
// `r.Handle("GET", …)`, `Any`), `Group("/prefix")` mounts composed onto them, and
// `func(*gin.Context)` tagged http_handler so a route binds to the method serving it.
// Two decisions are measured rather than assumed. The group prefix is JOINED, not
// concatenated: gin spells a no-prefix group `Group("/")` and real code leans on it —
// ente's server opens seven — so concatenating turns every route beneath one into
// "//ping", a path nothing serves and no client route matches. And `Group` is
// recognised by its argument being a STRING LITERAL rather than by naming the
// framework, because chi declares a `Group` too with an entirely different meaning
// (`r.Group(func(r chi.Router){…})` takes a function and mounts nothing) — the shape
// discriminates them structurally. Verified on ente's server: 359 registrations
// outside test files, 359 routes extracted, no doubled separators, and its Flutter
// client goes from 167 unresolved call sites to 167 resolved.
// v192: the Dart package index is read from disk instead of from the engine's file
// list. `**/*.yaml` is in the default ignore globs, so a pubspec.yaml never reaches an
// extractor — 0 of appflowy's 4,114 walked files — and the index was therefore always
// EMPTY, silently and in three places at once: modules carried no pub_package (so every
// legal Dart cycle was reported as something that "can cause initialization issues"),
// the repo's own `package:` imports classified as external rather than internal (10,125
// internal edges missing on appflowy alone), and Flutter was never detected from the
// manifest. Same deliberate bypass the OpenAPI extractor and PHP's Symfony route config
// already make, for the reason the glob rule states: the globs suppress config and data
// noise, and a pubspec is the definition of the compilation unit.
//
// v192 also carries three corrections the corpus exposed and the fixtures could not,
// all of them guessed node kinds that silently matched nothing:
//   - cyclomatic complexity counted NO logical operators, because Dart models `&&` as
//     its own node kind rather than as a generic binary expression. Counted on the
//     OPERATOR node, which is exact — counting occurrences in the enclosing
//     expression's text would recount them at every nested level.
//   - the constant-trip loop discount never applied, so a literal-bounded
//     `for (var i = 0; i < 10; i++)` inflated scaling depth and turned an honest O(n)
//     into a fabricated O(n2). A C-style for and a for-in are the same node kind here
//     and are told apart by whether for_loop_parts holds a relational_expression.
//   - a bare call resolved to ANY unique short name, including a constant. immich
//     declares the enum constant LogLevel.severe and separately calls log.severe(...),
//     so 117 call sites bound to the constant and god-class reported it as a
//     high-fan-in symbol with 117 dependents. Calls now resolve only to callables.
//
// v193: four corrections the enterprise tools exposed on the Dart corpus, none of
// which the OSS explainers could see.
//   - recursion required only a matching short NAME, so `dispose()` calling
//     `controller.dispose()` counted as recursing. 64 false findings on one app, 63 of
//     the analyzer's 75. Now the receiver must be absent, `this`, or the bare name.
//   - calls inside a closure BODY were dropped entirely: the invocation is a direct
//     child sequence of the closure node, which the walk descended past without
//     scanning. Arrow closures are pervasive in Flutter, so functions plainly in use
//     were reported dead.
//   - dependency facts are named `<importer> -> <imported>`, the shared convention the
//     package-metrics explainer splits on. Naming only the target made every Dart edge
//     unrecoverable there: Ce was 0 for every package and average instability 0.00.
//   - symbols now carry a leading `declares` edge to their module, which is how that
//     explainer attributes a symbol to a package. A Dart class also declares its
//     members, so the first member name was being read as a package — 1,746 phantom
//     packages on drift against 199 real modules, the .NET failure in Dart's clothing.
//
// v194: TypeScript tsconfig `paths` entries with no `*` are honoured. The parser
// required a wildcard on BOTH sides and silently dropped the exact form — which is how
// a monorepo names a sibling package:
//
//	"@acme/common":   ["./packages/common/src/index.ts"]   <- dropped
//	"@acme/common/*": ["./packages/common/src/*"]          <- kept
//
// So a bare-specifier import matched nothing, was classified external, and its call edge
// fell back to the CALLER's directory — one phantom node per calling package, none of
// them the real symbol. `impact_analysis` and `traverse` therefore returned nothing for
// any symbol imported across packages, the single most useful query on a TS monorepo.
// The subpath form resolved correctly throughout, which is why a 72-repo corpus never
// caught it: the bug is invisible on a single-package repo.
//
// Measured before/after on excalidraw, supabase and bitwarden-clients; the exact match
// mode is carried on the alias rather than inferred, because a bare `@acme/common` used
// as a prefix would also swallow `@acme/common-utils`.
// v195: the Java and Ruby grammars move to their newest releases — tree-sitter-java
// v0.21.1-20240824 -> v0.23.5 and tree-sitter-ruby v0.21.1-20240818 -> v0.23.1. Both are
// still tree-sitter ABI 14, which is the ceiling the vendored go-tree-sitter runtime
// accepts; the C#, Python, Scala and Dart grammars are all held back or regenerated for
// exactly that reason, and probe_test.go in each of these two packages now guards the
// same trap here.
//
// One version covers both grammars deliberately. Each bump alone would invalidate every
// cached fact in the tree, so landing them separately would make users pay for a full
// re-extraction twice to arrive at the same graph.
//
// The bump is NOT visible in the goldens — all 37 fixture graphs are byte-identical
// before and after, and TestDeterminism passes on both. It is versioned anyway because
// the goldens only speak for the fixture corpus, while the grammar is what turns bytes
// into facts for every repo: two minor releases of grammar fixes can parse constructs the
// old parser shredded, and the extractor cache is keyed on cacheVersion plus the file
// hash alone. Without the bump an upgraded binary would keep serving facts its own parser
// never produced, and nothing downstream could tell.
// v196: the C, C++ and PHP grammars leave the 2024 pre-release pseudo-versions they were
// stuck on — tree-sitter-c v0.21.5-20240818 -> v0.23.6, tree-sitter-cpp v0.22.4-20240818
// -> v0.23.4, tree-sitter-php v0.22.9-20240819 -> v0.23.12. Roughly two years of upstream
// grammar fixes each.
//
// They were not stuck for a good reason. Every one of these grammars has since been
// regenerated at tree-sitter ABI 15, which the vendored runtime refuses, so the only
// version ever offered was the unusable one and the pins simply never moved. The bounds
// in .github/dependabot.yml now cap each grammar at its last ABI-14 release, which is what
// made these three upgrades visible at all.
//
// Grouped into one version for the same reason as v195: any grammar change invalidates
// every cached fact, so landing three separately would charge users three full
// re-extractions to reach one graph.
//
// As with v195 the goldens do not move — all 37 fixture graphs are byte-identical and
// TestDeterminism passes. The bump is not a formality: the cache is keyed on cacheVersion
// plus the file hash alone, so without it an upgraded binary would keep serving facts its
// own parser never produced. What the goldens cannot check is the rest of the world, so
// probe_test.go in cppextractor and phpextractor now pins every node kind the walkers
// dispatch on (35 shared C/C++, 17 C++-only, 1 C-only, 38 PHP) — a renamed kind is the
// failure a two-year grammar jump actually causes, and it degrades extraction without
// erroring.
// v197: Rails route extraction stops being a single-file affair.
//
// Three defects, one root cause — the extractor read `config/routes.rb` and treated it
// as the route table, when in Rails it is only the entry point to one:
//
//   - Route-file discovery matched the repository root plus a packwerk `packages/*`
//     pattern and nothing else. solidus is six mountable engines with NO root config/
//     at all, so it reported ZERO Rails routes while declaring 195; discourse's 25
//     plugin route files (386 declarations) and GitLab's 38 ee/config/routes files
//     (451) were never opened either. The rule is now shape-based — any
//     `<dir>/config/routes.rb` or `.rb` below a `config/routes/` directory, at any
//     depth — minus generator templates and dummy apps, which look identical and are
//     served by nobody.
//   - `mount` was not implemented at all, in a corpus containing 23 mount sites. An
//     engine's whole route table is served below its mount path, so the mounted
//     constant is now resolved to the directory that owns it (by reading the
//     `lib/**/engine.rb` beside each engine route file) and that file is parsed under
//     the mount prefix. Same interprocedural prefix-composition shape as Go v125,
//     Axum v130 and FastAPI v133.
//   - Routes carried a handler only when written with an explicit `to:`, so every
//     `resources` declaration — the majority of Rails routes — produced an isolated
//     graph node. The controller is now derived from the enclosing module namespace
//     and the resource name, and each route carries a `handled_by` edge to the real
//     controller-action symbol. Without that edge, impact analysis from a controller
//     could not reach the endpoints it serves, and a controller reached only through
//     the route table read as dead code.
//
// Also: `concern`/`concerns` (a concern serves nothing where it is DEFINED and
// everything where it is referenced — the previous default-case descent got this
// exactly backwards), the `controller do` block form, and Rails detection for
// engine-only repositories, which have neither of the two root markers.
//
// A fourth defect found while measuring the first three: the route walker iterated only
// direct `call` children, so any route inside plain Ruby control flow was skipped. A
// route file is Ruby and real ones are full of conditionals — GitLab guards whole files
// with `unless @organization_scoped_routes`, solidus wraps its admin routes in `if
// SolidusSupport.admin_available?`, and Rails' own activestorage route file is a `draw`
// block closed by an `if` MODIFIER. Five route files across the corpus parsed cleanly
// and produced nothing, which is indistinguishable from a file with no routes. Both
// branches of a conditional are now walked; which one Rails takes depends on runtime
// configuration the extractor cannot see.
//
// A fifth: the hash-rocket route form `get 'path' => 'ctrl#action'` puts the handler in
// the pair's VALUE rather than in a `to:` keyword. Discourse and lobsters write nearly
// every route that way, so reading only `to:` left thousands of routes handler-less even
// after the derivation above.
//
// A sixth, found the same way: `namespace`, `resources` and `resource` accept a symbol
// OR a string, and only the symbol form was read. `namespace "recaptcha"` made its whole
// block invisible, taking every route inside it along — four openproject module route
// files declared 15 routes and produced none.
//
// Unrelated to the extractor but found by the same measurement: the ember-octane LAYER
// pattern claimed the bare path segment `lib` for its level-0 util layer. discourse is a
// Rails backend beside an Ember frontend, the Ember pattern wins the repo on confidence,
// and layer matching is by path segment with no notion of language — so every Ruby
// `lib/` and `plugins/*/lib/*` directory became the innermost layer and each model or
// service it legitimately called became a violation. 397 of discourse's 426 reported
// violations were that; removing `lib` takes it to 27. It is wrong on Ember's own terms
// too: Octane puts utilities in `app/utils/`, while `lib/` holds in-repo addons.
//
// Classes also now carry a `rails_component` prop — job, mailer, channel, policy,
// controller, model, component, concern — derived from the superclass or an included
// module first and the directory only as a fallback, because `< ApplicationJob` is what
// Rails dispatches on while `app/services` is a convention with no framework meaning.
// And `db/migrate` and `lib/tasks` are classified as TOOLING rather than production: a
// migration is a one-shot script nothing references by design, so calling it production
// code made every migration in a large Rails app a dead-code candidate.
//
// And a second framework: GRAPE, which had no extractor at all. GitLab's entire v4 REST
// API — 1,033 files, ~1,530 verb sites under 382 `resource` and 318 `namespace` blocks —
// was invisible, reached from Rails through one `mount ::API::API => '/'`. Grape is
// identified by transitive inheritance rather than by a route file (GitLab has exactly
// ONE class inheriting Grape directly and a thousand inheriting that), so the class set
// is computed as a closure over the `superclass` props the AST pass already emits — no
// extra I/O on a repository containing no Grape — and only the surviving files are
// re-parsed for their route bodies. Composition is class-to-class via `mount`, so a
// route's URL is assembled from a prefix chain that lives in other files.
//
// The goldens move: ruby_sample's 13 route facts each gain a handler prop and a
// handled_by relation. No route is added or lost in the fixture, which is the point —
// the fixture is a single-file application, the shape that already worked.
// v198: the TS extractor composes Express sub-router mounts ACROSS FILES, the last
// of the major frameworks still resolving mounts only within one file.
//
// serverroutes.go emits a route only when the mount is written in the same file as
// the router, and deliberately emits NOTHING otherwise: `router.post('/login')` in a
// module mounted at '/webhooks' elsewhere serves '/webhooks/login', and the fragment
// would be a wrong fact that can false-match another repo's route. Correct, and also
// silent on the layout every Express service uses — routes in `src/api/*.ts`, the
// `app.use('/api', router)` in `src/server.ts`. A four-route service split that way
// contributed zero route facts, which reads as a backend that serves nothing.
//
// tsextractor/routermount.go adds the repo-wide half, the same shape as Go v125,
// Axum v130, FastAPI v133 and Rails v197: each file reports its routers, the routes
// held back on them, its mounts, and its imports/exports resolved to files; a
// fixpoint then propagates prefixes outward from the application roots. It resolves
// ESM and CommonJS, renamed named exports, a router returned by a factory
// (`app.use('/api', routes())`), and mounts nested several files deep; a router
// mounted at N prefixes emits its routes once per prefix. Composed routes carry
// `mount_composed=true`. Everything unresolvable — a non-literal prefix, an external
// module, a router nothing mounts — still emits nothing, so the pass can add a route
// or correct its path but never invent one.
//
// Two fixes fall out of the same work. serverroutes.go now requires a mount's PARENT
// to be mounted before it composes: `apiRouter.use('/orders', orders)` in a file that
// does not itself mount apiRouter used to emit the fragment '/orders/:id' for a route
// really serving '/api/orders/:id' — the exact wrong-fact case the pass exists to
// avoid, reached through the same-file path. And an import written WITH an extension
// (`./orders.js`, which TypeScript's nodenext resolution makes mandatory) now
// resolves to the `.ts` file it names, where before it resolved to nothing.
//
// The ts_express_multirepo golden gains the two '/webhooks/login' routes it had been
// suppressing, and the consumer's call to that path stops being an unresolved edge:
// cross-repo endpoint count 2 → 3, http_client coverage resolved 3 → 4. Cached
// TypeScript/JavaScript snapshots must re-extract.
// v199: Ruby reads db/structure.sql when present — the database's own account of the
// schema, which model-derived storage facts can only infer. Each pg_dump CREATE TABLE
// yields a storage fact (or, for a table an ActiveRecord/Sequel model already claims,
// a census folded onto the model's existing fact — one table, one storage identity)
// carrying sorted `columns` and single-column `fk_constraints` ("column->reftable")
// props, which is what lets a declared require-rule verdict schema discipline (every
// company_id column carries its companies FK) from measured facts. Line/regex-based on
// the pg_dump shapes only; composite FKs and unrecognized lines are skipped, never
// guessed.
// v200: Stimulus markup bindings become named facts. A `data-controller="x"` or
// `data-action="click->x#y"` attribute in an .html.erb view emits one dependency-style
// fact per declared controller identifier ("stimulus-binding: <file> -> <x>", with a
// `binding` prop naming the declaring attributes) at resolution_level
// "markup-declared" — the honest level: the binding is stated in markup, not resolved
// through code. The fact links to app/javascript/controllers/<x>_controller.(js|ts)
// only when that conventional file exists; otherwise it stays name-only. Identifiers
// that are not plain Stimulus tokens (ERB interpolations) declare nothing — fail
// closed, never a guessed edge.
// v201: finding 0007's next markup slice, fail-closed at every shape. Literal Turbo
// frame ids (`turbo_frame_tag :post_1`, `data-turbo-frame="results"`) in view
// templates become dependency facts named "turbo-frame: <file> -> <id>" at
// markup-declared — the frame id is an identity two markup sites share, so it is
// recorded without resolution; dom_id calls, interpolation and the reserved `_top`
// target emit nothing. Model-side `broadcasts_to` with a literal symbol/string
// stream becomes "broadcast: <Model> -> <stream>" at literal-declared; the common
// lambda form computes its stream per record at runtime and emits nothing. And the
// TS extractor tags the static targets/values fields it already parses on
// conventionally-placed Stimulus controllers with classification props
// (framework=stimulus, stimulus_static=targets|values) — props only, on symbols
// that already exist, so a consumer can finally ask which controllers declare
// which magic accessors.
// v202: importmap-rails apps are detected as JavaScript projects. The TS extractor
// claimed every .js file (its FileOwner glob) but Detect knew only package.json and
// tsconfig shapes, so a Rails app whose pins live in config/importmap.rb — which
// ships no package.json at all — never ran the extractor: on the census that was
// 74 of the 8-repo sample's 100 skipped-with-cause files (once-campfire), every one
// of them a claimed, parseable, unparsed source file. config/importmap.rb presence
// now switches the extractor on; vendored minified bundles under vendor/javascript
// are still skipped by the existing minified gate, which is the honest account.
// v203: the rest of the view-composition surface, fail-closed at every shape. A
// hand-written `<turbo-frame id="composer">` element declares its frame id exactly
// as turbo_frame_tag does — it is the helper's rendered output, and the shape a
// helper-free view writes — so it now emits the same "turbo-frame:" fact; an id
// carrying ERB still fails the id gate. And a literal render target (`render
// "accounts/help_contact"`, quoted, with or without partial:) becomes a dependency
// fact "render: <view> -> <target>" at literal-declared, linked to the partial file
// only when Rails' underscore lookup finds it on disk — `render @post`,
// interpolation and variables emit nothing, so view-to-view composition enters the
// graph without a single guessed edge.
// v204: CommonJS export assignments declare symbols. `exports.name = function` and
// `module.exports.name = function` are the whole public surface of a classic Node
// module, and no declaration-shaped case ever fired on them — an Express
// controller written that way emitted nothing, which the census surfaced as
// "claimed by typescript, no facts emitted". The member-assignment-of-a-function
// shape now yields an exported function symbol; plain values, re-exported
// identifiers and whole-object `module.exports = {…}` still emit nothing, because
// there is no member name to carry or no declaration to classify without guessing.
// v205: Go interface methods declare symbols. An interface declaration's named
// methods each emit a symbol fact (pkgDir.Iface.Method, symbol_kind method,
// exported per the method name's own Go case, receiver carrying the interface
// name) beside the interface fact that was already emitted. The constraints
// evaluator resolves edge targets by exact fact name, fail closed, and a call
// through an interface value targets exactly that name — so a declared forbid
// rule over a dependency visibly in the source yielded zero verdicts (finding
// 0009, gin's c.engine.HTMLRender.Instance). The declaration is measured, not
// guessed: embedded interfaces expand nothing, and no edge to an implementation
// is fabricated. TS interfaces share the missing-member shape but not the
// defect: without type inference the TS extractor never resolves a call target
// to dir.Iface.method, so emitting the members would add facts no edge can
// ground on. Ruby has no interface construct — a module's methods are real
// definitions and already emit symbols.
// v206: three census-named vocabulary gaps closed. The TS extractor claims and
// parses .mjs — the file is the same ESM the extractor already reads in .js,
// only the extension differed, so a Node project's native-ESM half was
// excluded-by-kind — and .mjs joins the module-resolution extension order so
// an extensionless import can land on it. Jbuilder views (.jbuilder) go
// through the Ruby template reference pass with the whole file as the Ruby
// region: a Jbuilder template IS plain Ruby (the json builder DSL), so helpers
// and decorators called only from a JSON view stop reading as dead, while the
// reference-only shape keeps views out of the symbol set. And the Ruby
// extractor's FileOwner now claims what its Extract already reads — ERB/Slim/
// HAML templates and Jbuilder views — which both moves those files out of the
// census's excluded-by-kind bucket (they were parsed while reading as a
// vocabulary gap) and fixes a real cache defect: a template edit did not
// invalidate the extractor's cache key even though its facts carry the
// template's references.
//
// v207: TypeScript class members carry their decorators, and get accessors become
// their own symbol kind. Every decorated class member (method, getter, field) and
// every decorated class gains a `decorators` prop — the sorted, deduped decorator
// names with arguments stripped (`cached`, `tracked`, `action`, `service`,
// `Controller`, …), space-joined in the set-valued string form the constraint
// evaluator's prop containment and the prop-implication miner both read (the
// columns/fk_constraints precedent) — read from the same nodes the route/service
// passes already walk, so a convention like "expensive getters carry @cached"
// becomes a mechanically checkable prop instead of prose. A `get` accessor emits
// symbol_kind getter (a new vocabulary value beside function/method; consumers
// that treated methods as callable members treat getters the same) with a
// getter_calls prop counting its distinct outgoing call edges — emitted even at
// 0, so measured-cheap and unmeasured never look the same. Set accessors stay
// methods: only the read path is a getter. Template read fan-in is deliberately
// NOT emitted — no template->member edge exists to derive it from (the .hbs
// scanner refuses bare {{name}} as ambiguous, and strict-mode .gts tokens
// resolve against imports only), and a guessed fan-in is worse than an absent
// one.
//
// v208: a Rails namespace's declared table_name_prefix corrects the models nested
// under it. `def self.table_name_prefix` on a module records its literal on that
// module's symbol fact (a plain string only — an interpolated or computed prefix
// states nothing), and a whole-repo pass prepends it to the table of every model
// storage fact whose root namespace declares one and whose table_source is
// derived. A declared table is left exactly as the source states it: Rails does
// not prefix a `self.table_name`, so prefixing one would replace a stated fact
// with a derived guess. The correction runs before the structure.sql fold, so the
// dump's column census lands on the model that reads the prefixed relation rather
// than on whichever model the unprefixed name collided with.
//
// The same version carries finding 0007's method-level residual. A Stimulus
// data-action no longer loses the method after the `#`: the binding fact carries
// the sorted `stimulus_handlers` set the view invokes on that controller (action
// options like `:prevent` are not part of a method name), and the new
// stimulus-resolver binder grounds each one on the member the controller file
// declares, reporting the rest as `stimulus_unresolved` beside a
// stimulus:actions coverage fact. Nothing is derived from a class name — an
// identifier that grounded no file grounds no handler either. The controller
// file itself now also resolves outside app/javascript/controllers: the
// conventional root wins outright, and failing it the single file in the tree
// whose path ends with the identifier's relative path grounds it, so an app
// registering controllers from app/components stops being name-only. Two
// candidates are an ambiguity and ground nothing.
//
// And the TypeScript gRPC stub index grows the ambiguity guard its Go sibling
// has carried since finding 0003. Both of its keys are short names, so a service
// declared in two proto packages collides; the name is now dropped the moment a
// service with a DIFFERING fully-qualified name claims it, stickily, and the
// conventional "<Service>Client" / "<Service>" names go through the same gate so
// a derived name cannot put back what a collision dropped. Re-registration under
// the same fq is not a collision — a split _pb/_connect pair, a barrel
// re-export and a checked-in dist/ copy all do it — so no edge that resolves
// today stops resolving. Emitting nothing is the whole point: an edge to one of
// two API versions is wrong half the time, and the ambiguity is not published as
// a fact property, which is the settled answer on the Go side.
// v209: the Rails default schema dump joins the SQL one. db/schema.rb is read
// into exactly the census db/structure.sql already produces — the sorted
// `columns` set and the sorted `from_column->to_table` `fk_constraints` set,
// through the same fold onto whichever model claims the table — so a constraint
// written against either prop verdicts identically whichever format a project
// keeps, and the half of the Rails world that never opted into structure.sql
// stops producing no schema facts at all. Where both files exist structure.sql
// wins outright and schema.rb is not read: opting into the SQL format is what
// makes it the authoritative dump, and one database read twice would fold two
// censuses onto one storage identity. The reader is a bounded line parser
// rather than a Ruby grammar for the reason the pg_dump one is not a SQL
// grammar — SchemaDumper writes one statement per line in a handful of stable
// shapes, and a line outside them contributes nothing. The implicit primary key
// is synthesized (`id`, the declared `primary_key:`, or none under `id: false`)
// because it is a column the SQL dump would have written out. An
// add_foreign_key without an explicit `column:` does not invent the name
// ActiveSupport's inflector would derive: it CHOOSES the single `<stem>_id`
// column the table declares that the referenced table is a plural of, and
// states nothing where none or several match — the silence a composite key
// already gets on the SQL side.
//
// v210: a JavaScript class says which base class it extends, and which module
// that name came from. The class fact carries `superclass` — the identifier as
// written, one level, the same meaning rubyextractor gives the prop — and
// `superclass_module`, read from the file's own import table. The second prop is
// what JavaScript needs and Ruby does not: a Ruby superclass token is a globally
// resolvable constant, while a JavaScript one is bound by an import, so the bare
// `Controller` is @hotwired/stimulus' base class on 150 of one production Ember
// frontend's classes and @ember/controller's on 259 others, and a prop carrying
// only the identifier fuses two unrelated hierarchies. The module is the same
// string the file's own dependency fact already records as its imports target —
// the specifier for a package, the repo-relative path for a file, resolved through
// tsconfig aliases where the project declares them — and it is never derived
// from the identifier's spelling or the file's location: a base class the file
// declares itself or a global like Error or HTMLElement carries the name and no
// module at all.
//
// Only an identifier names a base class. `extends Base<T>` is one (the type
// arguments are applied to the base, not a second reading of it), while
// `extends Service.extend(Mixin)`, `extends Turbo.navigator.view.snapshot.
// constructor`, a ternary, a subscript and `extends new Factory()` reach their
// base through a value the source never states, so they emit nothing rather than
// name the mixin factory or the namespace object — the seven such classes in
// that frontend are exactly the forms where a nearest-identifier answer
// would have been wrong. No inheritance relation accompanies the props: the
// identifier alone is not a symbol identity when a repository writes the same
// `Controller` 409 times against two different base classes, and the local name a
// default or aliased import binds is not the name the exporting file declares, so
// an edge built from either would be a resolution nothing measured. The Ember
// component/service/model classifier now reads its heritage through the same
// single reader, and .vue and .svelte script blocks resolve theirs through the
// same import table.
// v211: NOT an extractor change — the first entry here that is not. Owner resolution
// wires a Ruby class to its instance methods ("Owner#method"), which no split on the
// last "." ever reached: 23,127 has_method edges appear on the monolith out of the
// same facts, and the two outlier explainers stop counting a type's own methods as
// calls out of it. facts.jsonl is byte-identical across the change (cmp, monolith and
// a third repository in this estate), so nothing cached is stale — but a baseline
// pinned by a v210 build and graded by this one reports 30 moved findings over an
// unedited tree, and without a bump prints "PASS — no architectural change" while
// doing it. The constant is the provenance marker as well as the cache key; this line
// is what widening it costs.
// v212: a TypeScript constructor is a fact. The member walk skipped `#`-private
// members and `constructor` in one condition, and the two are not alike: a private
// member has no callers to measure, while a constructor runs on every instantiation
// and what it calls is the whole of "this class fetches when it is built". Measured
// on a large Ember application, 323 constructors, 306 of which call something, and
// the finding count did not move at all — 503 before and after, with no finding
// naming a constructor, so the dead-symbol reasoning does not mistake a member
// invoked by `new` for an uncalled one. It is bumped on the provenance argument the
// header states rather than on cache staleness: a baseline pinned by a v211 build
// must not grade a tree that now carries constructors as unchanged.
// v213: a TypeScript method records the fields it assigns on itself, as the Ruby
// extractor already did. `this.args.user.name = x` records `args` — the outermost
// property after `this`, because that is what a convention speaks about and the
// path beyond it varies per call site without changing the answer. Only `this` is
// followed: an assignment to a local or to another object is not a claim about the
// member's own state. It is what makes "data flows down and actions flow up"
// enforceable — 212 methods on a large Ember application write through their own
// arguments — and the finding count did not move, 503 before and after.
// v214: a TypeScript member records whether it declares a parameter at all. A
// modifier is handed the element it is attached to, so a modifier declaring no
// parameter is not modifying anything — it is a side effect fired by render,
// which is the convention the prop exists to make selectable. The answer is
// "yes"/"no" on every member rather than a prop present only when true, because
// a rule matches a VALUE and a prop that is absent on the compliant half would
// select nobody to verdict. A rest parameter counts as one: the member still
// receives what it is handed. Measured on a large Ember application, 21,689
// members carry it, 6,341 of them declaring parameters, and the finding count
// did not move — 503 before and after. It is bumped on the provenance argument
// the header states rather than on cache staleness: a baseline pinned by a v213
// build must not grade a tree whose members now carry this prop as unchanged.
// v215: takes_parameters reaches the module-level function symbols it always
// meant to cover. v214 emitted it only from the class-member walk, so the
// dominant Ember modifier form — `export default modifier((element, ...) => ...)`
// at module scope — carried no answer at all, and a rule demanding "yes" read
// the silence as a breach rather than as an unmeasured member. Measured on a
// large Ember application, all eight members such a rule named were
// module-level modifiers that do take their element: 8 false verdicts, 0 true
// ones. The prop is now emitted wherever a callable symbol is, function
// declarations and arrow-bound consts included, which takes the estate from
// 21,689 members carrying it to 22,963 and drops those eight verdicts to zero.
// Absence stops being ambiguous for this prop, which is what the rule form
// needs: it cannot tell "measured, declares none" from "never looked".
// v216: the hash-rocket mount form whose key builds a Rack app inline names its
// app again. `mount Flipper::UI.app(Flipper) => "/admin/flipper"` reaches its
// constant through a call, which parseMount unwraps on the `at:` side and required
// to be a bare constant on the key side — so the declaration produced no constant,
// and a mount with no constant is dropped entirely, path and all. One route on the
// monolith (/admin/flipper) vanished this way, which is how the shape was found:
// the previous reader here took the pair's VALUE without inspecting its key, so
// the upgrade to upstream's constant-aware reader lost a form it had covered.
// Reading the receiver is the same rule the `at:` branch already applies, so no
// new guess enters: a key that names nothing resolvable still mounts nothing.
// v217: a Rails route names the controller Rails names, on the rules Rails
// actually uses rather than one of them everywhere. `Resource#controller` is
// `options[:controller] || @name`, so a plural `resources` takes its name VERBATIM;
// `SingletonResource#controller` is `options[:controller] || plural`, so only the
// singular pluralizes. Applying the singular rule to both turned every plural name
// that is not the pluralization of its singular into a controller no application
// has: the monolith declares `resources :meeting_self_schedule` and was reported as
// served from `meeting_self_schedules`, while the file on disk is
// `meeting_self_schedule_controller.rb`.
//
// The pluralization the singular rule needs is ActiveSupport's, which knows the
// irregulars — `resource :person` is served by people — and this extractor's
// inflector answers persons. So a singular resource that does not name its
// controller now gets NO handler, which is the refusal this file has always made
// here and the reason it never grew a pluralize-for-Rails rule. A verb inside such
// a block declines with it rather than inheriting the enclosing resource's
// controller, which serves different routes.
//
// A verb may also name its own controller, and that name wins over the enclosing
// one: map_match reads `controller:` off the call and only then falls back with
// `controller ||= @scope[:controller]`. Reading `action:` while leaving that option
// unread is worse than reading neither, because the route then resolves to the
// controller enclosing it — one that exists and does not serve it. The monolith
// writes that shape 36 times in one route table.
//
// The namespace is composed where the ROUTE is created, not where the resource is
// declared — `Mapping.build` captures `scope[:module]` at the route site and
// `add_controller_module` joins it there — so a `scope module:` entered between a
// `resources` declaration and a verb inside its block belongs to that verb. The
// scope carries the bare controller and every route site composes for itself, which
// puts the `controller ... do` block form on the same rule. add_controller_module
// has one exception and it is now honoured: a controller written with a leading
// slash is stripped of the slash and NOT composed.
//
// Measured on the monolith at f97ae49, against the binary this branch forked from:
// routes with no handler 250 -> 210, routes whose handler names a controller file
// that EXISTS 3,482 -> 3,531, routes whose handler names one that does NOT 6 -> 14.
// No route moved from having no handler to having a wrong one, and none moved from
// a controller that exists to one that does not; the 14 are the 5 that were already
// there, plus 9 the upstream 0.3.18 reader newly reads inside `Avo::Engine.routes.draw`
// and one of the original 6 now fixed. Upstream 0.3.18 alone puts that count at 57,
// so what this changelog entry describes removes 43 of the 51 it added. Route
// handlers naming an action that is not a known symbol: 1,762 before, 1,781 on the
// align, 1,662 here — fewer than either.
//
// The bump is the PROVENANCE argument in the header, not the cache one. The cache
// argument does not hold: buildIdentity mixes the executable's size and mtime into
// every entry, so a cache written by a different binary is discarded whether or not
// this constant moves, and an earlier draft of this entry claimed otherwise. What
// does not happen without a bump is everything keyed on ExtractorVersion — `enola
// check` compares it to decide whether a baseline is comparable, and every locally
// built binary reports the same "dev" EnolaVersion, so this constant is the only
// signal that 2,533 route->action edges appeared and 156 were withdrawn; and append
// mode discards prior state on it, without which a multi-repo union carries repos
// extracted under the old derivation and labels them current.
//
// benchmarks/rails-controller-derivation scores all of it, expanded through
// ActionDispatch::Routing::RouteSet on actionpack 8.1.3 and 8.1.1, which agreed,
// rather than written from memory. The route cases that existed before scored
// filters and nesting only, so every one of these derivations could be wrong while
// the suite read 149/149 — which is what happened.
// v218: four more constructs Rails reads to answer the same question. `scope
// controller:` is the one construct other than the `controller ... do` block that
// writes the @scope[:controller] a verb falls back to (merge_controller_scope keeps
// the child; map_match's `controller ||= @scope[:controller]` reads it), and leaving
// it unread did not leave the routes inside without a controller — the search walked
// outward to the enclosing resource and named one that exists and serves entirely
// different routes. The leading-slash escape now applies to the controller Rails
// splits out of a `to:` string as well as to a `controller:` option, because
// add_controller_module is the same function on both paths; honouring it on one
// spelling and not the other also joined the module onto a name still carrying its
// slash. Options are matched in BOTH of Ruby's hash spellings: a hash-rocket key's
// text carries a LEADING colon, so trimming a trailing one saw `controller: "x"` and
// missed `:controller => "x"`, `:on => :collection`, `:via => [...]` and every other
// option written that way. And get_to_from_path's shorthand is implemented: a
// multi-segment String path that names no endpoint of its own IS the endpoint, and
// the name it derives is handed on as the `to:`, so it outranks the enclosing
// controller rather than deferring to it.
//
// Measured on the monolith at f97ae49, challenger against current: routes with no
// handler 1,306 -> 1,306, routes whose handler names a controller file that EXISTS
// 3,531 -> 3,536, routes whose handler names one that does NOT 30 -> 31. No route
// moved from having no handler to having a wrong one. The one route that moved into
// the third column is /companies/:company_id/integrations/widgets, declared
// `get "/integrations/widgets", as: :integration_widgets` inside `namespace :app`:
// RouteSet expands it to app/integrations#widgets, this repository contains no
// app/controllers/app/integrations_controller.rb, and the answer it replaced named
// an action with a slash in it that no controller defines. 27 routes over the four
// `scope controller:` sites now name the controller Rails serves them from — 21 of
// them previously named app/companies or app/api/companies, which exist and serve
// other routes — and route handled_by edges resolving to a symbol the graph holds
// go 2,289 -> 2,315.
//
// The bump is the PROVENANCE argument in the header, not the cache one, for the same
// reason v217's was: buildIdentity already mixes the executable into every entry, so
// a cache written by a different binary is discarded either way, while
// WarnVersionMismatch and append mode's discard both key on ExtractorVersion — and
// without a bump a baseline pinned by a v217 build grades this one's 47 moved route
// facts as no architectural change at all.
//
// benchmarks/rails-controller-derivation scores each of the four, expanded through
// ActionDispatch::Routing::RouteSet on actionpack 8.1.3 and again on 8.1.1, which
// agreed line for line.
// v219: two Ruby method facts for the query-loops reader, both measured after
// a reviewer rejected two of the first sixteen findings on the monolith.
// `preloads` names every association handed to includes / preload / eager_load
// in the body (symbols, hash keys and values), so an association read on the
// elements of a preloaded relation stops reading as a query per element
// (CannedResponsesQuery#resolve). `unpersisted_locals` names the locals in
// local_types whose typing call was `new`, so association reads on a record
// that was never saved stop reading as queries while writes on it still do
// (BlockLayoutsController#mock_company). Recorded, not resolved: the extractor
// states what the method preloaded and how it typed a local, and the
// explainer joins.
// v220: the query-loops reader follows the relation to where it was built.
// Block bindings carry the receiver chain with arguments dropped and locals
// spliced (`user=Current.company.users.allowed_to_login.preload`), scope facts
// carry `model` and the associations their lambda preloads, method facts carry
// `params` and a `batch_loader` marker. The explainer resolves the chain back
// to its association or constant, joins preloads stated by scopes on that model
// and by same-class methods, reports name-only typing at half confidence, and
// stays quiet inside BatchLoader bodies. Measured on the monolith and the
// sibling branch that first rejected the reader's blind spots.
// v221: a block binding is recorded for a constant or namespaced receiver too
// (`Company.find_each do |company|`, `Billing::Invoice.in_batches`), so the
// query-loops reader, which already types a chain from the model at its base,
// hears the most Rails way to walk a table. A constant that is not a model
// resolves to nothing and stays silent.
// v222: two more references the Ruby extractor states. A symbol-to-proc block
// argument (`each(&:destroy_with_publication!)`) records a call to the symbol's
// method; the class-body DSL that names methods by symbol folds in beside the
// callbacks: `field`, `helper_method`, `alias_method`'s old name,
// `rescue_from ... with:`, and the `if:`/`unless:` symbol options of
// callbacks and validations. Both closed false "dead" readings in the
// dead-methods explainer's prototype on the monolith.
// v223: repo-relative fact paths are forward-slash on every host. They used to
// carry the host separator, because the walker handed extractors what
// filepath.Rel returned and the extractors built on it with path/filepath, whose
// output is host-flavoured — so on Windows a module was named `src\lib` while the
// import targets that had to match it, read out of source text, were
// forward-slash. Nothing errored: module resolution simply stopped matching, and
// a declared layer order reported itself in force at confidence 1.00 while
// classifying zero modules (issue #242). Paths are now normalised at the walker,
// built with internal/factpath, and backstopped in Store.Add.
//
// The bump matters on Windows and only there, which is exactly why it is easy to
// forget: a Windows user upgrading into this build has a cache keyed by the old
// version whose entries hold backslash paths, and reusing one would serve the bug
// this release fixes from a binary that no longer contains it. Upstream shipped
// this change as its v216 in 0.4.2; this channel had already spent v216 through
// v222, so the same behaviour change takes the next free number here.
// v224: a class owns the members it mixed in. The mixin-owner binder follows
// each literal include, extend or prepend to the module fact of the same name
// and writes has_method relations from the includer to the module's members,
// recording each projected member and its mixin kind on the includer. Ownership
// moves findings (dead-methods surfaces, god-class fan-in through has_method,
// constraint member sets), so the version moves with it.
// v225: where a framework enters the code is a fact. The framework-roots
// binder sets root on Rails routes and the actions they resolve to, and on the
// methods a framework invokes on a class the store ties to it (job perform,
// mailer actions, channel hooks, migration steps, methods a class body names
// as callbacks), then walks calls from every root and sets reached_from on
// what it reaches. Entry points and dead-methods read the props.
// v226: every markdown file in scope is a source. mdintent detects any
// markdown tree and, for a file declaring no intent, emits a document symbol,
// a section symbol per heading and a names relation per link that resolves on
// disk, counting the links that do not on its extraction fact.
// v227: a client path that reaches the call through a method parameter is
// derived from the literals the method's callers pass (one hop, same file),
// marked derived=parameter, with the non-literal and ambiguous cases counted
// on the ruby:http-client extraction fact.
// v228: forbid_name, the negative of require_name: a component's members must
// not match a bounded pattern, optionally judged on exported members only.
// A new rule form compiles into a new intent fact shape.
// v229: Ruby records what it constructs. `new` on a literal constant emits an
// instantiates relation from the calling member to the class, and a
// construction immediately sent a message names the ceremony under
// one_shot_call, from the tree-sitter extractor and the Prism provider alike.
// v230: a fact carries the span its extractor measured (end line, start and
// end column beside the start line), evidence carries the span of the fact it
// names, and check prints the offending line with the span underlined.
// v231: a provider's facts pass the repository's ignore globs. A provider
// walks the tree itself and cannot know the configuration, so a vendored tree
// the extractors never read was entering the graph through the seam; the drop
// is counted on the provider's census.
// v232: the module layer has the edges its symbols imply. A post-link binder
// rolls every resolved call, dependency and instantiation up to the modules
// declaring the two symbols and emits a weighted dependency fact for the
// pairs no extractor connected, so the readings that walk modules answer on
// languages without import statements.
// v233: a repository may write its laws in Ruby. The constraints directory
// reads .rb files through a surface that parses (never executes) them and
// compiles to the same declaration YAML produces, so both spellings merge,
// stamp and evaluate identically.
// v234: a part declared identically in two files is the same part, so a
// repository may keep one file per convention without renaming the pieces of
// the application each one speaks about. A name reused for a different
// selector is still the error.
// v235: the Ruby surface says everything the declaration vocabulary says.
// Explicit ids, literal far ends, the require form's antecedents, direction,
// guidance exemplars and recipe instantiation all have sentences, and a test
// walks the schema so a key added later without one fails the build.
// v236: convention sets ship with the binary. A repository instantiates
// rails-conventions without authoring it, and a local recipe of the same name
// replaces the shipped one with the replacement reported.
// v237: Rust server routes declared by `#[utoipa::path(get, path = "/x")]`,
// upstream's v224 in 0.4.3, taking the next free number here because this
// channel had already spent v224 through v236. The utoipa_axum
// `routes!(handler)` macro registers a handler without repeating its path, so
// an API declared that way has no `.route("/path", ...)` call to read and its
// endpoints were absent from the graph entirely: one corpus application stored
// 8 routes where it serves 74, and every call its own frontend made to them
// looked like a call to nothing. The attribute is now the second Rust route
// source.
// v238: a component may select by ancestry. `ancestor:` names a class every
// member descends from, read transitively over the resolved implements edges a
// provider emits, and refused with a named advisory when the store holds no
// resolved ancestry at all. The selector changes which facts are members, so
// every constraint verdict can move with it, and the version moves too.
// v239: three forms join the constraint vocabulary. forbid_cycles/among
// contracts the module graph to one node per declared part, with the rollup
// edges admitted, and reports every circle; independent states that a module
// never reaches a class whose resolved ancestry includes it; require_defines
// takes any_of beside method. Verdicts move with the forms.
// v240: five small spellings. require_name takes requires, a template that
// pairs a name with a sibling through the captured base; a component takes
// public, the files that are its visible surface, which private reads beside
// the measured exported prop; a forbid with to_name takes receiver: none to
// match only receiver-less calls; and constraints explain reads membership
// per file. Verdicts move with the first three.
// v241: laws only a graph can state. Component keys handles (the code behind
// routes by method) and governed_by (the files a compiled page anchors); forms
// storage_stays_home, cap_runtime, require_consumer, unique_across and
// require_governed; since and growth on rules; recipe roles carry selector
// defaults, so a repository's own recipe binds in one line; every edge and
// cycle verdict names its smallest cut.
// v242: Angular's decorator-declared classes and its dependency injection. A
// class carrying @Component/@Directive/@Pipe/@Injectable/@NgModule now records the
// role the container gives it, its selector or pipe name, where its template lives,
// and framework_registered — without which a container-instantiated class reads as
// code nothing names. Constructor parameters and inject() initializers become
// injects edges, resolved through the file's own import table or a class the file
// declares and COUNTED as unresolved otherwise; across a ten-repository corpus that
// is 21,418 injection sites which previously formed no edge at all. Everything is
// gated on an @angular/core dependency, so a decorator of the same name in another
// repository still models nothing.
// v243: Angular's routes. A route array declares a path fragment; the prefix it
// hangs under is decided by a parent's children:, by the entry whose loadChildren
// lazily loads the module the array belongs to, or by nothing at all — so the paths
// are composed by a repo-wide walk outward from the application roots (forRoot and
// provideRouter), the shape the Express, gorilla/mux and Axum passes already share.
// A lazy module names no array, so one is found by an exact export name, by the
// target file's single forChild array, or by the single one among that file's own
// imports; anything ambiguous is counted rather than guessed, and an array no root
// reaches emits nothing rather than a fragment. Every fact carries type=page, so an
// application's navigation can never surface as an unserved HTTP endpoint.
// v244: three resolution fixes in the TypeScript path resolver and the Angular
// router, each found by running the router against real workspaces.
// resolveModuleFile now accepts a path that already names a file, because a
// tsconfig EXACT alias maps a bare specifier onto its entry point with the
// extension included and appending another matched nothing; a wildcard alias keeps
// whatever follows the `*` in its target, instead of resolving one directory short;
// and a route path written as a constant member is folded to the literal it names,
// through an enum or an `as const` map in this file or the one it was imported
// from. The first two are TypeScript-wide and affect every repository with a
// workspace-style tsconfig, not only Angular ones.
// v245: Angular templates. A component member is very often referenced ONLY from
// its template — `(click)="save()"`, `{{ total }}` — and so is a child component,
// which appears as a tag and nowhere else in the class; 4,251 external templates and
// 10,844 inline ones were previously walked past, so every such symbol read as code
// nothing calls. Templates are now scanned (both the older `*ngIf` dialect and
// Angular 17 `@if`/`@for`/`@defer` blocks) and joined to the component that owns
// them. A binding is an edge only when it names a member that component declares; a
// tag resolves against a DECLARED selector, matched whole so a compound selector
// needs both of its halves; anything else is counted by cause, never guessed. The
// extractor now also owns .html for cache-invalidation purposes, since a template
// edit changes what it emits.
// v246: the Angular composition graph and the workspace shape. An application's
// dependency structure is in its @NgModule declarations/imports/exports/providers
// arrays and a standalone component's own imports — not in its import statements,
// which say which files were loaded rather than which declarations were assembled.
// Those arrays are now edges, resolved through the same import table as the
// injection edges and reconciled the same way, so none of them names a node that
// does not exist. Module facts additionally carry the workspace project that owns
// their directory, read from an Nx project.json or an angular.json projects map:
// in a monorepo the unit of ownership is the project, and every reading that groups
// by unit was inferring that boundary from the path.
// v247: Angular requests made through an injected HttpClient. The general client
// pass requires a "/"-rooted literal, which is right when the receiver is anonymous
// — it is what keeps map.get("key") out of the graph — and wrong here: a class that
// injects HttpClient has a member whose declared type says so, and this.<that
// member>.get(…) is a request whatever its argument looks like. Two shapes that rule
// was rejecting are now read: a path with no leading slash (one client's whole
// module contributed nothing) and a class-static base concatenated with a literal
// tail, folded repo-wide because the base belongs to the service that owns the
// resource and is named by every service that touches it. An unresolved LEADING
// operand means the prefix is unknown and the call contributes nothing.
// v248: three corrections found by auditing what the explainers now report on
// Angular repositories. A tsconfig `paths` target is resolved against `baseUrl`,
// which TypeScript does and this did not — in one workspace that meant every
// aliased import resolved to nothing, and with it every module composition edge
// those imports carry. `loadComponent: () => import('./page')` binds to the class
// that file declares, so a page reachable only through a lazy route is no longer a
// component nothing renders. And framework_registered is now set ONLY on NgModules:
// after the template, composition and injection passes a component, directive, pipe
// or service is named by edges the graph holds, and flagging them as
// framework-invoked would suppress the dead code those edges make findable.
// v249: dependency injection reaches the module layer. The module-edges binder
// rolled up calls, dependencies and instantiations but not injections — and a
// constructor parameter IS how a dependency is declared under a DI container, with
// frequently no call, instantiation or import edge beside it to carry the pair.
// Adding the relation takes an Angular storefront library from 3,895 derived module
// edges to 6,128, a Java monolith from 1,407 to 1,700 and an ASP.NET Core media
// server from 68 to 97. Every reading that walks the module graph moves with it,
// by value rather than by count: no explainer reports a different NUMBER of
// findings on the corpus, while one storefront's deepest chain becomes 78 not 77.
// v250: a markdown link resolves against the walked files, not the filesystem.
// mdintent stat'd each link target on disk, so a repository whose documentation
// names paths under its own output directory produced a different fact stream on
// every run: this one's docs cite `.enola/extractor_cache.json`, absent on a cold
// run and present on the next, and `.enola/previous`, which the run after that
// creates. Three passes, three hashes, on the one corpus row whose docs describe
// enola — the property the whole reproducibility claim rests on, broken by reading
// the disk instead of the file list the walker had already filtered.
// v251: a markdown document is not a public surface. Sections became exported
// symbols when mdintent started reading every page, and exported-surface read a
// document as a module exporting its whole API — a changelog "exports 1,405 of
// 1,405 symbols (100%)", 22 such findings across the corpus. Markdown joins Ruby as
// a language whose symbols carry no visibility signal, so the ratio measures the
// module rather than the format.
// v252: the output directory is ignored at any depth, not only at the repository
// root. A cluster config that snapshots subdirectories leaves an `.enola` in each,
// and only the rooted glob covered them — so enola indexed its own llm_context.md
// as a source document, and a repository's fact count depended on which of its
// subdirectories somebody had snapshotted before. Harmless while a stray markdown
// file was merely unread; a document with a section per heading once every page
// became a source.
// v253: detection stopped re-walking the tree. Every extractor answered Detect with
// its own bounded walk, and every bound was a cliff a real repository falls off:
// dotnet/runtime keeps all 3,270 of its C/C++ sources below the three levels the C++
// detector scanned, so the extractor never ran and 5,574 files it owns were missing
// from the graph. Raising a bound only moves the cliff — flutterfire's Java sits at
// depth 10, past the generous 8 the JVM extractors allowed — and root-anchored rules
// failed the same way without looking like depth at all: ente's Go backend is at
// server/go.mod, so 493 Go files went unindexed in a repository being read as a
// cross-repo cluster. Detection is now membership over the names the engine already
// walked (plugin.FileListDetector), which has no bound to beat and costs no walk;
// across a 20-repository polyglot corpus this recovered 13,496 of 13,828 files that
// an extractor claimed and no extractor read. The remaining 332 are .html and loose
// .js that OwnsFile over-claims and detection correctly declines.
// v254: the Rubydex provider emits one dependency per qualified read, for the
// leaf, carrying the file that defines it as target_file; the segments before
// the leaf are its path, not dependencies. A read that resolves to nothing, or
// to a constant alias, is a dependency fact with no relation and a named
// resolution_cause. A consumer resolving a target by name prefers the carried
// file when the name is defined in several, so a read of a reopened module's
// member no longer lands on a reopening. Verdicts move with the edges.
// v255: provider facts enter this cache. A per-file provider (files: per-file in
// its config entry) keeps one entry per file keyed by its name, reported version
// and the file's content digest, and runs only over the files with no entry; the
// built-in Rubydex provider keeps one whole-index entry keyed by the engine
// library version, the Ruby file set with content digests and Gemfile.lock, with
// its census beside it. The receipt's provider block says what was reused.
// v256: the provider seam spells receivers once and pairs what two producers
// read identically. A singleton notation is unified by a table the seam owns;
// a call relation two producers emitted at the same file, line and callee is
// kept once under the first producer in name order, in the scope-bearing
// spelling, stamped resolution_agreement; differing receivers stay as emitted
// and are counted by shape in the receipt. The merged fact set changes for
// every Ruby repository with both providers on.
// v257: a repository's DECLARED DIRECT dependencies enter the graph. The new
// manifests extractor reads go.mod, package.json, Gemfile, Cargo.toml,
// pubspec.yaml, requirements.txt and pyproject.toml — off-glob, because the
// ignore globs hide most of them — and emits one KindDependency fact per
// package, typed `package` to tell it apart from the import-edge dependency
// facts every language extractor already emits. Each carries the constraint as
// written, the version a lockfile beside the manifest resolved it to, and the
// `pinned` boolean those two decide. Transitive entries are skipped: the
// closure would add tens of thousands of nodes, and the top-level set is the
// boundary the CRA's bill-of-materials requirement draws. Every repository with
// a manifest gains facts.
//
// Three things the corpus corrected before this shipped. A lockfile is searched
// for AT OR ABOVE the manifest, because a monorepo keeps one at the workspace
// root; yarn.lock is read in both its formats; and `pinned` is left ABSENT,
// with `unresolved_lock` naming the file, when a lockfile enola cannot read
// sits beside the manifest — answering `false` there reported twelve of
// excalidraw's resolved dependencies as unpinned. Cargo's `[dependencies.name]`
// table sections are read too: tokio declares windows-sys only that way, so the
// list form alone lost the dependency rather than its version.
// v258: a Vue Single File Component's TEMPLATE becomes edges. Interpolations and
// directive values are resolved against the SFC's own declarations, and component
// tags through default/named imports, local aliases and unambiguous Nuxt
// `components/` auto-import conventions; the references that resolve become
// `calls` edges on the component, so a handler or child used only from the
// template stops reading as an orphan. `<script setup>` macros (defineProps,
// defineEmits, defineSlots, defineModel, defineExpose, defineOptions) are
// recorded on the component as `vue_macros` plus a boolean per macro, with the
// statically declared names in `vue_*_names` and generic declaration text in
// `vue_contract_types`. Nuxt page routes cover the other supported page
// extensions, omit route-group directories, and drop named-view/client/server
// suffixes from the URL; Vue Router `createRouter({ routes })` records with
// literal paths emit composed page routes whose statically or lazily imported
// components become `handled_by` edges. A Nuxt auto-imported `useXxx()` call is
// rebound to its unique exported declaration under `composables/`. Every Vue and
// Nuxt repository gains facts, and existing ones change shape.
//
// Resolution is deliberately narrow where it cannot be sure: macro-looking text
// inside comments and strings is ignored, HTML text and CSS tokens are not read
// as code, and an ambiguous auto-imported composable — two layers exporting the
// same name — stays unresolved rather than binding to whichever was seen first.
//
// v259: schema-first GraphQL servers emit their Query/Mutation/Subscription
// surface from typed and suffixed TypeScript SDL bindings (`schema: string`,
// `gqlSchema`, `userTypeDefs`) and provenance-backed standalone .graphql/.gql
// documents (a server-file import or Hasura metadata). Server activation comes
// from the TypeScript AST, not comments; integrationTests is test-only.
// v260: C++ calls on explicitly typed parameter and local-variable receivers
// resolve to a uniquely declared method on that type. Auto-deduced, wrapped,
// unknown and ambiguous receiver types remain unresolved rather than being
// guessed, and a receiver call binds only when the method actually exists.
// v261: a Python import that names a module in the importer's OWN package is
// classified internal and bound to that sibling file module. Previously the
// directory index could only match the shared parent — the importer's own dir —
// and a self-match was abandoned, so `from app.db import x` written inside app/
// was recorded as a THIRD-PARTY dependency with an unresolved dotted target. Every
// such edge was missing from the module graph, including a package __init__.py's
// re-exports of its own submodules, which truncated any import-closure walk at the
// barrel. Inheritance bound through such an import (and through every relative
// import, latent until now) also resolves: the composed "<module>.<symbol>" is
// already canonical when the module is a slash path, so it no longer goes to the
// dotted resolver that cannot read one.
// v262: a Python absolute import binds to the MODULE it names, not to the package
// directory that also matches one segment shorter — `from pkg.shared.logging import
// x` resolves to pkg/shared/logging, where it previously resolved to pkg/shared and
// became indistinguishable from an import of the package itself. This is the
// granularity the extractor's own relative-import handling has always produced, and
// the one TypeScript and Ruby produce for their file-addressed imports; the package
// directory remains the answer when the dotted path names a package. Module-level
// metrics are unchanged, since explainers roll a file target up to its directory.
// v263: Python import facts carry Props["deferred"]=true when the import does not
// run at module-import time — a function/class-body (lazy) import, or an
// `if TYPE_CHECKING:` block, which never executes at all. Absence means the import
// IS on the import path, following `from`. This is the distinction an import-closure
// walk needs and the one source indentation cannot draw: a module-level
// `try: import x` is indented yet runs, a TYPE_CHECKING import is indented and does
// not. Also fixes a method-local import being emitted twice with disagreeing props,
// because walkNestedScope registers body imports a second time.
// v264: the Python stdlib set is completed against Python's own
// sys.stdlib_module_names. Forty-two modules were missing — atexit, binascii,
// colorsys, optparse, curses among them — and a stdlib name absent from that set is
// classified as a third-party dependency, so it appeared in dependency breakdowns and
// on import paths as a package the project had taken on.
// v265: a yarn.lock descriptor whose range carries an `@` of its own resolves.
// The name/range separator was read as the LAST `@` in the spec, so an npm alias
// (`"@typescript/native": "npm:typescript@7.0.2"`, whose descriptor is
// `@typescript/native@npm:typescript@7.0.2`) was named
// `@typescript/native@npm:typescript`, matched no manifest entry, and the
// dependency the committed lockfile pins to one version was reported unpinned —
// two of mastodon's 298 direct packages, and every git+ssh range, on the `@`
// inside the URL. The separator is now the last `@` before the range's protocol
// colon, and a spec is unquoted per entry, since a multi-spec header quotes the
// whole list and the comma split leaves each end with a lone quote.
// v266: TypeScript reads requests made through in-house clients the config declares
// (clients:, internal/clientspec). `this.<member>.<method>(…)` on a member whose
// declared type a spec names becomes a client route: its path folded through the
// enclosing class's own fields and single-assigned locals, its verb read from an
// options object, its service name kept as a target hint. The declared clients enter
// the extractor's cache key, so editing them re-extracts.
// v267: TypeScript accounts for each declared in-house client per repository: one
// extraction fact per client (typescript:client:<name>) with its receivers, the calls
// made through it, the routes they became and the calls skipped by cause. Emitted even
// when a client found nothing, so a client that matches nothing anywhere is reported.
// v268: a Go package's own top-level function shadows the predeclared identifier of
// the same name, so `min(a, b)` in a package declaring min resolves to that function
// instead of being dropped as a builtin. Repositories predating Go 1.21 carry their
// own min/max helpers, and with no incoming edge the dead-code analyzer reported them
// at high confidence while they were called. Also adds `benches` to the shared
// test-path segments (Cargo builds it as bench targets), which the TypeScript
// extractor consults.
// v269: TypeScript path aliases follow relative tsconfig extends chains, preserving
// child-over-parent replacement and resolving each paths target relative to the file
// that declared it. JSONC comments and trailing commas are accepted as TypeScript does.
// v270: SvelteKit reads literal kit.alias fallbacks before generated config exists,
// keeps tsconfig paths authoritative, and classifies $app/$env/$service-worker imports
// as framework-provided rather than unresolved third-party dependencies.
const cacheVersion = "v276"

// ExtractorVersion is cacheVersion, named for callers outside this package.
//
// It is the same value facts.SnapshotMeta.ExtractorVersion records: what this build
// EXTRACTS LIKE, as opposed to what it is called. Two places need it from outside —
// the release manifest (`enola --version --json`, so a later build can tell whether
// the extractors moved) and the update notice that reads that manifest back.
//
// A function rather than an exported constant, so the changelog above stays the single
// place the value is written and nothing can come to reference a second copy of it.
func ExtractorVersion() string { return cacheVersion }

// extractorCache holds per-extractor facts keyed by a content hash of the files
// the extractor depends on. It is loaded from disk at the start of a snapshot and
// written back at the end, carrying forward only the keys used this run so stale
// entries are garbage-collected.
//
// Reuse is correct because an extractor is a deterministic function of its inputs
// (verified: parallel and serial runs produce byte-identical facts), and a key
// captures every input that can change its output — see computeExtractorKeys.
// # Write-through
//
// Entries are written to disk AS THEY ARE PRODUCED, into a temp file opened when the
// cache is created and renamed into place by save. Nothing accumulates in memory
// waiting to be written.
//
// Holding them was the single largest allocation in a snapshot. On a kernel-sized
// repository one extractor produces one 800 MB entry, and both routes retained it
// for the rest of the run: put marshalled the facts into a buffer (reaching 1.5 GB
// through bytes.Buffer's doubling — half the live heap at the measured peak), and on
// a warm run get moved the decoded bytes from prev to next, keeping them alive
// alongside the facts they had just been decoded into.
//
// Writing costs no more than it did — the same bytes reach the same file — it just
// happens earlier, and each entry becomes collectable the moment it is written.
type extractorCache struct {
	prev map[string]json.RawMessage // loaded from disk, drained by get
	hits int

	// noPersist records that save will never be called. Such a cache still SERVES
	// entries — a read-only mode should reuse a warm cache, that is most of what
	// makes it fast — but opens no temp file and writes nothing, because there is
	// nowhere for it to go. `enola --explain` sets it, via SetPersistCache(false).
	//
	// Negative form so the ZERO VALUE writes nothing rather than writing to a nil
	// file. Stated positively, a struct literal that forgot the field would try to
	// stream into a closed spool.
	noPersist bool

	// Spool state. dest is the final path; tmp lives beside it so the rename cannot
	// cross a filesystem boundary (os.Rename returns EXDEV and loses atomicity).
	dest    string
	tmp     *os.File
	w       *bufio.Writer
	entries int   // entries emitted this run
	werr    error // first write/encode failure; surfaced by save
	closed  bool  // save or discard has run; further writes are refused
}

// buildIdentity identifies the binary that produced a cache entry: the release
// version, plus the executable's size and modification time.
//
// cacheVersion alone is not enough, because it is a constant a human has to remember
// to bump. When an extractor's behaviour changes and the bump is missed — routine
// while developing, and possible in a release — every cache written by the old binary
// keeps being served by the new one, silently, for files whose contents never changed.
// That is not a theoretical risk: it produced a snapshot 568 facts larger than a
// cold run of the same binary on the same tree, which then surfaced as hundreds of
// phantom "removed" facts in a diff against a snapshot taken after the cache caught up.
// For a tool whose value rests on "a clean diff means something", facts that depend on
// which binary happened to write the cache are the most damaging kind of wrong.
//
// Size and mtime rather than a content hash: the binary is tens of megabytes and this
// runs on every snapshot, whereas a stat is free. The failure mode of the cheap check
// is a needless re-parse (a rebuilt-but-identical binary), never a stale reuse.
func buildIdentity() string {
	id := version.Version
	if exe, err := os.Executable(); err == nil {
		if fi, err := os.Stat(exe); err == nil {
			id = fmt.Sprintf("%s|%d|%d", id, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return id
}

// cacheFile is the on-disk shape of the extractor cache. Neither load nor save goes
// through it — both stream (see decode/encode) so a large cache is never materialized
// as one buffer — but it remains the authoritative declaration of the encoding, and
// the field order here is the order encode writes: version and build BEFORE entries,
// which is what lets decode reject a stale cache without parsing a single entry.
type cacheFile struct {
	Version string                     `json:"version"`
	Build   string                     `json:"build,omitempty"`
	Entries map[string]json.RawMessage `json:"entries"`
}

// errColdCache is the single internal signal for "this file cannot be reused" —
// unreadable, malformed, a foreign schema, or a foreign build. Every one degrades to
// a full run, so decode does not distinguish them to its caller.
var errColdCache = errors.New("cold cache")

// loadExtractorCache reads the cache file at path. A missing, unreadable, or
// foreign-build file yields an empty (but usable) cache, so caching degrades to a
// full run rather than to wrong facts.
//
// It STREAMS rather than reading the file whole. An extractor cache reaches 800 MB on
// a kernel-sized repository, and os.ReadFile + json.Unmarshal held the file bytes and
// the decoded entry map — each about that size — alive simultaneously, at the exact
// point in a run where the fact store is also being built. Decoding entry by entry
// retains only the entries.
func loadExtractorCache(path string, persist bool) *extractorCache {
	c := &extractorCache{
		prev:      map[string]json.RawMessage{},
		noPersist: !persist,
		dest:      path,
	}
	if persist {
		// A spool that cannot be opened degrades this run to no caching rather than
		// failing it: the snapshot is still correct, just cold next time. Reported,
		// because a repository that silently never caches looks like a slow enola.
		if err := c.openSpool(); err != nil {
			log.Printf("[engine] extractor cache not writable (%v); continuing without saving it", err)
			c.noPersist = true
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return c
	}
	defer func() { _ = f.Close() }() // read-only; a close error cannot affect what was read

	if err := c.decode(bufio.NewReaderSize(f, cacheBufSize)); err != nil {
		// A mismatch or a truncated file may have left entries behind. Drop them:
		// a partially populated cache would serve some extractors stale facts and
		// re-parse the rest, which is the one outcome worse than a cold run.
		c.prev = map[string]json.RawMessage{}
	}
	return c
}

// cacheBufSize buffers the streamed read and write. Large enough that an 800 MB cache
// is not a syscall per entry, small enough to be irrelevant next to the entries.
const cacheBufSize = 1 << 20

// decode streams a cacheFile from r into c.prev.
//
// The version and build checks fire as their fields arrive, before `entries` is
// reached, so the common invalidation cases (a cacheVersion bump, a rebuilt binary)
// abort after a few dozen bytes instead of after decoding the whole file only to throw
// it away.
func (c *extractorCache) decode(r io.Reader) error {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return errColdCache
	}
	var sawVersion, sawBuild bool
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		switch key {
		case "version":
			var v string
			if err := dec.Decode(&v); err != nil {
				return err
			}
			if v != cacheVersion {
				return errColdCache // schema mismatch
			}
			sawVersion = true
		case "build":
			// A cache written by a different binary is discarded wholesale. Entries
			// carry no record of which extractor logic produced them, so there is no
			// safe way to reuse part of it — and a cache written before this field
			// existed has no "build" key at all, so sawBuild stays false and the
			// entries case below rejects it, exactly as the empty-string comparison
			// used to.
			var b string
			if err := dec.Decode(&b); err != nil {
				return err
			}
			if b != buildIdentity() {
				return errColdCache
			}
			sawBuild = true
		case "entries":
			if !sawVersion || !sawBuild {
				// Both must have been seen AND matched by now. encode always writes
				// them first; a file that does not is not one of ours.
				return errColdCache
			}
			if err := c.decodeEntries(dec); err != nil {
				return err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeEntries reads the entries object one value at a time, so the largest single
// allocation is the largest extractor's facts rather than the whole file.
func (c *extractorCache) decodeEntries(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil // "entries": null — a cache with nothing in it
	}
	if tok != json.Delim('{') {
		return errColdCache
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return errColdCache
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		c.prev[name] = raw
	}
	_, err = dec.Token() // closing '}' of entries
	return err
}

// get returns the cached facts for key (deep-copied from JSON, so the caller may
// mutate them freely) and carries the original bytes forward to the next save.
func (c *extractorCache) get(key string) ([]facts.Fact, bool) {
	raw, ok := c.prev[key]
	if !ok {
		return nil, false
	}
	var ff []facts.Fact
	if err := json.Unmarshal(raw, &ff); err != nil {
		return nil, false
	}
	// Write the clean, pre-mutation bytes straight through to the spool. They are
	// already in memory, so this costs a copy to a buffered file and buys their
	// release: before write-through they stayed live until save, next to the facts
	// they had just been decoded into.
	c.writeEntry(key, raw)
	// Drop the last reference. Each key is fetched at most once per run (one lookup
	// per extractor in runExtractors), so keeping it in prev only pinned every
	// reused entry for the rest of the run — on a fully-warm kernel run, 800 MB of
	// bytes whose only remaining purpose was to be copied to disk unchanged.
	delete(c.prev, key)
	c.hits++
	return ff, true
}

// put stores ff for key. It marshals immediately (before the engine tags or
// otherwise mutates the facts) so the persisted bytes stay clean.
//
// A no-op when this cache will never be saved: encoding is the single largest
// allocation in a snapshot, and doing it for bytes nobody will write is the most
// expensive way to do nothing. See the noPersist field.
//
// The facts are encoded ONE AT A TIME into the spool's buffer rather than through
// json.Marshal (or json.Encoder, which is no better — it marshals the whole value
// into an internal buffer before writing a byte). A whole extractor's output is
// 800 MB on a kernel-sized repository; a single fact is a few hundred bytes, and
// becomes garbage as soon as it is written.
// peek decodes an entry without carrying it forward or dropping it, for a caller
// that is about to replace the entry under the same key: carrying it forward and
// then writing the replacement would put the key in the spool twice.
func (c *extractorCache) peek(key string) ([]facts.Fact, bool) {
	raw, ok := c.prev[key]
	if !ok {
		return nil, false
	}
	var ff []facts.Fact
	if err := json.Unmarshal(raw, &ff); err != nil {
		return nil, false
	}
	return ff, true
}

// providerCache is the view of the extractor cache the provider seam receives:
// the same spool, the same version and build stamps, keys scoped under a
// provider namespace so a provider entry can never collide with an extractor's.
type providerCache struct{ c *extractorCache }

func (p providerCache) key(key string) string {
	return cacheVersion + "\x00provider\x00" + key
}

func (p providerCache) Get(key string) ([]facts.Fact, bool) { return p.c.get(p.key(key)) }
func (p providerCache) Peek(key string) ([]facts.Fact, bool) {
	return p.c.peek(p.key(key))
}
func (p providerCache) Put(key string, ff []facts.Fact) { p.c.put(p.key(key), ff) }

func (c *extractorCache) put(key string, ff []facts.Fact) {
	if c.noPersist || c.closed {
		return
	}
	if !c.beginEntry(key) {
		return
	}
	_, _ = c.w.WriteString("[")
	for i := range ff {
		if i > 0 {
			_, _ = c.w.WriteString(",")
		}
		b, err := json.Marshal(ff[i])
		if err != nil {
			c.fail(fmt.Errorf("encoding cached fact %q: %w", ff[i].Name, err))
			return
		}
		_, _ = c.w.Write(b)
	}
	_, _ = c.w.WriteString("]")
}

// openSpool creates the temp file this run streams into and writes the header.
//
// The header goes out FIRST and in this order because decode depends on it: version
// and build precede entries, which is what lets a stale cache be rejected after a few
// dozen bytes instead of after parsing 800 MB and throwing it away.
func (c *extractorCache) openSpool() error {
	if err := os.MkdirAll(filepath.Dir(c.dest), 0o755); err != nil {
		return err
	}
	// Staged in the destination directory so the rename stays on one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(c.dest), "."+filepath.Base(c.dest)+".tmp-")
	if err != nil {
		return err
	}
	c.tmp = tmp
	c.w = bufio.NewWriterSize(tmp, cacheBufSize)

	ver, err := json.Marshal(cacheVersion)
	if err != nil {
		return c.abandonSpool(err)
	}
	build, err := json.Marshal(buildIdentity())
	if err != nil {
		return c.abandonSpool(err)
	}
	_, _ = c.w.WriteString(`{"version":`)
	_, _ = c.w.Write(ver)
	_, _ = c.w.WriteString(`,"build":`)
	_, _ = c.w.Write(build)
	_, _ = c.w.WriteString(`,"entries":{`)
	return nil
}

// abandonSpool tears down a half-opened spool and returns err, so openSpool's
// failure paths cannot leak the temp file.
func (c *extractorCache) abandonSpool(err error) error {
	if c.tmp != nil {
		name := c.tmp.Name()
		_ = c.tmp.Close()
		_ = os.Remove(name)
		c.tmp, c.w = nil, nil
	}
	return err
}

// beginEntry writes the `"key":` prefix (and the separating comma after the first
// entry), reporting whether the caller should go on to write the value. It returns
// false once a write has failed, so a broken spool stops accumulating rather than
// producing a file that is half-valid.
func (c *extractorCache) beginEntry(key string) bool {
	if c.noPersist || c.closed || c.werr != nil {
		return false
	}
	kb, err := json.Marshal(key)
	if err != nil {
		c.fail(err)
		return false
	}
	if c.entries > 0 {
		_, _ = c.w.WriteString(",")
	}
	c.entries++
	_, _ = c.w.Write(kb)
	_, _ = c.w.WriteString(":")
	return true
}

// writeEntry emits a whole pre-encoded entry, used for bytes carried forward from a
// previous run's file (see get). Empty values are skipped: decode never produces one
// and put never writes one, but emitting `"key":` with no value would make the entire
// file unparseable, which is worth one branch to prevent.
func (c *extractorCache) writeEntry(key string, raw json.RawMessage) {
	if len(raw) == 0 || !c.beginEntry(key) {
		return
	}
	_, _ = c.w.Write(raw)
}

// fail records the first write or encode error. bufio latches its own errors the same
// way, so later writes are harmless no-ops and save reports the original cause.
func (c *extractorCache) fail(err error) {
	if c.werr == nil {
		c.werr = err
	}
}

// save closes the entries object and renames the spool into place, so a reader either
// sees the previous cache or this one and never a half-written file. A process that
// dies mid-run leaves only the temp file, which nothing reads.
//
// It writes to the path the cache was opened with. There is no path argument: the temp
// file has to be created next to its destination for the rename to stay on one
// filesystem, so the destination is fixed when the cache is created, and a save
// elsewhere could not honour it.
//
// Entries appear in the order they were produced — extractor order — rather than
// sorted, which is the one observable change from buffering them. That order is still
// a pure function of the repository and config, so a cache file does not churn between
// runs; it simply is not sorted. Sorting would mean holding every entry to the end,
// which is the thing this exists to stop.
func (c *extractorCache) save() error {
	if c.noPersist || c.closed {
		return nil
	}
	c.closed = true
	if c.werr != nil {
		return c.abandonSpool(c.werr)
	}

	tmpName := c.tmp.Name()
	_, _ = c.w.WriteString("}}")
	if err := c.w.Flush(); err != nil {
		return c.abandonSpool(err)
	}
	if err := c.tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		c.tmp, c.w = nil, nil
		return err
	}
	c.tmp, c.w = nil, nil
	// CreateTemp makes 0600; match the mode os.WriteFile used to produce.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, c.dest); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// discard closes the spool without publishing it. It must be called on every path
// that does not reach save — a snapshot that fails during extraction, say — or the
// temp file survives the process. Safe to call after save, and safe to defer.
func (c *extractorCache) discard() {
	if c.closed {
		return
	}
	c.closed = true
	_ = c.abandonSpool(nil)
}

// computeExtractorKeys returns a cache key for every extractor that implements
// plugin.FileOwner. A key covers exactly the inputs that can change that
// extractor's output:
//
//   - the content hashes of the files it owns, and
//   - a shared hash of every file no FileOwner owns (configs, manifests, and
//     sources of non-cacheable extractors) — these are where detection markers
//     (tsconfig.json, build.gradle, Gemfile, …) live.
//
// Because cross-language source files never affect another language's output, a
// file owned by a *different* FileOwner is correctly excluded from this
// extractor's key. The shared-config hash over-invalidates (any manifest change
// busts every cache) but never under-invalidates, so reuse is always sound.
func computeExtractorKeys(all []extractors.Extractor, files []string, hashes map[string]string) map[string]string {
	owners := map[string]plugin.FileOwner{}
	for _, ext := range all {
		if fo, ok := ext.(plugin.FileOwner); ok {
			owners[ext.Name()] = fo
		}
	}
	if len(owners) == 0 {
		return nil
	}

	// Partition files: per-owner owned lists + the shared (un-owned) remainder.
	// keyFiles is owned ∪ AffectsKey — the full set whose contents feed the key,
	// so a cross-language file that a KeyDependent extractor reads (but does not
	// own) still invalidates its cache. Ownership (and thus the shared remainder)
	// is decided purely by OwnsFile; AffectsKey only widens the key, never the
	// ownership partition.
	owned := map[string][]string{}
	keyFiles := map[string][]string{}
	var shared []string
	for _, f := range files {
		ownedByAny := false
		for name, fo := range owners {
			owns := fo.OwnsFile(f)
			if owns {
				owned[name] = append(owned[name], f)
				ownedByAny = true
			}
			if owns {
				keyFiles[name] = append(keyFiles[name], f)
			} else if kd, ok := fo.(plugin.KeyDependent); ok && kd.AffectsKey(f) {
				keyFiles[name] = append(keyFiles[name], f)
			}
		}
		if !ownedByAny {
			shared = append(shared, f)
		}
	}
	sharedHash := hashFileSet(shared, hashes)

	keys := make(map[string]string, len(owners))
	for name := range owners {
		h := sha256.New()
		h.Write([]byte(cacheVersion + "\x00" + name + "\x00" + sharedHash + "\x00"))
		h.Write([]byte(hashFileSet(keyFiles[name], hashes)))
		// Configuration the extractor reads decides its facts as surely as its files
		// do. Written only when non-empty, so an extractor with nothing configured keys
		// exactly as before and its existing cache stays valid.
		if ck, ok := owners[name].(plugin.ConfigKeyed); ok {
			if k := ck.ConfigKey(); k != "" {
				h.Write([]byte("\x00config\x00" + k))
			}
		}
		keys[name] = hex.EncodeToString(h.Sum(nil))
	}
	return keys
}

// hashFileSet returns a stable hash over (path, contentHash) pairs, sorted by
// path so the result is independent of the input order.
func hashFileSet(files []string, hashes map[string]string) string {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f))
		h.Write([]byte{0})
		h.Write([]byte(hashes[f])) // empty for unreadable files — still deterministic
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// extractorCachePath returns the on-disk location of the cache for a repo.
func extractorCachePath(outDir string) string {
	return strings.TrimRight(outDir, "/") + "/extractor_cache.json"
}
