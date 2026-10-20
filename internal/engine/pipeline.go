package engine

// The stages of GenerateSnapshot. Each one reads and extends a snapshotRun, the
// state a single run carries from stage to stage; GenerateSnapshot itself is the
// list of stages in order. The order is a contract (a binder reads what every
// extractor emitted, an explainer reads a bound graph) and pipeline_order_test.go
// pins it.

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/pkg/plugin"
)

// snapshotRun is what one GenerateSnapshot knows, accumulated stage by stage.
type snapshotRun struct {
	ctx   context.Context
	start time.Time

	// Set by beginRun: which repository this is and what it declares.
	absRepo    string
	git        *facts.GitInfo
	repoLabel  string
	resolved   *intent.Declaration
	appendMode bool

	// Set by prepareStore: the published bundle this run replaces, and the store,
	// repo paths and declarations it is building to replace it with.
	prev      *snapshotBundle
	work      *facts.Store
	repoPaths map[string]string
	intent    map[string]*intent.Declaration

	// Set by walk.
	files, testFiles, allNames []string
	skips                      walkSkips
	hashes                     map[string]string

	// Set by extract. preCount is where this repository's facts begin in the store.
	preCount       int
	usedExtractors []string
	shadowed       []string
	parseErrs      []facts.ParseError
	provRecords    []facts.ProviderRecord
	progress       *extractionProgress

	// Set by buildSnapshot: how long the run took up to the point its snapshot
	// was assembled, which is the figure the snapshot records.
	duration time.Duration

	// Per-stage timing breakdown (logged at the end). Snapshotting is
	// extraction-dominated, so this makes it obvious where time goes.
	t struct{ walk, hash, extract, link, graph, explain, render time.Duration }
}

// beginRun identifies the repository: its absolute path, its git state, the label
// its facts will carry, and the intent it declares.
func (e *Engine) beginRun(ctx context.Context, repoPath string, appendMode bool) (*snapshotRun, error) {
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

	return &snapshotRun{
		ctx: ctx, start: start,
		absRepo: absRepo, git: git, repoLabel: repoLabel, resolved: resolved, appendMode: appendMode,
	}, nil
}

// prepareStore builds the store this run extracts into: empty, or in append mode
// the published union carried forward with this repository's prior slice removed.
// The published bundle is never written to.
func (e *Engine) prepareStore(r *snapshotRun) {
	// Build into a BRAND-NEW store off to the side. The currently-published bundle
	// (prev) is never mutated, so any in-flight reader keeps iterating a consistent,
	// frozen snapshot while we regenerate. We publish the new store atomically at the
	// end (see e.current.Store below), which is the single linearization point.
	r.prev = e.current.Load()
	r.work = facts.NewStore()
	// Declarations accumulate exactly like repoPaths: carried forward from the
	// published bundle in append mode, replaced wholesale otherwise, and handed
	// to the swap at the end. Nothing is written into the published map.
	r.intent = map[string]*intent.Declaration{}

	// A prior state produced by a different extraction behaviour cannot be
	// carried: its facts may be unlabeled (or labeled under different rules),
	// and the retroactive-tagging migration below would bulk-claim a stale
	// multi-repo union under one repo's label — manufacturing facts no repo's
	// source states. Discarding is the missing-edge-beats-wrong-one rule
	// applied to append mode; the discarded repos re-extract on their next
	// generate with the current behaviour.
	if r.appendMode && r.prev.snapshot != nil &&
		r.prev.snapshot.Meta.ExtractorVersion != cacheVersion {
		log.Printf("[engine] discarding prior state (extractor version %q != %q) — append would mislabel carried facts",
			r.prev.snapshot.Meta.ExtractorVersion, cacheVersion)
		r.appendMode = false
	}

	if r.appendMode {
		// Carry prior repos forward. All() returns an independent COPY, so mutating
		// `work` (TagUntagged/TagRange/SetRepoRange) can never touch prev.store, which
		// stays published and readable until we swap. This is the transient ~1x
		// fact-set memory cost of lock-free reads in append mode.
		r.repoPaths = make(map[string]string, len(r.prev.repoPaths)+1)
		for k, v := range r.prev.repoPaths {
			r.repoPaths[k] = v
		}
		for k, v := range r.prev.intent {
			r.intent[k] = v
		}
		if r.prev.store != nil {
			r.work.Add(r.prev.store.All()...)
			r.work.CarryDocTexts(r.prev.store, func(k facts.FactKey) bool { return k.Repo != r.repoLabel })
		}

		// A repository the union already holds is replaced, not doubled. Its
		// prior slice goes before extraction, so what this turn adds is the only
		// account of it; the linkers then rebuild every cross-repo edge into it
		// from scratch, as they do on every append. This is what lets one repo
		// be re-read into an existing union without re-reading the other twenty.
		if _, present := r.prev.repoPaths[r.repoLabel]; present {
			if dropped := r.work.RemoveWhere(func(f facts.Fact) bool { return f.Repo == r.repoLabel }); dropped > 0 {
				log.Printf("[engine] replacing %q in the union: dropped its %d prior facts", r.repoLabel, dropped)
			}
		}

		// Track repo label -> absolute path for multi-repo resolution.
		r.repoPaths[r.repoLabel] = r.absRepo
	}
	if r.resolved != nil {
		r.intent[r.repoLabel] = r.resolved
	} else {
		// A repo that stopped declaring must stop being declared, including when
		// its entry was carried forward from the previous bundle.
		delete(r.intent, r.repoLabel)
	}
	if r.appendMode {

		// Retroactively tag facts from a prior single-repo snapshot so they
		// are filterable by repo alongside the newly appended facts.
		if r.prev.snapshot != nil && r.work.Count() > 0 {
			// The label those facts actually carry, which since RepoLabel came from the
			// remote is no longer the directory name. Older snapshots recorded none, and
			// for them the directory IS what tagged the facts.
			prevLabel := r.prev.snapshot.Meta.RepoLabel
			if prevLabel == "" {
				prevLabel = filepath.Base(r.prev.snapshot.Meta.RepoPath)
			}
			if _, alreadyTracked := r.repoPaths[prevLabel]; !alreadyTracked {
				tagged := r.work.TagUntagged(prevLabel, prevLabel+"/")
				if tagged > 0 {
					r.repoPaths[prevLabel] = r.prev.snapshot.Meta.RepoPath
					log.Printf("[engine] retroactively tagged %d existing facts with repo label %q", tagged, prevLabel)
				}
			}
		}
	}
	// Non-append: `work` stays empty and workRepoPaths stays nil — the prior bundle
	// is left intact for in-flight readers until the swap (no in-place Clear()).

	// Point the build-scratch store at `work` so the pipeline helpers (which read
	// e.store under e.mu) operate on the new store with no signature changes.
	e.store = r.work
}

// walk lists the repository's files and hashes them.
func (e *Engine) walk(r *snapshotRun) error {
	// 1. Walk repository and collect files
	tStage := time.Now()
	var err error
	r.files, r.testFiles, r.allNames, r.skips, err = e.walkRepo(r.ctx, r.absRepo)
	if err != nil {
		return fmt.Errorf("walking repo: %w", err)
	}
	r.t.walk = time.Since(tStage)
	log.Printf("[engine] found %d files (%d test files, %d skipped) in %s", len(r.files), len(r.testFiles), r.skips.count, r.absRepo)

	// 2. Compute file hashes (for snapshot metadata)
	tStage = time.Now()
	r.hashes = e.computeFileHashes(r.ctx, r.absRepo, r.files)
	r.t.hash = time.Since(tStage)
	if err := r.ctx.Err(); err != nil {
		return err
	}
	return nil
}

// extract runs everything that adds this repository's facts: the extractors, the
// test-reference extractors, the compiled intent and the providers. It ends with
// those facts labeled, and in append mode path-prefixed.
func (e *Engine) extract(r *snapshotRun) error {
	tStage := time.Now()
	// 3. Detect and run extractors (with optional per-extractor caching).
	var cache *extractorCache
	cachePath := extractorCachePath(filepath.Join(r.absRepo, e.cfg.Output.Dir))
	if e.cfg.IncrementalEnabled() {
		cache = loadExtractorCache(cachePath, e.persistCache)
		// The cache now holds an open temp file from the moment it is created, so
		// every path out of this function has to close it. discard is a no-op after
		// a successful save; without it, a snapshot that fails during extraction
		// leaves the spool behind.
		defer cache.discard()
	}
	r.preCount = e.store.Count()
	r.progress = newExtractionProgress(r.ctx, r.absRepo, r.files, e.stagesAfterExtraction())
	var err error
	r.usedExtractors, r.shadowed, r.parseErrs, err = e.runExtractors(r.ctx, r.absRepo, r.files, r.allNames, r.hashes, cache, r.progress)
	if err != nil {
		return fmt.Errorf("extraction: %w", err)
	}
	r.progress.extractionDone()
	r.parseErrs = append(r.parseErrs, filesReadInPart(e.store.FactsRef()[r.preCount:])...)
	e.reportShadowed(r.absRepo, r.shadowed)
	// Reference-only extraction over test/spec files. Runs every snapshot (not
	// cached with the main extractors) and adds only KindTestRef facts, so a
	// production symbol exercised solely by a test is not mis-reported as dead.
	e.runTestRefExtractors(r.ctx, r.absRepo, r.testFiles, r.files, r.allNames)

	// Compile this repo's resolved intent declaration into intent facts, inside
	// the extraction window so SetRepoRange/TagRange treat declared facts
	// exactly as measured ones — snapshots carry them, diffs track them, and
	// the intent explainer reads them with no side channel.
	if r.resolved != nil {
		e.store.Add(intent.CompileFacts(r.resolved)...)
	}

	// Run configured external providers, still inside the extraction window so
	// their facts are repo-labeled (and prefixed, in append mode) exactly as
	// measured ones — and before linking, so a provider's edges participate in
	// the graph every explainer walks. Providers merge additively: an identity
	// an extractor already owns is never overwritten, so the census can say a
	// provider contributed less than it emitted but the graph cannot silently
	// change authorship.
	r.provRecords = e.runProviders(r.ctx, r.absRepo, r.preCount, providerInput(r.files, r.testFiles, r.hashes, e.computeFileHashes(r.ctx, r.absRepo, r.testFiles), cache))
	r.progress.stage("providers")
	if cache != nil {
		log.Printf("[engine] extractor cache: %d reused", cache.hits)
		// Saved after the providers, whose entries share the spool. save is a no-op
		// when this cache was opened non-persisting, and it creates its own
		// directory, so neither condition is repeated here.
		if err := cache.save(); err != nil {
			log.Printf("[engine] could not write extractor cache: %v", err)
		}
	}

	r.t.extract = time.Since(tStage)
	newCount := e.store.Count()
	log.Printf("[engine] extracted %d facts using %d extractors", newCount, len(r.usedExtractors))

	// Flag facts from codegen output before the file paths are repo-prefixed below,
	// while f.File is still resolvable against repoPath.
	e.markGeneratedFacts(r.absRepo, r.preCount)

	// Always set Repo on newly extracted facts so the repo filter works
	// even in single-repo mode.
	e.store.SetRepoRange(r.preCount, r.repoLabel)

	// In append mode, additionally prefix file paths so facts from
	// different repos are distinguishable by file path.
	if r.appendMode {
		e.store.TagRange(r.preCount, r.repoLabel, r.repoLabel+"/")
		log.Printf("[engine] prefixed %d facts with repo label %q", newCount-r.preCount, r.repoLabel)
	}

	// The full doc texts leave the facts here, now that their keys are final. See
	// facts.PropDocText.
	e.store.TakeDocTexts(r.preCount)
	return nil
}

// stagesAfterExtraction is how many stages a run reports once extraction is
// over: the providers, and unless linking is deferred to a later turn of the
// cluster, linking, the explainers and the renderers. It is announced with the
// walk so the total a client sees does not move as the run goes on.
func (e *Engine) stagesAfterExtraction() int {
	if e.deferLinking {
		return 1
	}
	return 4
}

// publishDeferred is where a cluster's intermediate turns stop. Linking, the graph and the
// explainers are recomputed from scratch on every append and only the last
// turn's result is kept, so running them after each of twenty-two
// repositories was twenty-one full passes over a growing union for nothing
// anyone read. The caller that walks a cluster sets deferLinking for every
// turn but the last; that turn runs the full pipeline once over the whole
// union. The bundle published here carries the store, the repo paths and
// the intent the next turn needs, and a meta that says what was extracted.
func (e *Engine) publishDeferred(r *snapshotRun) *facts.Snapshot {
	r.work.Freeze()
	duration := time.Since(r.start)
	deferredPrefix := ""
	if r.appendMode {
		deferredPrefix = r.repoLabel + "/"
	}
	snapshot := &facts.Snapshot{
		Meta: facts.SnapshotMeta{
			RepoPath:           r.absRepo,
			GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
			Duration:           duration.String(),
			Extractors:         r.usedExtractors,
			Renderers:          []string{},
			FileHashes:         fileHashesOf(r.absRepo, r.hashes),
			FactCount:          e.store.Count(),
			EnolaVersion:       version.Version,
			ExtractorVersion:   cacheVersion,
			Git:                r.git,
			RepoLabel:          r.repoLabel,
			Providers:          r.provRecords,
			FilesSeen:          len(r.files),
			FilesParsed:        e.store.CountFilesWithFacts(r.files, deferredPrefix),
			SourceBytes:        e.store.SourceBytesWithFacts(r.files, deferredPrefix, r.absRepo),
			FilesSkipped:       r.skips.count,
			DirsSkipped:        r.skips.dirCount,
			SkippedSample:      r.skips.sample,
			ShadowedExtractors: r.shadowed,
			ParseErrors:        len(r.parseErrs),
			ParseErrorSample:   capParseErrors(r.parseErrs),
			Census:             e.fileCensus(r.files, deferredPrefix, r.skips, r.usedExtractors, r.parseErrs),
		},
		Facts: e.store.FactsRef(),
	}
	e.current.Store(&snapshotBundle{store: r.work, snapshot: snapshot, repoPaths: r.repoPaths, intent: r.intent,
		members: memberMetas(r.prev, r.appendMode, r.absRepo, snapshot.Meta)})
	log.Printf("[engine] %s extracted into the union in %s (%d facts so far); linking and explaining deferred to the cluster's last turn",
		r.repoLabel, duration.Round(time.Millisecond), e.store.Count())
	log.Printf("[engine] timings: walk=%s hash=%s extract=%s", r.t.walk.Round(time.Millisecond), r.t.hash.Round(time.Millisecond), r.t.extract.Round(time.Millisecond))
	return snapshot
}

// link binds and links the extracted facts and builds the graph index over them.
func (e *Engine) link(r *snapshotRun) {
	// 3b. Link repos into a cross-repo "graph of graphs": derive service-level
	// nodes and consumer→provider edges from HTTP route role matching and
	// import/shared-lib references. Recomputed from scratch each run (prior
	// synthetic facts are dropped first) so it stays idempotent across appends.
	tStage := time.Now()
	e.runBinders(r.ctx, plugin.StagePreLink)
	e.linkCrossRepo(r.repoPaths)
	e.runBinders(r.ctx, plugin.StagePostLink)
	r.t.link = time.Since(tStage)

	// 3c. Build graph index for traversal queries
	tStage = time.Now()
	e.store.BuildGraph()
	r.t.graph = time.Since(tStage)
	log.Printf("[engine] built graph index (%d nodes, %d edges)", e.store.Graph().NodeCount(), e.store.Graph().EdgeCount())
	r.progress.stage("linking")
}

// explain runs the explainers over the bound graph.
func (e *Engine) explain(r *snapshotRun) ([]facts.Insight, []string, error) {
	// 4. Run explainers
	tStage := time.Now()
	allInsights, usedExplainers, err := e.runExplainers(r.ctx, r.allNames)
	if err != nil {
		return nil, nil, fmt.Errorf("explanation: %w", err)
	}
	positionInsights(allInsights, e.store)
	r.t.explain = time.Since(tStage)
	log.Printf("[engine] produced %d insights using %d explainers", len(allInsights), len(usedExplainers))
	r.progress.stage("explainers")
	return allInsights, usedExplainers, nil
}

// buildSnapshot assembles the snapshot and its metadata, the receipt included.
func (e *Engine) buildSnapshot(r *snapshotRun, allInsights []facts.Insight, usedExplainers []string) (*facts.Snapshot, error) {
	// 5. Build file hashes for the snapshot meta
	fileHashes := fileHashesOf(r.absRepo, r.hashes)

	// 6. Build snapshot.
	//
	// Receipt fields: the snapshot ID is a content fingerprint over the same
	// byte-stable serialization that becomes facts.jsonl, so it is stable across
	// reruns on identical inputs and keys snapshot equivalence. The extraction
	// -quality fields (files seen/parsed/skipped, parse errors, coverage) let a
	// consumer judge how complete this extraction was before trusting it.
	duration := time.Since(r.start)
	r.duration = duration
	ignoreGlobHash := computeIgnoreGlobHash(e.cfg)
	configHash := computeConfigHash(e.cfg)
	// In append mode this run's facts were path-prefixed with the repo label, so
	// match the walked files against that same prefix to count parsing coverage.
	parsedPrefix := ""
	if r.appendMode {
		parsedPrefix = r.repoLabel + "/"
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
			RepoPath:     r.absRepo,
			GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
			Duration:     duration.String(),
			Extractors:   r.usedExtractors,
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
			Git:              r.git,
			ConfigHash:       configHash,
			// The label the facts in this snapshot actually carry. Recorded rather than
			// re-derived on read: a baseline outlives the build that wrote it, and a
			// reader that recomputed the label would apply TODAY's rule to YESTERDAY's
			// facts and conclude they match when their keys do not.
			RepoLabel: r.repoLabel,

			Providers: r.provRecords,

			FilesSeen:          len(r.files),
			FilesParsed:        e.store.CountFilesWithFacts(r.files, parsedPrefix),
			SourceBytes:        e.store.SourceBytesWithFacts(r.files, parsedPrefix, r.absRepo),
			FilesSkipped:       r.skips.count,
			DirsSkipped:        r.skips.dirCount,
			SkippedSample:      r.skips.sample,
			IgnoreGlobHash:     ignoreGlobHash,
			ShadowedExtractors: r.shadowed,
			ParseErrors:        len(r.parseErrs),
			ParseErrorSample:   capParseErrors(r.parseErrs),
			HeuristicInsights:  countHeuristicInsights(allInsights),
			Coverage:           coverageSummary(e.store),
			Census:             e.fileCensus(r.files, parsedPrefix, r.skips, r.usedExtractors, r.parseErrs),
			Unseen:             e.unseenCensus(r.skips, r.provRecords, allInsights),
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
	return snapshot, nil
}

// render runs the renderers over the assembled snapshot.
func (e *Engine) render(r *snapshotRun, snapshot *facts.Snapshot) error {
	// 7. Run renderers
	tStage := time.Now()
	usedRenderers, err := e.runRenderers(r.ctx, snapshot)
	if err != nil {
		return fmt.Errorf("rendering: %w", err)
	}
	r.t.render = time.Since(tStage)
	snapshot.Meta.Renderers = usedRenderers
	log.Printf("[engine] produced %d artifacts using %d renderers", len(snapshot.Artifacts), len(usedRenderers))
	r.progress.stage("renderers")
	return nil
}

// publish freezes the store and swaps the new bundle in for readers.
func (e *Engine) publish(r *snapshotRun, snapshot *facts.Snapshot) {
	// Every writer has now run: extraction, linking, binders, explainers, renderers.
	// Collapse the structurally identical Props maps and Relations slices the
	// extractors emitted independently onto shared instances before the store becomes
	// visible. This must happen HERE — after the last mutation and before the
	// publication below — because sharing is sound exactly for a store nothing writes
	// again. Append mode carries these facts into a future mutable store via
	// Store.All, which copies them back apart.
	tStage := time.Now()
	r.work.Freeze()
	log.Printf("[engine] froze fact store in %s", time.Since(tStage).Round(time.Millisecond))

	// Publish atomically. This single Store() is the linearization point: before it
	// readers see the prior bundle, after it the new one — never a half-built store.
	e.current.Store(&snapshotBundle{store: r.work, snapshot: snapshot, repoPaths: r.repoPaths, intent: r.intent,
		members: memberMetas(r.prev, r.appendMode, r.absRepo, snapshot.Meta)})
	log.Printf("[engine] snapshot generated in %s", r.duration)
	log.Printf("[engine] timings: walk=%s hash=%s extract=%s link=%s graph=%s explain=%s render=%s",
		r.t.walk.Round(time.Millisecond), r.t.hash.Round(time.Millisecond), r.t.extract.Round(time.Millisecond),
		r.t.link.Round(time.Millisecond), r.t.graph.Round(time.Millisecond), r.t.explain.Round(time.Millisecond),
		r.t.render.Round(time.Millisecond))

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
}
