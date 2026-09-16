package facts

import (
	"path/filepath"
	"sort"
	"strings"
)

// Fact represents a language-agnostic architectural fact extracted from source code.
type Fact struct {
	Kind      string         `json:"kind"`                 // e.g. "module", "symbol", "route", "storage", "dependency"
	Name      string         `json:"name"`                 // Canonical name
	File      string         `json:"file,omitempty"`       // Source file (relative to repo root, or repo-prefixed in multi-repo mode)
	Line      int            `json:"line,omitempty"`       // 1-based start line in file
	EndLine   int            `json:"end_line,omitempty"`   // 1-based end line, when the extractor measured a span
	Column    int            `json:"column,omitempty"`     // 1-based start column, when measured
	EndColumn int            `json:"end_column,omitempty"` // 1-based column one past the span's end, when measured
	Repo      string         `json:"repo,omitempty"`       // Repository label; set on every fact, single-repo snapshots included (see RepoLabel)
	Props     map[string]any `json:"props,omitempty"`      // Kind-specific properties
	Relations []Relation     `json:"relations,omitempty"`  // Edges to other facts
}

// Relation represents a directed edge between two facts.
type Relation struct {
	Kind   string `json:"kind"`   // e.g. "declares", "imports", "calls", "implements", "depends_on"
	Target string `json:"target"` // Target fact name
}

// Fact kind constants.
const (
	KindModule     = "module"
	KindSymbol     = "symbol"
	KindRoute      = "route"
	KindStorage    = "storage"
	KindDependency = "dependency"
	KindService    = "service" // A whole repository, represented as a node in the cross-repo "graph of graphs".
	// KindIntent is a DECLARED fact: one entry of a repo's architectural intent
	// (a consumed seam, a served surface, a layer, a service identity), compiled
	// into the store from enola-intent.yaml or a cluster config's intent block.
	// Every other kind is measured from source; this one states what the source
	// is SUPPOSED to do, and the intent explainer verdicts the two against each
	// other. Carries intent_kind plus provenance (source, overridden).
	KindIntent = "intent"

	// KindExtraction is one extractor's account of what it saw and could not
	// resolve, for one repository. It carries the same `edge_coverage` prop the
	// cross-repo layer puts on a service, because coverage in this codebase
	// travels as facts — the linker's sink is an accumulator convenience, never
	// the channel. An extractor that reports nothing emits no fact at all, so a
	// consumer can tell "reported zero" from "never reported".
	KindExtraction = "extraction"

	// KindAssociation is one declared relationship between two models — a Rails
	// belongs_to/has_one/has_many/has_and_belongs_to_many, or another framework's
	// equivalent. It names its target rather than referencing the target's fact,
	// because an association whose other end was never extracted is exactly the
	// cross-boundary relationship most worth knowing about, and an edge that can
	// only exist when both ends were read would drop it silently.
	//
	// An association whose target cannot be named emits NO fact and increments
	// the extractor's coverage counter instead. Every consumer of this graph
	// treats an edge as something you can follow, so an edge with no target is
	// either ignored or miscounted; "I saw 45 of these and could not name their
	// targets" belongs in a report, not in an untraversable edge.
	KindAssociation = "association"
	// KindTestRef is a reference-only fact emitted from a test/spec file. It carries
	// only reference relations — RelCalls, and RelInstantiates where the reference is
	// a constructor call — naming the production symbols the test exercises
	// (Name/File are the test file path). Test files are excluded from normal
	// indexing, so their symbols never become facts; this kind lets the dead-code
	// detector still see that a production symbol is referenced by a test, without
	// any other explainer (which key off symbol/module/route facts) being affected.
	KindTestRef = "test_ref"
	// KindFileRef is a reference-only fact holding call edges made in file-scope
	// (top-level) code — fixtures, initializers, and plugin registration blocks —
	// that have no enclosing symbol to attach to. Like KindTestRef it carries only
	// reference relations — RelCalls and RelInstantiates — (Name/File are the source
	// file path) and is consumed only by the dead-code detector, so top-level
	// references mark a production symbol used without perturbing the coupling graph
	// or any other explainer.
	KindFileRef = "file_ref"

	// KindLint is a finding an external linter reported, brought in through the
	// provider seam: a rule id, a severity, a file and a line, and nothing the
	// graph measured itself. Reference-only like KindTestRef: it carries no
	// edges, nothing counts it as coupling, and a constraints component selects
	// it only by naming the kind, so a declared rule can wrap a linter's rule
	// with because-prose and a ratchet and the check's one comment covers
	// both engines. Props: lint_engine, lint_rule, lint_severity, line, message.
	KindLint = "lint"
)

// pathShapedName names the kinds whose Name IS a repo-relative path, and which the
// store therefore normalises to forward slashes on the way in (see Store.Add).
//
// The list is short on purpose. Most Names are not paths, and one language makes the
// distinction load-bearing rather than pedantic: PHP separates namespace segments with
// a BACKSLASH, so `App\Http\Controllers\UserController` is a correct symbol name and
// `Illuminate\Support\Facades\Route` a correct dependency target. Normalising Name
// wholesale — or normalising Relation.Target at all — would rewrite those into paths
// that name nothing, breaking PHP resolution to fix Windows. Only kinds whose name
// cannot be anything but a path belong here.
var pathShapedName = map[string]bool{
	KindModule:  true,
	KindFileRef: true,
	KindTestRef: true,
}

// Cross-repo dependency-fact "type" prop values. Both the linker that writes these
// facts and every reader (explainers, renderers, the MCP server) key off them, so they
// live here rather than being duplicated as literals on each side.
const (
	// TypeCrossRepo marks a real, DIRECTIONAL cross-repo edge: one repo imports or
	// calls the other. Only these attach a depends_on relation to the consumer's
	// service node, so only these are traversable.
	TypeCrossRepo = "cross_repo"
	// TypeCrossRepoSharedCode marks a SYMMETRIC shared-code coupling: two repos declare
	// many of the same distinctive type names but neither imports or calls the other.
	// It is queryable evidence with no relation attached, so it never appears in
	// traverse/find_path/impact_analysis — shared code is not a dependency, and unlike
	// a dependency it does not compose across hops.
	TypeCrossRepoSharedCode = "cross_repo_shared_code"
)

// Relation kind constants.
const (
	// RelDeclares is emitted on the DECLARED fact and points at the module that
	// contains it (symbol -> module, route -> module): read it as "declared in".
	// Nothing emits the module -> symbol direction, so a consumer building a
	// containment tree walks this edge child-to-parent.
	RelDeclares      = "declares"
	RelImports       = "imports"
	RelCalls         = "calls"
	RelImplements    = "implements"
	RelDependsOn     = "depends_on"
	RelInstantiates  = "instantiates"   // Source constructs an instance of target via a constructor call.
	RelInjects       = "injects"        // Source declares target as a DI-injected constructor parameter.
	RelHasMethod     = "has_method"     // Owner type (struct/interface/class) declares target as a method. Synthesized in NewGraph.
	RelHandledBy     = "handled_by"     // A route/endpoint is served by target (e.g. a gRPC RPC route → its Go handler method). Added post-extraction.
	RelImplementedBy = "implemented_by" // A declared contract operation is implemented by a code symbol. Added post-extraction.
	RelNames         = "names"          // Source names target by symbol literal without calling it: a method name passed as data (`perform_async(id, :on_done)`) for something else to dispatch. A reference, not a call; read by dead-code questions, ignored by call metrics.
)

// PropTargetFile is the repo-relative file a producer reports a relation's
// target to be defined in, carried on the dependency fact beside the relation
// so a consumer resolving the target by name can tell which of a reopened
// name's files the edge lands on. Absent, the target resolves by name alone.
const PropTargetFile = "target_file"

// StorageKindTopic is the storage_kind prop value for a KindStorage fact that
// represents a messaging topic reference (e.g. a Kafka topic a service produces to
// or consumes from), as opposed to a database table or object store. The cross-repo
// async linker keys producer/consumer binding on it. The topic name is the fact's
// Name; the messaging_role prop (MessagingRole*) records the side when known, and is
// otherwise inferred by the linker from topic-name ownership.
const StorageKindTopic = "topic"

// Messaging role property values (the messaging_role prop) for a topic storage fact:
// which side of a topic a reference represents. Left unset when only a bare topic
// reference is seen, in which case the linker infers direction from the topic name's
// owning-service prefix.
const (
	MessagingRoleProducer = "producer"
	MessagingRoleConsumer = "consumer"
)

// MethodAny is the HTTP-method value for a server route that handles every verb
// rather than one specific method — a raw servlet (doGet/doPost/…) or a mapping
// declared without a verb. The cross-repo HTTP linker treats it as a wildcard that
// a client call of any concrete method matches. Extractors that cannot attribute a
// route to a single verb emit this instead of guessing one (which would create a
// spurious method mismatch) or omitting the method (which drops the route from
// matching entirely).
const MethodAny = "*"

// Symbol kind property values.
const (
	SymbolFunc      = "function"
	SymbolMethod    = "method"
	SymbolGetter    = "getter"
	SymbolStruct    = "struct"
	SymbolInterface = "interface"
	SymbolType      = "type"
	SymbolClass     = "class"
	SymbolVariable  = "variable"
	SymbolConstant  = "constant"
	SymbolEnum      = "enum"
)

// Coupling-kind property values annotate a synthetic module-coupling dependency
// fact (currently emitted by the Ruby extractor) with the reference that produced
// the edge, so consumers can treat edge classes differently — e.g. the cycles
// explainer excludes CouplingAssociation, because ActiveRecord has_many/belongs_to
// pairs are bidirectional by nature and would manufacture false cycles. Set on the
// Props["coupling_kind"] of a synthetic edge; an absent value means unclassified
// (treated as a normal edge). When one edge arises from several references, the
// hardest kind wins (a real reference is never downgraded to an association).
const (
	PropCouplingKind = "coupling_kind"
	// PropDependencyPhase distinguishes imports that survive compilation from
	// type-only edges erased by TypeScript. Consumers such as cycle analysis can
	// then describe runtime risk separately from source-level coupling.
	PropDependencyPhase     = "dependency_phase"
	DependencyPhaseRuntime  = "runtime"
	DependencyPhaseTypeOnly = "type_only"

	CouplingReference   = "reference"   // constant-receiver method call
	CouplingInheritance = "inheritance" // superclass
	CouplingMixin       = "mixin"       // include/extend/prepend
	CouplingAssociation = "association" // ActiveRecord has_many/belongs_to/...
	CouplingRequire     = "require"     // require / require_relative
	CouplingPackwerk    = "packwerk"    // explicit package.yml dependency
	// CouplingSymbolRollup is a module edge derived from resolved symbol edges
	// rather than read from an import statement, for the languages that have
	// none. It carries the count of symbol edges behind it.
	CouplingSymbolRollup = "symbol-rollup"
)

// Module role property values classify a module fact as production code vs.
// non-production (test bundles, build tooling) so downstream analyses (package
// metrics, coverage, insights) can measure the production architecture. Extractors
// set the PropModuleRole prop; consumers treat an absent value as included.
const (
	PropModuleRole       = "module_role"
	ModuleRoleProduction = "production"
	ModuleRoleTest       = "test"
	ModuleRoleTooling    = "tooling"
	ModuleRoleUnknown    = "unknown"
)

// ModuleRoleForPath classifies a module by its directory path segments, for use
// when the extractor has no more authoritative signal (e.g. a leaf-directory
// fallback module covered by no declared target/package): a test-directory segment
// → test, a build-tooling segment → tooling, otherwise unknown. Segment names cover
// the common conventions across languages (Swift Tests/, Ruby spec//bin/, fastlane).
func ModuleRoleForPath(dir string) string {
	for _, seg := range strings.Split(filepath.ToSlash(dir), "/") {
		switch seg {
		case "Tests", "Test", "tests", "test", "spec", "specs",
			"androidTest", "androidUnitTest", "testFixtures":
			return ModuleRoleTest
		case "Scripts", "scripts", "fastlane", "ci_scripts", "bin":
			return ModuleRoleTooling
		}
		// Sub-token match for compound module names (Gradle modules whose purpose
		// is test automation but that compile as a `src/main` source set, e.g.
		// "release-tests", "ui-test-utils", "test-lab"). Split the segment on
		// '-'/'_' and match a token EXACTLY equal to a test word, so genuine
		// single-token names ("latest", "contest", the "abtest" feature) never
		// misfire — they have no separator and stay a single token.
		for _, tok := range strings.FieldsFunc(seg, func(r rune) bool { return r == '-' || r == '_' }) {
			switch tok {
			case "test", "tests", "spec", "specs":
				return ModuleRoleTest
			}
		}
	}
	return ModuleRoleUnknown
}

// Insight represents an architectural insight produced by an explainer.
type Insight struct {
	Title       string     `json:"title"`
	Source      string     `json:"source,omitempty"` // Name of the explainer that produced it (e.g. "unused-routes"). Set centrally by the engine.
	Description string     `json:"description"`
	Confidence  float64    `json:"confidence"` // 0.0 - 1.0
	Evidence    []Evidence `json:"evidence"`
	Actions     []string   `json:"suggested_actions,omitempty"`

	// Informational marks a finding that DESCRIBES the graph rather than complaining
	// about it — "Architecture pattern: declared (enola)", "Intent override: the cluster
	// config replaced this repo's declaration". They are worth reporting and must never
	// be gradeable, and the distinction is not expressible in confidence: both are exact,
	// which is precisely the problem. A repository that declares a layer order for the
	// first time emits a new `layers` finding at 1.00 describing the declaration, and
	// under --fail-on=layers that would fail the very pull request that adopted the
	// policy. See check.Policy.fails.
	Informational bool `json:"informational,omitempty"`

	// Metrics carries the numbers a finding computed, as data rather than as prose.
	//
	// Everything an explainer measures currently reaches a reader only through the
	// title and description, and every tool that wants a number back parses it out
	// again: pkg/explain regexes integers from titles (firstParenInt), and the
	// architecture benchmark regexed "N of M modules classified" out of a sentence
	// until adding the cohort's language to that sentence broke it. A sentence is the
	// right shape for a reader and the wrong one for a caller.
	//
	// It MIRRORS the description rather than replacing it. internal/diff compares
	// Title, Confidence, Description and Evidence by explicit enumeration, so a
	// number that moved out of the description and into here would stop being
	// something the gate can see change. The prose keeps the numbers; this is the
	// machine-readable copy of them, and a test asserts the two agree.
	//
	// Shaped like Fact.Props, and inheriting its one hazard: a map survives the JSON
	// round-trip of a restored snapshot with every number as a float64. Read it
	// through MetricInt/MetricFloat/MetricStrings rather than type-asserting, which
	// is the mistake declared.go already had to correct in place.
	Metrics map[string]any `json:"metrics,omitempty"`
}

// Label is the repo label the facts in this snapshot carry, and the only correct way to
// ask that question of a snapshot. Recomputing it from RepoPath is what let the engine
// and its readers disagree: since the label came to depend on the git remote, a caller
// deriving one from the directory would look up facts under a key nothing is stored at
// and quietly count zero. Snapshots written before the label was recorded fall back to
// the directory name, which is what tagged their facts.
func (m SnapshotMeta) Label() string {
	if m.RepoLabel != "" {
		return m.RepoLabel
	}
	return RepoDirName(m.RepoPath)
}

// Evidence links an insight back to concrete facts/files/symbols.
type Evidence struct {
	File   string `json:"file,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Fact   string `json:"fact,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Line and the span fields carry the position of the fact this evidence
	// cites, when its extractor measured one. They are never derived from a
	// name: a reader that prints a frame must show the line the extractor saw,
	// or none.
	Line      int `json:"line,omitempty"`
	EndLine   int `json:"end_line,omitempty"`
	Column    int `json:"column,omitempty"`
	EndColumn int `json:"end_column,omitempty"`
}

// EvidenceHere cites this fact at the position its extractor measured. Every
// explainer that points at a fact should build evidence this way, so a span
// reaches the reader without each explainer copying four fields.
func (f Fact) EvidenceHere(detail string) Evidence {
	return Evidence{
		File:      f.File,
		Symbol:    f.Name,
		Detail:    detail,
		Line:      f.Line,
		EndLine:   f.EndLine,
		Column:    f.Column,
		EndColumn: f.EndColumn,
	}
}

// Artifact represents a generated output file.
type Artifact struct {
	Name    string `json:"name"` // e.g. "llm_context.md"
	Content []byte `json:"-"`    // Raw content
	Type    string `json:"type"` // MIME type hint
}

// Snapshot holds the complete result of an analysis run.
type Snapshot struct {
	Meta      SnapshotMeta `json:"meta"`
	Facts     []Fact       `json:"facts"`
	Insights  []Insight    `json:"insights"`
	Artifacts []Artifact   `json:"artifacts"`
}

// SnapshotMeta contains metadata about a snapshot generation run. Beyond the
// core provenance fields (repo, time, plugin sets, per-file hashes) it carries
// the "snapshot receipt" fields — a compact, machine-readable record of what the
// deterministic graph was generated over, plus extraction-quality metrics that
// let a consumer (a human, a diff, or an agent improving enola itself) judge how
// complete the extraction was before trusting it.
type SnapshotMeta struct {
	RepoPath string `json:"repo_path"`
	// RepoLabel is the label the facts in this snapshot are tagged with, and therefore
	// part of every fact's diff key. Two snapshots of the same repository labelled
	// differently share no keys, so this is recorded to be COMPARED — see
	// diff.compareMeta. Empty on snapshots written before it existed, where the reader
	// falls back to the checkout directory name, which is what those builds used.
	RepoLabel    string     `json:"repo_label,omitempty"`
	GeneratedAt  string     `json:"generated_at"`
	Duration     string     `json:"duration"`
	Extractors   []string   `json:"extractors"`
	Explainers   []string   `json:"explainers"`
	Renderers    []string   `json:"renderers"`
	FileHashes   []FileHash `json:"file_hashes,omitempty"`
	FactCount    int        `json:"fact_count"`
	InsightCount int        `json:"insight_count"`

	// Providers is the external-provider census: one record per configured
	// provider, whether it contributed facts or was skipped. It feeds the
	// receipt and the provider-set comparability check.
	Providers []ProviderRecord `json:"providers,omitempty"`

	// Receipt / provenance fields. Hash values carry a "sha256:" prefix.
	EnolaVersion string   `json:"enola_version,omitempty"` // build version that produced this snapshot
	SnapshotID   string   `json:"snapshot_id,omitempty"`   // content fingerprint (see engine.newSnapshotIDHasher); stable across reruns on the same inputs
	Git          *GitInfo `json:"git,omitempty"`           // repo VCS state, nil when not a git repo
	ConfigHash   string   `json:"config_hash,omitempty"`   // hash of the effective config (extractors, explainers, renderers, globs, output) — a superset of IgnoreGlobHash

	// ExtractorVersion identifies the BEHAVIOUR that produced this graph:
	// internal/engine.cacheVersion, the constant bumped whenever an extractor starts
	// reading something differently — or whenever the same facts start producing a
	// different graph, which moves findings on an unedited tree just as surely (and
	// guarded by internal/cachecov, which refuses a bump without a covering test).
	//
	// It is provenance EnolaVersion cannot supply. A released binary carries a version
	// that moves with every release, so an upgrade is visible; a local build carries the
	// constant "dev" forever, so an extractor change is invisible in it. Every field
	// beside this one then reads identical across a change that rewrote the graph — which
	// is exactly how a fix that removed 21 fabricated facts came to be recorded, in this
	// repository's own architecture history, as somebody deleting 21 things from the
	// codebase.
	//
	// The alternative — identifying the binary itself by size and mtime, as the cache does
	// in buildIdentity — catches undeclared changes too, and was rejected: it would mark
	// every recompile as a new extraction regime, which for anyone developing enola is
	// several times an hour, and a marker that fires constantly stops carrying meaning.
	ExtractorVersion string `json:"extractor_version,omitempty"`

	// Extraction-quality fields (the loop signal).
	FilesSeen      int      `json:"files_seen,omitempty"`       // source files the walker enumerated (excludes ignored)
	FilesParsed    int      `json:"files_parsed,omitempty"`     // distinct files that produced at least one fact
	SourceBytes    int64    `json:"source_bytes,omitempty"`     // on-disk bytes of the FilesParsed set — the corpus this graph replaces reading (see pkg/status value model)
	FilesSkipped   int      `json:"files_skipped,omitempty"`    // ignored FILES the walker visited; a pruned directory counts once in DirsSkipped, not once per file
	DirsSkipped    int      `json:"dirs_skipped,omitempty"`     // ignored DIRECTORIES pruned whole; their contents are never visited, so they are counted nowhere else
	SkippedSample  []string `json:"skipped_sample,omitempty"`   // a capped sample of both, each naming the glob that matched it
	IgnoreGlobHash string   `json:"ignore_glob_hash,omitempty"` // hash of the sorted ignore+test globs
	// ShadowedExtractors names extractors that detected this repository but were
	// excluded by an explicit `extractors:` list — languages present in the source
	// and absent from the graph. Recorded because the alternative evidence is a
	// negative (an extractor that never appears in the log), which is unreadable
	// after the fact: a receipt showing thin extraction should say whose facts are
	// missing by configuration rather than by absence.
	ShadowedExtractors []string          `json:"shadowed_extractors,omitempty"`
	ParseErrors        int               `json:"parse_errors,omitempty"`       // count of extractor detect/parse failures (non-fatal)
	ParseErrorSample   []ParseError      `json:"parse_error_sample,omitempty"` // a capped sample of those failures
	HeuristicInsights  int               `json:"heuristic_insights,omitempty"` // count of insights with confidence < 1.0 (heuristics, vs. structural facts)
	Coverage           *CoverageSummary  `json:"coverage,omitempty"`           // cross-repo edge-coverage rollup, nil in single-repo mode
	Census             *FileCensus       `json:"census,omitempty"`             // per-file walk accounting: every visited file in exactly one bucket
	OutputHashes       map[string]string `json:"output_hashes,omitempty"`      // artifact name -> "sha256:"-prefixed hash of its written bytes
	// Unseen is what this run could not see, assembled once after providers
	// merged and the explainers ran, from counts the engine already holds. The
	// verdict prints it as one line so "the graph agreed" and "the graph was not
	// asked" never read the same.
	Unseen *UnseenCensus `json:"unseen,omitempty"`
}

// UnseenCensus is the one account of what a run could not see. Every field
// is a count the engine had anyway; assembling them here is what makes them
// printable as one line under a verdict. A zero everywhere is a real answer
// ("could not see: nothing"), which is why the struct is written even then.
type UnseenCensus struct {
	// FilesExcludedByIgnore and DirsExcludedByIgnore are what the ignore
	// globs removed from the walk: files visited and dropped, and directories
	// pruned whole.
	FilesExcludedByIgnore int `json:"files_excluded_by_ignore"`
	DirsExcludedByIgnore  int `json:"dirs_excluded_by_ignore"`
	// ProviderSkips names each configured provider that ran and what it could
	// not resolve (its top skip causes), or that did not run and why.
	ProviderSkips []ProviderSkip `json:"provider_skips,omitempty"`
	// OutsideGraph counts relations whose target is no fact in the graph, per
	// relation kind: a dependency on a gem, an import of a package nobody
	// indexed. Outside, never unresolved: the target exists, it is simply not
	// in this graph.
	OutsideGraph map[string]int `json:"outside_graph,omitempty"`
	// DeadExemptions counts declared exemptions whose witness matched no
	// violation, the constraints explainer's own finding.
	DeadExemptions int `json:"dead_exemptions"`
	// DynamicFeatureClasses counts classes defined in files that carry a
	// dynamic dispatch prefix (`send`, `public_send` and the like). A count,
	// never a verdict: the measurement that shaped this found doubt lowered
	// only real breaches.
	DynamicFeatureClasses int `json:"dynamic_feature_classes"`
}

// ProviderSkip is one provider's line in the unseen census.
type ProviderSkip struct {
	Name string `json:"name"`
	// Reason is why the provider contributed nothing, when it did not run.
	Reason string `json:"reason,omitempty"`
	// Causes are the provider's own top skip causes when it ran.
	Causes []CensusCause `json:"causes,omitempty"`
}

// RelationOverlap is the seam's account of one provider's relations of one
// kind against the extractor's: what it repeated, what it spelled differently,
// what it contradicted, what it stated about symbols the extractor never
// declared, and what only it had. The five sum to the provider's relations of
// that kind after merge.
type RelationOverlap struct {
	AlreadyResolved   int          `json:"already_resolved"`
	Respelled         int          `json:"respelled"`
	Conflict          int          `json:"conflict"`
	NoExtractorSymbol int          `json:"no_extractor_symbol"`
	ProviderOnly      int          `json:"provider_only"`
	Respellings       []TargetPair `json:"respellings,omitempty"`
	Conflicts         []TargetPair `json:"conflicts,omitempty"`
}

// TargetPair is one provider target beside the extractor target it was
// compared with, kept as evidence for a respelling or a conflict.
type TargetPair struct {
	Source    string `json:"source"`
	Provider  string `json:"provider"`
	Extractor string `json:"extractor"`
}

// ProviderRecord is one configured external fact provider's entry in the
// snapshot's provider census: what it is called, the version it reported, and
// how many facts it contributed — or, for a provider that contributed nothing,
// that it was skipped and why. Skips are recorded rather than dropped because
// the alternative evidence is an absence, and "this machine does not have the
// tool installed" must be readable from the receipt, not inferred from missing
// facts.
type ProviderRecord struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	FactCount int    `json:"fact_count"`
	Skipped   bool   `json:"skipped,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// ExcludedByIgnore counts facts dropped because they described a file the
	// repository's ignore globs exclude. A provider walks the tree itself and
	// cannot know the configuration, so the seam enforces it; the count is
	// recorded rather than silent, since a large number means a provider is
	// reading a vendored tree nobody asked it to read.
	ExcludedByIgnore int             `json:"excluded_by_ignore,omitempty"`
	Census           *ProviderCensus `json:"census,omitempty"`
	// Reuse says what the engine's cache did for this provider in this run.
	// Present only when the cache was consulted for a provider that ran, so a
	// skipped provider never reads as a hit and a run without a cache never
	// reads as zero reuse: absent is "not asked", the way the rest of the
	// receipt treats a number nobody measured.
	Reuse *ProviderReuse `json:"reuse,omitempty"`

	// Agreed counts this provider's call relations another provider emitted
	// identically at the same file, line and callee; the seam keeps one of
	// each pair. Differing counts the call sites where two providers resolved
	// different receivers, both kept, by cause in Differences. OneSided counts
	// the call relations no other provider read at that site.
	Agreed      int                  `json:"agreed,omitempty"`
	Differing   int                  `json:"differing,omitempty"`
	OneSided    int                  `json:"one_sided,omitempty"`
	Differences []ProviderDifference `json:"differences,omitempty"`

	// Overlap is the seam's join of this provider's relations against the
	// extractor's, per relation kind, computed after the merge and never
	// acted on: repeated, respelled, contradicting, about undeclared symbols,
	// or only the provider's.
	Overlap map[string]*RelationOverlap `json:"overlap,omitempty"`
	// Commit and Dirty say which tree the provider's facts describe. A provider
	// that writes them in its own output is believed; otherwise they are read
	// from the repository's git state at merge time, and CommitSource says
	// which ("provider" or "git at merge").
	Commit       string `json:"commit,omitempty"`
	Dirty        bool   `json:"dirty,omitempty"`
	CommitSource string `json:"commit_source,omitempty"`
}

// ProviderReuse is the cache's account of one provider run. For a per-file
// provider, Reused and Computed count facts and FilesReused and FilesComputed
// count the files behind them; OutsideScope counts facts the provider emitted
// about files it was not handed, dropped. For a whole-index provider, Cache
// reads hit or miss and Miss names what the key did not match: files, lockfile,
// version, or cold when no index had been recorded.
type ProviderReuse struct {
	Reused        int    `json:"reused"`
	Computed      int    `json:"computed"`
	FilesReused   int    `json:"files_reused,omitempty"`
	FilesComputed int    `json:"files_computed,omitempty"`
	OutsideScope  int    `json:"outside_scope,omitempty"`
	Cache         string `json:"cache,omitempty"`
	Miss          string `json:"miss,omitempty"`
}

// ProviderDifference is one shape of disagreement between producers at the
// same call site, with the first sites as evidence naming both spellings.
type ProviderDifference struct {
	Cause    string   `json:"cause"`
	Count    int      `json:"count"`
	Examples []string `json:"examples,omitempty"`
}

type ProviderCensus struct {
	FilesSeen          int           `json:"files_seen"`
	DeclarationsParsed int           `json:"declarations_parsed"`
	ConstructsSkipped  int           `json:"constructs_skipped"`
	SkipCauses         []CensusCause `json:"skip_causes,omitempty"`
}

// RanProviders returns the names of the providers that actually contributed to
// a census (not skipped), sorted. This is the set comparability compares: a
// provider that ran on one side only makes all of its facts appear added or
// removed, exactly as a differing extractor set does.
func RanProviders(records []ProviderRecord) []string {
	var names []string
	for _, r := range records {
		if !r.Skipped {
			names = append(names, r.Name)
		}
	}
	sort.Strings(names)
	return names
}

// FileHash tracks a file's content hash for incremental updates.
type FileHash struct {
	Path    string `json:"path"`
	Hash    string `json:"hash"`
	ModTime string `json:"mod_time"`
}

// GitInfo records the VCS state of the repository at snapshot time, so a receipt
// pins the graph to an exact commit and flags an uncommitted (dirty) tree.
type GitInfo struct {
	Ref    string `json:"ref,omitempty"`    // branch or symbolic ref (e.g. "main")
	Commit string `json:"commit,omitempty"` // full commit SHA of HEAD
	Dirty  bool   `json:"dirty"`            // true when the working tree has uncommitted changes

	// Remote is the origin URL, normalized to a comparable identity
	// ("github.com/org/repo" — no scheme, credentials, port or ".git" suffix). Empty
	// when there is no remote, no git, or no origin.
	//
	// It exists so two snapshots of the SAME repository taken at different absolute
	// paths — a CI runner's checkout and a developer's working copy — can be recognized
	// as comparable. Commit identifies a revision; this identifies the repository.
	// A remote is used rather than the root commit because the root commit is
	// unreachable in a shallow clone, which is what CI checkouts default to.
	Remote string `json:"remote,omitempty"`
}

// ParseError records a non-fatal extraction failure — an extractor that could
// not detect or parse its inputs. These are counted rather than swallowed so a
// receipt can surface thin extraction.
type ParseError struct {
	Extractor string `json:"extractor"`
	File      string `json:"file,omitempty"` // set when the failure is attributable to a specific file
	Msg       string `json:"msg"`
}

// FileCensus is the per-file accounting of one repository walk: every file the
// walker visited lands in exactly one bucket, so parsed + excluded + skipped
// always sums back to walked and no file can quietly fall out of the account.
// Files inside pruned directories are visited by nothing and are therefore in
// no bucket, matching how FilesSkipped/DirsSkipped already split that cost.
//
// The buckets state different claims. ExcludedByIgnore is configuration: the
// operator said not to look. ExcludedByKind is vocabulary: no enabled extractor
// declares ownership of the file's extension (via plugin.FileOwner — an
// extractor that probes content rather than paths claims no extension, so its
// unparsed candidates count here), and ExcludedKinds records which extensions,
// so a receipt names the languages a graph is blind to instead of hiding them
// in a smaller parse count. SkippedWithCause is the parseable-unparsed set —
// an extractor claimed the file and it still produced no fact — each with the
// most specific cause the run recorded.
type FileCensus struct {
	FilesWalked      int            `json:"files_walked"`
	Parsed           int            `json:"parsed"`
	ExcludedByIgnore int            `json:"excluded_by_ignore"`
	ExcludedByKind   int            `json:"excluded_by_kind"`
	ExcludedKinds    map[string]int `json:"excluded_kinds,omitempty"`
	SkippedWithCause int            `json:"skipped_with_cause"`
	TopSkipCauses    []CensusCause  `json:"top_skip_causes,omitempty"`
}

// CensusCause is one aggregated skip cause and how many files it accounts for,
// ordered by count so the receipt leads with the biggest hole.
type CensusCause struct {
	Cause string `json:"cause"`
	Count int    `json:"count"`
}

// Sums reports whether the buckets add back up to the walked count — the
// identity the census is built on.
func (c *FileCensus) Sums() bool {
	return c.Parsed+c.ExcludedByIgnore+c.ExcludedByKind+c.SkippedWithCause == c.FilesWalked
}

// CoverageSummary is a snapshot-level rollup of the cross-repo edge coverage the
// linker records per service, so a receipt reflects unresolved outbound edges
// without the consumer running coverage_report.
type CoverageSummary struct {
	ServicesTotal   int `json:"services_total"`
	CoverageGaps    int `json:"coverage_gaps"`            // services classified ServiceCoverageGap: no resolved outbound edge, yet unresolved call sites were detected. A service that resolved some edges is partially covered, not a gap
	UnresolvedEdges int `json:"unresolved_edges"`         // detected outbound edges that did not resolve to a loaded service (internal blind spots; excludes external)
	ExternalEdges   int `json:"external_edges,omitempty"` // detected outbound edges to hardcoded external hosts (third-party APIs) — expected, not a blind spot

	// ExtractorsReporting counts extractors that accounted for their own misses.
	// It is deliberately separate from the service tallies above: an extraction
	// blind spot ("this macro declares routes I cannot read") is a different
	// claim from a cross-repo one ("this call site names a service I cannot
	// find"), and summing them would answer neither question.
	//
	// Zero here means nobody reported, which is NOT the same as nothing missed —
	// the distinction this field exists to preserve, and the one that let an
	// unresolved counter read 0 over 1,363 templates it never examined.
	ExtractorsReporting int `json:"extractors_reporting,omitempty"`
	// ExtractionUnresolved is what those reporting extractors could not resolve.
	ExtractionUnresolved int `json:"extraction_unresolved,omitempty"`
}

// ArtifactFormatVersion is the generation of the snapshot artifact format —
// the JSON shapes documented in docs/schema/ — that this build writes. It is
// stamped on every receipt so a consumer can branch on format behavior without
// guessing from the enola version. Bump it when a documented field is renamed
// or removed, when the identity convention changes, or when an existing value's
// meaning is redefined; additive fields and new vocabulary values do not bump
// it, because a consumer that does not know about them ignores them. A zero
// value means "not stamped by the current writer" (a receipt reconstructed from
// a shared history payload), which a consumer must treat as unknown, not v0.
const ArtifactFormatVersion = 1

// Receipt is the compact, machine-readable manifest written to receipt.json — a
// projection of SnapshotMeta that proves what the deterministic graph was
// generated over (version, git, id, plugin sets, ignore-glob hash, output
// hashes) and carries the extraction-quality metrics a consumer or an agent
// improving enola can act on. It deliberately omits the large per-file FileHashes
// list that lives in snapshot.meta.json (the internal superset).
type Receipt struct {
	SnapshotID string `json:"snapshot_id"`
	// FormatVersion identifies which generation of the artifact format this
	// receipt describes (docs/schema/README.md, "Versioning"). A consumer that
	// cannot read it should fail loudly rather than partially parse an unknown
	// format into a graph that looks complete but is not.
	FormatVersion int `json:"format_version"`
	// EnolaVersion is the build; ExtractorVersion is what that build EXTRACTS LIKE. They
	// differ for every local build, where the former is the constant "dev". See
	// SnapshotMeta.ExtractorVersion.
	EnolaVersion     string            `json:"enola_version"`
	ExtractorVersion string            `json:"extractor_version,omitempty"`
	GeneratedAt      string            `json:"generated_at"`
	Duration         string            `json:"duration"`
	RepoPath         string            `json:"repo_path"`
	Git              *GitInfo          `json:"git,omitempty"`
	Extractors       []string          `json:"extractors"`
	Explainers       []string          `json:"explainers"`
	Renderers        []string          `json:"renderers,omitempty"`
	Providers        []ProviderRecord  `json:"providers,omitempty"`
	ConfigHash       string            `json:"config_hash,omitempty"`
	IgnoreGlobHash   string            `json:"ignore_glob_hash,omitempty"`
	OutputHashes     map[string]string `json:"output_hashes,omitempty"`

	FactCount    int            `json:"fact_count"`
	InsightCount int            `json:"insight_count"`
	Quality      ReceiptQuality `json:"quality"`
}

// ReceiptQuality groups the extraction-completeness metrics — the loop signal a
// consumer polls to detect thin extraction.
type ReceiptQuality struct {
	FilesSeen         int              `json:"files_seen"`
	FilesParsed       int              `json:"files_parsed"`
	FilesSkipped      int              `json:"files_skipped"`
	DirsSkipped       int              `json:"dirs_skipped"`
	SkippedSample     []string         `json:"skipped_sample,omitempty"`
	ParseErrors       int              `json:"parse_errors"`
	ParseErrorSample  []ParseError     `json:"parse_error_sample,omitempty"`
	HeuristicInsights int              `json:"heuristic_insights"`
	Coverage          *CoverageSummary `json:"coverage,omitempty"`
	Census            *FileCensus      `json:"census,omitempty"`
}

// Receipt projects a SnapshotMeta into the compact receipt manifest.
func (m SnapshotMeta) Receipt() Receipt {
	return Receipt{
		SnapshotID:       m.SnapshotID,
		FormatVersion:    ArtifactFormatVersion,
		EnolaVersion:     m.EnolaVersion,
		ExtractorVersion: m.ExtractorVersion,
		GeneratedAt:      m.GeneratedAt,
		Duration:         m.Duration,
		RepoPath:         m.RepoPath,
		Git:              m.Git,
		Extractors:       m.Extractors,
		Explainers:       m.Explainers,
		Renderers:        m.Renderers,
		Providers:        m.Providers,
		ConfigHash:       m.ConfigHash,
		IgnoreGlobHash:   m.IgnoreGlobHash,
		OutputHashes:     m.OutputHashes,
		FactCount:        m.FactCount,
		InsightCount:     m.InsightCount,
		Quality: ReceiptQuality{
			FilesSeen:         m.FilesSeen,
			FilesParsed:       m.FilesParsed,
			FilesSkipped:      m.FilesSkipped,
			DirsSkipped:       m.DirsSkipped,
			SkippedSample:     m.SkippedSample,
			ParseErrors:       m.ParseErrors,
			ParseErrorSample:  m.ParseErrorSample,
			HeuristicInsights: m.HeuristicInsights,
			Coverage:          m.Coverage,
			Census:            m.Census,
		},
	}
}

// GraphReceipt is the graph-wide manifest written to ~/.enola/receipt.json — it
// describes the CURRENT multi-repo "graph of graphs": which repositories compose
// it, the git commit each sits on, when each entered the graph and how long it has
// been a member, and what the graph consists of now. Unlike the per-repo Receipt
// (which proves what one deterministic snapshot was generated over), this is a
// machine-global picture of the live graph. It works for a single-repo graph too
// (Repos then has one entry).
type GraphReceipt struct {
	GeneratedAt        string           `json:"generated_at"`          // RFC3339 UTC, this write
	EnolaVersion       string           `json:"enola_version"`         // build version that produced the current graph
	SnapshotID         string           `json:"snapshot_id"`           // whole-graph content fingerprint (SnapshotMeta.SnapshotID)
	FactCount          int              `json:"fact_count"`            // total facts across all repos
	InsightCount       int              `json:"insight_count"`         // total insights
	ServiceCount       int              `json:"service_count"`         // KindService nodes = repos materialized in the cross-repo graph
	CrossRepoEdgeCount int              `json:"cross_repo_edge_count"` // consumer->provider edges in the cross-repo "graph of graphs" (NOT the total dependency-fact count, which also covers ordinary imports)
	Coverage           *CoverageSummary `json:"coverage,omitempty"`    // cross-repo edge-coverage rollup, nil in single-repo mode
	Repos              []GraphRepoEntry `json:"repos"`                 // one entry per repository in the graph, sorted by Label
}

// GraphRepoEntry describes one repository's membership in the current graph.
type GraphRepoEntry struct {
	Label           string   `json:"label"`                       // the store's repo label (see RepoLabel); NOT necessarily the directory name
	Path            string   `json:"path"`                        // absolute repo root
	Git             *GitInfo `json:"git,omitempty"`               // ref/commit/dirty; nil for non-git dirs
	AddedAt         string   `json:"added_at"`                    // RFC3339 UTC, first time this label entered the graph (merged forward across regenerations)
	InGraphFor      string   `json:"in_graph_for"`                // human duration since AddedAt (e.g. "72h3m0s"); derived, recomputed each write
	FactCount       int      `json:"fact_count"`                  // facts tagged with this repo label
	CommitChangedAt string   `json:"commit_changed_at,omitempty"` // RFC3339 UTC, last time the recorded commit moved; AddedAt is NOT reset when the commit changes

	// SourceBytes is the on-disk size of this repo's parsed source — the corpus
	// figure the value model prices against (see pkg/status). It is carried here
	// so a server restarting onto a restored graph knows how large that graph is
	// without re-snapshotting: the facts come back from disk, and so must the
	// measurement of what they were extracted from.
	//
	// Zero means "not recorded" — a receipt written before this field existed, or
	// a repo whose snapshot metadata could not be read. Consumers treat zero as
	// unknown rather than as an empty corpus.
	SourceBytes int64 `json:"source_bytes,omitempty"`
}
