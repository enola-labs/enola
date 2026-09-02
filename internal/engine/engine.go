package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/explainers"
	"github.com/enola-labs/enola/internal/extractors"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/internal/linkers/binders"
	"github.com/enola-labs/enola/internal/linkers/crossrepo"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/signals"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/internal/providers"
	"github.com/enola-labs/enola/internal/renderers"
	"github.com/enola-labs/enola/internal/version"
	pkghistory "github.com/enola-labs/enola/pkg/history"
	"github.com/enola-labs/enola/pkg/plugin"
)

// snapshotBundle is the immutable, reader-visible snapshot state. GenerateSnapshot
// builds a brand-new store off to the side and publishes a fresh bundle in a single
// atomic swap; once published, none of its fields are ever mutated again. Readers
// (Store/Snapshot/RepoPaths/Intent/ResolveFactFile) Load the current bundle
// lock-free and use it for as long as they like — a concurrent regeneration
// builds a different store and swaps the pointer, leaving the bundle an
// in-flight reader holds intact.
type snapshotBundle struct {
	store     *facts.Store      // immutable once published
	snapshot  *facts.Snapshot   // may be nil (no snapshot generated yet)
	repoPaths map[string]string // repo label -> absolute path; immutable once published, may be nil
	// intent is each repo's resolved declaration, label-keyed, immutable once
	// published, may be nil. It rides the bundle rather than living beside it
	// so a reader cannot observe a declaration that disagrees with the snapshot
	// it is reading: both change in the same atomic swap, or neither does.
	intent map[string]*intent.Declaration
	// members holds each repository's own turn meta, keyed by absolute path, so a
	// cluster's fan-out can write per-repo provenance beside the shared union.
	members map[string]facts.SnapshotMeta
}

// Engine orchestrates the snapshot generation pipeline.
type Engine struct {
	mu         sync.Mutex // serializes GenerateSnapshot calls and guards the build-scratch store
	cfg        *config.Config
	extractors *extractors.Registry
	explainers *explainers.Registry
	renderers  *renderers.Registry
	binders    *binders.Registry
	signals    *signals.Registry

	// store is BUILD SCRATCH: it is reassigned to a fresh store at the top of each
	// GenerateSnapshot and read only by the pipeline helpers, all under mu. It is
	// NOT the reader-visible store — that lives in `current` and is only swapped in
	// atomically once the build is complete. Never expose e.store to readers.
	store *facts.Store

	// current holds the published, immutable {store, snapshot, repoPaths, intent}
	// bundle. Readers Load it lock-free; GenerateSnapshot Stores a new bundle.
	current atomic.Pointer[snapshotBundle]

	// persistCache controls whether the per-extractor cache is written back to
	// disk after a snapshot. The read path is always active when caching is
	// enabled; one-shot --explain sets this false so it never touches .enola.
	persistCache bool
	// deferLinking makes an append-mode GenerateSnapshot stop after extraction;
	// see SetDeferLinking.
	deferLinking bool
	// lastFacts remembers the facts.jsonl this run last serialized, so the next
	// output dir that receives the same bundle copies the bytes instead.
	lastFacts lastFactsWrite
	// historyRecorded remembers which snapshot id this run recorded into which
	// history root, so a cluster that writes one union to many repo dirs sharing
	// a history store appends the revision once.
	historyRecorded map[string]string
	// summaries remembers this run's previous/ deltas; see summarizeOnce.
	summaries map[string]pkghistory.Summary
}

// New creates a new Engine with the given config.
// Extractors, explainers, and renderers must be registered after creation.
func New(cfg *config.Config) (*Engine, error) {
	// Normalize here as well as in config.Load: a config assembled in code — by a
	// test, by a wrapper, by anything that did not read a file — must get the same
	// derived ignore glob for its output directory, or it indexes its own artifacts.
	// Idempotent, so the file path pays nothing for it.
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}

	// The build-scratch store and the initial published store are the same empty
	// store, so AutoLoadSnapshot (which mutates Store() in place before serving)
	// and the first generate both start from a consistent, non-nil bundle.
	st := facts.NewStore()
	e := &Engine{
		cfg:          cfg,
		extractors:   extractors.NewRegistry(),
		explainers:   explainers.NewRegistry(),
		renderers:    renderers.NewRegistry(),
		binders:      binders.NewRegistry(),
		signals:      signals.NewRegistry(),
		store:        st,
		persistCache: true,
	}
	e.current.Store(&snapshotBundle{store: st})
	return e, nil
}

// SetDeferLinking tells the next GenerateSnapshot calls to stop after
// extraction: no linking, no graph, no explainers, no renderers. For a cluster
// walked repo by repo, only the last turn's linked and explained union is ever
// read, so the caller sets this for every turn but the last (the first turn
// included) and clears it before the last. Nothing else sets it; the server's
// generate_snapshot and a single-repo --generate never see it.
func (e *Engine) SetDeferLinking(defer_ bool) { e.deferLinking = defer_ }

// SetPersistCache controls whether the per-extractor cache is written to disk
// after a snapshot. One-shot --explain disables this so it leaves .enola
// untouched, while still reusing a cache a prior --generate may have written.
func (e *Engine) SetPersistCache(persist bool) { e.persistCache = persist }

// RegisterExtractor adds an extractor to the engine.
func (e *Engine) RegisterExtractor(ext extractors.Extractor) {
	e.extractors.Register(ext)
}

func (e *Engine) Extractors() []extractors.Extractor {
	return e.extractors.All()
}

// RegisterExplainer adds an explainer to the engine.
func (e *Engine) RegisterExplainer(exp explainers.Explainer) {
	e.explainers.Register(exp)
}

// RegisterRenderer adds a renderer to the engine.
func (e *Engine) RegisterRenderer(rnd renderers.Renderer) {
	e.renderers.Register(rnd)
}

// RegisterBinder adds a binder to the engine. Binders run in the link stage, each in
// the stage it declares; see plugin.Binder.
func (e *Engine) RegisterBinder(b plugin.Binder) {
	e.binders.Register(b)
}

// RegisterCrossRepoSignal adds a cross-repo signal to the engine. Signals only
// contribute in multi-repo (append) mode, where there is more than one repo for an edge
// to run between; see plugin.CrossRepoSignal.
func (e *Engine) RegisterCrossRepoSignal(s plugin.CrossRepoSignal) {
	e.signals.Register(s)
}

// Store returns the published fact store. The returned store is immutable for as
// long as the caller uses it: a concurrent regeneration builds a different store
// and swaps the published bundle, so it never mutates the store handed out here.
func (e *Engine) Store() *facts.Store {
	return e.current.Load().store
}

// Snapshot returns the last published snapshot, or nil. Lock-free: the returned
// snapshot is immutable once published.
func (e *Engine) Snapshot() *facts.Snapshot {
	return e.current.Load().snapshot
}

// Intent returns the resolved intent declaration for a repo label, or nil when
// the repo declares nothing. It reads the published bundle, so the declaration
// it returns is the one the current snapshot was built from — the verdicts in
// that snapshot are computed from the compiled intent facts, not from here.
func (e *Engine) Intent(repoLabel string) *intent.Declaration {
	return e.current.Load().intent[repoLabel]
}

// Config returns the engine config.
func (e *Engine) Config() *config.Config {
	return e.cfg
}

// SetRepoPaths sets the repo label -> absolute path mapping (used in tests). It
// republishes the bundle preserving the current store and snapshot.
func (e *Engine) SetRepoPaths(paths map[string]string) {
	b := e.current.Load()
	e.current.Store(&snapshotBundle{store: b.store, snapshot: b.snapshot, repoPaths: paths, intent: b.intent})
}

// SetSnapshot sets the snapshot (used in tests, and by AutoLoadSnapshot at
// startup). It republishes the bundle preserving the current store and repoPaths.
func (e *Engine) SetSnapshot(snap *facts.Snapshot) {
	b := e.current.Load()
	e.current.Store(&snapshotBundle{store: b.store, snapshot: snap, repoPaths: b.repoPaths, intent: b.intent})
}

// RestoreFromDir rebuilds and publishes the snapshot bundle from a persisted
// snapshot directory (facts.jsonl plus, when present, insights.json and
// snapshot.meta.json) WITHOUT re-running any extractor. It is the restart-restore
// counterpart to GenerateSnapshot: it reads facts into a brand-new store off to the
// side, builds the graph, then publishes a fresh bundle in one atomic swap — so no
// reader can ever observe a half-built store.
//
// repoPaths is the graph's label -> absolute-path map (one entry for a single-repo
// graph). When singleRepoLabel is non-empty, any untagged facts are labeled with it
// (a single-repo facts.jsonl carries no repo label); a multi-repo facts.jsonl is
// already tagged, so pass an empty singleRepoLabel to preserve the baked-in labels.
//
// Missing insights/meta are tolerated (a partial restore still serves facts); a
// missing facts.jsonl is an error. The complete replacement is built off to the
// side and atomically published, so a dashboard may use it to follow a newer
// on-disk snapshot while requests continue reading the previous bundle.
func (e *Engine) RestoreFromDir(dir string, repoPaths map[string]string, singleRepoLabel string) error {
	factsPath := filepath.Join(dir, "facts.jsonl")
	if _, err := os.Stat(factsPath); err != nil {
		return fmt.Errorf("no snapshot at %s: %w", dir, err)
	}

	work := facts.NewStore()
	if err := work.ReadJSONLFile(factsPath); err != nil {
		return fmt.Errorf("reading facts from %s: %w", factsPath, err)
	}
	if singleRepoLabel != "" {
		// Tags only facts whose Repo is empty, so a pre-tagged file is left intact.
		work.SetRepoRange(0, singleRepoLabel)
	}
	work.BuildGraph()

	// Default the primary repo path from the dir; snapshot.meta.json (loaded below)
	// overrides it when present.
	snap := &facts.Snapshot{Meta: facts.SnapshotMeta{RepoPath: filepath.Dir(dir)}}
	if data, err := os.ReadFile(filepath.Join(dir, "insights.json")); err == nil {
		var ins []facts.Insight
		if err := json.Unmarshal(data, &ins); err == nil {
			snap.Insights = ins
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "snapshot.meta.json")); err == nil {
		var meta facts.SnapshotMeta
		if err := json.Unmarshal(data, &meta); err == nil {
			snap.Meta = meta
		}
	}
	// Same as the generation path: nothing writes this store again, so collapse the
	// duplicate Props maps and Relations slices the snapshot was serialized with. A
	// restored graph is the long-lived case this matters most for — the server holds
	// it for hours without regenerating.
	work.Freeze()

	// FactsRef aliases the store's slice (never copied): `work` is published here and
	// then never mutated again, so a reader iterating snap.Facts sees a frozen array.
	snap.Facts = work.FactsRef()

	// intent is deliberately nil: a restore re-reads facts, not declaration files,
	// so the parsed declarations are not known here. The compiled intent FACTS are
	// in the restored store, which is what the explainer verdicts from; Intent()
	// reports nothing until the next generate re-reads the files.
	e.current.Store(&snapshotBundle{store: work, snapshot: snap, repoPaths: repoPaths})
	log.Printf("[engine] restored %d facts, %d insights from %s", work.Count(), len(snap.Insights), dir)
	return nil
}

// RepoPaths returns a copy of the repo label -> absolute path mapping (populated in
// append mode). Lock-free bundle load; the copy lets callers retain it safely.
func (e *Engine) RepoPaths() map[string]string {
	b := e.current.Load()
	if b.repoPaths == nil {
		return nil
	}
	cp := make(map[string]string, len(b.repoPaths))
	for k, v := range b.repoPaths {
		cp[k] = v
	}
	return cp
}

// ResolveFactFile returns the absolute filesystem path for a fact's File field.
// In multi-repo mode, it strips the repo-label prefix and joins with the
// corresponding repo root. In single-repo mode it falls back to the snapshot's
// RepoPath.
func (e *Engine) ResolveFactFile(f *facts.Fact) string {
	// Load the bundle once so repoPaths and snapshot are read as a consistent pair.
	b := e.current.Load()

	// Multi-repo: if the fact has a Repo label that maps to a known path,
	// strip the repo prefix from f.File and join with the absolute root.
	if f.Repo != "" && b.repoPaths != nil {
		if absRoot, ok := b.repoPaths[f.Repo]; ok {
			rel := strings.TrimPrefix(f.File, f.Repo+"/")
			return filepath.Join(absRoot, rel)
		}
	}

	// Single-repo fallback.
	if b.snapshot != nil {
		return filepath.Join(b.snapshot.Meta.RepoPath, f.File)
	}
	return f.File
}

// GenerateSnapshot runs the full pipeline: walk -> extract -> explain -> render.
// When appendMode is true the existing store is preserved and new facts are
// added with file paths prefixed by the repo basename, enabling multi-repo queries.
func (e *Engine) GenerateSnapshot(ctx context.Context, repoPath string, appendMode bool) (*facts.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	start := time.Now()

	if repoPath == "" {
		repoPath = e.cfg.Repo
	}

	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolving repo path: %w", err)
	}

	// Captured ONCE, here, because two things need it and they must agree: the repo
	// label every fact is tagged with (below) and the snapshot's own git provenance
	// (Meta.Git, further down). Deriving the label from a second `git remote` read
	// would let the two drift apart on a repository whose remote changed mid-run.
	git := gitInfo(absRepo, e.cfg.Output.Dir)
	var remote string
	if git != nil {
		remote = git.Remote
	}
	// The repository NAME, not the directory it happens to sit in — see facts.RepoLabel.
	repoLabel := repoLabelFor(absRepo, remote)
	// A cluster can hold two repositories whose NAMES agree and whose owners do not
	// (acme/web and other-org/web). Labels are how facts from different repos stay
	// apart, and graph nodes are name-keyed, so a collision would silently MERGE two
	// repositories into one — worse than the directory-derived label this replaced,
	// which at least differed whenever the checkouts did. Fall back to the directory
	// for the second arrival: it keeps them separate and it is what enola labelled
	// them before.
	if appendMode {
		if prev := e.current.Load(); prev != nil {
			if taken, ok := prev.repoPaths[repoLabel]; ok && taken != absRepo {
				repoLabel = filepath.Base(absRepo)
				log.Printf("[engine] repo label from remote collides with %q; using directory name %q", taken, repoLabel)
			}
		}
	}

	// Resolve this repo's declared intent: its own enola-intent.yaml, the
	// cluster config's entry, or the cluster entry overriding the file
	// wholesale (reported — an override must never be only implicit). An
	// invalid declaration fails the snapshot: a declaration that cannot be
	// trusted is worse than none, and silent skips are how vocabulary drift
	// would creep back in.
	fromFile, err := intent.LoadRepoFile(absRepo)
	if err != nil {
		return nil, err
	}
	resolved := intent.Resolve(fromFile, e.cfg.Intent[repoLabel])
	if resolved != nil && resolved.Overridden {
		log.Printf("[engine] intent for %q: cluster config overrides %s (wholesale)", repoLabel, intent.RepoFileName)
	}

	// Build into a BRAND-NEW store off to the side. The currently-published bundle
	// (prev) is never mutated, so any in-flight reader keeps iterating a consistent,
	// frozen snapshot while we regenerate. We publish the new store atomically at the
	// end (see e.current.Store below), which is the single linearization point.
	prev := e.current.Load()
	work := facts.NewStore()
	var workRepoPaths map[string]string
	// Declarations accumulate exactly like repoPaths: carried forward from the
	// published bundle in append mode, replaced wholesale otherwise, and handed
	// to the swap at the end. Nothing is written into the published map.
	workIntent := map[string]*intent.Declaration{}

	// A prior state produced by a different extraction behaviour cannot be
	// carried: its facts may be unlabeled (or labeled under different rules),
	// and the retroactive-tagging migration below would bulk-claim a stale
	// multi-repo union under one repo's label — manufacturing facts no repo's
	// source states. Discarding is the missing-edge-beats-wrong-one rule
	// applied to append mode; the discarded repos re-extract on their next
	// generate with the current behaviour.
	if appendMode && prev.snapshot != nil &&
		prev.snapshot.Meta.ExtractorVersion != cacheVersion {
		log.Printf("[engine] discarding prior state (extractor version %q != %q) — append would mislabel carried facts",
			prev.snapshot.Meta.ExtractorVersion, cacheVersion)
		appendMode = false
	}

	if appendMode {
		// Carry prior repos forward. All() returns an independent COPY, so mutating
		// `work` (TagUntagged/TagRange/SetRepoRange) can never touch prev.store, which
		// stays published and readable until we swap. This is the transient ~1x
		// fact-set memory cost of lock-free reads in append mode.
		workRepoPaths = make(map[string]string, len(prev.repoPaths)+1)
		for k, v := range prev.repoPaths {
			workRepoPaths[k] = v
		}
		for k, v := range prev.intent {
			workIntent[k] = v
		}
		if prev.store != nil {
			work.Add(prev.store.All()...)
		}

		// A repository the union already holds is replaced, not doubled. Its
		// prior slice goes before extraction, so what this turn adds is the only
		// account of it; the linkers then rebuild every cross-repo edge into it
		// from scratch, as they do on every append. This is what lets one repo
		// be re-read into an existing union without re-reading the other twenty.
		if _, present := prev.repoPaths[repoLabel]; present {
			if dropped := work.RemoveWhere(func(f facts.Fact) bool { return f.Repo == repoLabel }); dropped > 0 {
				log.Printf("[engine] replacing %q in the union: dropped its %d prior facts", repoLabel, dropped)
			}
		}

		// Track repo label -> absolute path for multi-repo resolution.
		workRepoPaths[repoLabel] = absRepo
	}
	if resolved != nil {
		workIntent[repoLabel] = resolved
	} else {
		// A repo that stopped declaring must stop being declared, including when
		// its entry was carried forward from the previous bundle.
		delete(workIntent, repoLabel)
	}
	if appendMode {

		// Retroactively tag facts from a prior single-repo snapshot so they
		// are filterable by repo alongside the newly appended facts.
		if prev.snapshot != nil && work.Count() > 0 {
			// The label those facts actually carry, which since RepoLabel came from the
			// remote is no longer the directory name. Older snapshots recorded none, and
			// for them the directory IS what tagged the facts.
			prevLabel := prev.snapshot.Meta.RepoLabel
			if prevLabel == "" {
				prevLabel = filepath.Base(prev.snapshot.Meta.RepoPath)
			}
			if _, alreadyTracked := workRepoPaths[prevLabel]; !alreadyTracked {
				tagged := work.TagUntagged(prevLabel, prevLabel+"/")
				if tagged > 0 {
					workRepoPaths[prevLabel] = prev.snapshot.Meta.RepoPath
					log.Printf("[engine] retroactively tagged %d existing facts with repo label %q", tagged, prevLabel)
				}
			}
		}
	}
	// Non-append: `work` stays empty and workRepoPaths stays nil — the prior bundle
	// is left intact for in-flight readers until the swap (no in-place Clear()).

	// Point the build-scratch store at `work` so the pipeline helpers (which read
	// e.store under e.mu) operate on the new store with no signature changes.
	e.store = work

	// Per-stage timing breakdown (logged at the end). Snapshotting is
	// extraction-dominated, so this makes it obvious where time goes.
	var tWalk, tHash, tExtract, tLink, tGraph, tExplain, tRender time.Duration

	// 1. Walk repository and collect files
	tStage := time.Now()
	files, testFiles, allNames, skips, err := e.walkRepo(absRepo)
	if err != nil {
		return nil, fmt.Errorf("walking repo: %w", err)
	}
	tWalk = time.Since(tStage)
	log.Printf("[engine] found %d files (%d test files, %d skipped) in %s", len(files), len(testFiles), skips.count, absRepo)

	// 2. Compute file hashes (for snapshot metadata)
	tStage = time.Now()
	currentHashes := e.computeFileHashes(absRepo, files)
	tHash = time.Since(tStage)

	// 3. Detect and run extractors (with optional per-extractor caching).
	tStage = time.Now()
	var cache *extractorCache
	cachePath := extractorCachePath(filepath.Join(absRepo, e.cfg.Output.Dir))
	if e.cfg.IncrementalEnabled() {
		cache = loadExtractorCache(cachePath, e.persistCache)
		// The cache now holds an open temp file from the moment it is created, so
		// every path out of this function has to close it. discard is a no-op after
		// a successful save; without it, a snapshot that fails during extraction
		// leaves the spool behind.
		defer cache.discard()
	}
	preCount := e.store.Count()
	usedExtractors, shadowedExtractors, parseErrs, err := e.runExtractors(ctx, absRepo, files, allNames, currentHashes, cache)
	if err != nil {
		return nil, fmt.Errorf("extraction: %w", err)
	}
	e.reportShadowed(absRepo, shadowedExtractors)
	// Reference-only extraction over test/spec files. Runs every snapshot (not
	// cached with the main extractors) and adds only KindTestRef facts, so a
	// production symbol exercised solely by a test is not mis-reported as dead.
	e.runTestRefExtractors(ctx, absRepo, testFiles, files, allNames)

	// Compile this repo's resolved intent declaration into intent facts, inside
	// the extraction window so SetRepoRange/TagRange treat declared facts
	// exactly as measured ones — snapshots carry them, diffs track them, and
	// the intent explainer reads them with no side channel.
	if resolved != nil {
		e.store.Add(intent.CompileFacts(resolved)...)
	}

	// Run configured external providers, still inside the extraction window so
	// their facts are repo-labeled (and prefixed, in append mode) exactly as
	// measured ones — and before linking, so a provider's edges participate in
	// the graph every explainer walks. Providers merge additively: an identity
	// an extractor already owns is never overwritten, so the census can say a
	// provider contributed less than it emitted but the graph cannot silently
	// change authorship.
	provRecords := e.runProviders(ctx, absRepo, preCount, providerInput(files, testFiles, currentHashes, e.computeFileHashes(absRepo, testFiles), cache))
	if cache != nil {
		log.Printf("[engine] extractor cache: %d reused", cache.hits)
		// Saved after the providers, whose entries share the spool. save is a no-op
		// when this cache was opened non-persisting, and it creates its own
		// directory, so neither condition is repeated here.
		if err := cache.save(); err != nil {
			log.Printf("[engine] could not write extractor cache: %v", err)
		}
	}

	tExtract = time.Since(tStage)
	newCount := e.store.Count()
	log.Printf("[engine] extracted %d facts using %d extractors", newCount, len(usedExtractors))

	// Flag facts from codegen output before the file paths are repo-prefixed below,
	// while f.File is still resolvable against repoPath.
	e.markGeneratedFacts(absRepo, preCount)

	// Always set Repo on newly extracted facts so the repo filter works
	// even in single-repo mode.
	e.store.SetRepoRange(preCount, repoLabel)

	// In append mode, additionally prefix file paths so facts from
	// different repos are distinguishable by file path.
	if appendMode {
		e.store.TagRange(preCount, repoLabel, repoLabel+"/")
		log.Printf("[engine] prefixed %d facts with repo label %q", newCount-preCount, repoLabel)
	}

	// A cluster's intermediate turns stop here. Linking, the graph and the
	// explainers are recomputed from scratch on every append and only the last
	// turn's result is kept, so running them after each of twenty-two
	// repositories was twenty-one full passes over a growing union for nothing
	// anyone read. The caller that walks a cluster sets deferLinking for every
	// turn but the last; that turn runs the full pipeline once over the whole
	// union. The bundle published here carries the store, the repo paths and
	// the intent the next turn needs, and a meta that says what was extracted.
	if e.deferLinking {
		work.Freeze()
		duration := time.Since(start)
		deferredPrefix := ""
		if appendMode {
			deferredPrefix = repoLabel + "/"
		}
		snapshot := &facts.Snapshot{
			Meta: facts.SnapshotMeta{
				RepoPath:           absRepo,
				GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
				Duration:           duration.String(),
				Extractors:         usedExtractors,
				Renderers:          []string{},
				FileHashes:         fileHashesOf(absRepo, currentHashes),
				FactCount:          e.store.Count(),
				EnolaVersion:       version.Version,
				ExtractorVersion:   cacheVersion,
				Git:                git,
				RepoLabel:          repoLabel,
				Providers:          provRecords,
				FilesSeen:          len(files),
				FilesParsed:        e.store.CountFilesWithFacts(files, deferredPrefix),
				SourceBytes:        e.store.SourceBytesWithFacts(files, deferredPrefix, absRepo),
				FilesSkipped:       skips.count,
				DirsSkipped:        skips.dirCount,
				SkippedSample:      skips.sample,
				ShadowedExtractors: shadowedExtractors,
				ParseErrors:        len(parseErrs),
				ParseErrorSample:   capParseErrors(parseErrs),
				Census:             e.fileCensus(files, deferredPrefix, skips, usedExtractors, parseErrs),
			},
			Facts: e.store.FactsRef(),
		}
		e.current.Store(&snapshotBundle{store: work, snapshot: snapshot, repoPaths: workRepoPaths, intent: workIntent,
			members: memberMetas(prev, appendMode, absRepo, snapshot.Meta)})
		log.Printf("[engine] %s extracted into the union in %s (%d facts so far); linking and explaining deferred to the cluster's last turn",
			repoLabel, duration.Round(time.Millisecond), e.store.Count())
		log.Printf("[engine] timings: walk=%s hash=%s extract=%s", tWalk.Round(time.Millisecond), tHash.Round(time.Millisecond), tExtract.Round(time.Millisecond))
		return snapshot, nil
	}

	// 3b. Link repos into a cross-repo "graph of graphs": derive service-level
	// nodes and consumer→provider edges from HTTP route role matching and
	// import/shared-lib references. Recomputed from scratch each run (prior
	// synthetic facts are dropped first) so it stays idempotent across appends.
	tStage = time.Now()
	e.runBinders(ctx, plugin.StagePreLink)
	e.linkCrossRepo(workRepoPaths)
	e.runBinders(ctx, plugin.StagePostLink)
	tLink = time.Since(tStage)

	// 3c. Build graph index for traversal queries
	tStage = time.Now()
	e.store.BuildGraph()
	tGraph = time.Since(tStage)
	log.Printf("[engine] built graph index (%d nodes, %d edges)", e.store.Graph().NodeCount(), e.store.Graph().EdgeCount())

	// 4. Run explainers
	tStage = time.Now()
	allInsights, usedExplainers, err := e.runExplainers(ctx, allNames)
	if err != nil {
		return nil, fmt.Errorf("explanation: %w", err)
	}
	positionInsights(allInsights, e.store)
	tExplain = time.Since(tStage)
	log.Printf("[engine] produced %d insights using %d explainers", len(allInsights), len(usedExplainers))

	// 5. Build file hashes for the snapshot meta
	fileHashes := fileHashesOf(absRepo, currentHashes)

	// 6. Build snapshot.
	//
	// Receipt fields: the snapshot ID is a content fingerprint over the same
	// byte-stable serialization that becomes facts.jsonl, so it is stable across
	// reruns on identical inputs and keys snapshot equivalence. The extraction
	// -quality fields (files seen/parsed/skipped, parse errors, coverage) let a
	// consumer judge how complete this extraction was before trusting it.
	duration := time.Since(start)
	ignoreGlobHash := computeIgnoreGlobHash(e.cfg)
	configHash := computeConfigHash(e.cfg)
	// In append mode this run's facts were path-prefixed with the repo label, so
	// match the walked files against that same prefix to count parsing coverage.
	parsedPrefix := ""
	if appendMode {
		parsedPrefix = repoLabel + "/"
	}
	// Stream the serialization straight into the hash. The ID is the only thing this
	// pass produces, so buffering the bytes to fingerprint and then discard them cost
	// 792 MiB on a kernel-sized graph for nothing. The digest is identical either way.
	idHasher := newSnapshotIDHasher()
	if err := e.store.WriteJSONL(idHasher); err != nil {
		return nil, fmt.Errorf("serializing facts for snapshot id: %w", err)
	}
	snapshot := &facts.Snapshot{
		Meta: facts.SnapshotMeta{
			RepoPath:     absRepo,
			GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
			Duration:     duration.String(),
			Extractors:   usedExtractors,
			Explainers:   usedExplainers,
			Renderers:    []string{},
			FileHashes:   fileHashes,
			FactCount:    e.store.Count(),
			InsightCount: len(allInsights),

			EnolaVersion: version.Version,
			// What this build EXTRACTS LIKE, which for a local build the version cannot
			// say — see facts.SnapshotMeta.ExtractorVersion.
			ExtractorVersion: cacheVersion,
			SnapshotID:       finishSnapshotID(idHasher, version.Version, configHash),
			Git:              git,
			ConfigHash:       configHash,
			// The label the facts in this snapshot actually carry. Recorded rather than
			// re-derived on read: a baseline outlives the build that wrote it, and a
			// reader that recomputed the label would apply TODAY's rule to YESTERDAY's
			// facts and conclude they match when their keys do not.
			RepoLabel: repoLabel,

			Providers: provRecords,

			FilesSeen:          len(files),
			FilesParsed:        e.store.CountFilesWithFacts(files, parsedPrefix),
			SourceBytes:        e.store.SourceBytesWithFacts(files, parsedPrefix, absRepo),
			FilesSkipped:       skips.count,
			DirsSkipped:        skips.dirCount,
			SkippedSample:      skips.sample,
			IgnoreGlobHash:     ignoreGlobHash,
			ShadowedExtractors: shadowedExtractors,
			ParseErrors:        len(parseErrs),
			ParseErrorSample:   capParseErrors(parseErrs),
			HeuristicInsights:  countHeuristicInsights(allInsights),
			Coverage:           coverageSummary(e.store),
			Census:             e.fileCensus(files, parsedPrefix, skips, usedExtractors, parseErrs),
			Unseen:             e.unseenCensus(skips, provRecords, allInsights),
		},
		// FactsRef aliases the store's slice rather than copying it. This is safe:
		// `work` is published (below) and then NEVER mutated again — the next
		// regeneration builds a different store — so a reader iterating snapshot.Facts
		// iterates a frozen backing array. Copying every fact would just double
		// steady-state RSS for a large repo. Baselines, which must stay immutable as
		// the store regenerates, still use the copying All().
		Facts:    e.store.FactsRef(),
		Insights: allInsights,
	}

	// 7. Run renderers
	tStage = time.Now()
	usedRenderers, err := e.runRenderers(ctx, snapshot)
	if err != nil {
		return nil, fmt.Errorf("rendering: %w", err)
	}
	tRender = time.Since(tStage)
	snapshot.Meta.Renderers = usedRenderers
	log.Printf("[engine] produced %d artifacts using %d renderers", len(snapshot.Artifacts), len(usedRenderers))

	// Every writer has now run: extraction, linking, binders, explainers, renderers.
	// Collapse the structurally identical Props maps and Relations slices the
	// extractors emitted independently onto shared instances before the store becomes
	// visible. This must happen HERE — after the last mutation and before the
	// publication below — because sharing is sound exactly for a store nothing writes
	// again. Append mode carries these facts into a future mutable store via
	// Store.All, which copies them back apart.
	tStage = time.Now()
	work.Freeze()
	log.Printf("[engine] froze fact store in %s", time.Since(tStage).Round(time.Millisecond))

	// Publish atomically. This single Store() is the linearization point: before it
	// readers see the prior bundle, after it the new one — never a half-built store.
	e.current.Store(&snapshotBundle{store: work, snapshot: snapshot, repoPaths: workRepoPaths, intent: workIntent,
		members: memberMetas(prev, appendMode, absRepo, snapshot.Meta)})
	log.Printf("[engine] snapshot generated in %s", duration)
	log.Printf("[engine] timings: walk=%s hash=%s extract=%s link=%s graph=%s explain=%s render=%s",
		tWalk.Round(time.Millisecond), tHash.Round(time.Millisecond), tExtract.Round(time.Millisecond),
		tLink.Round(time.Millisecond), tGraph.Round(time.Millisecond), tExplain.Round(time.Millisecond),
		tRender.Round(time.Millisecond))

	// Generation allocates large transient buffers (per-file fact slices, the
	// pre-dedup fact list, parser scratch) that the GC frees but Go's scavenger
	// returns to the OS only lazily. For a long-running server that loads a big
	// repo and then idles, hand that memory back now so idle RSS settles at the
	// live set instead of the extraction peak. Once per load, so the cost is
	// negligible. The MemStats line reports the retained footprint for visibility.
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Printf("[engine] memory after snapshot: heap=%d MiB sys=%d MiB (%d facts)",
		ms.HeapAlloc>>20, ms.Sys>>20, snapshot.Meta.FactCount)

	return snapshot, nil
}

// runProviders executes the configured external fact providers against this
// repository and merges their accepted facts into the build-scratch store,
// returning the census for the snapshot receipt. preCount marks where this
// repo's extraction window began: everything the extractors (and the intent
// compiler) added since is the identity set a provider fact may not collide
// with — the extractor's account of a name+kind wins, always.
func (e *Engine) runProviders(ctx context.Context, absRepo string, preCount int, in providers.Input) []facts.ProviderRecord {
	if len(e.cfg.Providers) == 0 {
		return nil
	}
	extracted := e.store.FactsRef()[preCount:]
	owned := make(map[string]bool, len(extracted))
	for _, f := range extracted {
		owned[f.Kind+"\x00"+f.Name] = true
	}
	in.Providers = e.cfg.Providers
	in.RepoPath = absRepo
	in.Taken = func(kind, name string) bool { return owned[kind+"\x00"+name] }
	in.Ignored = func(file string) bool { return e.isIgnored(file, false) }
	provFacts, records := providers.RunWith(ctx, in)
	// The join against the extractor's relations runs once per provider over
	// the facts that survived the merge, and stamps which tree they describe;
	// both are accounting on the record and change nothing in the graph.
	tJoin := time.Now()
	providers.Account(records, provFacts, extracted, gitInfo(absRepo, e.cfg.Output.Dir))
	log.Printf("[providers] overlap join over %d provider fact(s) took %s", len(provFacts), time.Since(tJoin).Round(time.Millisecond))
	e.store.Add(provFacts...)
	if annotated := providers.LinkRuntimeObservations(e.store, preCount); annotated > 0 {
		log.Printf("[providers] runtime observations annotated %d extracted route fact(s)", annotated)
	}
	if typed := providers.LinkDeclaredContracts(e.store, preCount); typed > 0 {
		log.Printf("[providers] declared contracts typed %d extracted symbol fact(s)", typed)
	}
	return records
}

// providerInput gathers what the seam needs to reuse provider facts: every walked
// file (the test files too, since a provider reads specs the extractors only
// reference) with its content digest, and the cache when the engine keeps one.
func providerInput(files, testFiles []string, hashes, testHashes map[string]string, cache *extractorCache) providers.Input {
	all := make([]string, 0, len(files)+len(testFiles))
	all = append(all, files...)
	all = append(all, testFiles...)
	merged := make(map[string]string, len(hashes)+len(testHashes))
	for k, v := range hashes {
		merged[k] = v
	}
	for k, v := range testHashes {
		merged[k] = v
	}
	in := providers.Input{Files: all, Hashes: merged}
	if cache != nil {
		in.Cache = providerCache{c: cache}
	}
	return in
}

// runBinders runs every registered binder declaring the given stage, over the
// build-scratch store.
//
// Order within a stage is registration order, but carries no meaning: the plugin.Binder
// contract requires each binder to be independent of whether its stage-mates have run,
// precisely so this loop can stay a loop. A binder that needs to observe another's output
// is in the wrong stage.
//
// A binder error is logged and the run continues, matching how a failing explainer is
// handled: a binder resolves edges that enrich the graph, so losing one degrades the
// snapshot but does not invalidate the facts already extracted. Failing the whole
// snapshot would trade a partial graph for none at all.
func (e *Engine) runBinders(ctx context.Context, stage plugin.BindStage) {
	for _, b := range e.binders.Stage(stage) {
		if err := b.Bind(ctx, e.store); err != nil {
			log.Printf("[engine] binder %s (%s) error: %v", b.Name(), stage, err)
		}
	}
}

// sourceReaderFor builds the crossrepo.SourceReader used to verify shared type names
// against the code behind them. It mirrors ResolveFactFile's repo-prefix-strip-and-join,
// but reads from the passed-in map rather than the published bundle (see linkCrossRepo).
// Returns nil when no repo paths are known, which turns verification off rather than
// silently comparing nothing. Files are read at most once per snapshot.
func sourceReaderFor(repoPaths map[string]string) crossrepo.SourceReader {
	if len(repoPaths) == 0 {
		return nil
	}
	cache := map[string]string{}
	missing := map[string]bool{}
	return func(f facts.Fact) (string, bool) {
		if f.File == "" || f.Repo == "" {
			return "", false
		}
		if text, ok := cache[f.File]; ok {
			return text, true
		}
		if missing[f.File] {
			return "", false
		}
		root, ok := repoPaths[f.Repo]
		if !ok {
			missing[f.File] = true
			return "", false
		}
		abs := filepath.Join(root, strings.TrimPrefix(f.File, f.Repo+"/"))
		data, err := os.ReadFile(abs)
		if err != nil {
			missing[f.File] = true
			return "", false
		}
		cache[f.File] = string(data)
		return cache[f.File], true
	}
}

// linkCrossRepo drops any previously-synthesized cross-repo facts and recomputes
// them over the full fact set, adding service nodes and consumer→provider edges.
// It is a no-op for single-repo snapshots (no cross-repo matches exist).
//
// repoPaths must be the IN-FLIGHT label -> absolute path map, not the published
// bundle's: this runs mid-snapshot, before the new bundle is stored, so
// ResolveFactFile would not yet know the repo currently being appended. It is used to
// read source for shared-symbol verification; a nil or incomplete map degrades that
// check to name-only matching rather than failing.
func (e *Engine) linkCrossRepo(repoPaths map[string]string) {
	e.store.RemoveWhere(func(f facts.Fact) bool {
		if f.Props == nil {
			return false
		}
		return f.Props["synthetic"] == crossrepo.SyntheticMarker
	})

	// A cross-repo edge needs two repos, and ComputeLinks says so itself — but only
	// after newInput has walked the copy it was handed. Asking the store first makes
	// the single-repo case, which is most snapshots, free: RepoLabels is O(repos) off
	// the byRepo index, where e.store.All() below is a deep copy of every fact
	// (Props map and Relations slice included) built solely to be counted and
	// dropped.
	//
	// The two conditions cannot disagree in the direction that would matter.
	// ComputeLinks counts labels after NormalizeRepoLabel folds case, '-' and '_';
	// RepoLabels counts them raw. Folding can only merge labels, never split them,
	// so distinct-raw < 2 implies distinct-normalized < 2. The reverse case (two raw
	// labels that normalize to one) skips this early-out and is caught by
	// ComputeLinks exactly as before — a missed saving, never a missed link.
	if len(e.store.RepoLabels()) < 2 {
		return
	}

	// An invalid `linking:` overlay is reported once and the run continues on the
	// built-in defaults: config validation belongs at load, and failing the whole
	// snapshot here would trade a graph linked with default vocabulary for no graph.
	v, err := e.cfg.LinkingVocab()
	if err != nil {
		log.Printf("[engine] invalid linking vocabulary, using defaults: %v", err)
		v = vocab.Default()
	}
	links := crossrepo.ComputeLinks(e.store.All(), sourceReaderFor(repoPaths), e.signals.All(), v)
	if len(links) == 0 {
		return
	}
	e.store.Add(links...)

	services, edges := 0, 0
	for _, f := range links {
		switch f.Kind {
		case facts.KindService:
			services++
		case facts.KindDependency:
			edges++
		}
	}
	log.Printf("[engine] cross-repo links: %d service nodes, %d dependency edges", services, edges)
}

// walkSkips is a lightweight tally of what the ignore globs dropped, kept so a
// snapshot receipt can report how much of the tree was excluded (and a sample of
// what) without retaining every skipped path.
//
// Files and directories are tallied separately because they cost differently to
// know. An ignored directory is pruned whole — the walker never descends, so its
// contents are counted nowhere. Counting them would mean walking node_modules/
// purely to size it, a stat per file for a number no architecture graph wants.
// One pruned directory is one architecturally meaningful fact; its 55,041 files
// are not.
type walkSkips struct {
	count    int      // ignored FILES the walker visited
	dirCount int      // ignored DIRECTORIES pruned; their contents are never visited
	sample   []string // capped sample of both, each annotated with the glob that matched
}

// record appends "<path> (glob: <pattern>)" until the sample is full. Directories
// arrive with a trailing slash, which is what distinguishes a pruned subtree from
// a single dropped file in the receipt.
func (s *walkSkips) record(path, pattern string) {
	if len(s.sample) < skippedSampleCap {
		s.sample = append(s.sample, path+" (glob: "+pattern+")")
	}
}

// skippedSampleCap bounds the number of skipped paths retained for the receipt.
const skippedSampleCap = 20

// walkRepo collects all files in the repo, applying ignore patterns. It returns
// the indexable source files, separately the test/spec files matched by
// TestGlobs (excluded from normal indexing but collected for reference-only
// extraction — see runTestRefExtractors), the names every visited file had before
// the ignore check (allNames, for plugin.FileListDetector — see detect), and a
// tally of what the ignore globs dropped — ignored files, and pruned directories —
// for the snapshot receipt.
//
// allNames is collected POST-PRUNE and PRE-IGNORE, and detection depends on both
// halves. Post-prune: a pruned directory is vendored or generated code the walker
// never descends into, and detection must not fire on it — a repository does not
// become a C++ project by carrying node_modules. Pre-ignore: an ignored FILE may be
// the only marker a language has, and the bundled config ignores **/*.yaml, which
// is how a Dart repository is spelled.
func (e *Engine) walkRepo(repoPath string) (files, testFiles, allNames []string, skips walkSkips, err error) {
	// A symlinked repo root walks as a single non-directory entry (WalkDir
	// Lstats the root), silently yielding zero files. Resolve the ROOT only —
	// symlinks inside the tree keep their non-followed semantics — so a config
	// may alias a checkout (a stable clone under the repo's own label) and
	// still extract. The label was already derived from the unresolved path.
	if resolved, rerr := filepath.EvalSymlinks(repoPath); rerr == nil && resolved != repoPath {
		repoPath = resolved
	}
	err = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rawRel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return err
		}
		// Repo-relative paths become forward-slash HERE, at the one boundary where
		// the host filesystem enters the pipeline, and stay that way through every
		// extractor, fact and finding. On Windows filepath.Rel yields
		// `src\lib\site-blocks.ts`, and everything downstream — module resolution,
		// layer classification, ignore globs, the declaration dialect — splits on
		// "/". A backslash path is not a different spelling of the same fact to any
		// of them; it is a fact that matches nothing. See ARCHITECTURE.md, "Fact
		// paths are forward-slash on every host".
		relPath := filepath.ToSlash(rawRel)

		// Every visited FILE, named before the ignore check decides anything. This is
		// the set plugin.FileListDetector answers from; see walkRepo's own comment for
		// why it is taken here and not two lines lower.
		if !d.IsDir() {
			allNames = append(allNames, relPath)
		}

		// Skip ignored paths
		if pattern, ok := e.ignoreMatch(relPath); ok {
			if d.IsDir() {
				// enola's own output directory is not part of the source tree.
				// Counting it would make dirs_skipped differ between a repo's
				// first-ever snapshot and every one after it, for no signal.
				if relPath != e.cfg.Output.Dir {
					skips.dirCount++
					skips.record(relPath+"/", pattern)
				}
				return filepath.SkipDir
			}
			// An ignored FILE that is a test/spec is not indexed as production
			// source, but is collected for reference-only extraction so a
			// production symbol exercised only by a test does not look dead.
			if e.matchesTestGlob(relPath) {
				testFiles = append(testFiles, relPath)
			}
			skips.count++
			skips.record(relPath, pattern)
			return nil
		}

		if !d.IsDir() {
			files = append(files, relPath)
		}
		return nil
	})
	return files, testFiles, allNames, skips, err
}

// detect answers whether an extractor applies to this repository, preferring the
// walked name set over a second walk of the tree.
//
// allNames is nil at the call sites that have no walk of their own to offer; those
// fall back to Detect, which is the pre-FileListDetector behaviour. Every call site
// inside a snapshot has one, and so does CurrentMeta — deliberately, because the
// two must agree. CurrentMeta's extractor list is compared against the recorded
// snapshot's by diff.CompareMeta, and a disagreement raises WarnExtractorSet, which
// check.BlockingKinds treats as fatal: a repository detected one way here and the
// other way there would report "not comparable" on every run, forever.
func (e *Engine) detect(ext extractors.Extractor, repoPath string, allNames []string) (bool, error) {
	if fd, ok := ext.(plugin.FileListDetector); ok && allNames != nil {
		return fd.DetectFiles(repoPath, allNames)
	}
	return ext.Detect(repoPath)
}

// matchesTestGlob reports whether a repo-relative path matches any TestGlob.
func (e *Engine) matchesTestGlob(relPath string) bool {
	return matchAnyGlob(filepath.ToSlash(relPath), e.cfg.TestGlobs)
}

// matchAnyGlob and matchGlob are thin aliases onto the shared matcher, which now
// lives in internal/facts alongside the other path predicates (IsTestPath) so that
// out-of-module consumers can reach it through pkg/facts. The enterprise performance
// analyzer's ENOLA_PERF_EXCLUDE globs documented `**` support that path.Match cannot
// give them; rather than grow a second glob implementation, it uses this one.
func matchAnyGlob(relPath string, patterns []string) bool {
	return facts.MatchAnyGlob(relPath, patterns)
}

func matchGlob(relPath string, patterns []string) (string, bool) {
	return facts.MatchGlob(relPath, patterns)
}

// isIgnored checks whether a path matches any ignore pattern. isDir is unused: the
// patterns discriminate on shape, not on file type, and a directory that matches is
// pruned by the caller.
func (e *Engine) isIgnored(relPath string, isDir bool) bool {
	return matchAnyGlob(filepath.ToSlash(relPath), e.cfg.Ignore)
}

// ignoreMatch reports whether a path is ignored, and by which pattern. The walker
// needs the pattern to record it in the receipt's skipped sample.
func (e *Engine) ignoreMatch(relPath string) (string, bool) {
	return matchGlob(filepath.ToSlash(relPath), e.cfg.Ignore)
}

// runExtractors detects applicable extractors and runs them. When cache is
// non-nil, extractors implementing plugin.FileOwner have their facts reused
// whenever the files they depend on are unchanged since the last snapshot.
//
// It also reports the SHADOWED extractors: those registered and applicable to this
// repository, but excluded by an explicit `extractors:` list. See reportShadowed.
func (e *Engine) runExtractors(ctx context.Context, repoPath string, files, allNames []string, hashes map[string]string, cache *extractorCache) ([]string, []string, []facts.ParseError, error) {
	var usedNames []string
	var shadowed []string
	var parseErrs []facts.ParseError

	var keys map[string]string
	if cache != nil {
		keys = computeExtractorKeys(e.extractors.All(), files, hashes)
	}

	for _, ext := range e.extractors.All() {
		if !e.cfg.IsExtractorEnabled(ext.Name()) {
			// Disabled by config. Detect anyway when the list was written by hand,
			// so the one case worth a warning — a language present in the repo that
			// this config will not extract — is named rather than left as an absence.
			// Detect is a cheap file-presence probe, and only disabled extractors
			// reach here, so this costs nothing on a config that enables everything.
			if e.cfg.ExtractorsExplicit {
				if detected, err := e.detect(ext, repoPath, allNames); err == nil && detected {
					shadowed = append(shadowed, ext.Name())
				}
			}
			continue
		}

		detected, err := e.detect(ext, repoPath, allNames)
		if err != nil {
			log.Printf("[engine] extractor %s detect error: %v", ext.Name(), err)
			parseErrs = append(parseErrs, facts.ParseError{Extractor: ext.Name(), Msg: "detect: " + err.Error()})
			continue
		}
		if !detected {
			log.Printf("[engine] extractor %s: not detected", ext.Name())
			continue
		}

		// Reuse cached facts when this extractor's inputs are unchanged.
		if cache != nil {
			if key, ok := keys[ext.Name()]; ok {
				if cached, hit := cache.get(key); hit {
					e.store.Add(cached...)
					usedNames = append(usedNames, ext.Name())
					log.Printf("[engine] extractor %s: reused %d cached facts", ext.Name(), len(cached))
					continue
				}
			}
		}

		log.Printf("[engine] running extractor: %s", ext.Name())
		tExt := time.Now()
		extracted, err := ext.Extract(ctx, repoPath, files)
		if err != nil {
			// A fatal extractor error fails the snapshot rather than being recorded
			// and stepped over. Declarations use it: an invalid one would otherwise
			// remove every verdict computed from it while the run still reports
			// success, which reads as "nothing to report" rather than "not asked".
			var fatal *plugin.FatalError
			if errors.As(err, &fatal) {
				return nil, nil, nil, fmt.Errorf("extractor %s: %w", ext.Name(), fatal.Err)
			}
			log.Printf("[engine] extractor %s error: %v", ext.Name(), err)
			parseErrs = append(parseErrs, facts.ParseError{Extractor: ext.Name(), Msg: err.Error()})
			continue
		}

		// Cache the raw (pre-tagging) facts before the engine mutates them.
		if cache != nil {
			if key, ok := keys[ext.Name()]; ok {
				cache.put(key, extracted)
			}
		}

		e.store.Add(extracted...)
		usedNames = append(usedNames, ext.Name())
		log.Printf("[engine] extractor %s: emitted %d facts in %s", ext.Name(), len(extracted), time.Since(tExt).Round(time.Millisecond))
	}

	return usedNames, shadowed, parseErrs, nil
}

// reportShadowed warns about extractors that would have contributed facts but were
// excluded by an explicit `extractors:` list.
//
// An explicit list replaces the defaults rather than extending them, so a config
// written before an extractor existed disables it permanently — and a disabled
// extractor is not merely quiet, it never appears in the log at all. The failure
// looks exactly like a repository with nothing to find: a 780-file Rust repo
// reported 0 facts, no error, no mention of Rust. This is the line that names it.
func (e *Engine) reportShadowed(repoPath string, shadowed []string) {
	if len(shadowed) == 0 {
		return
	}
	sort.Strings(shadowed)
	where := "your config"
	if e.cfg.SourcePath != "" {
		where = e.cfg.SourcePath
	}
	log.Printf("[engine] warning: extractor(s) %s detected %s but are absent from the extractors: list in %s — they contribute no facts. An explicit extractors: list REPLACES the built-in defaults; add them to index these languages.",
		strings.Join(shadowed, ", "), repoPath, where)
}

// runTestRefExtractors runs reference-only extraction over the test/spec files
// for every enabled, detected extractor that implements plugin.TestRefExtractor.
// It scopes each extractor to the test files it owns and adds the resulting
// KindTestRef facts to the store. Errors are logged, not fatal.
//
// prodFiles is the same non-test file list handed to runExtractors, forwarded so an
// extractor whose reference resolution depends on which production modules exist
// (Python — see plugin.TestRefExtractor) does not have to re-walk the repo and
// re-implement the ignore globs to find out.
func (e *Engine) runTestRefExtractors(ctx context.Context, repoPath string, testFiles, prodFiles, allNames []string) {
	if len(testFiles) == 0 {
		return
	}
	for _, ext := range e.extractors.All() {
		if !e.cfg.IsExtractorEnabled(ext.Name()) {
			continue
		}
		tr, ok := ext.(plugin.TestRefExtractor)
		if !ok {
			continue
		}
		if detected, err := e.detect(ext, repoPath, allNames); err != nil || !detected {
			continue
		}
		owned := testFiles
		if fo, ok := ext.(plugin.FileOwner); ok {
			owned = owned[:0:0]
			for _, f := range testFiles {
				if fo.OwnsFile(f) {
					owned = append(owned, f)
				}
			}
		}
		if len(owned) == 0 {
			continue
		}
		// prodFiles is passed whole, unscoped by FileOwner: an extractor that needs
		// it uses it to decide whether a referenced module EXISTS, and the answer
		// must not depend on which extractor owns the file.
		refFacts, err := tr.ExtractTestRefs(ctx, repoPath, owned, prodFiles)
		if err != nil {
			log.Printf("[engine] extractor %s test-ref error: %v", ext.Name(), err)
			continue
		}
		e.store.Add(refFacts...)
		log.Printf("[engine] extractor %s: emitted %d test-ref facts from %d files", ext.Name(), len(refFacts), len(owned))
	}
}

// runAnnotators lets enabled explainers write derived values back onto the facts
// before any of them run Explain.
//
// Two orderings matter and neither is incidental. It runs AFTER the graph is built, so
// whole-graph derivations (afferent/efferent coupling) are computable at all; and it runs
// BEFORE every Explain rather than interleaved per explainer, so one explainer's insights
// can never depend on whether another happened to be registered ahead of it — which would
// make the snapshot depend on registration order.
//
// An annotator failure is logged and skipped, exactly as an explainer failure is: a
// missing derived prop costs a diff some detail, and refusing to produce a snapshot over
// it would cost the caller everything else in the graph.
func (e *Engine) runAnnotators(ctx context.Context) {
	for _, exp := range e.explainers.All() {
		ann, ok := exp.(plugin.Annotator)
		if !ok || !e.cfg.IsExplainerEnabled(exp.Name()) {
			continue
		}
		log.Printf("[engine] running annotator: %s", exp.Name())
		if err := ann.Annotate(ctx, e.store); err != nil {
			log.Printf("[engine] annotator %s error: %v", exp.Name(), err)
		}
	}
}

// runExplainers runs all enabled explainers.
func (e *Engine) runExplainers(ctx context.Context, allNames []string) ([]facts.Insight, []string, error) {
	var allInsights []facts.Insight
	var usedNames []string

	e.runAnnotators(ctx)

	for _, exp := range e.explainers.All() {
		if !e.cfg.IsExplainerEnabled(exp.Name()) {
			continue
		}

		log.Printf("[engine] running explainer: %s", exp.Name())
		// An explainer whose evidence includes files no extractor parses gets the
		// walked names too; see plugin.WalkAware.
		var insights []facts.Insight
		var err error
		if wa, ok := exp.(plugin.WalkAware); ok {
			insights, err = wa.ExplainFiles(ctx, e.store, allNames)
		} else {
			insights, err = exp.Explain(ctx, e.store)
		}
		if err != nil {
			log.Printf("[engine] explainer %s error: %v", exp.Name(), err)
			continue
		}

		// Tag each insight with its producing explainer so clients can fetch and
		// filter findings by source (e.g. query_insights(explainer="unused-routes"))
		// without every explainer having to set the field itself.
		for i := range insights {
			insights[i].Source = exp.Name()
		}

		allInsights = append(allInsights, insights...)
		usedNames = append(usedNames, exp.Name())
		log.Printf("[engine] explainer %s: produced %d insights", exp.Name(), len(insights))
	}

	return allInsights, usedNames, nil
}

// runRenderers runs all enabled renderers.
func (e *Engine) runRenderers(ctx context.Context, snapshot *facts.Snapshot) ([]string, error) {
	var usedNames []string

	for _, rnd := range e.renderers.All() {
		if !e.cfg.IsRendererEnabled(rnd.Name()) {
			continue
		}

		log.Printf("[engine] running renderer: %s", rnd.Name())
		artifacts, err := rnd.Render(ctx, snapshot)
		if err != nil {
			log.Printf("[engine] renderer %s error: %v", rnd.Name(), err)
			continue
		}

		snapshot.Artifacts = append(snapshot.Artifacts, artifacts...)
		usedNames = append(usedNames, rnd.Name())
	}

	return usedNames, nil
}

// WriteArtifacts writes all snapshot artifacts to the output directory,
// including facts.jsonl, insights.json, and snapshot.meta.json.
func (e *Engine) WriteArtifacts(repoPath string) error {
	// Load the published bundle once and read only from it. The bundle is immutable,
	// so serializing facts/insights/meta here is race-free even if another generate
	// publishes a newer bundle meanwhile.
	b := e.current.Load()
	if b.snapshot == nil {
		return fmt.Errorf("no snapshot generated")
	}

	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolving repo path: %w", err)
	}
	outDir := filepath.Join(absRepo, e.cfg.Output.Dir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	// Rotate the prior snapshot into previous/ before overwriting, so diff_snapshot
	// can compare against the immediately-preceding run with no explicit pin. The
	// pinned baseline/ (SetBaseline) is left untouched here.
	if err := rotatePrevious(outDir); err != nil {
		log.Printf("[engine] warning: could not rotate previous snapshot: %v", err)
	}

	// Hash every written artifact so the receipt records the exact output bytes
	// (the verifiable counterpart to the per-input-file FileHashes). meta.json and
	// receipt.json themselves are not hashed — they carry these hashes.
	outputHashes := make(map[string]string)

	// Write renderer artifacts (e.g. llm_context.md)
	for _, a := range b.snapshot.Artifacts {
		path := filepath.Join(outDir, a.Name)
		if err := os.WriteFile(path, a.Content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", a.Name, err)
		}
		outputHashes[a.Name] = hashBytes(a.Content)
		log.Printf("[engine] wrote %s (%d bytes)", path, len(a.Content))
	}

	// Write facts.jsonl, hashing the bytes as they go past rather than building the
	// whole file in memory to write and then hash it. On a kernel-sized graph that
	// buffer was 792 MiB, allocated at the end of a run and never released before the
	// process went back to idle.
	// A cluster writes the same published bundle to every repository's output
	// dir. The first write serializes; the rest copy the bytes of that file,
	// which is the same serialization by construction and a fraction of the
	// time (a 1.5M-fact union serializes in seconds, copies in a fraction of
	// one). The copy is keyed on the frozen store, which every generate replaces
	// and the receipt republish below keeps, so a new generate never copies a
	// stale file.
	factsPath := filepath.Join(outDir, "facts.jsonl")
	var factsHash string
	if e.lastFacts.store == b.store && e.lastFacts.path != factsPath && fileExists(e.lastFacts.path) {
		if err := copyFile(e.lastFacts.path, factsPath); err != nil {
			return fmt.Errorf("copying facts.jsonl: %w", err)
		}
		factsHash = e.lastFacts.digest
		log.Printf("[engine] wrote %s (copied from this run's first write)", factsPath)
	} else {
		var err error
		factsHash, err = writeFactsJSONL(b.store, factsPath)
		if err != nil {
			return err
		}
		e.lastFacts = lastFactsWrite{store: b.store, path: factsPath, digest: factsHash}
		log.Printf("[engine] wrote %s", factsPath)
	}
	outputHashes["facts.jsonl"] = factsHash

	// Write insights.json. A nil slice marshals to `null`, not `[]`, so a repository
	// with no findings produced a document that breaks any consumer iterating the
	// parsed value without a nil check — on exactly the repositories least likely to
	// be used while testing a consumer.
	insights := b.snapshot.Insights
	if insights == nil {
		insights = []facts.Insight{}
	}
	insightsJSON, err := b.store.MarshalInsights(insights)
	if err != nil {
		return fmt.Errorf("marshaling insights: %w", err)
	}
	insightsPath := filepath.Join(outDir, "insights.json")
	if err := os.WriteFile(insightsPath, insightsJSON, 0o644); err != nil {
		return fmt.Errorf("writing insights.json: %w", err)
	}
	outputHashes["insights.json"] = hashBytes(insightsJSON)
	log.Printf("[engine] wrote %s (%d bytes)", insightsPath, len(insightsJSON))

	// Record the output hashes on a COPY of the meta rather than mutating the
	// published (shared, immutable) snapshot in place. SnapshotMeta is a value type,
	// so this copy shares its slices but overrides only OutputHashes.
	// A cluster shares one union and its hashes across every repository dir; the
	// repository's own provenance (path, git, extractors, walk and parse counts) is
	// the turn that read it, not the turn that happened to be last.
	unionMeta := b.snapshot.Meta
	unionMeta.OutputHashes = outputHashes
	meta := unionMeta
	if turn, ok := b.members[absRepo]; ok {
		meta = withMemberProvenance(unionMeta, turn)
	}

	// Write snapshot.meta.json (the internal superset, incl. per-file hashes)
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling meta: %w", err)
	}
	metaPath := filepath.Join(outDir, "snapshot.meta.json")
	if err := os.WriteFile(metaPath, metaJSON, 0o644); err != nil {
		return fmt.Errorf("writing snapshot.meta.json: %w", err)
	}
	log.Printf("[engine] wrote %s (%d bytes)", metaPath, len(metaJSON))

	// Write receipt.json (the compact provenance + quality manifest)
	receiptJSON, err := json.MarshalIndent(meta.Receipt(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling receipt: %w", err)
	}
	receiptPath := filepath.Join(outDir, "receipt.json")
	if err := os.WriteFile(receiptPath, receiptJSON, 0o644); err != nil {
		return fmt.Errorf("writing receipt.json: %w", err)
	}
	log.Printf("[engine] wrote %s (%d bytes)", receiptPath, len(receiptJSON))

	// Republish so in-memory receipt tools (snapshot_receipt/compare_receipts) reflect
	// the output hashes, WITHOUT mutating the snapshot we just read. Only publish if
	// the bundle hasn't been superseded by a concurrent generate in the meantime —
	// generate-vs-generate is serialized (e.mu) and the server serializes the whole
	// generate handler (genMu), so under normal use this always succeeds; the guard
	// just avoids clobbering a newer snapshot in any unexpected interleaving.
	if e.current.Load() == b {
		snapCopy := *b.snapshot
		snapCopy.Meta = unionMeta
		e.current.CompareAndSwap(b, &snapshotBundle{store: b.store, snapshot: &snapCopy, repoPaths: b.repoPaths, intent: b.intent, members: b.members})
	}

	// Record this revision in the architecture history. Last, and non-fatal: it reads
	// previous/, which the rotation above has just filled, and a snapshot must never fail
	// because the log of snapshots could not be appended to.
	//
	// The history stores the fact lines, and they must be the exact canonical
	// serialization that went into facts.jsonl — a second serialization path could
	// diverge from the first. They used to be handed over as the buffer that had just
	// been written. Now that nothing buffers the file, they are read back from it: the
	// same requirement, satisfied by the strongest possible evidence, since the bytes
	// come from the artifact itself. It also costs nothing when history blobs are off,
	// which the previous arrangement could not manage.
	e.recordHistory(repoPath, meta, b, factsPath)

	return nil
}

type lastFactsWrite struct {
	store  *facts.Store
	path   string
	digest string
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// writeFactsJSONL serializes the store straight to path, returning the
// "sha256:"-prefixed digest of exactly the bytes written.
//
// The digest comes from a hash tee'd off the same stream rather than from a second
// pass over a buffer, so it cannot describe anything other than the file on disk.
// Durability is deliberately unchanged from the os.WriteFile this replaced: neither is
// atomic, and making facts.jsonl atomic is a separate decision from making it cheap.
func writeFactsJSONL(store *facts.Store, path string) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("creating facts.jsonl: %w", err)
	}
	h := sha256.New()
	bw := bufio.NewWriterSize(f, 1<<20)
	if err := store.WriteJSONL(io.MultiWriter(bw, h)); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("serializing facts.jsonl: %w", err)
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("writing facts.jsonl: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("writing facts.jsonl: %w", err)
	}
	return hashPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// hashBytes returns the "sha256:"-prefixed digest of b, used for output-artifact
// digests in the receipt (matching every other receipt hash's notation).
func hashBytes(b []byte) string {
	return sha256Prefixed(b)
}

// GetArtifact returns the content of a named artifact, or the generated JSONL/JSON files.
func (e *Engine) GetArtifact(name string) ([]byte, error) {
	b := e.current.Load()
	if b.snapshot == nil {
		return nil, fmt.Errorf("no snapshot generated")
	}

	switch name {
	case "facts.jsonl":
		var buf bytes.Buffer
		if err := b.store.WriteJSONL(&buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	case "insights.json":
		// The same nil guard WriteArtifacts applies. Without it this path served
		// `null` where the file said `[]`, so a repository with no findings
		// handed the MCP server and the dashboard a document that breaks any
		// consumer iterating the parsed value — and handed it a DIFFERENT
		// document from the one on disk.
		insights := b.snapshot.Insights
		if insights == nil {
			insights = []facts.Insight{}
		}
		return b.store.MarshalInsights(insights)
	case "snapshot.meta.json":
		return json.MarshalIndent(b.snapshot.Meta, "", "  ")
	case "receipt.json":
		return json.MarshalIndent(b.snapshot.Meta.Receipt(), "", "  ")
	default:
		for _, a := range b.snapshot.Artifacts {
			if a.Name == name {
				return a.Content, nil
			}
		}
		return nil, fmt.Errorf("artifact %q not found", name)
	}
}

// computeFileHashes computes SHA-256 hashes for all files (used in snapshot
// metadata). This stays sequential on purpose: hashing is I/O-bound and already
// fast (~0.25s on Airflow), and parallelizing it measurably regressed — many
// concurrent random reads contend worse than the sequential reads the OS
// prefetches. The extraction parsing, not hashing, is the bottleneck worth
// parallelizing.
func (e *Engine) computeFileHashes(repoPath string, files []string) map[string]string {
	hashes := make(map[string]string, len(files))
	for _, relFile := range files {
		absFile := filepath.Join(repoPath, relFile)
		data, err := os.ReadFile(absFile)
		if err != nil {
			continue
		}
		h := sha256.Sum256(data)
		hashes[relFile] = hex.EncodeToString(h[:])
	}
	return hashes
}

// fileModTime returns the modification time of a file as an RFC3339 string.
func fileModTime(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return info.ModTime().UTC().Format(time.RFC3339)
}

func fileHashesOf(absRepo string, hashes map[string]string) []facts.FileHash {
	out := make([]facts.FileHash, 0, len(hashes))
	for path, hash := range hashes {
		out = append(out, facts.FileHash{
			Path:    path,
			Hash:    hash,
			ModTime: fileModTime(filepath.Join(absRepo, path)),
		})
	}
	return out
}
