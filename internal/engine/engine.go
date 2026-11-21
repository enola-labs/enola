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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/explainers"
	"github.com/enola-labs/enola/internal/extractors"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/internal/linkers/binders"
	"github.com/enola-labs/enola/internal/linkers/crossrepo"
	"github.com/enola-labs/enola/internal/linkers/crossrepo/signals"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/internal/pathglob"
	"github.com/enola-labs/enola/internal/providers"
	"github.com/enola-labs/enola/internal/renderers"
	"github.com/enola-labs/enola/internal/workspace"
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
	// cluster's directory carries each member's provenance beside the union
	// (members.json).
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
	// drift is what the drift walk last hashed, so a long-lived engine reads a file
	// again only after it changed; see driftHashes.
	drift driftMemo
	// sessionID is the served session this engine serves; see SetSessionID.
	sessionID atomic.Pointer[string]
	// sharedDrift, when set, replaces drift; see UseDriftMemo.
	sharedDrift *driftMemo
	// building is the union a GenerateCluster has extracted so far and not yet
	// published: each turn but the last appends into its store, and the next turn
	// extends it rather than the published bundle. Nil outside GenerateCluster.
	building *snapshotBundle
	// restoreDocTexts makes RestoreFromDir read docs.jsonl too; see SetRestoreDocTexts.
	restoreDocTexts bool
	// recordSnapshot is called at the end of WriteArtifacts; see SetSnapshotRecorder.
	recordSnapshot func(SnapshotRecord)
	// sessionClient is the PID of the agent process this server serves; zero for a
	// CLI run. See SetSessionClient.
	sessionClient atomic.Int64
	// pathGuard, when set, approves every repository a run reads; see SetPathGuard.
	pathGuard func(abs string) error
	// noGlobalReceipt turns WriteGlobalReceipt into a no-op; see SetGlobalReceipt.
	noGlobalReceipt bool
}

// SetPathGuard makes every snapshot run ask guard before it reads a repository. A
// server that serves other processes confines them to the directories its operator
// named, and this is the one place every repository a run reads passes through:
// one named by a tool argument, a member of a cluster config found inside it, the
// configured default. Nil, the default, approves everything. Set it before serving.
func (e *Engine) SetPathGuard(guard func(abs string) error) { e.pathGuard = guard }

// SetGlobalReceipt turns writing ~/.enola/receipt.json on or off. That receipt
// tracks the one workspace an agent on this machine worked in last; a server that
// serves many consumers has no such workspace and must not overwrite the one an agent
// relies on. On by default. Set it before serving.
func (e *Engine) SetGlobalReceipt(on bool) { e.noGlobalReceipt = !on }

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

// SetPersistCache controls whether the per-extractor cache is written to disk
// after a snapshot. One-shot --explain disables this so it leaves .enola
// untouched, while still reusing a cache a prior --generate may have written.
func (e *Engine) SetPersistCache(persist bool) { e.persistCache = persist }

// RegisterExtractor adds an extractor to the engine. An extractor that reads client
// specs is handed the ones declared for its language here, so it never reads the
// config itself, and an extractor a wrapper registers is treated the same way.
func (e *Engine) RegisterExtractor(ext extractors.Extractor) {
	if c, ok := ext.(clientspec.Consumer); ok {
		c.SetClientSpecs(clientspec.ForLanguage(e.cfg.Clients, ext.Name()))
	}
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
	// The three files are opened together, under the lock a write to dir holds, and
	// read through those handles after. A write renames new files over these rather
	// than writing through them, so what is read is one write's facts, insights and
	// meta, even when a write to dir runs while the facts are read (a pool serving one
	// repository alone and in a cluster).
	unlock := lockDir(dir)
	factsFile, err := os.Open(factsPath)
	if err != nil {
		unlock()
		return fmt.Errorf("no snapshot at %s: %w", dir, err)
	}
	defer func() { _ = factsFile.Close() }()
	insightsFile, _ := os.Open(filepath.Join(dir, "insights.json"))
	metaFile, _ := os.Open(filepath.Join(dir, "snapshot.meta.json"))
	membersFile, _ := os.Open(filepath.Join(dir, MembersMetaFile))
	var docsFile *os.File
	if e.restoreDocTexts {
		docsFile, _ = os.Open(filepath.Join(dir, docsArtifact))
	}
	unlock()
	for _, f := range []*os.File{insightsFile, metaFile, membersFile, docsFile} {
		if f != nil {
			defer func() { _ = f.Close() }()
		}
	}
	insightsData, insightsErr := readOpen(insightsFile)
	metaData, metaErr := readOpen(metaFile)
	// The files opened must be one write's: a reader in another process can open them
	// while a write publishes the next graph. The meta records the hash of each file it
	// was written with, so a facts.jsonl or insights.json it does not describe is
	// refused before anything is parsed.
	if metaErr == nil {
		var recorded struct {
			OutputHashes map[string]string `json:"output_hashes"`
		}
		if json.Unmarshal(metaData, &recorded) == nil {
			if want := recorded.OutputHashes["facts.jsonl"]; want != "" {
				if got, err := hashOpenFile(factsFile); err != nil || got != want {
					return fmt.Errorf("%w: %s", ErrGraphChanging, dir)
				}
			}
			if want := recorded.OutputHashes["insights.json"]; want != "" && insightsErr == nil && hashBytes(insightsData) != want {
				return fmt.Errorf("%w: %s", ErrGraphChanging, dir)
			}
			if want := recorded.OutputHashes[docsArtifact]; want != "" && docsFile != nil {
				if got, err := hashOpenFile(docsFile); err != nil || got != want {
					return fmt.Errorf("%w: %s", ErrGraphChanging, dir)
				}
			}
		}
	}

	// Shared as it is read (see ReadJSONLFileShared), so from here until Freeze below
	// nothing may write a fact's Props or Relations: the steps between label facts and
	// build indexes beside them, and must keep to that.
	work := facts.NewStore()
	if err := work.ReadJSONLShared(factsFile); err != nil {
		return fmt.Errorf("reading facts from %s: %w", factsPath, err)
	}
	if singleRepoLabel != "" {
		// Tags only facts whose Repo is empty, so a pre-tagged file is left intact.
		work.SetRepoRange(0, singleRepoLabel)
	} else {
		work.MarkLegacyRepoPrefixes()
	}
	// Indexes a binder derives beside the facts are not in facts.jsonl; rebuild them
	// before the graph reads them, or a restored graph would lose the cross-repo
	// edges they resolve. Only index binders run: they change no fact.
	for _, b := range e.binders.All() {
		if ib, ok := b.(plugin.IndexBinder); ok {
			if err := ib.Bind(context.Background(), work); err != nil {
				log.Printf("[engine] restore: rebuilding the %s index failed: %v", ib.Name(), err)
			}
		}
	}
	work.BuildGraph()
	if docsFile != nil {
		if err := readDocTexts(work, docsFile); err != nil {
			return fmt.Errorf("reading %s: %w", filepath.Join(dir, docsArtifact), err)
		}
	}

	// Default the primary repo path from the dir; snapshot.meta.json (loaded below)
	// overrides it when present.
	snap := &facts.Snapshot{Meta: facts.SnapshotMeta{RepoPath: filepath.Dir(dir)}}
	if insightsErr == nil {
		var ins []facts.Insight
		if err := json.Unmarshal(insightsData, &ins); err == nil {
			snap.Insights = ins
		}
	}
	if metaErr == nil {
		var meta facts.SnapshotMeta
		if err := json.Unmarshal(metaData, &meta); err == nil {
			snap.Meta = meta
		}
	}
	// A cluster's directory carries each member's own provenance. Restored with the
	// facts, so a refresh of one member writes every member's back, and drift is
	// measured per member, as after a generate.
	var members map[string]facts.SnapshotMeta
	if data, err := readOpen(membersFile); err == nil {
		var list []facts.SnapshotMeta
		if err := json.Unmarshal(data, &list); err == nil {
			members = make(map[string]facts.SnapshotMeta, len(list))
			for _, m := range list {
				members[m.RepoPath] = m
			}
		}
	}
	// Same as the generation path: nothing writes this store again. The read already
	// shared the duplicate Props maps and Relations slices the snapshot was serialized
	// with; Freeze marks the store published. A restored graph is the long-lived case
	// this matters most for — the server holds it for hours without regenerating.
	work.Freeze()

	// FactsRef aliases the store's slice (never copied): `work` is published here and
	// then never mutated again, so a reader iterating snap.Facts sees a frozen array.
	snap.Facts = work.FactsRef()

	// intent is deliberately nil: a restore re-reads facts, not declaration files,
	// so the parsed declarations are not known here. The compiled intent FACTS are
	// in the restored store, which is what the explainer verdicts from; Intent()
	// reports nothing until the next generate re-reads the files.
	e.current.Store(&snapshotBundle{store: work, snapshot: snap, repoPaths: repoPaths, members: members})
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
	// join its repository-relative path with the absolute root.
	if f.Repo != "" && b.repoPaths != nil {
		if absRoot, ok := b.repoPaths[f.Repo]; ok {
			return filepath.Join(absRoot, f.IdentityFile())
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
	return e.generate(ctx, repoPath, appendMode, false)
}

// RepoError is a GenerateCluster failure: the member that failed, and why.
type RepoError struct {
	Repo string
	Err  error
}

func (e *RepoError) Error() string {
	return fmt.Sprintf("snapshot generation failed for %s: %v", e.Repo, e.Err)
}
func (e *RepoError) Unwrap() error { return e.Err }

// GenerateCluster indexes repoPaths as one linked graph and publishes it once.
//
// Each repository is extracted into one store that the run keeps to itself. Linking,
// the graph, the explainers and the renderers run once, on the last repository, over
// the whole union. Nothing is published before that: readers keep the graph they had
// until the complete union replaces it in one swap, and a failure partway leaves that
// graph published. It used to publish after every repository and copy the whole
// union into a fresh store at the start of the next, which made a cluster quadratic
// in its size and showed readers unlinked partial unions while it ran.
//
// The repositories are extracted several at a time (Config.ClusterJobsFor), each into
// a store of its own, and taken into the union in the order given; see
// cluster_prefetch.go.
//
// appendFirst extends the published graph with the first repository instead of
// starting over. The returned snapshots are one per repository, in order; the last
// is the union. A failure is a *RepoError naming the repository.
func (e *Engine) GenerateCluster(ctx context.Context, repoPaths []string, appendFirst bool) ([]*facts.Snapshot, error) {
	if len(repoPaths) == 0 {
		return nil, errors.New("no repositories to index")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() { e.building = nil }()
	pre := e.startPrefetch(ctx, repoPaths, appendFirst)
	defer pre.stop()
	snaps := make([]*facts.Snapshot, 0, len(repoPaths))
	for i, repo := range repoPaths {
		snap, err := e.generateTurn(ctx, repo, appendFirst || i > 0, i < len(repoPaths)-1, pre.take(i))
		if err != nil {
			return nil, &RepoError{Repo: repo, Err: err}
		}
		snaps = append(snaps, snap)
	}
	return snaps, nil
}

// generate is one turn of the pipeline. deferred stops it after extraction and
// holds the result for the next turn; see holdDeferred. The caller holds e.mu.
func (e *Engine) generate(ctx context.Context, repoPath string, appendMode, deferred bool) (*facts.Snapshot, error) {
	return e.generateTurn(ctx, repoPath, appendMode, deferred, nil)
}

// generateTurn is generate for a turn whose repository may have been extracted
// ahead, in pf; see adoptPrefetched.
func (e *Engine) generateTurn(ctx context.Context, repoPath string, appendMode, deferred bool, pf *prefetched) (*facts.Snapshot, error) {
	var r *snapshotRun
	var err error
	if pf != nil && pf.err == nil {
		r, err = e.beginRunAs(ctx, pf.id, appendMode, pf.run.start)
	} else {
		r, err = e.beginRun(ctx, repoPath, appendMode)
	}
	if err != nil {
		return nil, err
	}
	r.deferred = deferred
	e.prepareStore(r)

	if !e.adoptPrefetched(r, pf) {
		if err := e.walk(r); err != nil {
			return nil, err
		}
		if err := e.extract(r); err != nil {
			return nil, err
		}
	}
	if r.deferred {
		return e.holdDeferred(r), nil
	}

	e.link(r)
	insights, usedExplainers, err := e.explain(r)
	if err != nil {
		return nil, err
	}
	snapshot, err := e.buildSnapshot(r, insights, usedExplainers)
	if err != nil {
		return nil, err
	}
	if err := e.render(r, snapshot); err != nil {
		return nil, err
	}
	e.publish(r, snapshot)
	return snapshot, nil
}

// runProviders executes the configured external fact providers against this
// repository and merges their accepted facts into the build-scratch store,
// returning the census for the snapshot receipt. preCount marks where this
// repo's extraction window began: everything the extractors (and the intent
// compiler) added since is the identity set a provider fact may not collide
// with — the extractor's account of a name+kind wins, always. The caller runs it only
// with providers configured.
func (e *Engine) runProviders(ctx context.Context, absRepo string, preCount int, in providers.Input) []facts.ProviderRecord {
	extracted := e.store.FactsRef()[preCount:]
	owned := make(map[string]bool, len(extracted))
	for _, f := range extracted {
		owned[f.Kind+"\x00"+f.Name] = true
	}
	in.Providers = e.cfg.Providers
	in.RepoPath = absRepo
	in.Taken = func(kind, name string) bool { return owned[kind+"\x00"+name] }
	// Compiled once for the whole provider run: this closure is asked about every
	// file a provider considers, and the bundled config ships 114 ignore patterns.
	provIgnore := pathglob.Compile(e.cfg.Ignore)
	provPkgs := newPyPackageExemption(absRepo, e.cfg.Ignore)
	in.Ignored = func(file string) bool {
		rel := filepath.ToSlash(file)
		pattern, ok := provIgnore.Match(rel)
		return ok && !provPkgs.exempt(rel, pattern, false)
	}
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
// silently comparing nothing.
//
// It keeps no state, so the linker may call it from several goroutines at once. The
// shared-code signal remembers what it derives from each file, so each is still read at
// most once per link.
func sourceReaderFor(repoPaths map[string]string) crossrepo.SourceReader {
	if len(repoPaths) == 0 {
		return nil
	}
	return func(f facts.Fact) (string, bool) {
		if f.File == "" || f.Repo == "" {
			return "", false
		}
		root, ok := repoPaths[f.Repo]
		if !ok {
			return "", false
		}
		data, err := os.ReadFile(filepath.Join(root, f.IdentityFile()))
		if err != nil {
			return "", false
		}
		return string(data), true
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
		return f.PropAny("synthetic") == crossrepo.SyntheticMarker
	})

	// A cross-repo edge needs two repos, and ComputeLinks says so itself — but only
	// after newInput has walked the copy it was handed. Asking the store first makes
	// the single-repo case, which is most snapshots, free: RepoLabels is O(repos) off
	// the byRepo index, where newInput walks every fact only to count the repos.
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
	// The store's own facts, not a copy: the linker and its signals only read them, and
	// nothing writes the store until it returns. A deep copy here was 5 GB, a third of
	// the live heap, on a 100-repository cluster.
	links := crossrepo.ComputeLinks(e.store.FactsRef(), sourceReaderFor(repoPaths), e.signals.All(), v, e.analysisJobs())
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
// ctx is checked at every entry: pointed at a folder of many repositories by mistake, the
// walk alone visits hundreds of thousands of files, and a cancelled request must stop it
// rather than hold the snapshot lock until it finishes.
func (e *Engine) walkRepo(ctx context.Context, repoPath string) (files, testFiles, allNames []string, skips walkSkips, err error) {
	// A symlinked repo root walks as a single non-directory entry (WalkDir
	// Lstats the root), silently yielding zero files. Resolve the ROOT only —
	// symlinks inside the tree keep their non-followed semantics — so a config
	// may alias a checkout (a stable clone under the repo's own label) and
	// still extract. The label was already derived from the unresolved path.
	if resolved, rerr := filepath.EvalSymlinks(repoPath); rerr == nil && resolved != repoPath {
		repoPath = resolved
	}
	// Both lists are asked about every entry the walk visits, so their shapes are
	// resolved once here rather than re-derived per entry. pathglob.Compile answers
	// exactly as matchGlob does; internal/facts proves the two agree.
	ignoreSet := pathglob.Compile(e.cfg.Ignore)
	testGlobSet := pathglob.Compile(e.cfg.TestGlobs)
	pkgs := newPyPackageExemption(repoPath, e.cfg.Ignore)
	err = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
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

		// Another checkout of a repository nested inside this one is not this
		// repository's source, whatever the ignore list says (workspace.IsLinkedWorktree).
		if d.IsDir() && path != repoPath && workspace.IsLinkedWorktree(path) {
			log.Printf("[engine] skipping %s/: a linked git worktree, not this checkout's source", relPath)
			return filepath.SkipDir
		}

		// Skip ignored paths
		if pattern, ok := ignoreSet.Match(relPath); ok && !pkgs.exempt(relPath, pattern, d.IsDir()) {
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
			if testGlobSet.MatchAny(relPath) {
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

// runExtractors detects applicable extractors and runs them. When cache is
// non-nil, extractors implementing plugin.FileOwner have their facts reused
// whenever the files they depend on are unchanged since the last snapshot.
//
// It also reports the SHADOWED extractors: those registered and applicable to this
// repository, but excluded by an explicit `extractors:` list. See reportShadowed.
func (e *Engine) runExtractors(ctx context.Context, repoPath string, files, allNames []string, hashes map[string]string, cache *extractorCache, progress *extractionProgress) ([]string, []string, []facts.ParseError, error) {
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
				if cached, hit := cache.getProbed(key, repoPath); hit {
					e.store.Add(cached...)
					usedNames = append(usedNames, ext.Name())
					log.Printf("[engine] extractor %s: reused %d cached facts", ext.Name(), len(cached))
					progress.finished(ext)
					continue
				}
			}
		}

		log.Printf("[engine] running extractor: %s", ext.Name())
		tExt := time.Now()
		extCtx, probes := plugin.WithDiskProbes(ctx)
		extracted, err := ext.Extract(extCtx, repoPath, files)
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
				cache.putProbed(key, repoPath, probes.Paths(), probes.Reads(), extracted)
			}
		}

		e.store.Add(extracted...)
		usedNames = append(usedNames, ext.Name())
		log.Printf("[engine] extractor %s: emitted %d facts in %s", ext.Name(), len(extracted), time.Since(tExt).Round(time.Millisecond))
		progress.finished(ext)
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

// runRenderers runs all enabled renderers.
func (e *Engine) runRenderers(ctx context.Context, snapshot *facts.Snapshot) ([]string, error) {
	var usedNames []string

	for _, rnd := range e.renderers.All() {
		if !e.cfg.IsRendererEnabled(rnd.Name()) {
			continue
		}

		log.Printf("[engine] running renderer: %s", rnd.Name())
		// A renderer that follows edges reads the store; see plugin.StoreRenderer.
		var artifacts []facts.Artifact
		var err error
		if sr, ok := rnd.(plugin.StoreRenderer); ok {
			artifacts, err = sr.RenderStore(ctx, snapshot, e.store)
		} else {
			artifacts, err = rnd.Render(ctx, snapshot)
		}
		if err != nil {
			log.Printf("[engine] renderer %s error: %v", rnd.Name(), err)
			continue
		}

		snapshot.Artifacts = append(snapshot.Artifacts, artifacts...)
		usedNames = append(usedNames, rnd.Name())
	}

	return usedNames, nil
}

// WriteArtifacts writes the published graph's artifacts: for one repository into its
// output directory, including facts.jsonl, insights.json, and snapshot.meta.json. A
// graph of several repositories is written to the cluster's own directory whichever
// member is named, never into a member's: see WriteGraph.
func (e *Engine) WriteArtifacts(repoPath string) error {
	// Load the published bundle once and read only from it. The bundle is immutable,
	// so serializing facts/insights/meta here is race-free even if another generate
	// publishes a newer bundle meanwhile.
	b := e.current.Load()
	if b.snapshot == nil {
		return fmt.Errorf("no snapshot generated")
	}
	if members := b.memberPaths(); len(members) > 1 {
		return e.writeClusterHome(b, members)
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolving repo path: %w", err)
	}
	return e.writeBundle(b, e.cfg.OutputDirFor(absRepo), repoPath, absRepo)
}

// WriteGraph writes the published graph where it belongs: one repository's into its
// own output directory, a cluster's once into the cluster's directory
// (config.GraphOutputDir). A cluster writes nothing into its members' output
// directories, so each member's own snapshot, previous/ and baseline/ are left as they
// were.
func (e *Engine) WriteGraph() error {
	b := e.current.Load()
	if b.snapshot == nil {
		return fmt.Errorf("no snapshot generated")
	}
	members := b.memberPaths()
	if len(members) > 1 {
		return e.writeClusterHome(b, members)
	}
	repo := b.snapshot.Meta.RepoPath
	if len(members) == 1 {
		repo = members[0]
	}
	return e.WriteArtifacts(repo)
}

// writeClusterHome writes a graph of several repositories to the cluster's own
// directory: the union artifacts, the union's meta and a receipt with an entry per
// member, and members.json, each member's own provenance.
func (e *Engine) writeClusterHome(b *snapshotBundle, members []string) error {
	dir := e.cfg.GraphOutputDir(members)
	if err := e.writeBundle(b, dir, "", ""); err != nil {
		return err
	}
	// --out (cluster_dir) is remembered beside the cluster's history, so a command
	// given a config that does not say it, the dashboard's included, still finds it.
	if e.cfg.Output.ClusterDir != "" {
		if err := e.cfg.RecordClusterDir(members, dir); err != nil {
			log.Printf("[engine] warning: could not record where the cluster's graph is: %v", err)
		}
	}
	return nil
}

// writeBundle writes b's artifacts into outDir: one repository's own output directory
// (repoPath, absRepo its absolute form), or a cluster's directory (repoPath "").
func (e *Engine) writeBundle(b *snapshotBundle, outDir string, repoPath, absRepo string) error {
	cluster := repoPath == ""

	defer lockDir(outDir)()
	// Everything is written into a staging directory first, while outDir still holds
	// the previous graph whole: a reader in another process (a dashboard, a served
	// instance, an agent's server starting) that reads outDir during a write reads the
	// old graph, not new facts beside old insights. Only once every file is on disk is
	// the graph published, by renames (see publishStaged).
	stage := filepath.Join(outDir, StagingSubdir)
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("clearing %s: %w", stage, err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if cluster {
		if err := writeMembersMeta(stage, b); err != nil {
			return err
		}
	}

	// Hash every written artifact so the receipt records the exact output bytes
	// (the verifiable counterpart to the per-input-file FileHashes). meta.json and
	// receipt.json themselves are not hashed — they carry these hashes.
	outputHashes := make(map[string]string)

	// Every artifact is written beside its path and renamed over it, never through
	// it: the file at the path may be linked from previous/ and baseline/, and writing
	// through it would rewrite those.
	for _, a := range b.snapshot.Artifacts {
		if err := writeFileAtomic(filepath.Join(stage, a.Name), a.Content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", a.Name, err)
		}
		outputHashes[a.Name] = hashBytes(a.Content)
	}

	// Write facts.jsonl, hashing the bytes as they go past rather than building the
	// whole file in memory to write and then hash it. On a kernel-sized graph that
	// buffer was 792 MiB, allocated at the end of a run and never released before the
	// process went back to idle.
	factsPath := filepath.Join(outDir, "facts.jsonl")
	factsHash, err := writeFactsJSONL(b.store, filepath.Join(stage, "facts.jsonl"))
	if err != nil {
		return err
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
	if err := writeFileAtomic(filepath.Join(stage, "insights.json"), insightsJSON, 0o644); err != nil {
		return fmt.Errorf("writing insights.json: %w", err)
	}
	insightsHash := hashBytes(insightsJSON)
	outputHashes["insights.json"] = insightsHash

	// Record the output hashes on a COPY of the meta rather than mutating the
	// published (shared, immutable) snapshot in place. SnapshotMeta is a value type,
	// so this copy shares its slices but overrides only OutputHashes.
	unionMeta := b.snapshot.Meta
	unionMeta.OutputHashes = outputHashes
	// One repository's dir carries its own provenance. A cluster's directory carries
	// its anchor member's (BaselineAnchor), as the anchor's own dir did when members
	// held the union: the union's own names whichever member was indexed last, so a
	// refresh and a full generate of one cluster would read as two repositories.
	meta := unionMeta
	provenance := absRepo
	if cluster {
		provenance = BaselineAnchor(b.memberPaths())
	}
	if turn, ok := b.members[provenance]; ok {
		meta = withMemberProvenance(unionMeta, turn)
	}

	// Write snapshot.meta.json (the internal superset, incl. per-file hashes)
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling meta: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(stage, "snapshot.meta.json"), metaJSON, 0o644); err != nil {
		return fmt.Errorf("writing snapshot.meta.json: %w", err)
	}

	// Write receipt.json (the compact provenance + quality manifest)
	// A cluster's directory carries the cluster's receipt, a member entry each.
	rcpt := meta.Receipt()
	if cluster {
		rcpt = clusterReceipt(meta, b.members)
	}
	receiptJSON, err := json.MarshalIndent(rcpt, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling receipt: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(stage, "receipt.json"), receiptJSON, 0o644); err != nil {
		return fmt.Errorf("writing receipt.json: %w", err)
	}

	// Rotate the prior snapshot into previous/, so diff_snapshot can compare against
	// the immediately-preceding run with no explicit pin, then publish. The pinned
	// baseline/ (SetBaseline) is left untouched here.
	if err := rotatePrevious(outDir); err != nil {
		log.Printf("[engine] warning: could not rotate previous snapshot: %v", err)
	}
	if err := publishStaged(stage, outDir); err != nil {
		return err
	}
	log.Printf("[engine] wrote %s (%d facts)", outDir, meta.FactCount)

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
	if e.recordSnapshot != nil && b.snapshot != nil {
		// A cluster is recorded once, into its own history, and its preceding
		// snapshot is its own previous/; git parents are read from the anchor member.
		var members []string
		if cluster {
			members = b.memberPaths()
			repoPath = BaselineAnchor(members)
		}
		e.recordSnapshot(SnapshotRecord{
			Members:     members,
			RepoPath:    repoPath,
			Meta:        meta,
			Snapshot:    b.snapshot,
			FactsPath:   factsPath,
			PreviousDir: filepath.Join(outDir, PreviousSubdir),
			Parents:     func() []string { return gitParents(repoPath) },
			Repos:       func() []RepoState { return repoStates(e, b) },
		})
	}

	// Say which agent session wrote this run. Rotated into previous/ with the rest by
	// the next write, where it tells a diff whose run it is comparing against.
	if err := e.writeSessionMark(outDir, RunMarkFile); err != nil {
		log.Printf("[engine] warning: could not record the run's session: %v", err)
	}
	return nil
}

// writeFactsJSONL serializes the store straight to path, returning the
// "sha256:"-prefixed digest of exactly the bytes written.
//
// The digest comes from a hash tee'd off the same stream rather than from a second
// pass over a buffer, so it cannot describe anything other than the file on disk.
// Durability is deliberately unchanged from the os.WriteFile this replaced: neither is
// atomic, and making facts.jsonl atomic is a separate decision from making it cheap.
func writeFactsJSONL(store *facts.Store, path string) (string, error) {
	// Written beside and renamed over: the file at path may be linked from other
	// repositories' dirs of a cluster, and truncating it in place would rewrite theirs.
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
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
	if err := os.Rename(tmp, path); err != nil {
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
		return json.MarshalIndent(e.Receipt(), "", "  ")
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
// It stops early once ctx is done, returning what it has; the caller checks ctx.
func (e *Engine) computeFileHashes(ctx context.Context, repoPath string, files []string) map[string]string {
	hashes, _ := e.hashFilesWithStat(ctx, repoPath, files)
	return hashes
}

// fileStat is a file's size and modification time as they were when it was hashed.
// mod is empty when the file had been modified too recently to be told apart from a
// modification later in the same second, which makes a later check read it.
type fileStat struct {
	size int64
	mod  string
}

// hashFilesWithStat hashes files and records each one's size and modification time,
// taken before it is read: a file modified after that has a later time, so a later run
// that finds the recorded size and time may take the recorded hash as the file's
// content (see CheckUnchanged). The recorded time has a resolution of a second, so a
// file modified in the last moments before it was read gets no time at all.
func (e *Engine) hashFilesWithStat(ctx context.Context, repoPath string, files []string) (map[string]string, map[string]fileStat) {
	hashes := make(map[string]string, len(files))
	stats := make(map[string]fileStat, len(files))
	for _, relFile := range files {
		if ctx.Err() != nil {
			break
		}
		absFile := filepath.Join(repoPath, relFile)
		info, statErr := os.Stat(absFile)
		data, err := os.ReadFile(absFile)
		if err != nil {
			continue
		}
		h := sha256.Sum256(data)
		hashes[relFile] = hex.EncodeToString(h[:])
		if statErr == nil && info.Size() == int64(len(data)) {
			st := fileStat{size: info.Size()}
			if time.Since(info.ModTime()) > driftRacyWindow {
				st.mod = info.ModTime().UTC().Format(time.RFC3339)
			}
			stats[relFile] = st
		}
	}
	return hashes, stats
}

// fileHashesOf lists the hashes by path, so the meta holding them is the same bytes
// for the same files.
func fileHashesOf(hashes map[string]string, stats map[string]fileStat) []facts.FileHash {
	paths := make([]string, 0, len(hashes))
	for path := range hashes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]facts.FileHash, 0, len(hashes))
	for _, path := range paths {
		st := stats[path]
		out = append(out, facts.FileHash{Path: path, Hash: hashes[path], ModTime: st.mod, Size: st.size})
	}
	return out
}

// readOpen reads all of f, which may be nil for a file that was not there.
func readOpen(f *os.File) ([]byte, error) {
	if f == nil {
		return nil, os.ErrNotExist
	}
	return io.ReadAll(f)
}

// StagingSubdir is where a write puts a graph's files before publishing them.
const StagingSubdir = ".next"

// publishStaged moves every file of stage into outDir by rename: the artifacts, then
// facts.jsonl and insights.json, members.json, snapshot.meta.json, and receipt.json
// last. A rename replaces a file in one step, so the window in which outDir holds part
// of each graph is these few renames, not the writing of the files; a reader that opens
// files inside it is told by RestoreFromDir, which checks them against the meta. A
// dashboard reloads on the receipt, and a served instance on the meta, so neither starts
// a reload before the graph's files are in place.
func publishStaged(stage, outDir string) error {
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	last := map[string]int{"facts.jsonl": 1, "insights.json": 2, MembersMetaFile: 3, snapshotMetaFileName: 4, "receipt.json": 5}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		if last[names[i]] != last[names[j]] {
			return last[names[i]] < last[names[j]]
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		if err := os.Rename(filepath.Join(stage, n), filepath.Join(outDir, n)); err != nil {
			return fmt.Errorf("publishing %s: %w", n, err)
		}
	}
	return nil
}

// ErrGraphChanging is a restore that found files of two writes in one directory: a write
// was publishing the next graph while they were opened. The directory is whole again a
// moment later, and its previous/ is the graph before.
var ErrGraphChanging = errors.New("the graph's files are being replaced by a newer write")

// hashOpenFile is the sha256 of f's whole content, read from its start without moving
// its offset, in the form output_hashes records.
func hashOpenFile(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, info.Size())); err != nil {
		return "", err
	}
	return hashPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// docsArtifact is the docs renderer's file: the whole comment of each documented fact.
const docsArtifact = "docs.jsonl"

// SetRestoreDocTexts makes a restore read every documented fact's whole comment back
// from docs.jsonl. facts.jsonl carries only the one-line summary, so a graph that is
// restored and then extended (a refresh of one member) would otherwise render the
// members it did not re-read with their comments flattened. A restore that only
// serves the graph has no use for them, and docs.jsonl is the largest artifact, so it
// is not read unless asked for.
func (e *Engine) SetRestoreDocTexts(on bool) { e.restoreDocTexts = on }

// readDocTexts records the whole comments docs.jsonl in f holds, the way the generate
// that wrote it held them. A text is kept per fact key (repo, kind, name, file), which
// overloads share, and a fact the generate kept none for rendered its own one-line
// summary: so a line whose doc is its fact's summary records nothing. Recording every
// line gave all of a name's overloads the last one's comment.
func readDocTexts(store *facts.Store, f *os.File) error {
	type at struct {
		key  facts.FactKey
		line int
	}
	summary := map[at]string{}
	for _, fact := range store.FactsRef() {
		if doc := fact.PropString(facts.PropDoc); doc != "" {
			summary[at{facts.FactKey{Repo: fact.Repo, Kind: fact.Kind, Name: fact.Name, File: fact.File}, fact.Line}] = doc
		}
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(io.NewSectionReader(f, 0, info.Size()))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var l struct {
			Repo string `json:"repo"`
			Kind string `json:"kind"`
			Name string `json:"name"`
			File string `json:"file"`
			Line int    `json:"line"`
			Doc  string `json:"doc"`
		}
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return err
		}
		key := facts.FactKey{Repo: l.Repo, Kind: l.Kind, Name: l.Name, File: l.File}
		if l.Doc == summary[at{key, l.Line}] {
			continue
		}
		store.SetDocText(key, l.Doc)
	}
	return sc.Err()
}
