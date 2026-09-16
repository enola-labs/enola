// Package facts re-exports enola's internal fact model as public type aliases and
// constants so code outside this module can implement plugin.Explainer — whose
// method signatures name these types — and read facts without importing the
// internal package directly.
//
// These are Go type ALIASES, not new types: facts.Store here is the exact same
// type as the internal facts.Store, so a method written against the alias
// satisfies an interface (plugin.Explainer) declared against the original.
package facts

import internal "github.com/enola-labs/enola/internal/facts"

// Core model types (aliases — identical to the internal types).
type (
	Fact     = internal.Fact
	Relation = internal.Relation
	Evidence = internal.Evidence
	Insight  = internal.Insight
	Store    = internal.Store
	Snapshot = internal.Snapshot
	Artifact = internal.Artifact
)

// Receipt / provenance types (aliases — identical to the internal types).
// Re-exported so consumers that read a receipt back — the dashboard, and any
// out-of-module tool checking provenance or extraction quality — unmarshal into
// the engine's own shapes instead of hand-written JSON mirrors that drift.
type (
	// SnapshotMeta is the full provenance record a snapshot carries. Re-exported
	// because a consumer that derives anything from a snapshot's IDENTITY (the
	// architecture history's epoch fingerprint and repo identity) needs the fields
	// the Receipt projection drops, and must read them from the engine's own shape.
	SnapshotMeta    = internal.SnapshotMeta
	Receipt         = internal.Receipt
	ReceiptQuality  = internal.ReceiptQuality
	FileCensus      = internal.FileCensus
	CensusCause     = internal.CensusCause
	GraphReceipt    = internal.GraphReceipt
	GraphRepoEntry  = internal.GraphRepoEntry
	CoverageSummary = internal.CoverageSummary
	ParseError      = internal.ParseError
	GitInfo         = internal.GitInfo
	ProviderRecord  = internal.ProviderRecord
	ProviderCensus  = internal.ProviderCensus
)

var RanProviders = internal.RanProviders

// Graph / impact-analysis types (aliases — identical to the internal types).
// Re-exported so out-of-module consumers (the enterprise architecture validator)
// can run reverse-dependency impact analysis over a snapshot's facts.
type (
	Graph         = internal.Graph
	ImpactResult  = internal.ImpactResult
	TraversalNode = internal.TraversalNode
)

// RepoIdentity returns the portable identity of the repository a snapshot describes —
// its normalized git remote, falling back to the checkout directory name. Re-exported
// because anything that PERSISTS a reference to a repository (the architecture history)
// must key on something that survives being read on another machine, and an absolute
// path is not that. See internal/facts for the full contract.
func RepoIdentity(m SnapshotMeta) string { return internal.RepoIdentity(m) }

// NormalizeRemote reduces a git remote URL to a comparable repository identity (host
// plus path, no scheme, credentials, port or ".git"). Re-exported alongside RepoIdentity
// for consumers holding a raw remote rather than a whole SnapshotMeta.
func NormalizeRemote(raw string) string { return internal.NormalizeRemote(raw) }

// NewStore builds an empty fact store; callers Add() facts (e.g. those read from a
// persisted baseline snapshot) to rebuild an in-memory store off the graph path.
// NewGraph builds a dependency graph directly from a slice of facts. Both are
// re-exported so out-of-module code can reconstruct a store/graph from a loaded
// baseline snapshot without importing the internal package.
var (
	NewStore = internal.NewStore
	NewGraph = internal.NewGraph
)

// Fact kind constants.
const (
	KindModule     = internal.KindModule
	KindSymbol     = internal.KindSymbol
	KindRoute      = internal.KindRoute
	KindStorage    = internal.KindStorage
	KindDependency = internal.KindDependency
	KindService    = internal.KindService
	KindTestRef    = internal.KindTestRef
	KindFileRef    = internal.KindFileRef
)

// Relation kind constants.
const (
	RelDeclares     = internal.RelDeclares
	RelImports      = internal.RelImports
	RelCalls        = internal.RelCalls
	RelImplements   = internal.RelImplements
	RelDependsOn    = internal.RelDependsOn
	RelInstantiates = internal.RelInstantiates
	RelInjects      = internal.RelInjects
	RelHasMethod    = internal.RelHasMethod
	RelHandledBy    = internal.RelHandledBy
)

// Symbol kind property values.
const (
	SymbolFunc      = internal.SymbolFunc
	SymbolMethod    = internal.SymbolMethod
	SymbolStruct    = internal.SymbolStruct
	SymbolInterface = internal.SymbolInterface
	SymbolType      = internal.SymbolType
	SymbolClass     = internal.SymbolClass
	SymbolVariable  = internal.SymbolVariable
	SymbolConstant  = internal.SymbolConstant
	SymbolEnum      = internal.SymbolEnum
)

// Module role property key + values, re-exported for out-of-module consumers
// (e.g. the enterprise package-metrics tool) that classify the production
// population.
const (
	PropModuleRole       = internal.PropModuleRole
	ModuleRoleProduction = internal.ModuleRoleProduction
	ModuleRoleTest       = internal.ModuleRoleTest
	ModuleRoleTooling    = internal.ModuleRoleTooling
	ModuleRoleUnknown    = internal.ModuleRoleUnknown
)

// ModuleRoleForPath classifies a module directory by its path segments (test /
// tooling / unknown). Re-exported so consumers share one source of truth with the
// extractors instead of re-implementing the heuristic.
func ModuleRoleForPath(dir string) string { return internal.ModuleRoleForPath(dir) }

// IsTestPath reports whether a repo-relative path is test or test-support code.
// Re-exported so out-of-module consumers (the enterprise dead-code and performance
// analyzers) share one definition with the OSS explainers instead of each carrying
// its own copy — they previously carried three, which drifted in both directions.
// See internal/facts for the full contract, in particular why it trusts the
// directory and not the filename.
func IsTestPath(p string) bool { return internal.IsTestPath(p) }

// MatchGlob reports which of the patterns matches a forward-slash relative path, and
// MatchAnyGlob whether any does. Unlike path.Match / filepath.Match, these understand
// `**`:
//
//	vendor/**                 anchored directory prefix
//	**/build/**               a directory named "build" at any depth
//	**/*_test.go              a basename glob at any depth
//	**/spec/**/*_spec.rb      a basename glob under a directory named "spec"
//
// Re-exported so out-of-module consumers share the engine's matcher instead of
// reaching for path.Match — which silently reads `**` as `*` and cannot cross a `/`,
// so every documented `**` pattern quietly matches nothing.
func MatchGlob(relPath string, patterns []string) (string, bool) {
	return internal.MatchGlob(relPath, patterns)
}

// MatchAnyGlob reports whether relPath matches any of the patterns. See MatchGlob.
func MatchAnyGlob(relPath string, patterns []string) bool {
	return internal.MatchAnyGlob(relPath, patterns)
}

// CanonicalSymbols collapses #if/#else conditional-compilation duplicates in a
// symbol-fact slice, keeping non-conditional overloads intact. Re-exported so
// out-of-module consumers that count symbols (package-metrics) apply the same rule
// as the OSS explainers. See internal/facts for the full contract.
func CanonicalSymbols(syms []Fact) []Fact { return internal.CanonicalSymbols(syms) }
