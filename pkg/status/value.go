package status

import (
	"math"
	"sort"
	"sync"
	"time"
)

// The value model estimates what enola saved you, in two currencies: tokens and
// time. It answers one question — *what would an agent have had to ingest to
// reach the same answer using ordinary tools (grep, glob, open a file, read it,
// infer)?* — and prices that counterfactual, not the size of enola's own
// response. See ARCHITECTURE.md, "The value model", for the full rationale.
//
// Two consequences of that framing drive the whole design:
//
//  1. Snapshot value scales with the CORPUS, not with the call. Reconstructing a
//     33M-token monorepo graph and a 72K-token service graph are not the same act
//     of work, and no flat per-call price can be right for both — the two are
//     more than two orders of magnitude apart.
//
//  2. Savings are accumulated AT CALL TIME, not recomputed from call counts at
//     render time. Corpus size, changed-file fraction and response size are only
//     known when the call happens. Persisting the resulting token figure — rather
//     than a weight to be re-applied later — also means every binary reports the
//     same number for the same usage file, instead of a build that has never heard
//     of a tool pricing it at defaultWeight.
const (
	// rediscoveryFactor is the fraction of a corpus an agent actually ingests
	// while rebuilding this understanding by hand. It reads many files, greps
	// the rest, and stops when it believes it understands enough — usually
	// before it does. Crediting the full corpus would assume an exhaustiveness
	// no agent exhibits.
	rediscoveryFactor = 0.6

	// crossRepoPremiumFactor prices the extra work of resolving a new repo's
	// edges against every repo already loaded. It applies to the PRIOR corpus,
	// because that is what has to be re-examined to find the cross-repo edges —
	// the result an agent cannot produce at all once the combined corpus exceeds
	// its context window.
	crossRepoPremiumFactor = 0.05

	// refreshConfirmCredit is the per-repo credit for establishing that nothing
	// changed. Re-snapshotting an unchanged repo is not redundant: it answers
	// "is my understanding still valid?", which by hand means re-deriving the
	// graph to discover it did not move. Real, but far below a first build.
	refreshConfirmCredit = 2000

	// tokensPerManualOp converts a query tool's ordinal weight into tokens. It is
	// the MEDIAN parsed source file across the measured corpora (~800 tokens),
	// not the mean, which outliers inflate by roughly 2.5x.
	tokensPerManualOp = 800

	// queryScaleReferenceCorpus is the graph size at which a query tool's weight
	// is taken at face value. Below it the weight stands unscaled; above it, the
	// same question displaces more manual searching, because the work a query
	// replaces is driven by the size of the haystack rather than the number of
	// needles — finding three cycles in a kernel is harder than finding three
	// thousand in a small service.
	queryScaleReferenceCorpus = 1_800_000

	// maxQueryCorpusScale bounds that multiplier. The growth is logarithmic
	// because a haystack 100x larger does not take 100x the greps, just a few
	// more rounds of them; the cap then stops the largest graphs running away
	// entirely. It is the most arbitrary number in this file — it exists to be
	// conservative at the top end, not because 8 is special.
	maxQueryCorpusScale = 8

	// agentTokensPerSecond is end-to-end agent discovery throughput — tool round
	// trips and reasoning included, not raw generation speed. It converts tokens
	// an agent did not have to ingest into time a human did not have to wait.
	agentTokensPerSecond = 2500

	// reworkFactor is the non-determinism premium. An agent re-deriving your
	// architecture from grep does not merely take longer; it gets things subtly
	// wrong often enough that some of the work is done twice — the migration
	// attempted again, the caller missed until review, the refactor undone.
	// enola returns the same graph every time.
	reworkFactor = 1.5

	// agentContextWindow is the point past which the counterfactual stops being
	// expensive and becomes impossible: a corpus larger than this cannot be held
	// at once, so cross-repo edges within it are not derivable by re-reading at
	// any budget. Entries over it keep their credit but are flagged.
	agentContextWindow = 1_000_000

	// charsPerTokenApprox converts response bytes to tokens, matching the
	// engine's own heuristic (mcputil.approxTokensPerChar).
	charsPerTokenApprox = 4

	// defaultWeight prices any query tool not in toolWeights, so the model
	// degrades gracefully if the tool set grows.
	defaultWeight = 5
)

// toolWeights maps each query tool to an ordinal judgement: how much manual
// exploration one call displaces, relative to the other tools. It covers the
// tools this engine registers (see pkg/cli.OSSTools); a wrapper binary that adds
// its own MCP tools prices them via RegisterToolWeights rather than by editing
// this table.
//
// generate_snapshot is deliberately absent: its value is corpus-derived (see
// SnapshotValue) and cannot be expressed as a fixed number of lookups.
//
// weightsMu guards the map: registration happens once at startup, but reads run
// from the MCP server's tool callback and, in wrapper binaries, from concurrent
// HTTP handlers.
var (
	weightsMu   sync.RWMutex
	toolWeights = map[string]int{
		"show_symbol":      3,
		"snapshot_receipt": 3,
		"set_baseline":     4,
		"query_facts":      8,
		"compare_receipts": 10,
		"traverse":         10,
		"find_path":        12,
		"explore":          15,
		"diff_snapshot":    15,
		"coverage_report":  20,
		"impact_analysis":  25,
		"query_insights":   30,
		// The three analyzers sit above the band deliberately: reproducing
		// find_orphans or analyze_performance by hand means a whole-graph
		// reachability or loop-nesting analysis, not a lookup.
		"package_metrics":     20,
		"find_orphans":        40,
		"analyze_performance": 50,
	}
)

// RegisterToolWeights merges caller-supplied per-tool weights into the value
// model, so a wrapper binary that registers additional MCP tools can price them
// instead of letting them fall back to defaultWeight. Call it during startup,
// before the server begins serving. Later registrations win for a given tool.
//
// Note that the weight is applied when the call is recorded, and the resulting
// token figure is what gets persisted — so a wrapper's pricing survives into
// usage files that an OSS binary later reads and renders.
func RegisterToolWeights(extra map[string]int) {
	weightsMu.Lock()
	defer weightsMu.Unlock()
	for tool, w := range extra {
		toolWeights[tool] = w
	}
}

// weightFor returns the manual-ops-avoided weight for a query tool.
func weightFor(tool string) int {
	weightsMu.RLock()
	defer weightsMu.RUnlock()
	if w, ok := toolWeights[tool]; ok {
		return w
	}
	return defaultWeight
}

// SnapshotValue describes what one generate_snapshot call actually did, which is
// what its value is derived from. It is supplied by the server at call time.
type SnapshotValue struct {
	// CorpusTokens is the token size of the source that produced facts — the
	// engine's own measurement, never an estimate.
	CorpusTokens int

	// PriorCorpusTokens is the combined corpus already loaded before this call,
	// used to price cross-repo edge resolution. Zero for a first/fresh snapshot.
	PriorCorpusTokens int

	// Append is true when this repo joined an existing multi-repo graph.
	Append bool

	// Unchanged is true when the snapshot id matched the previous one for this
	// repo — the graph did not move, and the call's value is the confirmation.
	Unchanged bool

	// ChangedFraction is the share of parsed files whose content moved since the
	// previous snapshot, in [0,1]. Ignored when Unchanged is true.
	//
	// A NEGATIVE value means "no previous snapshot to compare against" — a first
	// build, which re-derives everything. Zero is meaningfully different: it says
	// the files are identical even though the snapshot id moved (a version or
	// config change), and earns confirmation credit rather than a full rebuild.
	ChangedFraction float64
}

// ToolCall is one recorded call, as the server observed it. Everything the value
// model needs that cannot be recovered later lives here.
type ToolCall struct {
	Tool string
	Repo string

	// OK is false when the handler returned an error result — a validation
	// failure, or a read tool called before any snapshot exists. Failed calls
	// displace no work and are recorded as calls but credited nothing.
	OK bool

	// ResponseBytes is the size of the result the agent had to read. It is
	// subtracted from the credit, which is what makes the output_mode ladder
	// visible in the estimate: summary mode genuinely saves more than full mode.
	ResponseBytes int

	// CorpusTokens is the size of the graph this call ran against — the whole
	// loaded set, not one repo, since a query searches everything resident. It
	// scales query credit and caps it. Zero means unknown, which scales by one.
	// Ignored for generate_snapshot, which prices from Snapshot instead.
	CorpusTokens int

	// Snapshot is non-nil only for generate_snapshot.
	Snapshot *SnapshotValue
}

// queryCorpusScale returns the multiplier applied to a query tool's ordinal
// weight for a graph of the given size: 1 at or below the reference corpus,
// growing logarithmically above it, hard-capped at maxQueryCorpusScale.
func queryCorpusScale(corpusTokens int) float64 {
	if corpusTokens <= queryScaleReferenceCorpus {
		return 1
	}
	scale := 1 + math.Log2(float64(corpusTokens)/queryScaleReferenceCorpus)
	if scale > maxQueryCorpusScale {
		return maxQueryCorpusScale
	}
	return scale
}

// TokensSaved prices a single call: the tokens an agent did not have to ingest,
// net of what reading enola's own response cost. Never negative.
func (c ToolCall) TokensSaved() int {
	if !c.OK {
		return 0
	}

	var gross int
	if c.Snapshot != nil {
		gross = c.Snapshot.tokens()
	} else {
		gross = c.queryTokens()
	}

	net := gross - c.ResponseBytes/charsPerTokenApprox
	if net < 0 {
		return 0
	}
	return net
}

// queryTokens prices one query tool call: its ordinal weight in tokens, scaled
// by the size of the graph it searched, and capped by what reading that graph
// would have cost outright.
//
// The cap matters as much as the scaling. Without it a single query against a
// small repo is credited more than the repo's entire source — no call can
// displace more work than reading everything it searched.
func (c ToolCall) queryTokens() int {
	gross := float64(weightFor(c.Tool)*tokensPerManualOp) * queryCorpusScale(c.CorpusTokens)

	if c.CorpusTokens > 0 {
		if ceiling := float64(c.CorpusTokens) * rediscoveryFactor; gross > ceiling {
			return int(ceiling)
		}
	}
	return int(gross)
}

// tokens prices a snapshot from the corpus it indexed.
func (s SnapshotValue) tokens() int {
	// Nothing moved: the call's value is the confirmation itself.
	if s.Unchanged || s.ChangedFraction == 0 {
		return refreshConfirmCredit
	}

	// A first build (no previous snapshot) re-derives everything; a refresh
	// re-derives only what moved, plus the cost of establishing that the rest
	// did not.
	fraction := 1.0
	refresh := false
	if s.ChangedFraction > 0 && s.ChangedFraction < 1 {
		fraction = s.ChangedFraction
		refresh = true
	}

	total := float64(s.CorpusTokens) * fraction * rediscoveryFactor
	if s.Append {
		total += float64(s.PriorCorpusTokens) * crossRepoPremiumFactor
	}
	if refresh {
		total += refreshConfirmCredit
	}

	// Ceiling: a repo can never be credited more than its own corpus. Reading
	// every line of it is the most an agent could possibly have ingested on its
	// account, so that is the honest upper bound — without this, the cross-repo
	// premium lets a tiny repo joining a large graph earn hundreds of times its
	// own source. It also gives the model a property worth stating out loud:
	// cumulative snapshot credit never exceeds the size of the code itself.
	if s.CorpusTokens > 0 && total > float64(s.CorpusTokens) {
		return s.CorpusTokens
	}
	return int(total)
}

// BeyondContext reports whether the corpus this call spanned exceeds what an
// agent could hold at once — the point at which the counterfactual is not merely
// expensive but impossible. Renderers flag these rather than presenting the
// number alone, because "not reproducible by re-reading" is a stronger and more
// honest claim than any figure.
func (c ToolCall) BeyondContext() bool {
	if c.Snapshot == nil {
		return false
	}
	return c.Snapshot.CorpusTokens+c.Snapshot.PriorCorpusTokens > agentContextWindow
}

// TimeSaved converts tokens an agent did not ingest into time a human did not
// spend waiting for it, including the rework a non-deterministic reconstruction
// implies. This is the ONLY place tokens become time, so the two figures can
// never drift apart.
func TimeSaved(tokens int) time.Duration {
	seconds := float64(tokens) / agentTokensPerSecond * reworkFactor
	return time.Duration(seconds * float64(time.Second))
}

// ToolValue is the estimated value delivered by a single tool.
type ToolValue struct {
	Tool          string
	Calls         int
	TimeSaved     time.Duration
	TokensSaved   int
	BeyondContext bool // at least one call spanned more than a context window
}

// ValueReport aggregates per-tool value plus totals.
type ValueReport struct {
	Tools            []ToolValue // sorted by tool name
	TotalCalls       int
	TotalTimeSaved   time.Duration
	TotalTokensSaved int
	BeyondContext    bool
}

// ComputeValue builds a report from accumulated per-tool figures: call counts and
// the token credit recorded for them at call time.
//
// saved may be nil or partial — a usage file written by a build that predates
// accumulation has counts but no token figures. Those tools are priced from
// counts via the legacy path so an upgrade never blanks out existing history,
// which is also why callers must not treat a zero as "no value".
func ComputeValue(counts map[string]int, saved map[string]int) ValueReport {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)

	var rep ValueReport
	rep.Tools = make([]ToolValue, 0, len(names))
	for _, name := range names {
		calls := counts[name]
		tokens, ok := saved[name]
		if !ok {
			tokens = legacyTokens(name, calls)
		}
		tv := ToolValue{
			Tool:        name,
			Calls:       calls,
			TokensSaved: tokens,
			TimeSaved:   TimeSaved(tokens),
		}
		rep.Tools = append(rep.Tools, tv)
		rep.TotalCalls += calls
		rep.TotalTokensSaved += tokens
	}
	// Derive the total from the total token count rather than summing per-tool
	// durations: accumulating rounded per-row values makes the TOTAL row disagree
	// with its own column by a few hundred milliseconds.
	rep.TotalTimeSaved = TimeSaved(rep.TotalTokensSaved)
	return rep
}

// legacyCorpusTokens is the corpus assumed for a generate_snapshot recorded
// before corpus measurement existed. It is the median of the measured corpora
// rather than the largest, so upgrading never inflates existing history.
const legacyCorpusTokens = 1_780_000

// legacyTokens prices calls from a usage file written before per-call token
// figures were recorded. Query tools are priced from their weight; snapshots get
// a single median-corpus credit regardless of how many times they ran, because
// such a file cannot say which of those calls were first builds and which were
// refreshes. Crediting each one as a first build would multiply one corpus by
// the call count, so the conservative reading is the only defensible one.
func legacyTokens(tool string, calls int) int {
	if tool == "generate_snapshot" {
		if calls == 0 {
			return 0
		}
		return int(float64(legacyCorpusTokens) * rediscoveryFactor)
	}
	return calls * weightFor(tool) * tokensPerManualOp
}
