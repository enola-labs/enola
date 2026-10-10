// Package perf implements the "analyze_performance" MCP tool, which
// estimates per-function algorithmic complexity (Big-O) and ranks performance
// risks — nested loops, expensive calls inside loops (N+1 patterns), recursion,
// and complexity that compounds across the call graph — from an Enola snapshot.
//
// Determinism: every signal is derived from parser-emitted facts (loop nesting
// depth, cyclomatic complexity, call-in-loop targets from the OSS Go extractor,
// plus the call graph). The Big-O labels are a deterministic function of those
// facts, not an LLM guess — they are estimates of structural worst case, not
// proofs.
//
// Engine-boundary note: this package knows nothing about the engine. Register
// takes an accessor that yields the store the latest snapshot published, because
// a tool handler runs per call and must not be pinned to the store that existed
// when it was registered; ExplainSection takes a store outright, because it runs
// once, straight after a snapshot. Keeping the engine out is what lets this code
// live either side of the module boundary.
package perf

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/explainers/queryloops"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/pathglob"
	"github.com/enola-labs/enola/pkg/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Prop keys are the extractors' own string keys, which internal/facts does not
// export as constants. Fact-kind, relation, and symbol-kind values come from it
// (facts.Kind*, facts.Rel*, facts.Symbol*).
const (
	propSymbolKind         = "symbol_kind"
	propExported           = "exported"
	propHandler            = "handler"
	propCyclomatic         = "cyclomatic"
	propLoopDepth          = "loop_depth"
	propLoopCount          = "loop_count"
	propCallsInLoop        = "calls_in_loop"
	propCallsInScalingLoop = "calls_in_scaling_loop" // extractor: in-loop calls inside an unbounded loop
	// extractor: for each entry of calls_in_scaling_loop, in order, the deepest
	// scaling nesting it is called at
	propCallsInScalingLoopDepth = "calls_in_scaling_loop_depth"
	propCallsOnLoopElement      = "calls_on_loop_element" // extractor: in-loop calls handed the loop's element
	propLoopsOverParam          = "loops_over_param"      // extractor: a scaling loop walks a parameter/receiver
	// extractor: for each entry of calls_on_loop_element, in order, the argument
	// position that carries the element (-1 for the receiver)
	propCallsOnLoopElementArg = "calls_on_loop_element_arg"
	// extractor: the parameter positions a scaling loop walks (-1 for the receiver)
	propLoopsOverParamIndex = "loops_over_param_index"
	propRecursiveSelf       = "recursive_self"
	propIODirect            = "io_direct"          // extractor: the body itself makes a network/file/database call
	propIOCalls             = "io_calls"           // extractor: the calls in this body that are I/O entry points
	propPureCalls           = "pure_calls"         // extractor: the calls in this body into a library it describes that perform no I/O
	propPerformsIO          = "performs_io"        // extractor: method transitively performs network/file I/O
	propScalingLoopDepth    = "scaling_loop_depth" // extractor: loop nesting counting only unbounded loops
	propAssociation         = "association"        // Rails association name on a dependency fact
)

// funcInfo is the per-function input to the pure analysis. It is populated from
// the snapshot and contains no facts.* types (module boundary).
type funcInfo struct {
	Name       string
	File       string
	Line       int
	Repo       string
	Package    string
	Exported   bool
	Cyclomatic int
	LoopDepth  int
	// ScalingLoopDepth is the loop nesting depth counting only loops whose bound
	// grows with input — loops over literal/constant collections, range(<const>),
	// fixed varargs, or infinite while(true) event/retry loops are excluded. It is
	// the exponent used for Big-O. HasScalingDepth records whether the extractor
	// emitted it; when it did not (older/other-language extractors), scalingDepth()
	// falls back to LoopDepth so their behavior is unchanged.
	ScalingLoopDepth int
	HasScalingDepth  bool
	LoopCount        int
	// Claimed records that the query-loops explainer already reports a
	// per-iteration query against this symbol. That explainer answers the same
	// question for Ruby from the receiver's type, measured down from 1,698
	// candidates to 97, where the gate here is a keyword heuristic spanning ten
	// languages. Where both can see a symbol the measured answer is the one worth
	// having, and two findings on one loop read as two problems. It suppresses
	// only call-in-loop: nested-loop, compounded and recursion are questions
	// query-loops does not ask.
	Claimed     bool
	CallsInLoop []string // call targets invoked at loop depth >= 1
	// CallsInScalingLoop is the subset of CallsInLoop made inside an input-scaling
	// (unbounded) loop — a call only ever in a bounded loop (literal/constant/while(true))
	// is not an N+1. HasScalingLoopCalls records whether the extractor emitted it; when it
	// did not, scalingLoopCalls() falls back to CallsInLoop so behavior is unchanged.
	CallsInScalingLoop  []string
	HasScalingLoopCalls bool
	// CallDepth is, per CallsInScalingLoop target, the deepest scaling nesting the
	// call sits in. It is what a call-in-loop finding's exponent should be: the
	// function's own scalingDepth() is its deepest nest ANYWHERE, including one
	// that does not contain the call. nil when the extractor did not emit it, in
	// which case callDepth falls back to that function-wide depth.
	CallDepth map[string]int
	// CallsOnLoopElement is the subset of in-loop calls handed an element of the
	// caller's loop, and LoopsOverParam says this function's own loop walks something
	// a caller passed it. Together they identify a call that CONTINUES the caller's
	// traversal instead of starting one per element; see computeEffectiveDepths.
	CallsOnLoopElement []string
	LoopsOverParam     bool
	// ElementArg is, per CallsOnLoopElement target, the argument position that
	// carries the element, and LoopParams the parameter positions this function's
	// scaling loops walk; -1 is the receiver on both sides. They sharpen the pair
	// above from "passes an element" and "loops over a parameter" to "loops over
	// THE parameter the element arrives in". Either is nil when its extractor does
	// not report positions, and continuesWalk then falls back to the pair.
	ElementArg map[string]int
	LoopParams map[int]bool
	Recursive  bool     // extractor flagged a direct self-call
	PerformsIO bool     // extractor flagged transitive network/file I/O
	IODirect   bool     // extractor flagged the body's own I/O call
	IOCalls    []string // extractor: the calls in this body that are I/O entry points
	PureCalls  []string // extractor: the library calls in this body that perform no I/O
	Calls      []string // all resolved call targets
	// BoundedFanout marks a bounded background-job/mailer fan-out (see isBoundedFanout).
	// It is precomputed by markNonScaling rather than derived where it is needed, because
	// computeEffectiveDepths (per-name) and effectiveDepthOf (per-fact) must never disagree
	// about the same function. collect() cannot set it: isBoundedFanout needs the byName
	// resolution index, which does not exist until analyze() builds it.
	BoundedFanout bool
}

// scalingLoopCalls returns the in-loop calls that are N+1 candidates — those inside a
// loop that repeats a non-constant number of times, when the extractor emitted that
// subset, else all in-loop calls. Presence of the prop is the signal, not its length:
// Go/Python/TypeScript/Kotlin emit it even when empty, so an empty subset means "no call
// repeats" rather than "this extractor never computed it" (Ruby, Swift, Java, C/C++, PHP).
func (f funcInfo) scalingLoopCalls() []string {
	if f.HasScalingLoopCalls {
		return f.CallsInScalingLoop
	}
	return f.CallsInLoop
}

// continuesWalk reports whether callee g, called from f's loop, is finishing f's
// traversal rather than starting one of its own per element: f hands it the loop's
// element, and g loops over what it was handed. Such a call adds no depth.
//
// Where both sides report positions the two have to meet. `itemHasQuery(item,
// words)` receives the element first and loops over `words`, a different
// parameter and an independent size, so it multiplies. Without positions (the Go
// extractor reports only the two booleans) the pair alone decides, as it did.
func continuesWalk(f funcInfo, onElement bool, callee string, g funcInfo) bool {
	if !onElement || !g.LoopsOverParam {
		return false
	}
	pos, known := f.ElementArg[callee]
	if !known || g.LoopParams == nil {
		return true
	}
	return g.LoopParams[pos]
}

// callDepth returns the scaling loop depth at which this function calls target, and
// whether the extractor recorded one. Without a record the function's deepest
// scaling nest stands in, which is an upper bound.
func (f funcInfo) callDepth(target string) (int, bool) {
	if f.CallDepth == nil {
		return f.scalingDepth(), false
	}
	return f.CallDepth[target], true
}

// scalingDepth returns the input-scaling loop nesting depth used for Big-O — the
// bounded-loop-discounted depth when the extractor emitted it, else the raw depth.
func (f funcInfo) scalingDepth() int {
	if f.HasScalingDepth {
		return f.ScalingLoopDepth
	}
	return f.LoopDepth
}

// loopDiscounted reports whether bounded loops were discounted from this function's
// scaling depth — used to flag a lower-confidence finding.
func (f funcInfo) loopDiscounted() bool {
	return f.HasScalingDepth && f.ScalingLoopDepth < f.LoopDepth
}

// Finding is one ranked performance risk.
type Finding struct {
	Symbol string `json:"symbol"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Repo   string `json:"repo,omitempty"`
	// Package is the declaring module (the `declares` target — "app/models" on Rails,
	// "app/src/main/java/de/nebenan/app/ui" on Gradle). collect() has always had it and
	// analyze() used to drop it, which forced the package= filter to match the symbol
	// NAME instead. That works only where the name embeds the path (Kotlin/Swift/Go)
	// and returns a silent, permanent zero on Ruby, whose names do not.
	Package string `json:"package,omitempty"`
	Kind    string `json:"kind"` // nested-loop | compounded | call-in-loop | recursion
	// BigO is the estimated worst case, and is set only where one can be stated:
	// a call-in-loop whose call sits in a single loop (O(n), once per element), and
	// recursion.
	//
	// nested-loop and compounded do NOT carry one, and neither does a call-in-loop
	// whose call sits in a nest. They carry Depth instead. A
	// count of nested loops is a fact the parser can establish; the complexity it
	// implies is not, because loops multiply only when each walks an independent
	// collection. Three hand-read samples of what this analyzer reported (1,042
	// findings over 14 repositories) found 8 of 181 such findings right as a
	// complexity on the last of them, and 1 of 93 above O(n²): the nest was real
	// and was a parent-then-children walk, a loop over a registry, or a callee
	// continuing its caller's traversal. So the depth is reported as what it is.
	BigO string `json:"big_o,omitempty"`
	// Depth is the loop nesting depth of a nested-loop finding (within the
	// function), a compounded one (across the call graph), or the nest around the
	// call of a call-in-loop finding, counting only loops that scale with some
	// input. It is a structural count, not an exponent.
	Depth    int    `json:"depth,omitempty"`
	Severity string `json:"severity"` // high | medium | low
	// Confidence in [0,1]: how much to trust this as a real risk vs. a structural
	// over-estimate. Decays with the reported Big-O exponent (deep polynomials are
	// almost always over-counts) and when bounded loops were discounted or the
	// finding sits on a one-shot cold path.
	Confidence float64 `json:"confidence,omitempty"`
	// RiskScore ranks findings within a severity tier: estimated cost (Big-O weight) ×
	// confidence, lifted for a request hot path (route handler). Higher = act first.
	RiskScore float64  `json:"risk_score,omitempty"`
	Why       string   `json:"why"`
	Evidence  []string `json:"evidence,omitempty"`
	// Calls is set on a call-in-loop finding: one entry per call the finding names,
	// in the order Evidence names them, with what each rests on. A finding lists
	// every expensive call in the loop, and they are not equally well founded: one
	// can be a storage fact and the next a verb in a helper's name.
	Calls []CallEvidence `json:"calls,omitempty"`
}

// CallEvidence is one call a call-in-loop finding names, and why it is there.
type CallEvidence struct {
	Callee string `json:"callee"`
	// Basis is the strongest ground the analyzer has for calling this I/O. See
	// the basis* constants.
	Basis string `json:"basis"`
	// Depth is the nesting of the scaling loops around this call, where the
	// extractor reports it per call.
	Depth int `json:"depth,omitempty"`
}

// What a named call rests on, strongest first. The first three are facts about the
// callee. The rest are readings of its name, and differ in how much of the name
// is read: a method some function flagged performs_io also bears, a curated ORM
// or client method, a receiver or module token, and last a generic verb or a
// per-language name list.
//
// An ActiveRecord association name is not among them. `record.posts` in a loop is
// the query-loops explainer's question, which answers it from the receiver's type;
// matched here against every association the application declares, `tag.tag`, a
// string column, read as one.
const (
	basisStorage    = "storage"      // the callee is a storage fact
	basisPrimitive  = "io-primitive" // the extractor identified the call as an I/O entry point
	basisResolvedIO = "resolved-io"  // the callee resolves to a function flagged performs_io
	basisIOIndex    = "io-index"     // the method name is shared with a performs_io function
	basisIOMethod   = "io-method"    // a curated ORM/client method name
	basisIOReceiver = "io-receiver"  // a receiver, module or package token
	basisName       = "name"         // a generic verb, or a per-language name list
)

// langFamily groups a file with the others whose functions it can call by name:
// its own language, and for the JVM, JavaScript, C and .NET the languages that
// share a runtime or a linker with it.
func langFamily(file string) string {
	ext := file
	if i := strings.LastIndexByte(file, '.'); i >= 0 {
		ext = file[i:]
	}
	switch ext {
	case ".kt", ".java", ".scala":
		return "jvm"
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".vue", ".svelte":
		return "js"
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hxx":
		return "c"
	case ".cs", ".vb", ".fs":
		return "dotnet"
	}
	return ext
}

// strictIOIndex are the language families whose short-name index is narrowed, each
// on a removed-side reading of what narrowing took out: array `find`s, `subscribe`
// and event handlers in TypeScript, getters and a JSON-path `delete` on the JVM,
// overloads and parsers in C and C++, channel and runtime calls in Rust. .NET does
// not gate on the index, so there it only decides what a call is said to rest on.
var strictIOIndex = map[string]bool{"js": true, "jvm": true, "c": true, ".rs": true, "dotnet": true}

// basisIsFact reports whether a basis is a fact about the callee and not a reading
// of its name. Only a fact confirms a call as I/O.
func basisIsFact(basis string) bool {
	return basis == basisStorage || basis == basisPrimitive || basis == basisResolvedIO
}

// callBasis names the strongest ground for treating an in-loop call as I/O. It is
// asked only of a call the per-language gate already admitted, and does not decide
// whether the call is reported.
//
// Go is read by its own rule: the method index and the ORM names say nothing about
// Go code.
func callBasis(caller funcInfo, callee string, golang bool, storage map[string]bool, byName, byDotted map[string]funcInfo, ioMethods map[string]bool) string {
	if storage[callee] {
		return basisStorage
	}
	if slices.Contains(caller.IOCalls, callee) {
		return basisPrimitive
	}
	if f, ok := byName[callee]; ok && f.PerformsIO {
		return basisResolvedIO
	}
	if f, ok := byDotted[callee]; ok && f.PerformsIO {
		return basisResolvedIO
	}
	if golang {
		if goIOPackage(callee, byName) {
			return basisIOReceiver
		}
		return basisName
	}
	m := methodSegment(callee)
	if ioMethods[m] {
		return basisIOIndex
	}
	if pyIOMethods[m] || m == "fetch" {
		return basisIOMethod
	}
	for _, kw := range tsExpensiveMethods {
		if containsKeyword(m, kw) {
			return basisIOMethod
		}
	}
	for _, kw := range expensivePrefixes {
		if strings.Contains(callee, kw) {
			return basisIOReceiver
		}
	}
	return basisName
}

// deepCompoundedDepth is the label a compounded finding carries from
// deepEstimateDepth up: beyond three levels the exact count of a cross-call-graph
// estimate is not worth stating.
const deepCompoundedDepth = "depth 4+"

// Label is what a finding shows where a Big-O used to be shown for every kind: the
// Big-O where one is claimed, and "depth N" where the finding is a structural one.
func (f Finding) Label() string {
	if f.BigO != "" {
		return f.BigO
	}
	if f.Kind == "compounded" && f.Depth >= deepEstimateDepth {
		return deepCompoundedDepth
	}
	return fmt.Sprintf("depth %d", f.Depth)
}

// --- Prop readers (snapshot values decode as float64/[]any after JSONL round-trip) ---

func intProp(props map[string]any, key string) int {
	switch n := props[key].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func boolProp(props map[string]any, key string) bool {
	b, _ := props[key].(bool)
	return b
}

func stringSliceProp(props map[string]any, key string) []string {
	switch s := props[key].(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, a := range s {
			if str, ok := a.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// intsProp reads a prop holding a list of integers, in either the form an
// extractor writes or the one a JSONL round trip leaves. ok is false when the prop
// is absent or holds anything else.
func intsProp(props map[string]any, key string) (out []int, ok bool) {
	switch v := props[key].(type) {
	case []int:
		return v, true
	case []any:
		out = make([]int, 0, len(v))
		for _, d := range v {
			switch n := d.(type) {
			case float64:
				out = append(out, int(n))
			case int:
				out = append(out, n)
			case int64:
				out = append(out, int(n))
			default:
				return nil, false
			}
		}
		return out, true
	}
	return nil, false
}

// alignedInts reads an integer-list prop that runs parallel to a list of call
// targets, as a map from target to its value. nil when the prop is absent or does
// not line up with the targets it describes, so a half-written fact degrades to
// the caller's fallback instead of to a wrong answer.
func alignedInts(props map[string]any, key string, targets []string) map[string]int {
	vals, ok := intsProp(props, key)
	if !ok || len(vals) != len(targets) {
		return nil
	}
	out := make(map[string]int, len(targets))
	for i, c := range targets {
		out[c] = vals[i]
	}
	return out
}

// callDepthProp reads calls_in_scaling_loop_depth, a slice parallel to
// calls_in_scaling_loop.
func callDepthProp(props map[string]any, calls []string) map[string]int {
	return alignedInts(props, propCallsInScalingLoopDepth, calls)
}

// intSetProp reads an integer-list prop as a set; nil when absent.
func intSetProp(props map[string]any, key string) map[int]bool {
	vals, ok := intsProp(props, key)
	if !ok {
		return nil
	}
	out := make(map[int]bool, len(vals))
	for _, v := range vals {
		out[v] = true
	}
	return out
}

// hasComplexityMetrics reports whether a symbol carries any of the per-body
// complexity signals (loop nesting, in-loop calls, or direct recursion). Used to
// pull in non-function symbols that still have a measurable body — a Swift
// computed-property getter or willSet/didSet observer — while leaving plain stored
// properties/fields (no such props) out of the analysis.
func hasComplexityMetrics(props map[string]any) bool {
	if _, ok := props[propLoopDepth]; ok {
		return true
	}
	if _, ok := props[propCallsInLoop]; ok {
		return true
	}
	if _, ok := props[propRecursiveSelf]; ok {
		return true
	}
	return false
}

// isTestPath reports whether a repo-relative path is test or mock code, whose
// performance is not actionable.
//
// The rule lives in the OSS module (facts.IsTestPath), shared with the dead-code
// analyzer and the god-class/hotspots explainers. It used to be a local copy whose
// segment list knew nothing of the Gradle/KMP test source sets, so every function
// under `androidTest/`, `commonTest/`, `jvmTest/` was analyzed as production code
// (24 such files on nan/nebenan-android-app alone).
//
// The old copy's Swift/Kotlin basename suffixes (`Tests.swift`, `Test.kt`, …) did
// NOT survive the merge: they are not tool-enforced, and measured against the
// corpus they had no true positives to contribute — 393 of the 402 files they
// matched on nan/nebenan-iOS were already claimed by a directory segment, and every
// one of the remaining 9 was a production A/B-test feature that perf was therefore
// silently skipping (`KindnessReminderABTest.swift`, `Tracking+ShortenNameABTest.swift`,
// …). Real Swift/Kotlin tests live in a test target or source set. See
// internal/facts/testpath.go.
func isTestPath(p string) bool { return facts.IsTestPath(p) }

// isIgnoredPath reports whether a path matches an operator-supplied exclusion glob.
// Globs come from the ENOLA_PERF_EXCLUDE environment variable (os.PathListSeparator-
// or comma-separated), so an operator can hide their own generated/vendored trees
// without a rebuild — e.g.:
//
//	ENOLA_PERF_EXCLUDE=**/proto/**,src/legacy/**,**/*.pb.go,*.thrift.go
//
// Matched with pathglob.Match (the OSS engine's matcher) against the full path, then
// against the basename. Supported forms:
//
//	vendor/**                 anchored directory prefix
//	**/proto/**               a directory named "proto" at any depth
//	**/*.pb.go                a basename glob at any depth
//	prefix/**/*_spec.rb       a basename glob under a named directory
//	*.thrift.go               a bare basename glob, at any depth (basename pass)
//
// Empty/unset → no-op.
//
// The examples here used to read `**/proto/*` and claim `path.Match` semantics. Both
// were wrong: path.Match has no `**` at all (it reads it as `*`, which cannot cross a
// `/`), so those patterns silently matched nothing — and `**/proto/*` is not a
// supported form even now. The recursive spelling is `**/proto/**`.
func isIgnoredPath(p string) bool {
	globs := ignoreGlobs()
	if len(globs) == 0 {
		return false
	}
	// pathglob.MatchAny is the OSS engine's matcher — the one behind the ignore list
	// and the test globs — and it understands `**`. This used to call path.Match, which
	// does NOT: `*` cannot cross a `/`, and `**` is simply read as `*`. So every `**`
	// pattern this function's own doc advertised matched nothing at all, silently, with
	// no error for the operator to notice.
	if pathglob.MatchAny(p, globs) {
		return true
	}
	// Basename fallback, kept: it is what lets a bare `*.thrift.go` match at any depth.
	// pathglob.Match has no bare-basename form (it wants `**/*.thrift.go`), and the doc has
	// promised the short spelling for as long as this has existed.
	base := p
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return pathglob.MatchAny(base, globs)
}

func ignoreGlobs() []string {
	raw := os.Getenv("ENOLA_PERF_EXCLUDE")
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == os.PathListSeparator
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// coldPathSegments are directory segments whose code runs once (a schema migration),
// out-of-band (build/release/dev tooling and one-off scripts), or as an operator-
// invoked command — never on a request/scheduler hot path. Findings here are real
// structurally but not worth acting on with urgency: they are downranked to `low` so the
// high/medium buckets stay dominated by production hot paths. Kept visible (not excluded)
// so they remain filterable. `versions` pairs with `migrations` (Alembic's tree).
//
// Matched without regard to case: .NET names the directory `Migrations`, and the
// lowercase-only test left every one of a media server's 56 migration findings
// ranked as a production risk.
//
// `cli` is an operator-invoked command tree (`lib/<app>/cli`, `<pkg>/cli`), `docs`
// a documentation site's own plugins and components, `.github` CI actions, and
// `build-logic` a Gradle convention-plugin build. Each is here because hand-read
// findings sat under it, ranked high or medium, in code that never serves a request.
var coldPathSegments = map[string]bool{
	"migrations": true, "versions": true, "alembic": true,
	"dev": true, "scripts": true, "devel-common": true,
	"benchmark": true, "benchmarks": true, "examples": true,
	"cli": true, "docs": true, ".github": true, "build-logic": true,
}

// isColdPath reports whether a finding's location is one-shot / non-runtime code
// (migrations, dev & build tooling, CLI command modules, module main guards). Such a
// finding is downranked to `low` but not hidden.
//
// It used to take a `pkg` argument that the body never read, and that every call site
// passed as "". Gone.
func isColdPath(file string) bool {
	parts := strings.Split(file, "/")
	dirs := parts[:len(parts)-1] // a FILE called docs.go or cli.py is not a directory of them
	for i, part := range dirs {
		if coldPathSegments[strings.ToLower(part)] {
			return true
		}
		// Rails keeps its migrations under db/, in more directories than one:
		// db/migrate, db/post_migrate, and whatever a project archives old ones
		// into (db/old_migrations). Anchored on db/ so that a production package
		// with "migrate" in its name elsewhere is left alone.
		if i > 0 && dirs[i-1] == "db" && strings.Contains(part, "migrat") {
			return true
		}
		// lib/tasks is where Rails keeps its rake tasks.
		if i > 0 && dirs[i-1] == "lib" && part == "tasks" {
			return true
		}
	}
	base := parts[len(parts)-1]
	switch {
	case strings.HasSuffix(base, ".rake"):
		return true
	case strings.HasSuffix(file, "/__main__.py") || base == "__main__.py":
		return true
	case base == "setup.py" || base == "conftest.py":
		return true
	// `/cli/commands/` only. There used to be a bare `strings.Contains(file,
	// "/commands/")` sitting on the same line, and it matched ANY path with a
	// commands/ directory — but commands/ is a first-class PRODUCTION directory in
	// CQRS, command buses and event sourcing, not a CLI marker. On python/superset it
	// downranked all 101 flaggable symbols under superset/commands/ (the command layer
	// invoked from the API) to `low` with confidence capped at 0.4, including an
	// O(n³+) call-in-loop in a streaming CSV export executor. None of them was cold by
	// any other rule; the downrank is silent, and `low` findings sort below every real
	// one.
	//
	// Deliberately NOT replaced with a root-level `commands/` rule: no repo on the
	// corpus has one, so it would be an unexercised path heuristic — which is how the
	// last three of these went wrong. A false `cold` suppresses a real finding, so err
	// the other way, and bring a fixture if a root-level CLI tree ever turns up.
	case strings.Contains(file, "/cli/commands/"):
		return true
	}
	return false
}

// collect reads the fact store via the public bootstrap API and produces the
// pure-analysis inputs: the function list, the set of storage-fact names, the set
// of symbol names that back a route handler (used to raise severity), and the set
// of ActiveRecord association names (used to flag lazy-loaded reads in loops).
// collectRouteHandlers returns the set of symbols that serve an HTTP/gRPC route, keyed
// by CANONICAL SYMBOL NAME — which is how analyze() and riskScore() look them up.
//
// It used to be keyed by the route's raw `handler` prop. The extractors render that from
// the REGISTRATION site, so for Go it is the receiver VARIABLE chain
// ("h.weatherHandler.GetDailyWeatherRange") and for Rails it is "clients#index" — and
// neither is a symbol name. The two key spaces are disjoint: on fairwayhub/golf, 1397
// distinct handler props intersect 13482 symbol names in exactly TWO places (both Python,
// where the prop happens to equal the canonical name); on nan/nebenan, zero. So no Go or
// Ruby route handler had ever been escalated, and the comment promising a handler is
// "always hot" described behaviour the code did not have.
//
// The engine now resolves each route to the symbol that serves it and records it as a
// handled_by EDGE (bindHTTPHandlers, v111; bindGRPCHandlers before it), rejecting
// same-named non-handlers by signature rather than guessing. Read the resolved edge, and
// keep the raw prop as a fallback for the extractors whose prop already IS the canonical
// name (Python/Flask) so nothing that used to match stops matching.
func collectRouteHandlers(routes []facts.Fact) map[string]bool {
	out := make(map[string]bool)
	for _, f := range routes {
		for _, r := range f.Relations {
			if r.Kind == facts.RelHandledBy && r.Target != "" {
				out[r.Target] = true
			}
		}
		if h, ok := f.PropAny(propHandler).(string); ok && h != "" {
			out[h] = true
		}
	}
	return out
}

// Analyze returns every performance finding in the store, using the same
// collect+analyze core as the analyze_performance tool. It is what the package's
// own annotate pass runs to mark findings on the store.
func Analyze(store *facts.Store) []Finding {
	funcs, storage, routeHandlers, assoc := collect(store)
	return analyze(funcs, storage, routeHandlers, assoc)
}

func claimedHas(claimed map[string]struct{}, name string) bool {
	_, ok := claimed[name]
	return ok
}

func collect(store *facts.Store) (funcs []funcInfo, storage, routeHandlers, assoc map[string]bool) {
	claimed := queryloops.ClaimedSymbols(store)
	storage = make(map[string]bool)
	for _, f := range store.ByKind(facts.KindStorage) {
		storage[f.Name] = true
	}

	// Route handlers sit on the request hot path, so a finding on one is escalated
	// (analyze) and its risk lifted 1.3x (riskScore). Both look the handler up by its
	// CANONICAL SYMBOL NAME.
	//
	// This used to be keyed by the route's raw `handler` prop — which the extractors
	// render from the REGISTRATION site, so for Go it is the receiver VARIABLE chain
	// ("h.weatherHandler.GetDailyWeatherRange") and for Rails it is "clients#index".
	// Neither is a symbol name. The two key spaces are disjoint: on fairwayhub/golf, 1397
	// distinct handler props intersect 13482 symbol names in exactly TWO places (both
	// Python, where the prop happens to equal the canonical name), and on nan/nebenan,
	// zero. So no Go or Ruby route handler had ever been escalated, and the comment
	// promising a handler is "always hot" described behaviour the code did not have.
	//
	// The engine now resolves the route to the symbol that serves it and records it as a
	// handled_by EDGE (bindHTTPHandlers for HTTP, bindGRPCHandlers for gRPC), rejecting
	// same-named non-handlers by signature. Read the resolved edge; fall back to the raw
	// prop for the extractors whose prop already IS the canonical name (Python/Flask), so
	// nothing that used to match stops matching.
	routeHandlers = collectRouteHandlers(store.ByKind(facts.KindRoute))

	// ActiveRecord association names (Rails). A no-arg call to one of these inside
	// a loop is a lazy-loaded association read — the classic N+1.
	assoc = make(map[string]bool)
	for _, f := range store.ByKind(facts.KindDependency) {
		if a, ok := f.PropAny(propAssociation).(string); ok && a != "" {
			assoc[a] = true
		}
	}

	for _, f := range store.ByKind(facts.KindSymbol) {
		sk, _ := f.PropAny(propSymbolKind).(string)
		// Analyze functions and methods, plus any other symbol that carries
		// complexity metrics — a Swift computed-property getter or willSet/didSet
		// observer is emitted as a variable/constant symbol but can hold a loop or
		// per-iteration I/O, so a hotspot inside `var x: [T] { … }` is otherwise
		// invisible here. Only symbols with loop/recursion metrics qualify, so plain
		// stored properties and fields are still skipped.
		if sk != facts.SymbolFunc && sk != facts.SymbolMethod && !hasComplexityMetrics(f.Props) {
			continue
		}
		if mcputil.IsGeneratedPath(f.File) || isTestPath(f.File) || isIgnoredPath(f.File) {
			continue
		}
		pkg := ""
		var calls []string
		for _, r := range f.Relations {
			switch r.Kind {
			case facts.RelDeclares:
				if pkg == "" {
					pkg = r.Target
				}
			case facts.RelCalls:
				calls = append(calls, r.Target)
			}
		}
		_, hasScaling := f.Prop(propScalingLoopDepth)
		_, hasScalingCalls := f.Prop(propCallsInScalingLoop)
		scalingCalls := stringSliceProp(f.Props, propCallsInScalingLoop)
		onElementCalls := stringSliceProp(f.Props, propCallsOnLoopElement)
		funcs = append(funcs, funcInfo{
			Name:                f.Name,
			File:                f.File,
			Line:                f.Line,
			Repo:                f.Repo,
			Package:             pkg,
			Exported:            boolProp(f.Props, propExported),
			Cyclomatic:          intProp(f.Props, propCyclomatic),
			LoopDepth:           intProp(f.Props, propLoopDepth),
			ScalingLoopDepth:    intProp(f.Props, propScalingLoopDepth),
			HasScalingDepth:     hasScaling,
			LoopCount:           intProp(f.Props, propLoopCount),
			Claimed:             claimedHas(claimed, f.Name),
			CallsInLoop:         stringSliceProp(f.Props, propCallsInLoop),
			CallsInScalingLoop:  scalingCalls,
			CallDepth:           callDepthProp(f.Props, scalingCalls),
			HasScalingLoopCalls: hasScalingCalls,
			CallsOnLoopElement:  onElementCalls,
			LoopsOverParam:      boolProp(f.Props, propLoopsOverParam),
			ElementArg:          alignedInts(f.Props, propCallsOnLoopElementArg, onElementCalls),
			LoopParams:          intSetProp(f.Props, propLoopsOverParamIndex),
			Recursive:           boolProp(f.Props, propRecursiveSelf),
			PerformsIO:          boolProp(f.Props, propPerformsIO),
			IODirect:            boolProp(f.Props, propIODirect),
			IOCalls:             stringSliceProp(f.Props, propIOCalls),
			PureCalls:           stringSliceProp(f.Props, propPureCalls),
			Calls:               calls,
		})
	}
	return funcs, storage, routeHandlers, assoc
}

// bigOForDepth maps a loop-nesting depth to a Big-O label.
func bigOForDepth(d int) string {
	switch {
	case d <= 0:
		return "O(1)"
	case d == 1:
		return "O(n)"
	case d == 2:
		return "O(n²)"
	case d == 3:
		return "O(n³)"
	default:
		return fmt.Sprintf("O(n^%d)", d)
	}
}

// depthIsNotComplexity is the sentence every nested-loop and compounded finding
// carries after its count. See Finding.BigO for why these two kinds state a depth
// and not a Big-O.
const depthIsNotComplexity = "That is a count of nested loops, not a complexity: they multiply only when each walks an independent collection. A walk from parents to their children, a loop over a fixed list, or a callee finishing its caller's traversal nests without multiplying. Check which loops here do."

// structuralSeverity is the severity of a nested-loop or compounded finding at any
// depth. It is a fact about the shape of the code and a lead worth a look, never
// evidence of a cost by itself: `high` is for a finding that has evidence, which
// here means a confirmed I/O call in a loop or a route handler.
const structuralSeverity = "medium"

// deepEstimateDepth is the nesting beyond which the exact count of an estimated
// depth (compounded, call-in-loop) is not worth stating or ranking on.
const deepEstimateDepth = 4

// expensiveMethods flags call targets that are likely I/O / DB / network work by
// their METHOD NAME — matched against the method segment (the part of the target
// after the last '.'), not the whole string. This anchors matching to the called
// method so a keyword can't fire on a receiver, path, or argument (e.g. "Query"
// must not match the receiver "searchQuery", nor "update" the arg "ticket.updated_at").
// Case-sensitive, so the Go CamelCase verbs do not match lowercase JS/Python names.
// Matching is whole-word (see containsKeyword): a keyword must sit on word boundaries
// in the method name, so "flush" does not fire on the C stdlib "fflush" nor "load"
// on "preload" — within-token infixes that are not real DB/IO calls.
var expensiveMethods = []string{
	// Go (CamelCase verbs — e.g. "stmt.ExecContext", "result.LastInsertId")
	"Query", "Find", "Save", "Exec", "Fetch",
	"Insert", "Update", "Delete", "Request", "RPC",
	// Python DB (SQLAlchemy / DBAPI)
	"execute", "executemany", "scalars", "scalar",
	"fetchone", "fetchall", "fetchmany",
	"commit", "flush", "refresh", "merge",
	"bulk_create", "bulk_update", "bulk_save_objects",
	"bulk_insert_mappings", "add_all",
	"get_or_create", "update_or_create",
	// Ruby / ActiveRecord
	"where", "find_by", "find_each", "find_in_batches",
	"pluck", "update_all", "destroy_all", "upsert_all", "insert_all",
	"increment!", "decrement!", "touch", "reload", "exists?",
	"save", "save!", "destroy", "destroy!", "update", "update!",
	// Swift — Core Data / SwiftData / network / file
	"fetch", "fetchRequest", "dataTask", "contentsOf", "load",
	// Kotlin — Room / Retrofit-OkHttp / coroutine
	"insert", "enqueue", "upsert", "query", "deleteBy",
	"findBy", "getAll", "loadAll", "await", "awaitResponse",
	// TypeScript — Prisma / TypeORM
	"findMany", "findFirst", "findUnique", "findOne", "createMany",
	"updateMany", "deleteMany", "aggregate", "queryRaw",
	// Java — JPA / Spring Data / JDBC / RestTemplate-WebClient
	"findAll", "findById", "findBy", "saveAll", "deleteById", "deleteAll",
	"existsById", "getForObject", "postForObject", "queryForObject",
	"queryForList", "batchUpdate", "createQuery", "createNativeQuery",
	"getResultList", "getSingleResult", "exchange", "retrieve",
}

// expensivePrefixes flags call targets by a receiver/module/type token whose
// expensiveness depends on the receiver, not the method (e.g. anything under a `db`
// package, or an `axios`/`URLSession` call). Matched against the FULL target.
// urllib is scoped to `urllib.request` (the network submodule) on purpose: bare
// `urllib` also covers `urllib.parse` (urlparse/urljoin/unquote), which is pure
// in-memory string work, not I/O — matching it turned URL-parsing loops into false N+1s.
var expensivePrefixes = []string{
	"db.", "sql.", "http.",
	"requests.", "httpx.", "aiohttp.", "urllib.request", "urlopen",
	"axios", "URLSession", "NSFetchRequest",
}

func isExpensiveCall(target string, storage, assoc map[string]bool) bool {
	if storage[target] {
		return true
	}
	method := methodSegment(target)
	for _, kw := range expensiveMethods {
		if containsKeyword(method, kw) {
			return true
		}
	}
	for _, kw := range expensivePrefixes {
		if strings.Contains(target, kw) {
			return true
		}
	}
	// Rails: a no-arg call whose method name is a known ActiveRecord association is a
	// lazy-loaded read (N+1). Match on the method-name segment so both bare names
	// ("posts") and instance reads ("u.posts") are caught — but NOT a call on a
	// constant/class receiver (`SystemEventService.trigger`): associations are
	// instance-level, so `Const.method` is a class-method/scope call, not an
	// association read that merely shares the name.
	if assoc[method] && !constantReceiver(target) {
		return true
	}
	return false
}

// --- Go-specific expensive-call detection ---
//
// Go used the generic gate unqualified, which matches every language's keywords
// against every language's code. On Go that read `mergeBack` as SQLAlchemy's Session.merge and
// `aggregateField` as a Prisma aggregate, and it has no notion of what part of
// speech a keyword is in: `IsPullRequest` is a predicate, `NewAzureMonitorQuery` a
// constructor, `ErrCancelledf` an error value, and none of them sends anything.

// goForeignKeywords are the entries of expensiveMethods that name another
// ecosystem's API and no Go one: SQLAlchemy's Session.merge, Prisma's aggregate,
// ActiveRecord's where. The rest of the list stays in force for Go. Much of it is
// another language's spelling of a verb Go code uses for the same thing (`load`,
// `commit`, `flush`), and Go has no performs_io to fall back on, so a real
// `loadJobTaskOutputs` is found by its name or not at all.
//
// The Prisma and TypeORM method names are here too. They are multi-word API names
// chosen because nothing else is called that, which holds in TypeScript and not
// across languages: `findFirst` whole-word-matches `findFirstIdentifier`, a helper
// that walks a syntax tree, and being on the "unambiguous" list it was promoted
// to confirmed I/O and ranked high.
var goForeignKeywords = map[string]bool{
	"merge": true, "aggregate": true, "where": true,
	"findMany": true, "findFirst": true, "findUnique": true, "findOne": true,
	"createMany": true, "updateMany": true, "deleteMany": true, "queryRaw": true, "executeRaw": true,
}

// goNonIOPrefixes open a name that states or builds something rather than doing it.
//
// `With` is deliberately absent. An option (`WithTimeout`) builds, but
// `WithTransactionalDbSession(ctx, fn)` runs fn inside a transaction, and the two
// cannot be told apart by name.
var goNonIOPrefixes = []string{"Is", "Has", "New", "Err", "To", "As", "Can", "Should"}

// goStatesOrBuilds reports whether a Go method name is a predicate, a constructor,
// an error value or a conversion, by its leading word.
func goStatesOrBuilds(method string) bool {
	for _, p := range goNonIOPrefixes {
		if len(method) <= len(p) {
			continue
		}
		head := method[:len(p)]
		if head != p && head != strings.ToLower(p) {
			continue
		}
		// The prefix has to be a whole word: `Issue` is not `Is` + `sue`.
		if next := method[len(p)]; isUpper(next) || next == '_' {
			return true
		}
	}
	return false
}

func isExpensiveGoCall(target string, storage map[string]bool, byName map[string]funcInfo) bool {
	if storage[target] {
		return true
	}
	method := methodSegment(target)
	if goStatesOrBuilds(method) {
		return false
	}
	for _, kw := range expensiveMethods {
		if !goForeignKeywords[kw] && containsKeyword(method, kw) {
			return true
		}
	}
	return goIOPackage(target, byName)
}

// goIOPackage reports whether a Go call target sits in a package that is I/O by
// what it is: `net/http`, `database/sql`, a `db` package, a package whose name
// ends in `http`.
//
// One case is refused: a package of THIS repository named exactly `http`. That
// name is the standard library's, and a project that reuses it is as likely naming
// its subject as its transport. A linker that matches HTTP routes to their callers
// lives in such a package, does no I/O, and had every loop that called a sibling
// function reported as a confirmed network call per iteration. A wrapper package
// (`lokihttp`, `errhttp`) and the in-repo `db` and `sql` layers keep matching:
// there the name is the convention for code that does talk to the outside.
func goIOPackage(target string, byName map[string]funcInfo) bool {
	_, inRepo := byName[target]
	ownHTTP := inRepo && goPackageName(target) == "http"
	for _, kw := range expensivePrefixes {
		if kw == "http." && ownHTTP {
			continue
		}
		if strings.Contains(target, kw) {
			return true
		}
	}
	return false
}

// goPackageName returns the package a Go call target is in: `http` for
// `internal/signals/http.Signal.Contribute`.
func goPackageName(target string) string {
	pkg := target
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	if i := strings.Index(pkg, "."); i >= 0 {
		pkg = pkg[:i]
	}
	return pkg
}

// phpInMemory reports whether a PHP call target is in-memory work that the generic
// keywords claim. `array_merge` and its family are the standard library's array
// functions, matched by `merge`. `where…` is `where` from the ActiveRecord list: on
// a Laravel or Flarum query builder it adds a clause to a query that something
// else (`get`, `first`, `count`) later runs.
func phpInMemory(target string) bool {
	method := methodSegment(target)
	if strings.HasPrefix(method, "array_") && !strings.ContainsAny(target, ".:>\\") {
		return true
	}
	return method == "where" || (strings.HasPrefix(method, "where") && len(method) > 5 && isUpper(method[5]))
}

// csInMemoryMethods are .NET collection methods the generic verbs match: a
// List<T>.InsertRange, a ConcurrentDictionary.AddOrUpdate, an index search.
var csInMemoryMethods = map[string]bool{
	"AddOrUpdate": true, "InsertRange": true, "FindIndex": true, "FindLastIndex": true,
}

// --- Swift-specific expensive-call detection ---
//
// The cross-language expensiveMethods list matches Swift's verb-prefixed method
// names (updateCenter, updateValue, loadView, insertArrangedSubview, refresh) that
// are ubiquitous in-memory UI/state operations, not I/O — the Swift analog of the
// Ruby Hash#merge collision rubyInMemory guards against. So for Swift callers we do
// NOT keyword-match; instead we trust the extractor's per-method `performs_io` prop
// (a call-graph closure over real I/O primitives, computed where the candidate sets
// for ambiguous edges are known), falling back to a direct-primitive check for a
// bare/unresolved callee that the closure could not attribute to a fact.

// swiftIOMethods are Swift method names that denote real network / file I/O
// regardless of receiver — matched against the method segment (after the last '.').
// Used only as a direct-primitive fallback for a bare in-loop callee (e.g.
// `session.dataTask`) that has no resolved fact carrying performs_io.
var swiftIOMethods = map[string]bool{
	"dataTask": true, "downloadTask": true, "uploadTask": true,
	"fetchRequest": true, "contentsOf": true, "download": true, "upload": true,
}

// swiftIOPrefixes flag a call whose receiver/type token denotes I/O — matched
// against the FULL target (URLSession.shared.dataTask, an NSFetchRequest execute).
var swiftIOPrefixes = []string{
	"URLSession", "NSFetchRequest", "URLRequest", ".dataTask",
}

// isSwiftIOSink reports whether a call target is itself a high-confidence Swift I/O
// primitive — the direct-callee fallback when no resolved fact is available.
func isSwiftIOSink(target string) bool {
	if swiftIOMethods[methodSegment(target)] {
		return true
	}
	for _, kw := range swiftIOPrefixes {
		if strings.Contains(target, kw) {
			return true
		}
	}
	return false
}

// isExpensiveSwiftCall reports whether a Swift in-loop call is per-iteration I/O: a
// storage fact, a resolved callee the extractor flagged performs_io (the transitive
// network/file signal), or — for a bare/unresolved callee — a direct I/O primitive.
// A bare callee that resolves to no fact and names no primitive is treated as
// in-memory (an accepted false negative), which clears the update/load/insert
// keyword false positives.
func isExpensiveSwiftCall(target string, storage map[string]bool, byName map[string]funcInfo) bool {
	if storage[target] {
		return true
	}
	if f, ok := byName[target]; ok {
		return f.PerformsIO
	}
	return isSwiftIOSink(target)
}

// --- Kotlin/Java (JVM) expensive-call detection ---
//
// Like Swift, the JVM must NOT use the cross-language keyword list: the generic verbs
// there (update on a StateFlow, save/load on a presenter, find on a List, await on any
// coroutine Deferred, the Compose awaitPointerEvent) are ubiquitous in-memory work on
// Kotlin/Java and produced the bulk of the false positives. Instead we use a curated
// list of DB/network method names that name a real round-trip regardless of receiver —
// Room, SQLDelight, Retrofit/OkHttp, JPA/Spring Data, JDBC, RestTemplate/WebClient —
// plus storage facts and the extractor's performs_io signal when present.

// jvmExpensiveMethods are Kotlin/Java method names denoting real DB / network I/O.
// Deliberately excludes the bare in-memory-ambiguous verbs (update, save, load, fetch,
// find, merge, where, await, delete) — the explicit Room/Retrofit/Spring forms below
// keep the genuine N+1 signals (Room insert, Retrofit awaitResponse) without the noise.
var jvmExpensiveMethods = []string{
	// Room / SQLDelight / JDBC — the receiver is a DAO / statement / cursor.
	"insert", "upsert", "execute", "executeUpdate", "enqueue", "query",
	"createNativeQuery", "getResultList", "getSingleResult", "batchUpdate",
	// Retrofit coroutine adapter (a bare `.await()` is dropped as too broad).
	"awaitResponse",
	// Spring Data / JPA / RestTemplate / WebClient — explicit forms only, since the
	// bare verbs (find/save/load) are excluded as in-memory-ambiguous.
	"findById", "findBy", "findAll", "getAll", "loadAll", "saveAll",
	"deleteById", "deleteAll", "existsById",
	"getForObject", "postForObject", "exchange", "retrieve",
}

// jvmExpensivePrefixes flag a call whose receiver token denotes DB access (a `db.`
// package/handle, a raw `sql.` statement) — matched against the FULL target.
var jvmExpensivePrefixes = []string{"db.", "sql."}

// jvmIONameDenylist are ubiquitous collection/accessor method names that must NEVER be
// treated as I/O by a performs_io short-name match, even when a Retrofit/Room method
// happens to share the name (e.g. a Furly `@GET fun get(@Url …)` puts "get" in the I/O
// set). Without this, every list/map `.get(...)` inside a loop reads as a false N+1.
// These names are never a real per-iteration network/DB call; the exact-fact performs_io
// path (byName) is unaffected — only the short-name index is gated.
var jvmIONameDenylist = map[string]bool{
	"get": true, "set": true, "put": true, "add": true, "remove": true,
	"getValue": true, "getOrNull": true, "getOrDefault": true, "getOrElse": true,
	"getOrPut": true, "contains": true, "indexOf": true, "first": true, "last": true,
	"component1": true, "component2": true, "component3": true,
}

// isExpensiveJvmCall reports whether a Kotlin/Java in-loop call is per-iteration I/O:
// a storage fact, a resolved callee the extractor flagged performs_io (future-proofing
// a JVM transitive-I/O pass), or a curated DB/network method / receiver — NOT the
// cross-language keyword set.
func isExpensiveJvmCall(target string, storage map[string]bool, byName map[string]funcInfo, ioMethods map[string]bool) bool {
	if isJvmStorageBackedCall(target, storage, byName, ioMethods) {
		return true
	}
	method := methodSegment(target)
	for _, kw := range jvmExpensiveMethods {
		if containsKeyword(method, kw) {
			return true
		}
	}
	for _, kw := range jvmExpensivePrefixes {
		if strings.Contains(target, kw) {
			return true
		}
	}
	return false
}

// isJvmStorageBackedCall reports whether a JVM in-loop call is backed by a real
// storage/I/O fact (not just a keyword guess): a storage fact, a resolved callee the
// extractor flagged performs_io, or an in-loop callee whose method segment names a
// Retrofit/Room I/O method (the extractor's performs_io leaves, matched by short name
// because in-loop callees are receiver-qualified — `service.fetchFurly` — not canonical
// fact names). Used both to classify the call as expensive and to escalate a JVM
// call-in-loop finding above the default `medium`, where `public` is a useless hotness
// signal.
func isJvmStorageBackedCall(target string, storage map[string]bool, byName map[string]funcInfo, ioMethods map[string]bool) bool {
	if storage[target] {
		return true
	}
	if f, ok := byName[target]; ok && f.PerformsIO {
		return true
	}
	return ioMethods[methodSegment(target)]
}

// --- TypeScript/JavaScript expensive-call detection ---
//
// Frontend TS/JS reuses the generic CamelCase verbs as ordinary in-memory helper
// names: a Redux reducer helper `getFetchAllUpdate`, a selector `findIndex`, a
// setter `updateState`. The cross-language keyword list (Fetch/Update/Find/Save)
// therefore fires on locally-defined pure functions and — because almost every
// symbol in a frontend module is exported, and an exported call-in-loop is
// escalated to "high" — those false positives dominate the high-severity bucket.
// So, exactly as with Swift and the JVM, we do NOT keyword-match the generic
// verbs. A TS/JS in-loop call is expensive only when it is genuinely I/O-shaped:
// a storage fact, a resolved callee flagged performs_io, an HTTP/DB receiver
// token, the global `fetch`, or a distinctive ORM query method (Prisma/TypeORM
// findMany/queryRaw/… — multi-word names that do not collide with app helpers).

// tsExpensiveMethods are distinctive ORM/query method names (Prisma, TypeORM)
// that denote a real DB round-trip — matched whole-word against the method
// segment. The generic single verbs (find/fetch/update/save) are intentionally
// absent: they collide with in-memory helper names in frontend code.
var tsExpensiveMethods = []string{
	"findMany", "findFirst", "findUnique", "findOne",
	"createMany", "updateMany", "deleteMany",
	"aggregate", "queryRaw", "executeRaw",
}

// tsExpensivePrefixes flag a call whose receiver token denotes HTTP/DB access —
// matched against the FULL target (axios.get, http.request, prisma.$queryRaw).
var tsExpensivePrefixes = []string{"axios", "http.", "https.", "db.", "sql.", "prisma."}

// tsLifecycleNames are method names the short-name performs_io index must not
// carry in TypeScript, the way jvmIONameDenylist guards `get` and `put` on the JVM.
// One `init` that opens a socket puts `init` in the index, and every
// `renderer.init()` and `calendar.init()` in a loop then reads as a per-iteration
// network call. A resolved callee flagged performs_io is unaffected: only the
// match by bare name is refused.
//
// Two names, each from a read finding. A rule general enough to need no list (admit
// a name only when most functions bearing it do I/O) was tried and took real
// `dao.save` and `client.delete` calls with it, because an interface declaration
// and an in-memory namesake outnumber the one implementation that matters.
var tsLifecycleNames = map[string]bool{"init": true, "setup": true}

// isExpensiveTSCall reports whether a TypeScript/JavaScript in-loop call is
// per-iteration I/O: a storage fact, a resolved callee the extractor flagged
// performs_io, an HTTP/DB receiver, the global fetch, or a distinctive ORM query
// method. A bare locally-defined helper (no fact, no primitive) is treated as
// in-memory — an accepted false negative that clears the CamelCase-verb false
// positives (getFetchAllUpdate, updateState, findIndex, …).
func isExpensiveTSCall(target string, storage map[string]bool, byName map[string]funcInfo, ioMethods map[string]bool) bool {
	if storage[target] {
		return true
	}
	resolved, isResolved := byName[target]
	if isResolved && resolved.PerformsIO {
		return true
	}
	method := methodSegment(target)
	// The extractor's transitive performs_io signal, matched by method segment: an
	// in-loop call to a wrapper is recorded as a receiver-qualified metric string
	// (`api.updateNotificationPreferencesChannels`) or a misresolved default-import
	// edge, so the exact-name byName lookup above misses — the short-name index catches it.
	if ioMethods[method] && !tsLifecycleNames[method] {
		return true
	}
	if method == "fetch" {
		return true
	}
	// The ORM method names are a guess about a callee nobody could see. One that
	// resolved to a function in this repository is not a Prisma method for sharing
	// its name: a local `aggregate(rows)` that sums an array read as a database
	// aggregate. Only this test is skipped. performs_io is not complete enough to
	// overrule the others, so they stand as they were.
	if !isResolved {
		for _, kw := range tsExpensiveMethods {
			if containsKeyword(method, kw) {
				return true
			}
		}
	}
	for _, kw := range tsExpensivePrefixes {
		if strings.Contains(target, kw) {
			return true
		}
	}
	return false
}

// --- Dart/Flutter-specific expensive-call detection ---
//
// Dart is the fourth ecosystem to need one, and unlike Scala it measurably does. The
// generic expensiveMethods list carries `where` for Ruby, where `Model.where(...)` is a
// lazy ActiveRecord query — but in Dart `.where()` is `Iterable.where`, the standard
// in-memory filter and the exact equivalent of JavaScript's `.filter()`. It is
// everywhere. Measured on AppFlowy's Flutter half, the generic gate produced 58
// call-in-loop findings dominated by `children.where` inside a loop, which is ordinary
// list processing and not I/O of any kind. `update` (Map.update), `insert`
// (List.insert), `getAll` and `load` collide the same way.
//
// The signal to use instead already exists: the Dart extractor tags a body `io_direct`
// only when the FILE imports something that can perform I/O (dart:io, package:http,
// dio, drift, sqflite, isar, hive, firebase, …) and propagates it transitively into
// `performs_io`. That gate is a language rule rather than a guess, because Dart imports
// are mandatory and there is no ambient namespace.

// dartExpensiveMethods are the distinctive round-trip methods, safe on name alone: no
// Dart collection or widget API declares any of them.
var dartExpensiveMethods = []string{
	// drift / moor
	"insertOnConflictUpdate", "insertReturning", "selectOnly", "customSelect",
	"customStatement", "getSingleOrNull", "getSingle", "watchSingle",
	// sqflite
	"rawQuery", "rawInsert", "rawUpdate", "rawDelete", "openDatabase",
	// http / dio / assets / prefs
	"readAsString", "readAsBytes", "writeAsString", "writeAsBytes",
	"loadString", "getApplicationDocumentsDirectory", "getTemporaryDirectory",
	// isar / hive / objectbox
	"putAll", "getAllSync", "findAll", "findFirst",
}

// dartExpensivePrefixes flag a receiver token that denotes I/O access, matched against
// the FULL target (`_db.select`, `dio.get`, `http.post`). This is what keeps immich's
// real drift N+1s — `_db.select` and `_db.insertOnConflictUpdate` inside a loop — while
// `children.where` is dropped.
var dartExpensivePrefixes = []string{
	"db.", "_db.", "database.", "_database.", "http.", "dio.", "_dio.",
	"client.", "_client.", "prefs.", "_prefs.", "isar.", "_isar.",
	"firestore.", "_firestore.", "rootBundle.", "supabase.",
}

// dartDispatchNames are Dart/Flutter indirection and construction primitives. They are
// excluded from the loose short-name performs_io path (not from the exact-name or
// receiver paths), because a name this generic carries no information about what the
// call actually reaches.
var dartDispatchNames = map[string]bool{
	"call":         true, // callback.call(x) — invoking a function object
	"build":        true, // a widget constructing its subtree
	"createState":  true,
	"noSuchMethod": true,
	"toString":     true,
}

// isExpensiveDartCall reports whether a Dart in-loop call is per-iteration I/O: a
// storage fact, a resolved callee the extractor flagged performs_io, an I/O receiver,
// or a distinctive round-trip method.
//
// A bare locally-defined helper with no I/O evidence is treated as in-memory. That is
// the same accepted false negative the Swift, JVM and TypeScript handlers take, and it
// buys the same thing: the ubiquitous collection verbs stop manufacturing N+1 findings
// about list filtering.
func isExpensiveDartCall(target string, storage map[string]bool, byName map[string]funcInfo, ioMethods map[string]bool) bool {
	if storage[target] {
		return true
	}
	if f, ok := byName[target]; ok && f.PerformsIO {
		return true
	}
	method := methodSegment(target)
	// The transitive performs_io signal by short name: an in-loop call to a wrapper is
	// recorded as a receiver-qualified metric string (`repo.loadPage`), which the exact
	// byName lookup above misses.
	//
	// Skipped for Dart's dispatch primitives. A short-name match is evidence only when
	// the name is distinctive, and `call` is the least distinctive name in the
	// language: `callback.call(x)` is how you invoke a function stored in a variable,
	// so ANY symbol named `call` that performs I/O makes every callback invocation in
	// the codebase look like per-iteration I/O. Measured on AppFlowy, 12 of 28
	// remaining call-in-loop findings were driven by nothing but `.call`, and 4 more by
	// `build` — a widget constructing its subtree, which is in-memory by definition.
	// The exact-name and receiver paths still apply, so a genuinely resolved I/O callee
	// is unaffected.
	if ioMethods[method] && !dartDispatchNames[method] {
		return true
	}
	for _, kw := range dartExpensiveMethods {
		if containsKeyword(method, kw) {
			return true
		}
	}
	for _, kw := range dartExpensivePrefixes {
		if strings.Contains(target, kw) {
			return true
		}
	}
	return false
}

// rustExpensiveMethods are the Rust round-trip calls that are distinctive enough to
// stand on their name alone: the async filesystem and process APIs, and the query
// methods of the ecosystem's database crates.
//
// Deliberately absent: read, write, send, recv, connect, flush, poll and next. Every
// one is a channel, stream or buffer primitive before it is ever I/O, and it is the
// generic list's matching of exactly those that this handler exists to stop.
var rustExpensiveMethods = []string{
	// sqlx / diesel / sea-orm / tokio-postgres / rusqlite — query round trips
	"fetch_one", "fetch_all", "fetch_optional", "execute_many", "query_as",
	"query_one", "query_opt", "query_row", "load_iter", "get_results", "get_result",
	// reqwest / hyper — an HTTP request, not a channel send
	"send_request", "request_builder",
	// tokio::fs / std::fs — named operations rather than the read/write verbs
	"read_to_string", "read_to_end", "read_dir", "write_all", "create_dir_all",
	"copy_file", "remove_file", "metadata_at", "open_file",
	// serde round trips against a reader/writer, which carry the I/O with them
	"from_reader", "to_writer",
}

// rustIOReceivers are receiver tokens that make a call I/O whatever the method is
// named. A `.send` on a pool is a query; a `.send` on an mpsc Sender is a move
// between tasks, which is why the receiver rather than the method decides.
var rustIOReceivers = []string{
	"pool.", "db.", "conn.", "connection.", "client.", "transaction.", "tx_db.",
	"repo.", "repository.", "store.", "fs::", "tokio::fs", "reqwest::", "sqlx::",
	"diesel::", "redis.",
}

// rustPrimitiveNames are Rust and Tokio's in-memory primitives, excluded from the
// loose short-name performs_io path the way Dart's dispatch names are. They are the
// measured cause: on tokio, 42 of the high-severity call-in-loop findings named
// new, send, recv, poll and iter — a constructor and a channel send, reported as
// per-iteration database access. The exact-name, storage and receiver paths still
// apply, so a genuinely resolved I/O callee is unaffected.
var rustPrimitiveNames = map[string]bool{
	"new": true, "with_capacity": true, "default": true, "clone": true,
	"send": true, "recv": true, "try_send": true, "try_recv": true,
	"poll": true, "poll_next": true, "poll_ready": true, "next": true,
	"iter": true, "iter_mut": true, "into_iter": true, "collect": true,
	"lock": true, "read": true, "write": true, "borrow": true, "borrow_mut": true,
	"push": true, "insert": true, "get": true, "set": true, "update": true,
	"len": true, "is_empty": true, "await": true, "spawn": true, "wait": true,
}

// isExpensiveRustCall reports whether a Rust in-loop call is per-iteration I/O.
//
// It is the fifth ecosystem handler, and it exists for the reason the other four do:
// the language spells its in-memory primitives with the generic list's I/O verbs.
// Rust is worse than most, because an async runtime's whole vocabulary is send,
// recv, read, write and poll over channels and buffers that never leave the process.
//
// The rule is the same as Dart's and TypeScript's: evidence of I/O SHAPE, not a verb.
// A storage fact, a resolved callee the extractor flagged performs_io, an I/O
// receiver token, or a distinctive round-trip method. A bare call to a locally
// defined helper is treated as in-memory, which accepts a false negative for an
// unresolved helper that genuinely does I/O.
func isExpensiveRustCall(target string, storage map[string]bool, byName map[string]funcInfo, ioMethods map[string]bool) bool {
	if storage[target] {
		return true
	}
	if f, ok := byName[target]; ok && f.PerformsIO {
		return true
	}
	method := methodSegment(target)
	if ioMethods[method] && !rustPrimitiveNames[method] {
		return true
	}
	for _, kw := range rustExpensiveMethods {
		if containsKeyword(method, kw) {
			return true
		}
	}
	lower := strings.ToLower(target)
	for _, kw := range rustIOReceivers {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// composeAwaitMethods are Jetpack Compose gesture/frame suspend primitives. A loop that
// awaits these — the canonical `while (true) { awaitPointerEvent() }` gesture detector —
// is an event loop whose iteration count is driven by user input / frames, not a data
// size, so its Big-O in "n" is meaningless. Treated like a bounded fan-out: non-scaling.
var composeAwaitMethods = map[string]bool{
	"awaitPointerEvent": true, "awaitPointerEventScope": true,
	"awaitFirstDown": true, "awaitEachGesture": true, "awaitGesture": true,
	"awaitFrame": true, "withFrameNanos": true, "withFrameMillis": true,
	"awaitDragOrCancellation": true, "awaitTouchSlopOrCancellation": true,
	"awaitLongPressOrCancellation": true, "awaitVerticalDragOrCancellation": true,
	"awaitHorizontalDragOrCancellation": true,
}

// isEventLoop reports whether f's loop is a Compose input/frame event loop (see
// composeAwaitMethods) rather than a data-scaling loop.
func isEventLoop(f funcInfo) bool {
	for _, c := range f.CallsInLoop {
		if composeAwaitMethods[methodSegment(c)] {
			return true
		}
	}
	return false
}

// constantReceiver reports whether a call target's receiver is a constant/class — a
// `Const.method` or `Ns::Class.method` call — as opposed to a bare name or a
// lowercase-variable instance receiver. Such a call is a class-method or scope
// invocation, never an instance association read.
func constantReceiver(target string) bool {
	i := strings.LastIndex(target, ".")
	if i <= 0 {
		return false // bare name, no receiver
	}
	recv := target[:i]
	if j := strings.LastIndex(recv, "::"); j >= 0 {
		recv = recv[j+2:]
	}
	return recv != "" && recv[0] >= 'A' && recv[0] <= 'Z'
}

// methodSegment returns the method-name part of a call target — the substring
// after the last '.', or the whole string when there is no receiver.
func methodSegment(target string) string {
	if i := strings.LastIndex(target, "."); i >= 0 {
		return target[i+1:]
	}
	return target
}

// rubyInMemory reports whether a Ruby call target names an in-memory Hash/Array or
// ActiveModel dirty-tracking operation that collides with the cross-language
// expensive-method keywords (merge/fetch/insert/save) but is never a DB/IO round-trip.
// It is consulted ONLY for Ruby callers, so Python `session.merge`, JS `fetch`, and
// Kotlin `insert` stay expensive.
func rubyInMemory(target string) bool {
	method := methodSegment(target)
	switch method {
	case "merge", "merge!", "reverse_merge", "reverse_merge!":
		// Hash#merge / lazy ActiveRecord relation composition — never executes a query.
		return true
	case "insert":
		// Array#insert (in-memory). insert_all / insert! are distinct names, stay expensive.
		return true
	case "fetch":
		// Hash#fetch / Array#fetch are in-memory; cache.fetch / Rails.cache.fetch / redis
		// are real I/O and stay expensive.
		return !cacheReceiver(target)
	}
	// ActiveModel dirty tracking (`will_save_change_to_x?`, `saved_change_to_x`): matched
	// by the `save`/`change` keywords but purely in-memory attribute inspection.
	return strings.HasPrefix(method, "will_save_change_to_") || strings.HasPrefix(method, "saved_change_to_")
}

// cacheReceiver reports whether a call target's receiver names a cache/redis store,
// which makes an otherwise-ambiguous `fetch` a real I/O read (`cache.fetch`).
func cacheReceiver(target string) bool {
	i := strings.LastIndex(target, ".")
	if i < 0 {
		return false
	}
	recv := strings.ToLower(target[:i])
	return strings.Contains(recv, "cache") || strings.Contains(recv, "redis")
}

// callInLoopWhy builds the message for an aggregated call-in-loop finding from the
// expensive callees found inside the loop.
func callInLoopWhy(expensive []string) string {
	return fmt.Sprintf("Calls %s inside a loop — a likely N+1 / per-iteration I/O pattern; batch or hoist out of the loop.",
		strings.Join(expensive, ", "))
}

// containsKeyword reports whether kw appears in method as a whole word — bounded by
// a word boundary (string edge, separator, or camelCase hump) on each side. This is
// stricter than strings.Contains: it lets the CamelCase verb "Insert" match
// "LastInsertId" while preventing "flush" from firing on "fflush" or "update" on
// "updatedAt" — the within-token infix matches that caused false-positive N+1
// findings on C-style names.
func containsKeyword(method, kw string) bool {
	for from := 0; ; {
		i := strings.Index(method[from:], kw)
		if i < 0 {
			return false
		}
		i += from
		j := i + len(kw)
		leftOK := i == 0 || wordBoundary(method[i-1], method[i])
		rightOK := j == len(method) || wordBoundary(method[j-1], method[j])
		if leftOK && rightOK {
			return true
		}
		from = i + 1
	}
}

// wordBoundary reports whether the transition from byte a to byte b is a word
// boundary: a non-alphanumeric byte on either side, or a lower/digit→upper
// "camelCase" hump.
func wordBoundary(a, b byte) bool {
	if !isAlnum(a) || !isAlnum(b) {
		return true
	}
	return isLowerOrDigit(a) && isUpper(b)
}

func isUpper(c byte) bool        { return c >= 'A' && c <= 'Z' }
func isLowerOrDigit(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }
func isAlnum(c byte) bool        { return isUpper(c) || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

// dispatchMethods are background-job / mailer enqueue calls: their loop iterates a
// bounded handler/recipient set and does no scaling work in-process. These are Rails
// verbs; they cannot occur in another language, so they are matched regardless of file.
var dispatchMethods = map[string]bool{
	"perform_now": true, "perform_later": true, "perform_async": true,
	"perform_in": true, "perform_at": true,
	"deliver_now": true, "deliver_later": true,
}

// jvmDispatchMethods are the JVM equivalent: the loop hands each item to an executor /
// thread pool and does not do the work in-process, so it is a bounded fan-out exactly like
// Rails' perform_later. Kept SEPARATE from dispatchMethods and matched only in a .java/.kt
// file, because `submit` is an ordinary in-memory verb elsewhere (a form submit).
//
// Deliberately minimal. Two verbs are excluded on purpose — do not "complete" the set:
//
//   - `execute` — Executor.execute collides with JDBC Statement.execute, which is in
//     jvmExpensiveMethods. The dispatch check below runs BEFORE the expensive check, so
//     admitting `execute` would let a real per-iteration query be read as a job dispatch and
//     silence a true N+1. TestBoundedFanout_JdbcExecuteIsNotDispatch pins this.
//   - `enqueue` — OkHttp Call.enqueue is real async network I/O (it is in jvmExpensiveMethods
//     for that reason), not a job hand-off.
//
// Every verb added here must be justified by a receipt, not by symmetry: on the current corpus
// `submit` alone removes 2 false compounded findings (thingsboard), while tell/schedule/publish/
// send each remove zero.
var jvmDispatchMethods = map[string]bool{
	"submit": true,
}

// isDispatchCall reports whether an in-loop call hands work off to a job queue, mailer or
// executor rather than doing it in-process.
func isDispatchCall(target, file string) bool {
	m := methodSegment(target)
	if dispatchMethods[m] {
		return true
	}
	if strings.HasSuffix(file, ".java") || strings.HasSuffix(file, ".kt") {
		return jvmDispatchMethods[m]
	}
	return false
}

// isBoundedFanout reports whether f's loop is a bounded background-job / mailer / executor
// fan-out — it loops but only dispatches work (perform_later/deliver_later on Rails,
// executor.submit on the JVM), doing no scaling I/O and calling no further looping function.
// Such a loop hands each item off rather than doing it in-process, so it must not compound a
// caller's complexity to O(n²). Examples: SystemEventService.trigger, which loops a handler
// registry calling perform_now/perform_later; and thingsboard's processRuleNodePack, whose
// loop only calls executorService.submit.
func isBoundedFanout(f funcInfo, funcs map[string]funcInfo, storage, assoc map[string]bool) bool {
	if f.LoopDepth < 1 || len(f.CallsInLoop) == 0 {
		return false
	}
	// dispatchMethods holds Rails verbs only, so in practice only a Ruby caller ever gets
	// past the sawDispatch gate below. Apply the same Ruby in-memory guard analyze() applies
	// to its in-loop calls: without it the generic keyword list reads a Hash#merge / Array#insert
	// / Hash#fetch as a DB round-trip, suppresses the discount, and compounds the caller to O(n²).
	ruby := strings.HasSuffix(f.File, ".rb")
	sawDispatch := false
	for _, c := range f.CallsInLoop {
		if isDispatchCall(c, f.File) {
			sawDispatch = true
			continue
		}
		// A looping callee or a real I/O call means this is not a pure fan-out.
		if g, ok := funcs[c]; ok && g.LoopDepth >= 1 {
			return false
		}
		if isExpensiveCall(c, storage, assoc) && (!ruby || !rubyInMemory(c)) {
			return false
		}
		// otherwise a cheap read (e.g. an attribute like `inline`) — ignore.
	}
	return sawDispatch
}

// markNonScaling precomputes each function's BoundedFanout flag, once, where the byName
// resolution index exists. Both consumers of the flag — computeEffectiveDepths (which memoizes
// per name) and effectiveDepthOf (which recomputes per fact, so overloaded siblings don't borrow
// each other's depth) — read it instead of re-deriving it, so the two paths cannot disagree.
// byName is refreshed too: computeEffectiveDepths reads its funcInfo values out of that map.
func markNonScaling(funcs []funcInfo, byName map[string]funcInfo, storage, assoc map[string]bool) {
	for i := range funcs {
		funcs[i].BoundedFanout = isBoundedFanout(funcs[i], byName, storage, assoc)
	}
	for i := range funcs {
		if _, ok := byName[funcs[i].Name]; ok {
			byName[funcs[i].Name] = funcs[i]
		}
	}
}

// branchFree reports whether a function's body has no decision point at all: no
// conditional, no loop, no early exit the extractor counts.
//
// Such a function cannot be recursive and return. A self-call with no branch
// around it is reached on every call, so if it really named the function it would
// never terminate; what it names instead is a different function that shares the
// name. That is the whole of the forwarding idiom:
//
//	impl Listener for TcpListener {
//	    fn local_addr(&self) -> Result<SocketAddr> { self.local_addr() }
//	}
//
// which resolves to the inherent method, and with it a same-arity overload, an
// extension's method wrapping the one it extends, and a call that differs only in
// its return type. None of those can be told from a self-call by the call site,
// in a language-specific way or at all without types. All of them are branch-free.
//
// A cyclomatic complexity of exactly 1 is the test. A value of 0 means the
// extractor did not report one, which proves nothing and keeps the finding.
func branchFree(f funcInfo) bool { return f.Cyclomatic == 1 }

// computeEffectiveDepths estimates, per function, the worst-case loop nesting that
// compounds across the call graph: effDepth(f) is the larger of f's own scaling
// depth and, over the callees g invoked inside f's loops, the depth g is called at
// plus effDepth(g). Without a per-call depth from the extractor the call is taken
// to sit in f's deepest nest, which gives the old loopDepth(f) + effDepth(g).
// Cycles (recursion) are cut by charging only the node's own loop depth, so the DFS
// always terminates.
//
// Callers must run markNonScaling first: the non-scaling special cases are read from
// funcInfo.BoundedFanout, not re-derived here, so this path and effectiveDepthOf agree.
func computeEffectiveDepths(funcs map[string]funcInfo) map[string]int {
	memo := make(map[string]int)
	visiting := make(map[string]bool)

	var dfs func(name string) int
	dfs = func(name string) int {
		f, ok := funcs[name]
		if !ok {
			return 0
		}
		if visiting[name] {
			return f.scalingDepth() // cycle cut
		}
		if v, ok := memo[name]; ok {
			return v
		}
		// A bounded background-job/mailer fan-out loops a fixed number of times (its
		// handler set), so it neither scales itself nor compounds a caller to O(n²).
		// A Compose input/frame event loop is likewise non-scaling in "n".
		if f.BoundedFanout || isEventLoop(f) {
			memo[name] = 0
			return 0
		}
		visiting[name] = true
		best := 0
		// Only a call inside a loop that SCALES compounds. Calling a looping function
		// from a loop with a constant trip count multiplies the work by that constant,
		// not by n, and compounding over the raw in-loop call list charged it as a
		// factor of n anyway — which is how a walk over a three-element string literal
		// came to report a cubic worst case.
		onElement := map[string]bool{}
		for _, c := range f.CallsOnLoopElement {
			onElement[c] = true
		}
		for _, callee := range f.scalingLoopCalls() {
			g, known := funcs[callee]
			if !known {
				continue
			}
			// The cross-call form of the hierarchical rule the extractor applies
			// within one body. `for _, x := range xs { g(x) }`, where g loops over
			// what it was passed, visits each element of each x once across the whole
			// nest: the callee is finishing this walk, not running one per element.
			// Adding its depth reported the commonest shape in a traversal-heavy
			// codebase — a loop that hands each element to a helper — as quadratic.
			//
			// Both halves are required. Passing the element proves nothing on its own
			// (the callee may loop over a global), and looping over a parameter proves
			// nothing when the caller passes something else.
			if continuesWalk(f, onElement[callee], callee, g) {
				continue
			}
			// A callee multiplies with the loops AROUND ITS CALL, not with the
			// function's deepest nest. Adding its depth to the latter charged a helper
			// called once per row for a matrix the function builds further down.
			at, _ := f.callDepth(callee)
			if d := at + dfs(callee); d > best {
				best = d
			}
		}
		visiting[name] = false
		// The function's own nesting stands when no call reaches deeper. Both sides
		// are scaling depths (bounded loops discounted), so a loop over a constant or
		// literal set adds no factor of n across the call graph.
		res := f.scalingDepth()
		if best > res {
			res = best
		}
		memo[name] = res
		return res
	}

	// Entry points are visited in SORTED order, not map order.
	//
	// The cycle cut above returns the in-progress function's own scaling depth, so for a
	// call CYCLE the memoised result depends on which member the DFS happened to enter
	// from: enter at A and B is computed with A cut, enter at B and the reverse. Ranging
	// over the map directly made that entry point vary per run, so a mutually-recursive
	// function could be labelled O(n³) on one run and O(n³+) on the next — from an
	// unchanged tree.
	//
	// Sorting does not make the cut value more "correct" (a cycle has no well-defined
	// compounded depth), but it makes it the SAME answer every time, which is what a
	// snapshot has to be. Observed on 7 of 38 corpus repositories before this.
	names := make([]string, 0, len(funcs))
	for name := range funcs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]int, len(funcs))
	for _, name := range names {
		out[name] = dfs(name)
	}
	return out
}

// effectiveDepthOf computes f's own compounded depth from its own loop_depth and
// in-loop callees, so overloaded siblings (which share a name in `eff`) don't
// borrow each other's depth. A loop-free fact (no calls_in_loop) yields its
// loop_depth (0), so it can never produce a spurious compounded finding.
func effectiveDepthOf(f funcInfo, eff map[string]int, byName map[string]funcInfo) int {
	// Both non-scaling special cases must mirror computeEffectiveDepths, or the two paths
	// disagree about the same function: a bounded fan-out that is an overload sibling would
	// return its scaling depth here while the memo says 0. BoundedFanout is precomputed by
	// markNonScaling so both read one answer.
	if isEventLoop(f) || f.BoundedFanout {
		return 0
	}
	onElement := map[string]bool{}
	for _, c := range f.CallsOnLoopElement {
		onElement[c] = true
	}
	best := 0
	// The call list, the bounded-loop filter and the continued-walk skip all have to
	// match computeEffectiveDepths. They are the same question asked twice — once
	// memoised per name, once recomputed per fact so overloaded siblings do not
	// borrow each other's depth — and a difference between them shows up as one
	// function reported two ways.
	for _, callee := range f.scalingLoopCalls() {
		// Skip a self-call: eff[f.Name] already folds in f.LoopDepth, so adding
		// f.LoopDepth again below would double-count a directly-recursive function up
		// to O(n³). This mirrors the cycle-cut in computeEffectiveDepths, which
		// effectiveDepthOf must repeat because it recomputes per-fact (not per-name).
		if callee == f.Name {
			continue
		}
		if continuesWalk(f, onElement[callee], callee, byName[callee]) {
			continue // the callee is finishing this walk, not starting one per element
		}
		if d, ok := eff[callee]; ok {
			at, _ := f.callDepth(callee)
			if at+d > best {
				best = at + d
			}
		}
	}
	if own := f.scalingDepth(); own > best {
		return own
	}
	return best
}

// analyze is the pure analysis core: from the function list it produces a ranked
// list of findings. No I/O — this is the unit-tested heart of the tool.
func analyze(funcs []funcInfo, storage, routeHandlers, assoc map[string]bool) []Finding {
	byName := make(map[string]funcInfo, len(funcs))
	// The Python extractor emits in-loop calls to UNRESOLVED local functions in dotted
	// import-path form ("a.b.c.func"), while byName is keyed by the slash-form canonical
	// name ("a/b/c.func"), so byName[callee] never resolves them. byDotted indexes every
	// symbol by its dotted form too, letting the Python N+1 downgrade (below) recognise
	// that such a callee is a known non-I/O local. It is the same symbol reformatted — not
	// a short-name match — so it cannot over-suppress an unrelated same-named function.
	byDotted := make(map[string]funcInfo, len(funcs))
	nameCount := make(map[string]int, len(funcs))
	for _, f := range funcs {
		byName[f.Name] = f
		byDotted[strings.ReplaceAll(f.Name, "/", ".")] = f
		nameCount[f.Name]++
	}
	// In languages with method overloading (Java, Kotlin, Swift) sibling overloads
	// share one fact name, so a call from one overload to another resolves to that
	// shared name and looks like self-recursion. We cannot tell delegation from
	// recursion without signatures, so for an overloaded name we suppress the
	// name-based self-recursion signal (genuine *mutual* recursion between distinct
	// names is unaffected — it goes through the multi-node SCC branch below).
	overloaded := func(n string) bool { return nameCount[n] > 1 }

	// Detect recursion ONLY from the extractor's recursive_self flag, which the
	// language extractors set on a genuine direct self-call. We deliberately do NOT
	// infer recursion from a resolved self-edge in f.Calls: a self-edge is also
	// produced by a call to a different overload/override/stdlib method that shares
	// the enclosing method's bare name (`super.setSelected(…)` in an override,
	// `decode(_:forKey:)` from a `decode(key:)` extension), which is not recursion —
	// the extractor, which alone can see argument labels, already excludes those from
	// recursive_self. The flag is still gated on non-overloaded names as defense in
	// depth. Mutual recursion via Tarjan SCCs was likewise removed: in call graphs
	// that include closure/delegate callback edges (pervasive in iOS coordinators)
	// SCCs flag event-driven callback cycles as "recursion". Both are accepted false
	// negatives — rare in application code, far outweighed by the noise removed.
	recursive := make(map[string]bool)
	for _, f := range funcs {
		if f.Recursive && !overloaded(f.Name) && !branchFree(f) {
			recursive[f.Name] = true
		}
	}

	// Mark the non-scaling special cases before any depth is computed, so the per-name and
	// per-fact paths below read one shared answer rather than each deriving their own.
	markNonScaling(funcs, byName, storage, assoc)

	eff := computeEffectiveDepths(byName)

	// Short-name index of I/O methods. An in-loop call on a receiver of unknown type
	// is recorded as its text (`service.fetchFurly`, `dao.insert`), and the method
	// segment is all there is to go on.
	//
	// In the languages of strictIOIndex a name is on the list when every function of
	// that language that bears it performs I/O, and it is read only for a callee
	// that does not resolve. One namesake that does no I/O keeps a name off: `find`,
	// `get` and `update` are each borne by a query somewhere and by a dozen helpers,
	// and a call to the name says nothing about which. And a callee that resolves to
	// a function of this repository is judged by that function, not by what else
	// shares its name.
	//
	// Elsewhere the list is what it always was: every name some I/O function of any
	// language bears, read for any callee. Read one call at a time that was right 8
	// times in 23, and it stays for Python because the alternative is worse there:
	// Python's unresolved callees are mostly real clients held in attributes
	// (`s3_hook.get_key`, `credential.get_token`), whose names are common words.
	// Narrowing it removed real I/O in about half the Python findings read.
	//
	// Go has no list: its callers resolve or are read by package.
	type ioNameCount struct{ all, io int }
	counts := make(map[string]map[string]*ioNameCount)
	loose := make(map[string]bool)
	for _, f := range funcs {
		lang := langFamily(f.File)
		if lang == ".go" {
			continue
		}
		m := methodSegment(f.Name)
		if f.PerformsIO && !jvmIONameDenylist[m] {
			loose[m] = true
		}
		if counts[lang] == nil {
			counts[lang] = make(map[string]*ioNameCount)
		}
		c := counts[lang][m]
		if c == nil {
			c = &ioNameCount{}
			counts[lang][m] = c
		}
		c.all++
		if f.PerformsIO {
			c.io++
		}
	}
	strict := make(map[string]map[string]bool, len(counts))
	for lang, names := range counts {
		strict[lang] = make(map[string]bool)
		for m, c := range names {
			if c.io == c.all && !jvmIONameDenylist[m] {
				strict[lang][m] = true
			}
		}
	}

	var findings []Finding
	for _, f := range funcs {
		// Compute this fact's own compounded depth from its own loop_depth and
		// in-loop callees, rather than reading eff[f.Name]: with overloads, byName
		// (and thus eff) collapses to one representative per name, which would
		// otherwise make a loop-free overload borrow a looping sibling's depth.
		effHere := effectiveDepthOf(f, eff, byName)

		// A Compose input/frame event loop (`while (true) { awaitPointerEvent() }`)
		// iterates on user input, not a data size, so its structural Big-O is
		// meaningless — suppress its nested-loop / compounded / call-in-loop findings.
		eventLoop := isEventLoop(f)

		// 1. Nested loops within the function body. Depth counts only loops whose
		// bound scales with input — nesting over literal/constant/varargs collections
		// or a while(true) event loop does not scale in "n", so it neither triggers a
		// finding nor inflates the exponent.
		scaling := f.scalingDepth()
		if scaling >= 2 && !eventLoop {
			ev := []string{fmt.Sprintf("loop_depth=%d, loop_count=%d, cyclomatic=%d",
				f.LoopDepth, f.LoopCount, f.Cyclomatic)}
			why := fmt.Sprintf("Loops nest %d deep. %s", scaling, depthIsNotComplexity)
			if f.loopDiscounted() {
				ev = append(ev, fmt.Sprintf("scaling_loop_depth=%d (bounded loops discounted)", scaling))
				why += " (Enclosing loops over bounded or constant ranges are already excluded.)"
			}
			findings = append(findings, Finding{
				Symbol: f.Name, File: f.File, Line: f.Line, Repo: f.Repo, Package: f.Package,
				Kind: "nested-loop", Depth: scaling, Severity: structuralSeverity,
				Confidence: confidenceFor("nested-loop", false, scaling, f.loopDiscounted()),
				Why:        why,
				Evidence:   ev,
			})
		}

		// 2. Nesting that continues across the call graph (beyond local nesting).
		// Skip directly-recursive functions: the recursion finding below already covers
		// them, and their self-edge does not represent independent nested iteration.
		if effHere >= 2 && effHere > scaling && !recursive[f.Name] && !eventLoop {
			levels := fmt.Sprintf("%d levels", effHere)
			if effHere >= deepEstimateDepth {
				levels = "four or more levels"
			}
			findings = append(findings, Finding{
				Symbol: f.Name, File: f.File, Line: f.Line, Repo: f.Repo, Package: f.Package,
				Kind: "compounded", Depth: effHere, Severity: structuralSeverity,
				Confidence: confidenceFor("compounded", false, effHere, f.loopDiscounted()),
				Why: fmt.Sprintf("A loop here calls a function that itself loops: %s of nesting across the call graph. %s",
					levels, depthIsNotComplexity),
				Evidence: callsInLoopEvidence(f, byName),
			})
		}

		// 3. Direct recursion. A loop in the same body warrants a closer look, but is
		// NOT asserted to be exponential: the common recursive-with-loop shape is a
		// tree / divide-and-conquer traversal that visits each node once (≈O(n) or
		// O(n log n)), not a combinatorial blow-up. Over-claiming "exponential" here
		// was a systematic false alarm, so the wording asks the reader to confirm the
		// input shrinks each level rather than declaring a complexity class.
		if recursive[f.Name] {
			bigO, why := "O(?) — recursive",
				"Directly recursive (calls itself); depth is bounded by input shape — verify a base case and stack-depth limits."
			if f.LoopDepth >= 1 {
				bigO, why = "O(?) — recursive (with loop)",
					"Recurses and loops in the same body — confirm each level consumes a disjoint subset of the input (a tree / divide-and-conquer traversal) rather than re-scanning the whole input; verify a base case and stack-depth limits."
			}
			findings = append(findings, Finding{
				Symbol: f.Name, File: f.File, Line: f.Line, Repo: f.Repo, Package: f.Package,
				Kind: "recursion", BigO: bigO, Severity: "medium", Confidence: confidenceFor("recursion", false, 0, false), Why: why,
			})
		}

		// 4. Expensive calls inside a loop (likely N+1 / per-iteration I/O). Emit ONE
		// finding per function that enumerates every offending callee, rather than a
		// near-duplicate row per callee — every such row carried the same symbol,
		// declaration line and Big-O, so they read as noise; the distinguishing
		// callee already lives in the evidence.
		// Only calls inside a loop that REPEATS are N+1 candidates; a call in a provably
		// constant loop (a literal/constant collection, range(<const>), 0..2) runs a fixed
		// number of times, not a pattern that grows with input. Note this is a weaker test
		// than the Big-O one: `while (true)` / `for {}` adds no factor of n, yet a
		// parent-chain walk inside one issues a query per level, so its calls DO stay
		// candidates. Extractors that emit calls_in_scaling_loop supply that subset — even
		// when empty; others fall back to all in-loop calls, unchanged.
		inLoopCalls := f.scalingLoopCalls()
		var expensive []string
		evidence := make([]string, 0, len(inLoopCalls))
		var calls []CallEvidence
		ruby := strings.HasSuffix(f.File, ".rb")
		swift := strings.HasSuffix(f.File, ".swift")
		jvm := strings.HasSuffix(f.File, ".kt") || strings.HasSuffix(f.File, ".java")
		py := strings.HasSuffix(f.File, ".py")
		ts := strings.HasSuffix(f.File, ".ts") || strings.HasSuffix(f.File, ".tsx") ||
			strings.HasSuffix(f.File, ".js") || strings.HasSuffix(f.File, ".jsx") ||
			strings.HasSuffix(f.File, ".vue") || strings.HasSuffix(f.File, ".svelte")
		golang := strings.HasSuffix(f.File, ".go")
		cs := strings.HasSuffix(f.File, ".cs")
		dart := strings.HasSuffix(f.File, ".dart")
		rust := strings.HasSuffix(f.File, ".rs")
		rs := strings.HasSuffix(f.File, ".rs")
		php := strings.HasSuffix(f.File, ".php")
		cpp := strings.HasSuffix(f.File, ".cpp") || strings.HasSuffix(f.File, ".cc") ||
			strings.HasSuffix(f.File, ".cxx") || strings.HasSuffix(f.File, ".hpp") ||
			strings.HasSuffix(f.File, ".hxx") || strings.HasSuffix(f.File, ".h") ||
			strings.HasSuffix(f.File, ".c")
		// confirmedIO: a named call is known to do I/O, by a fact about the callee:
		// it is a storage fact, or it resolves to a function flagged performs_io.
		// It is what promotes a call-in-loop to "high".
		//
		// It used to accept readings of the name as well: a method name some
		// function flagged performs_io also bears, a curated ORM method, a receiver
		// token, an association of that name. Read one call at a time those were
		// right 24 times in 58, and they carried the same 0.9 as a fact.
		confirmedIO := false
		// The deepest scaling loop any of the reported calls sits in, and whether
		// every one of those depths came from the extractor.
		callNesting, callNestingKnown := 0, true
		for _, callee := range inLoopCalls {
			// The extractor resolved this call to a library member it knows to do
			// no I/O (`sqlalchemy.update`, `re.search`). That is a fact about the
			// call, and no reading of its name stands against it.
			if slices.Contains(f.PureCalls, callee) {
				continue
			}
			ioMethods := loose
			if lang := langFamily(f.File); strictIOIndex[lang] {
				ioMethods = strict[lang]
				// The index is for a callee nobody could see.
				if _, ok := byName[callee]; ok {
					ioMethods = nil
				}
			}
			var isExpensive bool
			switch {
			case swift:
				// Swift: the generic keyword list matches ubiquitous in-memory verbs
				// (updateCenter, updateValue, insertArrangedSubview), so trust the
				// extractor's transitive performs_io signal on the resolved callee.
				isExpensive = isExpensiveSwiftCall(callee, storage, byName)
			case jvm:
				// Kotlin/Java: the cross-language verbs (update on a StateFlow, load on
				// a view, await on a Deferred) are in-memory here, so use the curated
				// Room/Retrofit/JPA list plus storage/performs_io facts.
				isExpensive = isExpensiveJvmCall(callee, storage, byName, ioMethods)
			case ts:
				// TS/JS: the generic verbs collide with exported in-memory helpers
				// (getFetchAllUpdate, updateState), so use the I/O-shape gate — storage
				// / performs_io (direct or transitive) / HTTP-DB receiver / global fetch /
				// ORM query method.
				isExpensive = isExpensiveTSCall(callee, storage, byName, ioMethods)
			case dart:
				// Dart: the generic list carries `where` for Ruby's ActiveRecord, but
				// Dart's `.where()` is Iterable.where — the in-memory filter, and the
				// single most common collection call in the language. On AppFlowy it
				// produced 58 call-in-loop findings about list processing. Use the
				// extractor's import-gated performs_io signal plus an I/O receiver.
				isExpensive = isExpensiveDartCall(callee, storage, byName, ioMethods)
			case rust:
				// Rust: an async runtime's vocabulary IS the generic keyword list.
				// send/recv on a channel, read/write on a buffer, poll on a future and
				// new for construction are all in-memory, and on tokio they produced 42
				// high-severity findings claiming per-iteration I/O.
				isExpensive = isExpensiveRustCall(callee, storage, byName, ioMethods)
			case py:
				// Python: the cross-language keyword "merge" (SQLAlchemy Session.merge)
				// collides with pure helpers like merge_dicts, and every module-level
				// function is "exported", so a name-only match escalates in-memory code to
				// high. Prefer resolution: a callee that resolves to a known non-I/O local
				// function is never an N+1, whatever its name; keep the keyword match only
				// as a fallback for unresolved callees.
				nameMatch := isExpensiveCall(callee, storage, nil)
				resolved, ok := byName[callee]
				if !ok {
					// Unresolved local calls arrive in dotted import-path form; resolve them
					// against the dotted index so the downgrade sees non-I/O locals too.
					resolved, ok = byDotted[callee]
				}
				if ok && !resolved.PerformsIO && !storage[callee] {
					nameMatch = false
				}
				isExpensive = isConfirmedIOCall(callee, storage, byName, ioMethods) || nameMatch
			case rs:
				// Rust: there is no reliable cross-language verb list for Rust I/O
				// (method names are highly project-specific), so — like Swift/TS —
				// trust the extractor's signal: a callee that resolves to a
				// performs_io function (transitively), a storage fact, or an
				// unambiguous DB/HTTP method. Bare in-memory verbs never fire.
				isExpensive = isConfirmedIOCall(callee, storage, byName, ioMethods)
			case cpp:
				// C/C++: there is no reliable cross-language verb list for C++ I/O
				// (method names are project-specific), so — like Rust/Swift — trust the
				// extractor's signal: a callee resolving to a performs_io function
				// (transitively from a fopen/fread/socket primitive), a storage fact,
				// or an unambiguous DB/HTTP method. Bare in-memory verbs never fire.
				isExpensive = isConfirmedIOCall(callee, storage, byName, ioMethods)
			case php:
				// PHP/WordPress: the codebase is full of exported in-memory helpers
				// named with DB verbs (get_*, wp_get_*, query_*), so a name-only match
				// over-fires. Like Python, prefer resolution — a callee resolving to a
				// known non-I/O local is never an N+1 — and keep the keyword match only
				// as a fallback for unresolved callees, on top of the extractor's
				// io_direct/performs_io signal.
				nameMatch := isExpensiveCall(callee, storage, nil)
				if resolved, ok := byName[callee]; ok && !resolved.PerformsIO && !storage[callee] {
					nameMatch = false
				}
				if phpInMemory(callee) {
					nameMatch = false
				}
				isExpensive = isConfirmedIOCall(callee, storage, byName, ioMethods) || nameMatch
			case cs:
				// C#: the generic gate, minus the collection methods that own several
				// of its verbs. Not PHP's "a callee resolving to a method that does no
				// I/O is not an N+1": C# performs_io stops at an injected interface, so
				// `DeleteEpisode`, whose body is `_libraryManager.DeleteItem(…)`,
				// carries no flag, and trusting that removed it.
				isExpensive = isExpensiveCall(callee, storage, nil) && !csInMemoryMethods[methodSegment(callee)]
			case golang:
				// Go: the generic verbs minus other ecosystems' API names, and a
				// predicate or constructor is not I/O. See isExpensiveGoCall.
				isExpensive = isExpensiveGoCall(callee, storage, byName)
			default:
				// nolint:staticcheck // QF1001 wants De Morgan's law applied here.
				// "expensive, and NOT a Ruby in-memory call" is the rule as anyone
				// states it; "(not ruby) or (not in-memory)" is the same predicate
				// and reads as neither.
				isExpensive = isExpensiveCall(callee, storage, nil) && !(ruby && rubyInMemory(callee))
			}
			// Positive evidence overrules every name gate above: a callee that resolves
			// to a function flagged performs_io is I/O whatever it is called, in any
			// language.
			//
			// The converse is NOT applied. "Resolves, and carries no flag" does not
			// mean "does no I/O": performs_io follows calls edges, and a call on a
			// field that holds a client (`self.hook.copy(…)`, `_repo.Save(…)`) is not
			// one. Tried over every language at once, that rule removed 5 of 5 labelled
			// Java findings it touched, 4 of 4 TypeScript and 3 of 6 C#, and of 18
			// Python removals read against source 11 were real I/O.
			if !isExpensive {
				r, ok := byName[callee]
				if !ok {
					r, ok = byDotted[callee]
				}
				isExpensive = ok && r.PerformsIO && callee != f.Name
			}
			// The same for a call the extractor identified as an I/O entry point
			// in this body: `net/http.Client.Do`, not a name that resembles it.
			if !isExpensive {
				isExpensive = slices.Contains(f.IOCalls, callee)
			}
			if !isExpensive {
				continue
			}
			basis := callBasis(f, callee, golang, storage, byName, byDotted, ioMethods)
			if basisIsFact(basis) {
				confirmedIO = true
			}
			expensive = append(expensive, callee)
			evidence = append(evidence, "call_in_loop="+callee)
			d, known := f.callDepth(callee)
			if d > callNesting {
				callNesting = d
			}
			callNestingKnown = callNestingKnown && known
			ce := CallEvidence{Callee: callee, Basis: basis}
			if known {
				ce.Depth = d
			}
			calls = append(calls, ce)
		}
		// f.Claimed says the query-loops explainer already reports a query per
		// iteration against this symbol, from the receiver's type rather than from a
		// keyword. Deferring here rather than earlier keeps confirmedIO, which the
		// nested-loop and compounded findings on the same function still read.
		if len(evidence) > 0 && !eventLoop && !f.Claimed {
			// Severity is evidence-based, not export-based (exported is noise in Python and
			// on the JVM). Every emitted finding has already passed a per-language
			// expensiveness gate, so it is at least worth review (medium). It rises to high
			// only when there is real evidence: a confirmed I/O call (a storage fact, or
			// a resolved callee flagged performs_io) or a route handler (always hot). A
			// match by name (a curated method, a receiver token, a generic verb) stays
			// medium. Cold-path findings drop to low in the post-process below.
			sev := "medium"
			if routeHandlers[f.Name] || confirmedIO {
				sev = "high"
			}
			// The exponent is the nesting of the loops AROUND THE CALL. A
			// call-in-loop finding says how often the call is made, and nothing else
			// decides that: not what the callee does per call (the `compounded`
			// finding's subject, reported beside this one when it applies), and not a
			// deeper nest elsewhere in the function that the call is not inside.
			// Both were once folded in. The first printed O(n³) on a single loop over
			// rows whose callee looped twice; the second printed O(n²) for one query
			// per row because the function also built a matrix further down. None of
			// 215 hand-read call-in-loop findings above O(n) was right.
			//
			// Extractors that do not report a per-call depth leave callNesting at the
			// function's deepest nest, which is an upper bound.
			depth := callNesting
			if depth < 1 {
				depth = 1
			}
			// In one loop the claim is O(n): the call is made once per element, and
			// that is something a reader can check against the loop. In a nest it
			// is not a claim this can make. The loops around the call are counted,
			// and whether they multiply is exactly what the count does not say: of
			// 55 hand-read findings that printed O(n²) or more for a call in a nest,
			// one was right. The rest were a parent-then-children walk, a batch
			// being drained, or a loop over a fixed list around the real one. So a
			// nest is reported as the depth it is, as nested-loop and compounded
			// findings are.
			finding := Finding{
				Symbol: f.Name, File: f.File, Line: f.Line, Repo: f.Repo, Package: f.Package,
				Kind: "call-in-loop", Severity: sev,
				Confidence: confidenceFor("call-in-loop", confirmedIO, depth, f.loopDiscounted()),
				Why:        callInLoopWhy(expensive),
				Evidence:   evidence,
				Calls:      calls,
			}
			if depth == 1 {
				finding.BigO = bigOForDepth(1)
			} else {
				finding.Depth = depth
				finding.Why += fmt.Sprintf(" The call sits inside %d nested loops. %s", depth, depthIsNotComplexity)
			}
			findings = append(findings, finding)
		}
	}

	// Downrank one-shot / non-runtime code (migrations, dev & build tooling, CLI
	// commands, module main guards): the loop nesting is real but it never runs on a
	// request/scheduler hot path, so it should not compete with production risks. Drop to
	// `low` and lower confidence — kept visible (still returned/filterable), just out of
	// the high/medium buckets, which stay dominated by production hot paths.
	for i := range findings {
		if severityRank(findings[i].Severity) > severityRank("low") && isColdPath(findings[i].File) {
			findings[i].Severity = "low"
			findings[i].Confidence = minConfidence(findings[i].Confidence, 0.4)
			findings[i].Why += " (One-shot / non-runtime path — downranked.)"
		}
	}

	// Rank within each severity tier: cost (Big-O weight) × confidence, with a hot-path
	// bonus. Computed after the cold-path pass so it reflects the final confidence.
	for i := range findings {
		findings[i].RiskScore = riskScore(findings[i], routeHandlers)
	}

	sortFindings(findings)
	return findings
}

// confidenceFor scores how much to trust a finding as a real risk (distinct from severity,
// which is how bad it is if real). The base is set by the finding kind's reliability: a
// fact-confirmed I/O call-in-loop is the most trustworthy; a cross-call-graph `compounded`
// estimate the least; a heuristic-only (name-keyword) call-in-loop and a recursion (whose
// class we deliberately do not assert) sit low. It then decays with the reported Big-O
// exponent — deep polynomials are almost always structural over-counts — and drops further
// when bounded loops had to be discounted to reach the reported depth.
func confidenceFor(kind string, confirmed bool, depth int, discounted bool) float64 {
	var conf float64
	switch kind {
	case "call-in-loop":
		if confirmed {
			conf = 0.9 // a storage fact, or a resolved callee flagged performs_io
		} else {
			conf = 0.6 // name-keyword heuristic only
		}
	case "nested-loop":
		conf = 0.8 // local lexical nesting, directly observed
	case "compounded":
		conf = 0.55 // worst-case nesting inferred across the call graph — least reliable
	case "recursion":
		conf = 0.5 // flagged, but we do not assert a complexity class
	default:
		conf = 0.6
	}
	switch {
	case depth >= 6:
		conf = minConfidence(conf, 0.35)
	case depth >= 4:
		conf = minConfidence(conf, 0.5)
	}
	if discounted {
		conf = minConfidence(conf, 0.6)
	}
	return conf
}

func minConfidence(a, b float64) float64 {
	if a == 0 || b < a {
		return b
	}
	return a
}

// findingWeight is the cost weight of a finding for the risk score: its Big-O's
// weight where it has one, and otherwise its depth on the same scale, so that
// stating a depth where an exponent used to be printed did not reorder anything.
func findingWeight(f Finding) float64 {
	if f.BigO != "" {
		return complexityWeight(f.BigO)
	}
	// Capped where the Big-O label for the same depth was: past three levels the
	// exact count of an estimate is not worth ranking on.
	if (f.Kind == "compounded" || f.Kind == "call-in-loop") && f.Depth >= deepEstimateDepth {
		return 4
	}
	return float64(f.Depth)
}

// complexityWeight maps a Big-O label to a numeric cost weight for the risk score. Unlike
// bigORank (which sorts the recursive buckets last at 1000), this keeps recursion at a
// moderate weight so it does not dominate the ranking.
func complexityWeight(bigO string) float64 {
	switch bigO {
	case "O(1)":
		return 0.5
	case "O(n)":
		return 1
	case "O(n²)":
		return 2
	case "O(n³)":
		return 3
	case "O(n³+)":
		return 4
	}
	if strings.HasPrefix(bigO, "O(n^") {
		var k int
		if _, err := fmt.Sscanf(bigO, "O(n^%d)", &k); err == nil {
			return float64(k)
		}
	}
	return 2.5 // recursive / unknown
}

// riskScore ranks a finding by estimated cost (Big-O weight) × trust (confidence), lifted
// for a known request hot path (an HTTP route handler — the one hot path we can identify
// precisely; the scheduler is not tagged, so it ranks on cost × confidence alone).
func riskScore(f Finding, routeHandlers map[string]bool) float64 {
	s := findingWeight(f) * f.Confidence
	if routeHandlers[f.Symbol] {
		s *= 1.3
	}
	return math.Round(s*100) / 100
}

// pyIOMethods are Python method names that are unambiguously a DB/session round-trip
// (SQLAlchemy / DBAPI). Unlike the broad expensiveMethods keyword list, this set omits
// verbs that collide with in-memory helpers (merge → merge_dicts, get/save/update), so
// a match here is trusted to escalate a Python call-in-loop to high.
var pyIOMethods = map[string]bool{
	"execute": true, "executemany": true, "scalars": true, "scalar": true,
	"fetchone": true, "fetchall": true, "fetchmany": true,
	"bulk_create": true, "bulk_update": true, "bulk_save_objects": true,
	"bulk_insert_mappings": true, "add_all": true,
	"get_or_create": true, "update_or_create": true,
}

// isConfirmedIOCall reports whether an in-loop call has the shape of real I/O — a storage
// fact, a resolved callee flagged performs_io, an unambiguous DB method (SQLAlchemy/DBAPI
// or a distinctive ORM query method), the global fetch, an I/O method from the transitive
// performs_io short-name index, or an HTTP/DB receiver/module prefix — as opposed to a
// name that merely collides with a generic DB verb (Find/Save/Update/where). It gates
// whether a call is reported, for the languages that have no usable verb list.
//
// It no longer decides severity, despite its name. Most of what it accepts is a
// reading of the callee's name, and only a fact about the callee confirms: see
// basisIsFact.
func isConfirmedIOCall(target string, storage map[string]bool, byName map[string]funcInfo, ioMethods map[string]bool) bool {
	if storage[target] {
		return true
	}
	if f, ok := byName[target]; ok && f.PerformsIO {
		return true
	}
	m := methodSegment(target)
	if pyIOMethods[m] || ioMethods[m] || m == "fetch" {
		return true
	}
	for _, kw := range tsExpensiveMethods {
		if containsKeyword(m, kw) {
			return true
		}
	}
	for _, kw := range expensivePrefixes {
		if strings.Contains(target, kw) {
			return true
		}
	}
	return false
}

func callsInLoopEvidence(f funcInfo, byName map[string]funcInfo) []string {
	var ev []string
	for _, c := range f.CallsInLoop {
		if cf, ok := byName[c]; ok && cf.LoopDepth >= 1 {
			ev = append(ev, fmt.Sprintf("%s (loop_depth=%d)", c, cf.LoopDepth))
		}
	}
	if len(ev) == 0 {
		ev = append(ev, fmt.Sprintf("loop_depth=%d", f.LoopDepth))
	}
	return ev
}

func severityRank(s string) int {
	switch s {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

// sortFindings orders by severity (desc), then risk score (desc) so the worst, most-certain
// risks lead within a tier, then symbol/kind for a deterministic tiebreak.
func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		ri, rj := severityRank(f[i].Severity), severityRank(f[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if f[i].RiskScore != f[j].RiskScore {
			return f[i].RiskScore > f[j].RiskScore
		}
		if f[i].Symbol != f[j].Symbol {
			return f[i].Symbol < f[j].Symbol
		}
		if f[i].Kind != f[j].Kind {
			return f[i].Kind < f[j].Kind
		}
		// Overloads share a symbol name. A page boundary must fall in the same
		// place on every call, so the order between them is fixed too.
		if f[i].File != f[j].File {
			return f[i].File < f[j].File
		}
		return f[i].Line < f[j].Line
	})
}

// --- MCP tool ---

type args struct {
	Package     string `json:"package,omitempty" jsonschema:"Package or function name substring, e.g. 'internal/app'."`
	Repo        string `json:"repo,omitempty" jsonschema:"Repository label (multi-repo snapshots), e.g. 'go-service'."`
	Symbol      string `json:"symbol,omitempty" jsonschema:"Symbol name substring."`
	MinSeverity string `json:"min_severity,omitempty" jsonschema:"'low' (default), 'medium', or 'high'."`
	Kind        string `json:"kind,omitempty" jsonschema:"Finding kind: call-in-loop, query-loop, recursion, nested-loop or compounded. Several are comma-separated. Default: all."`
	Limit       int    `json:"limit,omitempty" jsonschema:"Maximum findings (1-1000). Default 100."`
	Offset      int    `json:"offset,omitempty" jsonschema:"Findings to skip, for paging past limit. Default 0."`
	OutputMode  string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, counts by severity and kind plus the top findings), 'compact' (markdown table), or 'full' (complete JSON with the summary)."`
	MaxTokens   int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

// summary holds the aggregate counts. Every field except RepoWideFindings is
// computed over the FILTERED findings — i.e. over exactly the set the caller is
// shown. See summarize().
type summary struct {
	FunctionsAnalyzed int            `json:"functions_analyzed"`
	TotalFindings     int            `json:"total_findings"`
	BySeverity        map[string]int `json:"by_severity"`
	// ByBigO counts the findings that state a Big-O (call-in-loop, recursion);
	// ByDepth the structural ones (nested-loop, compounded), by their depth label.
	ByBigO  map[string]int `json:"by_big_o"`
	ByDepth map[string]int `json:"by_depth,omitempty"`
	// RepoWideFindings is the unfiltered total, carried so a filtered result can
	// state the population it was drawn from. It is the ONLY unfiltered number here,
	// and it is named so it cannot be mistaken for the others.
	RepoWideFindings int `json:"repo_wide_findings,omitempty"`
}

type response struct {
	Findings []Finding `json:"findings"`
	Summary  summary   `json:"summary"`
	Returned int       `json:"returned"`
	// Offset is where this page starts in the ranked, filtered findings, and
	// NextOffset where the next one does. NextOffset is absent on the last page.
	Offset     int    `json:"offset,omitempty"`
	NextOffset int    `json:"next_offset,omitempty"`
	Note       string `json:"note"`
}

const toolDescription = "Ranked performance risks per function: I/O or database calls inside loops (a likely N+1), recursion, " +
	"nested loops, and nesting that continues across the call graph. Each finding has symbol (file:line), kind, severity " +
	"(high, medium, low) and a plain-English why. A recursion finding, and a call-in-loop finding whose call sits in one loop, " +
	"has big_o, the estimated worst case. A nested-loop or compounded finding, and a call-in-loop finding whose call sits in " +
	"a nest, has depth instead: a count of nested loops, which is a fact about the code's shape and NOT a complexity, since " +
	"nested loops multiply only when each walks an independent collection. Parser-derived and " +
	"deterministic: an estimate, not a proof. " +
	"A query-loop finding is one the query-loops explainer made (Rails): a database query per iteration, read from the " +
	"receiver's type. All of them are returned here, including those that explainer counts in its rollup and does not list. " +
	"Supports Go, Python, Ruby, Swift, Kotlin, Scala, Dart, TypeScript, Java, C++ and C#. " +
	"kind narrows the result to one or more kinds. A call returns at most 1000 findings; a full result says where the " +
	"next page starts (next_offset), and offset reads it. " +
	"Findings of medium severity and up also appear in query_insights(explainer=\"performance\"); query-loop findings " +
	"appear in query_insights(explainer=\"query-loops\")."

// Register adds the analyze_performance tool to the given MCP server. Calls are
// recorded by the OSS value middleware, which is registered once on this shared
// MCP server and so observes the analyzers too.
func Register(srv *mcp.Server, store func() *facts.Store) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "analyze_performance",
		Description: toolDescription,
	}, func(_ context.Context, _ *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
		if store().Count() == 0 {
			return mcputil.ErrorResult("No facts available. Run generate_snapshot first."), nil, nil
		}

		if bad := unknownKinds(in.Kind); len(bad) > 0 {
			return mcputil.ErrorResult(fmt.Sprintf("Unknown kind %s. Kinds: %s.",
				strings.Join(bad, ", "), strings.Join(kindNames(), ", "))), nil, nil
		}

		funcs, storage, routeHandlers, assoc := collect(store())
		all := analyze(funcs, storage, routeHandlers, assoc)
		// And the query-loops explainer's findings, every one of them: see
		// queryLoopFindings.
		all = append(all, queryLoopFindings(store(), routeHandlers)...)
		sortFindings(all)

		// Filter FIRST, then summarize. The summary must be computed from the same
		// set the caller is shown; building it beforehand is how a filtered query came
		// to print repo-wide totals above an empty table (new/56). `all` stays around
		// only to report the population a zero was drawn from — never to count.
		matched := filterFindings(all, in)
		sum := summarize(all, matched)

		limit := in.Limit
		if limit <= 0 {
			limit = 100
		}
		if limit > 1000 {
			limit = 1000
		}
		// `shown` is the display slice; `matched` is what the counts describe. Keep them
		// separate: the summary renderer used to receive the TRUNCATED slice and count
		// it, so the headline reported the display limit — "Performance: 100 findings"
		// above "(high 145 / medium 499 / low 267)", which sums to 911. That is the very
		// defect new/56 fixed, reintroduced by its own fix, because the tests called
		// summarize() directly and never exercised a result set larger than the limit.
		shown, offset, next := page(matched, in.Offset, limit)

		switch mcputil.ResolveOutputMode(in.OutputMode, mcputil.ModeSummary) {
		case mcputil.ModeFull:
			return mcputil.JSONResultCapped(response{
				Findings: shown,
				Summary:  sum,
				Returned: len(shown), Offset: offset, NextOffset: next,
				Note: "big_o (recursion, and a call-in-loop whose call sits in one loop) is a deterministic estimate of worst case from parser facts, not a proof. " +
					"depth (nested-loop, compounded, and a call-in-loop whose call sits in a nest) is a count of nested loops and NOT a complexity: nested loops multiply only when each walks an independent collection. Supports Go, Python, Ruby, Swift, Kotlin, Scala, Dart, TypeScript, Java, C++, and C#. " +
					"SCALA: a `for … yield` comprehension and flatMap/fold are monadic binds as often as iteration, so they raise loop_depth but NOT scaling depth — a finding over one is downgraded rather than claimed; a combinator applied to an Option is discounted the same way. " +
					strings.ReplaceAll(strings.TrimSpace(kindLegend(shown)), "\n", " "),
			}, in.MaxTokens)
		case mcputil.ModeCompact:
			return mcputil.TextResult(mcputil.CapTokens(withLegend(renderPerfCompact(shown)+pageLine(offset, len(shown), len(matched)), shown), in.MaxTokens, false)), nil, nil
		default:
			// The summary renders its own topN list, so it gets the full matched set —
			// its counts and its headline must describe the same thing.
			return mcputil.TextResult(mcputil.CapTokens(withLegend(renderPerfSummary(sum, matched, in), matched), in.MaxTokens, false)), nil, nil
		}
	})
}

// kindQueryLoop is a finding the query-loops explainer made, listed here.
const kindQueryLoop = "query-loop"

// basisTypedReceiver is what a query-loop finding's call rests on: the receiver
// is a model the graph knows, or a record whose type a block binding states.
const basisTypedReceiver = "typed-receiver"

// queryLoopFindings lists the query-loops explainer's findings as findings of
// this tool.
//
// The analyzer stays silent on a symbol that explainer reports, and leaves a lazy
// association read to it altogether, because it answers the question from the
// receiver's type where this analyzer has a name. But the explainer names a fixed
// number of findings per repository and counts the rest in one line, so a query
// per iteration that ranked past that number was named nowhere. This tool has no
// such limit and is the surface that returns every finding, so they are returned
// here: all of them, the listed ones too, so that the answer to "what does this
// function do in its loops" is in one place.
//
// They are added for the tool only. The explainer publishes its own insights, and
// publishing them again under this one would report each loop twice.
func queryLoopFindings(store *facts.Store, routeHandlers map[string]bool) []Finding {
	found := queryloops.All(store)
	if len(found) == 0 {
		return nil
	}
	type key struct{ repo, name string }
	lines := make(map[key]int)
	for _, f := range store.ByKind(facts.KindSymbol) {
		lines[key{f.Repo, f.Name}] = f.Line
	}
	out := make([]Finding, 0, len(found))
	for _, q := range found {
		depth := q.Depth
		if depth < 1 {
			depth = 1
		}
		f := Finding{
			Symbol: q.Symbol, File: q.File, Line: lines[key{q.Repo, q.Symbol}], Repo: q.Repo,
			Kind: kindQueryLoop, Confidence: q.Confidence,
			Evidence: []string{"query_in_loop=" + q.Call},
			Calls:    []CallEvidence{{Callee: q.Call, Basis: basisTypedReceiver, Depth: depth}},
		}
		// One query per element is a claim this can make. A product of the
		// enclosing loops is not, so deeper nesting is stated as a depth.
		if depth == 1 {
			f.BigO = bigOForDepth(1)
		} else {
			f.Depth = depth
		}
		switch {
		case q.Write:
			f.Why = fmt.Sprintf("Writes with %s once per iteration, on a %s. A write per element cannot be eager-loaded away; replace it with one bulk update or insert.", q.Call, q.Element)
		case q.Element != "":
			f.Why = fmt.Sprintf("Reads %s once per iteration: the loop's element is a %s, and each read loads its %s. Eager-load the association on the collection.", q.Call, q.Element, q.Target)
		default:
			f.Why = fmt.Sprintf("Calls %s once per iteration. The receiver is a model and the method reaches the database; read the set once outside the loop.", q.Call)
		}
		if !q.Listed {
			f.Why += " Counted in the query-loops rollup, not listed there individually."
		}
		// Graded as the explainer grades it: a typed receiver is evidence, a
		// one-shot surface is not a production risk, and an element typed by a
		// parameter's name alone is a candidate.
		switch {
		case q.OneOff:
			f.Severity = "low"
		case q.Confidence >= 0.8:
			f.Severity = "high"
		default:
			f.Severity = "medium"
		}
		f.RiskScore = riskScore(f, routeHandlers)
		out = append(out, f)
	}
	return out
}

// kindMeanings define the finding kinds, for the legend a response carries.
var kindMeanings = []struct{ kind, meaning string }{
	{"nested-loop", "loops nested inside one function; the depth is a count, not a complexity"},
	{"compounded", "nesting that continues across the call graph, as a loop calls a function that loops; a count, not a complexity"},
	{"call-in-loop", "an I/O, database or network call inside a loop, a likely N+1"},
	{"recursion", "a function that calls itself"},
	{"query-loop", "a database query issued once per iteration, read from the receiver's type by the query-loops explainer"},
}

// kindNames lists the finding kinds, in the legend's order.
func kindNames() []string {
	names := make([]string, 0, len(kindMeanings))
	for _, k := range kindMeanings {
		names = append(names, k.kind)
	}
	return names
}

// kindSet reads the kind argument, a comma-separated list, or nil for none.
func kindSet(arg string) map[string]bool {
	var set map[string]bool
	for _, k := range strings.Split(arg, ",") {
		if k = strings.TrimSpace(k); k != "" {
			if set == nil {
				set = map[string]bool{}
			}
			set[k] = true
		}
	}
	return set
}

// unknownKinds returns the names in the kind argument that are no finding kind.
// A misspelt kind would otherwise match nothing and read as a clean repository.
func unknownKinds(arg string) []string {
	set := kindSet(arg)
	for _, k := range kindNames() {
		delete(set, k)
	}
	bad := make([]string, 0, len(set))
	for k := range set {
		bad = append(bad, k)
	}
	sort.Strings(bad)
	return bad
}

// page cuts one page out of the ranked, filtered findings. It returns the offset
// it used, clamped to the findings there are, and the offset of the next page, or
// 0 when this one is the last.
func page(matched []Finding, offset, limit int) (shown []Finding, at, next int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(matched) {
		offset = len(matched)
	}
	shown = matched[offset:]
	if len(shown) > limit {
		shown = shown[:limit]
	}
	if end := offset + len(shown); end < len(matched) {
		next = end
	}
	return shown, offset, next
}

// pageLine says which part of the matched findings a table shows, when it is not
// all of them, and where the next page starts.
func pageLine(offset, shown, total int) string {
	if shown == total {
		return ""
	}
	if shown == 0 {
		return fmt.Sprintf("\nNo findings at offset %d: %d matched.\n", offset, total)
	}
	line := fmt.Sprintf("\nShowing %d to %d of %d.", offset+1, offset+shown, total)
	if offset+shown < total {
		line += fmt.Sprintf(" Next page: offset=%d.", offset+shown)
	}
	return line + "\n"
}

// kindLegend defines the kinds that occur in findings, one line each, or "" for none.
func kindLegend(findings []Finding) string {
	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.Kind] = true
	}
	var b strings.Builder
	for _, k := range kindMeanings {
		if seen[k.kind] {
			fmt.Fprintf(&b, "  %s: %s\n", k.kind, k.meaning)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "Kinds:\n" + b.String()
}

// withLegend appends the kind legend to a text rendering.
func withLegend(text string, findings []Finding) string {
	legend := kindLegend(findings)
	if legend == "" {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n\n" + legend
}

// summarize computes the aggregate counts over the FILTERED findings. `population`
// is the unfiltered set, and is used only for its length — so a zero result can say
// what it was drawn from, and so no count can accidentally be taken over it.
//
// The argument order is the contract: population first, findings second. Every count
// in the returned summary comes from `findings`.
func summarize(population, findings []Finding) summary {
	sum := summary{
		TotalFindings:     len(findings),
		RepoWideFindings:  len(population),
		FunctionsAnalyzed: countFunctions(findings),
		BySeverity:        map[string]int{},
		ByBigO:            map[string]int{},
		ByDepth:           map[string]int{},
	}
	for _, f := range findings {
		sum.BySeverity[f.Severity]++
		if f.BigO != "" {
			sum.ByBigO[f.BigO]++
		} else {
			sum.ByDepth[f.Label()]++
		}
	}
	return sum
}

// countFunctions counts the distinct functions the reported findings sit on. It used
// to be len(funcs) — every function in the repo — which stayed constant no matter how
// the caller filtered, so a filtered query reported the repo-wide denominator.
func countFunctions(findings []Finding) int {
	seen := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		seen[f.Symbol] = struct{}{}
	}
	return len(seen)
}

// describeFilter renders the active filters for the headline, so a zero result is
// distinguishable from a filter that silently matched nothing.
func describeFilter(a args) string {
	return mcputil.DescribeFilters(
		"package", a.Package,
		"repo", a.Repo,
		"symbol", a.Symbol,
		"min_severity", a.MinSeverity,
		"kind", a.Kind,
	)
}

// renderPerfSummary is the default (token-light) view: the aggregate counts plus
// the top findings — all from the filtered/limited set, and all agreeing with each
// other. Previously the headline came from a summary built before the filter ran, so
// `package="androidTest"` printed the repo-wide count above an empty table.
func renderPerfSummary(sum summary, findings []Finding, in args) string {
	var b strings.Builder
	scope := mcputil.Scope(sum.RepoWideFindings, findings, describeFilter(in))
	fmt.Fprintf(&b, "Performance: %s\n", scope.Headline("findings"))
	fmt.Fprintf(&b, "Across %d function(s) (high %d / medium %d / low %d).\n",
		sum.FunctionsAnalyzed,
		sum.BySeverity["high"], sum.BySeverity["medium"], sum.BySeverity["low"])
	const topN = 15
	if len(findings) > 0 {
		b.WriteString("\nTop findings:\n")
		for i, f := range findings {
			if i >= topN {
				fmt.Fprintf(&b, "… (+%d more — use output_mode=full or tighten filters)\n", len(findings)-topN)
				break
			}
			loc := f.Symbol
			if f.File != "" {
				loc = fmt.Sprintf("%s (%s:%d)", f.Symbol, f.File, f.Line)
			}
			fmt.Fprintf(&b, "  [%s] %s %s — %s\n", f.Severity, f.Label(), loc, f.Kind)
		}
	}
	return b.String()
}

// renderPerfCompact renders the findings as a markdown table.
func renderPerfCompact(findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Performance findings — %d:\n\n", len(findings))
	b.WriteString("| Severity | Risk | Estimate | Kind | Symbol | Location |\n")
	b.WriteString("|----------|------|----------|------|--------|----------|\n")
	for _, f := range findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "| %s | %.2f | %s | %s | %s | %s |\n", f.Severity, f.RiskScore, f.Label(), f.Kind, f.Symbol, loc)
	}
	return b.String()
}

func filterFindings(in []Finding, a args) []Finding {
	minRank := severityRank(a.MinSeverity)
	if a.MinSeverity == "" {
		minRank = 0
	}
	kinds := kindSet(a.Kind)
	if a.Package == "" && a.Repo == "" && a.Symbol == "" && minRank == 0 && kinds == nil {
		return in
	}
	out := in[:0:0]
	for _, f := range in {
		if minRank > 0 && severityRank(f.Severity) < minRank {
			continue
		}
		if kinds != nil && !kinds[f.Kind] {
			continue
		}
		// package= matches the declaring package OR the symbol name — the schema
		// documents it as "package or name", so the name match is contract, not
		// accident. It used to be the name match ALONE, because Finding carried no
		// Package field. That works only where the symbol name embeds the path
		// (Kotlin/Swift/Go) and returns a silent, permanent zero on Ruby, whose names
		// do not: on a Rails repo with 64 flaggable symbols under app/models/,
		// package="app/models" matched none of them.
		if a.Package != "" &&
			!strings.Contains(f.Package, a.Package) && !strings.Contains(f.Symbol, a.Package) {
			continue
		}
		if a.Repo != "" && f.Repo != a.Repo {
			continue
		}
		if a.Symbol != "" && !strings.Contains(f.Symbol, a.Symbol) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// bigORank maps a Big-O label to an orderable complexity weight, so distribution
// lines sort from cheapest to worst. The two recursive buckets ("O(?) — …") rank
// above any polynomial so they sort last.
func bigORank(bigO string) int {
	switch bigO {
	case "O(1)":
		return 0
	case "O(n)":
		return 1
	case "O(n²)":
		return 2
	case "O(n³)":
		return 3
	case "O(n³+)":
		return 4 // capped deep estimate — sorts just above O(n³)
	}
	if strings.HasPrefix(bigO, "O(n^") {
		var k int
		if _, err := fmt.Sscanf(bigO, "O(n^%d)", &k); err == nil {
			return k
		}
	}
	return 1000 // recursive / unknown — sort last
}

// Summary is the performance block `enola --explain` prints: how much was looked
// at, what was found, and the worst of it.
type Summary struct {
	FunctionsAnalyzed int
	Total             int
	High, Medium, Low int
	// ByKind and ByComplexity are ordered for printing, not maps: kinds in a fixed
	// order, complexity cheapest to worst with the recursive bucket last, and empty
	// buckets already dropped.
	ByKind       []Bucket
	ByComplexity []Bucket
	// ByDepth is the structural findings (nested-loop, compounded) by nesting
	// depth, shallowest first. They state no complexity, so they are not in
	// ByComplexity.
	ByDepth []Bucket
	// Top is the worst findings, already ranked by analyze().
	Top []Finding
}

// Bucket is one labelled count in a distribution.
type Bucket struct {
	Label string
	Count int
}

// summaryTopN is how many findings the report lists individually. The rest are a
// number; analyze_performance is where the full set lives.
const summaryTopN = 8

// Summarize analyzes the whole store with the same core as the
// analyze_performance tool, so the report and the tool cannot disagree.
func Summarize(store *facts.Store) Summary {
	funcs, storage, routeHandlers, assoc := collect(store)
	findings := analyze(funcs, storage, routeHandlers, assoc)

	s := Summary{FunctionsAnalyzed: len(funcs), Total: len(findings)}
	byKind := map[string]int{}
	byBigO := map[string]int{}
	byDepth := map[string]int{}
	var recursive int
	for _, f := range findings {
		switch f.Severity {
		case "high":
			s.High++
		case "medium":
			s.Medium++
		default:
			s.Low++
		}
		byKind[f.Kind]++
		// The two "O(?)" buckets both mean "recursion, so no exponent"; collapsing
		// them keeps the distribution readable.
		switch {
		case f.BigO == "":
			byDepth[f.Label()]++
		case bigORank(f.BigO) >= 1000:
			recursive++
		default:
			byBigO[f.BigO]++
		}
	}

	for _, k := range []string{"nested-loop", "compounded", "call-in-loop", "recursion"} {
		if byKind[k] > 0 {
			s.ByKind = append(s.ByKind, Bucket{Label: k, Count: byKind[k]})
		}
	}

	bigos := make([]string, 0, len(byBigO))
	for o := range byBigO {
		bigos = append(bigos, o)
	}
	sort.Slice(bigos, func(i, j int) bool { return bigORank(bigos[i]) < bigORank(bigos[j]) })
	for _, o := range bigos {
		s.ByComplexity = append(s.ByComplexity, Bucket{Label: o, Count: byBigO[o]})
	}
	if recursive > 0 {
		s.ByComplexity = append(s.ByComplexity, Bucket{Label: "recursive", Count: recursive})
	}
	// "depth 2" < "depth 3" < … < "depth 4+" < "depth 5": ordered by the number,
	// with the open-ended compounded bucket after its own exact value.
	depths := make([]string, 0, len(byDepth))
	for d := range byDepth {
		depths = append(depths, d)
	}
	sort.Slice(depths, func(i, j int) bool {
		var a, b int
		_, _ = fmt.Sscanf(depths[i], "depth %d", &a)
		_, _ = fmt.Sscanf(depths[j], "depth %d", &b)
		if a != b {
			return a < b
		}
		return depths[i] < depths[j]
	})
	for _, d := range depths {
		s.ByDepth = append(s.ByDepth, Bucket{Label: d, Count: byDepth[d]})
	}

	if len(findings) > summaryTopN {
		s.Top = findings[:summaryTopN]
	} else {
		s.Top = findings
	}
	return s
}
