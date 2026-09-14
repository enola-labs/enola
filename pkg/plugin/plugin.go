package plugin

import (
	"context"
	"fmt"

	"github.com/enola-labs/enola/internal/facts"
)

// FatalError marks an extractor error the engine must NOT absorb. An ordinary
// extractor error is a parse failure over one language in one repository: the
// snapshot is still worth having, so the engine records it and carries on. That
// is the right default for source code, where an unparseable file is a gap.
//
// It is the wrong default for a DECLARATION. A declaration states what the
// architecture is supposed to be, and every verdict about that repo is computed
// from it — so absorbing an invalid one silently deletes findings rather than
// degrading a report, and a gate reading those findings passes because there is
// nothing left to fail on. An extractor wraps its error in FatalError when
// continuing would publish a snapshot that quietly says less than it should.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }

// Fatalf builds a fatal error, so the engine fails the snapshot instead of
// recording a parse error and continuing. Formatted rather than wrapping,
// because every caller has a file to name; %w works as usual.
func Fatalf(format string, args ...any) error {
	return &FatalError{Err: fmt.Errorf(format, args...)}
}

// Extractor parses source files for a specific language and emits architectural facts.
type Extractor interface {
	// Name returns the extractor identifier (e.g. "go", "typescript").
	Name() string
	// Detect returns true if this extractor supports the given repository.
	Detect(repoPath string) (bool, error)
	// Extract parses files in the repository and returns extracted facts.
	Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error)
}

// FileListDetector is an optional interface an Extractor may implement to answer
// detection from the file names the engine has ALREADY walked, instead of walking
// the tree again itself.
//
// Every detector that re-walked had to bound its own walk to stay affordable, and
// every one of those bounds was a cliff a real repository falls off: dotnet/runtime
// carries 3,270 C/C++ sources with NOT ONE inside the three levels cppextractor
// scanned, so the extractor never ran and the language was absent from the graph
// with nothing but a log line to say so. Raising the bound moves the cliff rather
// than removing it — flutterfire's Java sits at depth 10, past even the generous 8
// the JVM extractors allowed. Membership over the walked set has no bound to beat.
//
// It is also strictly cheaper than what it replaces. A full detection pass over
// dotnet/runtime cost 1.44s of bounded walking, and linux 2.46s, repeated for every
// registered extractor and again for the test-ref gate; answering from a list the
// engine already holds costs nothing.
//
// The names include files the ignore globs EXCLUDE, and exclude every file under a
// directory the walk pruned. Both halves are load-bearing. A pruned tree is
// vendored code that must never trigger detection, while an ignored FILE may be the
// only marker a language has: the bundled mcp-arch.yaml ignores **/*.yaml, and a
// Dart repository is spelled by pubspec.yaml. Detecting on a file that will not be
// indexed costs at worst one extractor running to emit nothing.
//
// Implementations must be a pure function of the names (repoPath is passed for the
// stat fast paths a root marker allows, not for a second walk). Extractors that do
// not implement it keep answering through Detect, unchanged.
type FileListDetector interface {
	// DetectFiles reports whether this extractor supports the repository, given
	// every file name the engine's walk visited (repo-relative, forward-slash).
	DetectFiles(repoPath string, files []string) (bool, error)
}

// FileOwner is an optional interface an Extractor may implement to declare which
// files it parses. The engine uses it to scope incremental caching: an
// extractor's previously computed facts are reused only when the contents of the
// files it owns (and the repo's shared config/manifest files) are unchanged.
// Extractors that do not implement it are never cached and always re-run.
type FileOwner interface {
	// OwnsFile reports whether this extractor parses the given repo-relative
	// file. It must be a pure function of the path (a superset is safe — owning a
	// file the extractor ignores only narrows what counts as "shared config").
	OwnsFile(relFile string) bool
}

// KeyDependent is an optional interface a FileOwner Extractor may implement to
// widen its incremental-cache key beyond the files it parses. The engine folds
// any file for which AffectsKey returns true into the extractor's cache key, so a
// change to a file that influences the extractor's output — without being one it
// owns — still invalidates its cache. The JVM extractors use this: their
// multi-module import resolution reads the package declarations of the OTHER JVM
// language's files (Kotlin resolves against .java packages and vice versa), so a
// cross-language package change must re-run the extractor.
type KeyDependent interface {
	// AffectsKey reports whether the given repo-relative file influences this
	// extractor's output even though OwnsFile returns false for it. It must be a
	// pure function of the path; a superset is safe (over-inclusion only reduces
	// cache reuse, never soundness).
	AffectsKey(relFile string) bool
}

// ConfigKeyed is an optional interface a FileOwner Extractor may implement when
// configuration, not only file contents, decides its output. A non-empty ConfigKey is
// folded into the extractor's incremental-cache key, so editing that configuration
// re-runs the extractor instead of serving facts extracted under the old one. An empty
// key leaves the cache key exactly as it would be without the interface, so an
// extractor with nothing configured keeps the caches it already has.
type ConfigKeyed interface {
	// ConfigKey returns a stable rendering of the configuration the extractor reads,
	// or "" when it reads none.
	ConfigKey() string
}

// TestRefExtractor is an optional interface an Extractor may implement to parse
// test/spec files for their outbound references into production code only. The
// engine calls it with the test files (matched by config.TestGlobs) that the
// extractor owns. Implementations must emit ONLY reference facts (facts.KindTestRef
// carrying RelCalls relations) and no symbol/module/route facts, so test code
// never becomes a dead-code candidate and no other explainer is affected.
// Extractors that do not implement it are simply skipped for test-ref extraction.
type TestRefExtractor interface {
	// ExtractTestRefs parses the repo-relative test files in testFiles and returns
	// reference-only facts (facts.KindTestRef).
	//
	// prodFiles is the production file list the same snapshot passed to Extract —
	// every non-test file, across all languages. It exists because resolving a test's
	// reference can require knowing which production modules are real: Python spells
	// an absolute-import call target as a dotted path ("pkg.mod.func") and can only
	// rewrite it to a canonical symbol name by checking that path against the set of
	// files that exist. Without it, every such target looks external and is dropped,
	// and the pass emits facts with no usable edges.
	//
	// Implementations that resolve purely lexically (Go via import paths, Ruby and
	// TypeScript via their own conventions) ignore it. It is passed rather than
	// re-derived inside the extractor deliberately: only the engine knows which files
	// survived the ignore globs, and a second walk would duplicate that knowledge —
	// the mistake that let fixture directories into the graph via the self-walking
	// OpenAPI and gRPC extractors.
	ExtractTestRefs(ctx context.Context, repoPath string, testFiles, prodFiles []string) ([]facts.Fact, error)
}

// Annotator is an optional interface an Explainer may implement to write DERIVED
// VALUES back onto the facts it analysed, in addition to emitting insights.
//
// It exists because insights and measurements are different things. An insight is a
// finding — discrete, evidenced, worth a reader's attention — and an analyzer that
// computes a continuous value for every module has no honest way to publish it as one:
// emitting a finding per module would be noise, and emitting only the outliers throws
// the rest away. A prop on the fact is the right home, and it comes with a property
// insights do not have: the snapshot diff already reports prop movement, so an annotated
// metric becomes comparable across two snapshots for free, attributed to the module it
// belongs to.
//
// Contract:
//
//   - Annotate runs AFTER extraction and linking, so the whole graph is present — which
//     is what makes whole-graph derivations (afferent/efferent coupling) computable here
//     and not in an extractor.
//   - It runs BEFORE Explain, so an implementation can compute once, annotate, and read
//     its own props back when building insights.
//   - It may only ADD OR UPDATE PROPS on existing facts. Adding, removing or renaming
//     facts here would make the graph depend on which explainers were enabled, and two
//     snapshots taken with different sets would no longer be comparable.
//   - Values MUST be deterministic and stable in their rendered form. They are written
//     into facts.jsonl and hashed into the snapshot id, so an unrounded float — whose
//     last bits can move with summation order — would make an unchanged tree produce a
//     different snapshot every run. Round before storing.
//
// Gated exactly like Explain: an explainer excluded by config annotates nothing.
type Annotator interface {
	// Annotate writes derived values onto facts already in the store.
	Annotate(ctx context.Context, store *facts.Store) error
}

// Explainer analyzes facts and produces architectural insights.
type Explainer interface {
	// Name returns the explainer identifier (e.g. "cycles", "layers").
	Name() string
	// Explain analyzes the fact store and returns insights.
	Explain(ctx context.Context, store *facts.Store) ([]facts.Insight, error)
}

// WalkAware is an optional interface an Explainer may implement to receive the
// names of every file the walk visited, alongside the fact store.
//
// It exists for findings whose evidence is a file that no extractor parses. A
// LICENSE has no extension and belongs to no language, so it appears in no fact —
// yet it is the thing that tells a vendored dependency apart from a directory
// that merely shares its name. Only the walker knows it is there.
//
// The names are passed rather than a repository path deliberately: an explainer
// that reads the disk stops being a pure function of the snapshot, and two runs
// over the same facts could then disagree. A name list keeps explain-time I/O at
// zero and the finding reproducible, which is what the determinism tests assert.
//
// Names are repo-relative and forward-slash, collected pre-ignore and post-prune —
// the same set detection answers from, so a marker the ignore globs drop is still
// visible and a pruned dependency tree is still invisible.
type WalkAware interface {
	// ExplainFiles analyses the fact store with the walked file names available.
	// An Explainer implementing it has ExplainFiles called INSTEAD of Explain.
	ExplainFiles(ctx context.Context, store *facts.Store, files []string) ([]facts.Insight, error)
}

// Renderer produces output artifacts from a snapshot.
type Renderer interface {
	// Name returns the renderer identifier (e.g. "llm_context").
	Name() string
	// Render produces artifacts from the given snapshot.
	Render(ctx context.Context, snapshot *facts.Snapshot) ([]facts.Artifact, error)
}
