package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/drift"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/explainers/complexity"
	"github.com/enola-labs/enola/internal/explainers/constraints"
	"github.com/enola-labs/enola/internal/explainers/coverage"
	crossrepoexp "github.com/enola-labs/enola/internal/explainers/crossrepo"
	"github.com/enola-labs/enola/internal/explainers/cycles"
	"github.com/enola-labs/enola/internal/explainers/deadmethods"
	"github.com/enola-labs/enola/internal/explainers/depth"
	"github.com/enola-labs/enola/internal/explainers/domain"
	"github.com/enola-labs/enola/internal/explainers/entrypoints"
	"github.com/enola-labs/enola/internal/explainers/godclass"
	"github.com/enola-labs/enola/internal/explainers/hotspots"
	"github.com/enola-labs/enola/internal/explainers/importclosure"
	"github.com/enola-labs/enola/internal/explainers/intentcheck"
	"github.com/enola-labs/enola/internal/explainers/layers"
	"github.com/enola-labs/enola/internal/explainers/messagingcoverage"
	"github.com/enola-labs/enola/internal/explainers/queryloops"
	"github.com/enola-labs/enola/internal/explainers/surface"
	"github.com/enola-labs/enola/internal/explainers/unusedroutes"
	"github.com/enola-labs/enola/internal/explainers/vendoredcandidates"
	"github.com/enola-labs/enola/internal/extractors/ansibleextractor"
	"github.com/enola-labs/enola/internal/extractors/asyncapiextractor"
	"github.com/enola-labs/enola/internal/extractors/cppextractor"
	"github.com/enola-labs/enola/internal/extractors/dartextractor"
	"github.com/enola-labs/enola/internal/extractors/dotnetextractor"
	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/extractors/grpcextractor"
	"github.com/enola-labs/enola/internal/extractors/hclextractor"
	"github.com/enola-labs/enola/internal/extractors/javaextractor"
	"github.com/enola-labs/enola/internal/extractors/kotlinextractor"
	"github.com/enola-labs/enola/internal/extractors/manifestextractor"
	"github.com/enola-labs/enola/internal/extractors/mdintent"
	"github.com/enola-labs/enola/internal/extractors/openapiextractor"
	"github.com/enola-labs/enola/internal/extractors/phpextractor"
	"github.com/enola-labs/enola/internal/extractors/pythonextractor"
	"github.com/enola-labs/enola/internal/extractors/rubyextractor"
	"github.com/enola-labs/enola/internal/extractors/rustextractor"
	"github.com/enola-labs/enola/internal/extractors/scalaextractor"
	"github.com/enola-labs/enola/internal/extractors/swiftextractor"
	"github.com/enola-labs/enola/internal/extractors/tsextractor"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/binders/clientseam"
	"github.com/enola-labs/enola/internal/linkers/binders/emberresolver"
	"github.com/enola-labs/enola/internal/linkers/binders/frameworkroots"
	"github.com/enola-labs/enola/internal/linkers/binders/grpcclientfqn"
	"github.com/enola-labs/enola/internal/linkers/binders/grpcimpl"
	"github.com/enola-labs/enola/internal/linkers/binders/httphandler"
	"github.com/enola-labs/enola/internal/linkers/binders/messagingcontract"
	"github.com/enola-labs/enola/internal/linkers/binders/mixinowner"
	"github.com/enola-labs/enola/internal/linkers/binders/moduleedges"
	"github.com/enola-labs/enola/internal/linkers/binders/stimulusresolver"
	"github.com/enola-labs/enola/internal/linkers/binders/unmatchedroutes"
	"github.com/enola-labs/enola/internal/linkers/binders/vendoredspecs"
	graphqlsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/graphqlsig"
	httpsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/http"
	importsignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/imports"
	kafkasignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/kafka"
	sharedcodesignal "github.com/enola-labs/enola/internal/linkers/crossrepo/signals/sharedcode"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/internal/renderers/llmcontext"
	"github.com/enola-labs/enola/internal/server"
	"github.com/enola-labs/enola/pkg/plan"
	"github.com/enola-labs/enola/pkg/plugin"
	"github.com/enola-labs/enola/pkg/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Engine wraps the internal engine with a public interface for
// extension by enterprise or third-party code.
type Engine struct {
	eng *engine.Engine
}

// Store returns the underlying fact store.
func (e *Engine) Store() *facts.Store {
	return e.eng.Store()
}

// Snapshot returns the last generated snapshot, or nil.
func (e *Engine) Snapshot() *facts.Snapshot {
	return e.eng.Snapshot()
}

// Drift is the set of files that moved since the snapshot was taken. Aliased rather
// than redefined so its methods (Any, Count, Summary) come with it and the two
// packages cannot drift apart.
type Drift = engine.Drift

// Drift reports whether the loaded snapshot still matches repoPath's working tree, by
// re-walking and re-hashing. It re-reads every walked file, so it belongs at a
// deliberate decision point (a diff, a validation) rather than on a hot read path.
func (e *Engine) Drift(repoPath string) (Drift, error) {
	return e.eng.Drift(repoPath)
}

func (e *Engine) DriftFromMeta(repoPath string, meta facts.SnapshotMeta) (Drift, error) {
	return e.eng.DriftFromMeta(repoPath, meta)
}

func (e *Engine) MetaFor(repoPath string) facts.SnapshotMeta {
	return e.eng.MetaFor(repoPath)
}

// ActiveRepo returns the absolute repo path of the currently loaded snapshot,
// or "" if none is loaded. Used to attribute tool usage to the repo a call
// actually operated on.
func (e *Engine) ActiveRepo() string {
	if snap := e.eng.Snapshot(); snap != nil {
		return snap.Meta.RepoPath
	}
	return ""
}

// SetSnapshot sets the snapshot (used when auto-loading from disk).
func (e *Engine) SetSnapshot(snap *facts.Snapshot) {
	e.eng.SetSnapshot(snap)
}

// RestoreFromDir restores a persisted snapshot into the engine (see engine.RestoreFromDir).
func (e *Engine) RestoreFromDir(dir string, repoPaths map[string]string, singleRepoLabel string) error {
	return e.eng.RestoreFromDir(dir, repoPaths, singleRepoLabel)
}

// SetPersistCache controls whether the per-extractor cache is written to disk.
func (e *Engine) SetPersistCache(persist bool) {
	e.eng.SetPersistCache(persist)
}

// ResolveFactFile returns the absolute filesystem path for a fact's File field.
func (e *Engine) ResolveFactFile(f *facts.Fact) string {
	return e.eng.ResolveFactFile(f)
}

// RepoPaths returns the repo label -> absolute path mapping.
func (e *Engine) RepoPaths() map[string]string {
	return e.eng.RepoPaths()
}

// Config returns the engine config.
func (e *Engine) Config() *config.Config {
	return e.eng.Config()
}

// GenerateSnapshot runs the full pipeline: walk -> extract -> explain -> render.
// SetDeferLinking is engine.Engine.SetDeferLinking: a cluster walked repo by
// repo sets it for every turn but the last.
func (e *Engine) SetDeferLinking(defer_ bool) { e.eng.SetDeferLinking(defer_) }

func (e *Engine) GenerateSnapshot(ctx context.Context, repoPath string, appendMode bool) (*facts.Snapshot, error) {
	return e.eng.GenerateSnapshot(ctx, repoPath, appendMode)
}

// WriteArtifacts writes all snapshot artifacts to the output directory.
func (e *Engine) WriteArtifacts(repoPath string) error {
	return e.eng.WriteArtifacts(repoPath)
}

// WriteGlobalReceipt refreshes the graph-wide receipt at ~/.enola/receipt.json
// and this workspace's own copy under ~/.enola/graphs/.
func (e *Engine) WriteGlobalReceipt() error {
	return e.eng.WriteGlobalReceipt()
}

// GraphReceipt returns the receipt for the graph THIS engine currently holds,
// assembled in memory. A viewer with several servers running must use this
// rather than reading ~/.enola/receipt.json, which describes whichever process
// generated a snapshot last. Nil when no snapshot is loaded.
func (e *Engine) GraphReceipt() *facts.GraphReceipt {
	return e.eng.GraphReceipt()
}

// SetBaseline pins the current on-disk snapshot as the diff baseline.
func (e *Engine) SetBaseline(repoPath string) error {
	return e.eng.SetBaseline(repoPath)
}

// OutputDir returns the absolute .enola output directory for repoPath.
// CurrentMeta returns the identity half of the SnapshotMeta this engine would write,
// without parsing anything — enough for diff.CompareMeta to say whether a baseline is
// still usable.
func (e *Engine) CurrentMeta(repoPath string) *facts.SnapshotMeta { return e.eng.CurrentMeta(repoPath) }

func (e *Engine) OutputDir(repoPath string) string {
	return e.eng.OutputDir(repoPath)
}

// LoadSnapshotDir reads a persisted snapshot (facts.jsonl + insights.json +
// snapshot.meta.json) from dir into an in-memory Snapshot for diffing.
func LoadSnapshotDir(dir string) (*facts.Snapshot, error) {
	return engine.LoadSnapshotDir(dir)
}

// AddDriftWarning appends a comparability caveat to d when repoPath's working tree no
// longer matches the snapshot eng holds. rerunTool names the tool to call again.
//
// Exported because every tool that computes its OWN delta needs this caveat, and they do
// not all live in this module. A consumer that builds a diff the server never sees is not
// covered by the server's own call, so the check has to be reachable from outside. See
// internal/drift for why the implementation lives a layer down.
func AddDriftWarning(d *diff.SnapshotDiff, eng *Engine, repoPath, rerunTool string) {
	drift.AddWarning(d, eng, repoPath, rerunTool)
}

// ResolveBaselineDir maps a baseline selector to the directory holding that snapshot's
// artifacts: "" / "pinned" → the explicit SetBaseline pin, "previous" → the
// automatically-rotated preceding run, anything else → an explicit path.
//
// Re-exported so out-of-module callers resolve a selector the same way the MCP tools
// and the `check` gate do. What `previous` means is a contract, not a path convention:
// a consumer that spelled it out locally would keep working right up until the rotation
// changed, and then disagree with every other surface about which snapshot it named.
func ResolveBaselineDir(outDir, selector string) string {
	return engine.ResolveBaselineDir(outDir, selector)
}

// GetArtifact returns the content of a named artifact.
func (e *Engine) GetArtifact(name string) ([]byte, error) {
	return e.eng.GetArtifact(name)
}

// RegisterExtractor adds an extractor to the engine.
func (e *Engine) RegisterExtractor(ext plugin.Extractor) {
	e.eng.RegisterExtractor(ext)
}

func (e *Engine) Extractors() []plugin.Extractor {
	return e.eng.Extractors()
}

// RegisterExplainer adds an explainer to the engine.
func (e *Engine) RegisterExplainer(exp plugin.Explainer) {
	e.eng.RegisterExplainer(exp)
}

// RegisterRenderer adds a renderer to the engine.
func (e *Engine) RegisterRenderer(rnd plugin.Renderer) {
	e.eng.RegisterRenderer(rnd)
}

// Server wraps the MCP server with a public interface.
type Server struct {
	srv *server.Server
}

// Run starts the MCP server on the stdio transport.
func (s *Server) Run(ctx context.Context) error {
	return s.srv.Run(ctx)
}

// SeedCorpus publishes the corpus of a graph restored from disk, so queries are
// priced against it before this process takes a snapshot of its own. Pass the map
// returned by AutoLoadSnapshot, and call it before Run.
func (s *Server) SeedCorpus(byRepo map[string]int) {
	s.srv.SeedCorpus(byRepo)
}

// SetToolCallback sets a callback invoked once per completed tool call, with the
// tool name, the absolute repo path the call operated on, whether it succeeded,
// its response size and any snapshot detail — everything the value model needs
// that cannot be recovered from a call count afterwards.
//
// It fires for every tool on the server, including those a wrapper binary
// registers, so a wrapper does not report its own calls.
func (s *Server) SetToolCallback(cb func(status.ToolCall)) {
	s.srv.SetToolCallback(cb)
}

// StartTime returns the time the server started (zero value if Run() hasn't been called).
func (s *Server) StartTime() time.Time {
	return s.srv.GetStartTime()
}

// MCP returns the underlying MCP server so enterprise code can register
// additional (license-gated) tools before calling Run.
func (s *Server) MCP() *mcp.Server {
	return s.srv.MCPServer()
}

func (s *Server) SetPlanEngineFactory(factory plan.EngineFactory) {
	s.srv.SetPlanEngineFactory(factory)
}

func PlanEngineFactory(cfg *config.Config) plan.EngineFactory {
	return func() (plan.Generator, error) {
		eng, err := engine.New(cfg)
		if err != nil {
			return nil, err
		}
		registerOSSPlugins(eng, cfg)
		wrapped := &Engine{eng: eng}
		wrapped.SetPersistCache(false)
		return wrapped, nil
	}
}

// Options controls bootstrap behavior.
type Options struct {
	// ConfigPath is the path to the YAML config file. Default: "mcp-arch.yaml".
	ConfigPath string
}

// defaultConfigName is the config file every command looks for when none is named.
const defaultConfigName = "mcp-arch.yaml"

// ResolveConfig loads the effective configuration and returns it alongside a
// one-line description of WHERE it came from, for the caller to print.
//
// The description is not decoration. A config governs which extractors run and
// which paths are ignored, so a run against the wrong config does not fail — it
// quietly analyses something other than what was asked for. The case that made
// this necessary: a config predating the Rust extractor, sitting next to a
// `go build` output, disabled Rust for every repository that binary was ever
// pointed at, from any directory, with no warning and no mention of Rust in the
// log. Naming the resolved path on every command converts that from a silent
// wrong answer into an obvious one.
//
// Lookup order: the given path (or mcp-arch.yaml) relative to the working
// directory, then — only for an unpacked bundle, see bundledConfigDir — a copy
// beside the executable. Built-in defaults when neither exists.
//
// A config that EXISTS but cannot be used is an error, not a fallback. The two
// cases look similar and are not: a missing mcp-arch.yaml means "no preferences,
// use the defaults", while an unparseable one — or one naming an output.dir that
// cannot be honoured — means the user configured something and it is not in force.
// Falling back there substitutes `repo: "."`, so a typo makes enola analyse the
// working directory and present the result as an answer about the repository the
// config named. The CLI already learned this once, for an explicitly-named path
// that does not exist (see the default case in cmd/enola/main.go); this is the same
// rule for a file that is present and wrong.
func ResolveConfig(cfgPath string) (*config.Config, string, error) {
	if cfgPath == "" {
		cfgPath = defaultConfigName
	}

	cfg, err := config.Load(cfgPath)
	if err == nil {
		return cfg, "enola: using config " + cfg.SourcePath, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}

	if !filepath.IsAbs(cfgPath) {
		if exeDir, ok := bundledConfigDir(); ok {
			bundled, bErr := config.Load(filepath.Join(exeDir, cfgPath))
			if bErr == nil {
				wd, _ := os.Getwd()
				return bundled, fmt.Sprintf("enola: using config %s (found next to the enola binary, not in %s)",
					bundled.SourcePath, wd), nil
			}
			if !errors.Is(bErr, fs.ErrNotExist) {
				return nil, "", bErr
			}
		}
	}

	wd, _ := os.Getwd()
	return config.Default(), fmt.Sprintf("enola: no %s in %s, using built-in defaults", cfgPath, wd), nil
}

// bundledConfigDir returns the executable's directory when a config sitting there
// should be honoured, and false otherwise.
//
// The fallback is defensible for a distribution unpacked as a unit — binary and
// config shipped together, run from anywhere. It is indefensible for an INSTALLED
// binary: a config beside enola on PATH governs every invocation from every
// directory that has no config of its own, which is most of them, and the user has
// no reason to connect the two. Being on PATH is what separates the cases, so an
// exe directory that is a PATH entry is refused.
func bundledConfigDir() (string, bool) {
	exePath, err := os.Executable()
	if err != nil {
		return "", false
	}
	// Resolve symlinks: a Homebrew-style /usr/local/bin/enola -> ../Cellar/... link
	// is an installed binary, and its real directory is where a bundled config would
	// live. Best-effort — an unresolvable link falls back to the literal path.
	if resolved, rErr := filepath.EvalSymlinks(exePath); rErr == nil {
		exePath = resolved
	}
	exeDir := filepath.Clean(filepath.Dir(exePath))

	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		if abs, aErr := filepath.Abs(entry); aErr == nil {
			entry = abs
		}
		if filepath.Clean(entry) == exeDir {
			return "", false
		}
	}
	return exeDir, true
}

// NewEngine creates an Engine with all OSS plugins registered.
// Use the returned Engine's methods to add additional (enterprise) plugins
// before starting the server or generating snapshots.
func NewEngine(opts Options) (*Engine, *config.Config, error) {
	cfg, note, err := ResolveConfig(opts.ConfigPath)
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintln(os.Stderr, note)

	eng, err := engine.New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create engine: %w", err)
	}
	registerOSSPlugins(eng, cfg)
	return &Engine{eng: eng}, cfg, nil
}

func registerOSSPlugins(eng *engine.Engine, cfg *config.Config) {
	// Register all OSS extractors
	eng.RegisterExtractor(cppextractor.New())
	eng.RegisterExtractor(asyncapiextractor.New())
	eng.RegisterExtractor(dotnetextractor.New())
	eng.RegisterExtractor(goextractor.New())
	eng.RegisterExtractor(manifestextractor.New())
	eng.RegisterExtractor(mdintent.New())
	eng.RegisterExtractor(grpcextractor.New())
	eng.RegisterExtractor(hclextractor.New())
	eng.RegisterExtractor(ansibleextractor.New())
	eng.RegisterExtractor(javaextractor.New())
	eng.RegisterExtractor(kotlinextractor.New())
	eng.RegisterExtractor(openapiextractor.New())
	eng.RegisterExtractor(phpextractor.New())
	eng.RegisterExtractor(pythonextractor.New())
	eng.RegisterExtractor(tsextractor.New())
	eng.RegisterExtractor(swiftextractor.New())
	eng.RegisterExtractor(rubyextractor.New())
	eng.RegisterExtractor(rustextractor.New())
	eng.RegisterExtractor(scalaextractor.New())
	eng.RegisterExtractor(dartextractor.New())

	// Resolve the linking vocabulary once and hand it to everything that matches under
	// it, so the signals and the unmatched-route binder cannot disagree about what
	// counts as a generic path. An invalid overlay is reported and the defaults used;
	// the same fallback the engine applies, kept here so it is visible at wiring time.
	linkVocab, err := cfg.LinkingVocab()
	if err != nil {
		log.Printf("[bootstrap] invalid linking vocabulary, using defaults: %v", err)
		linkVocab = vocab.Default()
	}

	// Register all OSS binders. Stage() decides when each runs relative to cross-repo
	// linking, so the order here is presentation only — see plugin.Binder.
	eng.RegisterBinder(clientseam.New())
	eng.RegisterBinder(grpcclientfqn.New())
	eng.RegisterBinder(emberresolver.New())
	eng.RegisterBinder(grpcimpl.New())
	eng.RegisterBinder(httphandler.New())
	eng.RegisterBinder(mixinowner.New())
	eng.RegisterBinder(frameworkroots.New())
	eng.RegisterBinder(moduleedges.New())
	eng.RegisterBinder(stimulusresolver.New())
	eng.RegisterBinder(vendoredspecs.New())
	eng.RegisterBinder(messagingcontract.New())
	eng.RegisterBinder(unmatchedroutes.New(linkVocab))

	// Register all OSS cross-repo signals. Phase() decides when each runs, so the
	// order here is presentation only — see plugin.CrossRepoSignal.
	eng.RegisterCrossRepoSignal(httpsignal.New(linkVocab))
	eng.RegisterCrossRepoSignal(importsignal.New())
	eng.RegisterCrossRepoSignal(kafkasignal.New(linkVocab))
	eng.RegisterCrossRepoSignal(graphqlsignal.New())
	eng.RegisterCrossRepoSignal(sharedcodesignal.New(linkVocab))

	// Register all OSS explainers
	eng.RegisterExplainer(cycles.New())
	eng.RegisterExplainer(layers.New())
	eng.RegisterExplainer(crossrepoexp.New())
	eng.RegisterExplainer(coverage.New())
	eng.RegisterExplainer(unusedroutes.New())
	eng.RegisterExplainer(domain.New())
	eng.RegisterExplainer(queryloops.New())
	eng.RegisterExplainer(deadmethods.New())
	eng.RegisterExplainer(entrypoints.New())
	eng.RegisterExplainer(messagingcoverage.New())
	eng.RegisterExplainer(godclass.New())
	eng.RegisterExplainer(hotspots.New())
	eng.RegisterExplainer(depth.New())
	eng.RegisterExplainer(surface.New())
	eng.RegisterExplainer(complexity.New())
	eng.RegisterExplainer(intentcheck.New())
	eng.RegisterExplainer(constraints.New())
	eng.RegisterExplainer(vendoredcandidates.New())
	eng.RegisterExplainer(importclosure.New())

	// Register all OSS renderers
	eng.RegisterRenderer(llmcontext.New(cfg.Output.MaxContextTokens))
}

// NewServer creates an MCP server wired to the given Engine.
func NewServer(eng *Engine, cfg *config.Config) (*Server, error) {
	srv, err := server.New(eng.eng, cfg)
	if err != nil {
		return nil, err
	}
	srv.SetPlanEngineFactory(PlanEngineFactory(cfg))
	srv.SetReloader(func() map[string]int { return AutoLoadSnapshot(eng, cfg) })
	// The one place the soft memory limit is worth announcing. ConfigureRuntime is
	// silent (see its doc) because a working default is not news on every CLI
	// invocation — but a server is long-lived, holds whole graphs in memory, and its
	// log is what somebody reads after it was OOM-killed. Logged here rather than in
	// internal/server so it cannot depend on which binary started the server, and
	// because internal/server cannot reach this package.
	log.Printf("[runtime] %s", MemoryLimitLine())
	return &Server{srv: srv}, nil
}

// GraphStateFunc returns a callback reporting what this engine currently holds,
// for status.Tracker.SetGraphFunc. It is what lets a user with several servers
// running tell them apart — each instance record then names the repos, snapshot
// and fact counts of its own process rather than of whichever one wrote last.
//
// The callback is invoked on every status write, so it only reads the published
// (immutable) snapshot bundle and never blocks on IO.
func GraphStateFunc(eng *Engine) status.GraphFunc {
	return func() status.GraphState {
		g := status.GraphState{PrimaryRepo: eng.ActiveRepo()}

		snap := eng.Snapshot()
		if snap != nil {
			g.SnapshotID = snap.Meta.SnapshotID
			if t, err := time.Parse(time.RFC3339, snap.Meta.GeneratedAt); err == nil {
				g.SnapshotAt = t
			}
			g.FactCount = snap.Meta.FactCount
			g.InsightCount = snap.Meta.InsightCount
		}

		store := eng.Store()
		repos := eng.RepoPaths()
		if len(repos) == 0 && snap != nil && snap.Meta.RepoPath != "" {
			// Single-repo graph: RepoPaths stays empty until a repo is appended.
			repos = map[string]string{snap.Meta.Label(): snap.Meta.RepoPath}
		}
		for label, path := range repos {
			r := status.InstanceRepo{Label: label, Path: path}
			if store != nil {
				r.FactCount = store.CountByRepo(label)
			}
			g.Repos = append(g.Repos, r)
		}
		sort.Slice(g.Repos, func(i, j int) bool { return g.Repos[i].Label < g.Repos[j].Label })
		return g
	}
}

// AutoLoadSnapshot restores an existing snapshot from disk if available, so queries
// (and the enterprise tools) work immediately after a restart WITHOUT a
// generate_snapshot call.
//
// It prefers a graph registry listing every repo in the graph and their paths, so
// a restart restores the WHOLE multi-repo graph — not just cfg.Repo — with no
// extractor runs. Two registries exist and are tried in order:
//
//  1. ~/.enola/graphs/<workspace>.json — the receipt for THIS workspace (cfg.Repo).
//  2. ~/.enola/receipt.json — the machine-wide receipt, describing whichever
//     server generated a snapshot last.
//
// The workspace file comes first because a user typically runs several servers at
// once, one per agent terminal: reading the shared file would restore a sibling
// terminal's repo set into this process. The shared file remains the fallback for
// graphs snapshotted before the workspace receipt existed. Failing both, it falls
// back to a single-repo restore of cfg.Repo. Either way it restores facts +
// insights + the snapshot meta (incl. generated_at, which the freshness check
// needs), unlike the old facts-only load.
//
// It publishes a fresh bundle via engine.RestoreFromDir. This is safe ONLY because
// it runs single-threaded at startup, before the MCP server begins serving tool
// calls — no reader can observe a half-built store. Callers must keep it strictly
// before Server.Run.
// It returns the parsed-source size of each restored repo, keyed by absolute
// path, or nil when nothing was restored. That map comes from the SAME receipt
// the facts came from, which is the point of returning it rather than letting the
// caller resolve one independently: the two could otherwise disagree, and a
// server would size its graph by a repo set it is not actually holding. Pass it
// to Server.SeedCorpus so queries are priced against the restored graph without
// waiting for a snapshot this process did not need to take.
func AutoLoadSnapshot(eng *Engine, cfg *config.Config) map[string]int {
	// Preferred path: this workspace's own graph.
	if gr, err := engine.LoadWorkspaceReceipt(cfg.Repo); err == nil && len(gr.Repos) > 0 {
		if restoreFromGlobalReceipt(eng, cfg, gr) {
			return corpusFromReceipt(gr)
		}
		log.Printf("[bootstrap] workspace receipt present but multi-repo restore incomplete; trying the machine-wide receipt")
	}

	// Fallback: the machine-wide registry — but only when it actually describes
	// this workspace. It is written by every server on the machine, so adopting it
	// blindly would restore a graph another agent terminal snapshotted, which is
	// the cross-talk the workspace receipt above exists to prevent.
	if gr, err := engine.LoadGlobalReceipt(); err == nil && len(gr.Repos) > 0 && receiptCovers(gr, cfg.Repo) {
		if restoreFromGlobalReceipt(eng, cfg, gr) {
			return corpusFromReceipt(gr)
		}
		log.Printf("[bootstrap] global receipt present but multi-repo restore incomplete; falling back to single-repo")
	}

	// Fallback: single-repo restore of cfg.Repo.
	repoPath, err := filepath.Abs(cfg.Repo)
	if err != nil {
		return nil
	}
	dir := filepath.Join(repoPath, cfg.Output.Dir)
	if _, err := os.Stat(filepath.Join(dir, "facts.jsonl")); err != nil {
		return nil // nothing on disk; start empty
	}
	// The label from the snapshot on disk, not from the directory it sits in. The map
	// below is what repo-scoped queries and fact counts are keyed by, so guessing it
	// from the path restores a graph whose facts nothing can find — silently, as an
	// empty result rather than an error.
	label := restoredLabel(dir, repoPath)
	if err := eng.RestoreFromDir(dir, map[string]string{label: repoPath}, label); err != nil {
		log.Printf("[bootstrap] warning: failed to restore snapshot from %s: %v", dir, err)
		return nil
	}
	log.Printf("[bootstrap] restored single-repo snapshot for %s", label)

	// No graph receipt on this path, so read the size from the snapshot we just
	// restored — it carries the same measurement.
	if snap := eng.Snapshot(); snap != nil && snap.Meta.SourceBytes > 0 {
		return map[string]int{repoPath: int(snap.Meta.SourceBytes / charsPerToken)}
	}
	return nil
}

// LoadDashboardSnapshot restores exactly the scope selected by the dashboard
// command. An explicit multi-repository config restores its graph; an ordinary
// repository launch reads only that repository's own snapshot and never adopts
// an accumulated MCP workspace graph by surprise.
func LoadDashboardSnapshot(eng *Engine, cfg *config.Config) bool {
	if len(cfg.Repos) > 1 {
		repos, err := cfg.RepoPaths()
		if err != nil || len(repos) == 0 {
			return false
		}
		paths := make(map[string]string, len(repos))
		for _, repo := range repos {
			paths[filepath.Base(repo)] = repo
		}
		dir := filepath.Join(repos[0], cfg.Output.Dir)
		return eng.RestoreFromDir(dir, paths, "") == nil
	}
	repoPath, err := filepath.Abs(cfg.Repo)
	if err != nil {
		return false
	}
	dir := filepath.Join(repoPath, cfg.Output.Dir)
	label := restoredLabel(dir, repoPath)
	if err := eng.RestoreFromDir(dir, map[string]string{label: repoPath}, label); err != nil {
		return false
	}
	return true
}

// restoredLabel reads the repo label recorded in a snapshot directory, falling back to
// the checkout directory name when the snapshot predates the recorded label — which is
// exactly what labelled the facts in that older snapshot.
func restoredLabel(dir, repoPath string) string {
	data, err := os.ReadFile(filepath.Join(dir, "snapshot.meta.json"))
	if err != nil {
		return filepath.Base(repoPath)
	}
	var meta facts.SnapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil || meta.RepoLabel == "" {
		return filepath.Base(repoPath)
	}
	return meta.RepoLabel
}

// charsPerToken converts source bytes to tokens, matching the heuristic used
// throughout the engine and the value model.
const charsPerToken = 4

// corpusFromReceipt projects a graph receipt into the corpus map the server
// prices queries with. Repos whose size was never recorded (a receipt written
// before the field existed) are omitted rather than entered as zero, so a partial
// receipt under-reports instead of claiming a repo has no source.
func corpusFromReceipt(gr *facts.GraphReceipt) map[string]int {
	out := make(map[string]int, len(gr.Repos))
	for _, r := range gr.Repos {
		if r.SourceBytes > 0 && r.Path != "" {
			out[r.Path] = int(r.SourceBytes / charsPerToken)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// receiptCovers reports whether a graph receipt includes the given workspace
// repo, i.e. whether restoring it would give this server its OWN graph. Paths are
// compared canonically (absolute, symlinks resolved) because a receipt records
// the path as the writing server resolved it.
func receiptCovers(gr *facts.GraphReceipt, repo string) bool {
	want, err := filepath.Abs(repo)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	for _, r := range gr.Repos {
		got := r.Path
		if resolved, err := filepath.EvalSymlinks(got); err == nil {
			got = resolved
		}
		if got == want {
			return true
		}
	}
	return false
}

// restoreFromGlobalReceipt reloads the complete multi-repo graph named by the
// global receipt. In append/multi-repo mode WriteArtifacts writes the ENTIRE
// in-memory store to each repo's .enola, so the most-recently-generated repo dir
// holds every repo's facts; that dir is loaded once (facts are already tagged with
// their repo labels). Returns false if it cannot find a complete snapshot dir, so
// the caller can fall back. repoPaths comes from the receipt so multi-repo file
// resolution works after restore.
func restoreFromGlobalReceipt(eng *Engine, cfg *config.Config, gr *facts.GraphReceipt) bool {
	repoPaths := make(map[string]string, len(gr.Repos))
	for _, r := range gr.Repos {
		if r.Path != "" {
			repoPaths[r.Label] = r.Path
		}
	}
	if len(repoPaths) == 0 {
		return false
	}

	completeDir, ok := newestSnapshotDir(gr.Repos, cfg.Output.Dir)
	if !ok {
		return false
	}

	// Single-repo graph: the file may be untagged, so pass its label; a genuine
	// multi-repo file is already tagged and SetRepoRange leaves it untouched.
	singleLabel := ""
	if len(repoPaths) == 1 {
		for l := range repoPaths {
			singleLabel = l
		}
	}

	if err := eng.RestoreFromDir(completeDir, repoPaths, singleLabel); err != nil {
		log.Printf("[bootstrap] warning: multi-repo restore from %s failed: %v", completeDir, err)
		return false
	}
	if got := eng.Store().Count(); gr.FactCount > 0 && got != gr.FactCount {
		log.Printf("[bootstrap] note: restored %d facts but global receipt records %d (partial restore from %s)", got, gr.FactCount, completeDir)
	}
	log.Printf("[bootstrap] restored graph of %d repo(s) from %s", len(repoPaths), completeDir)
	return true
}

// newestSnapshotDir picks the repo snapshot directory with the newest generated_at
// among the graph's repos — the one whose facts.jsonl holds the complete store.
// Returns false when no repo dir has a readable snapshot timestamp.
func newestSnapshotDir(repos []facts.GraphRepoEntry, outDir string) (string, bool) {
	var bestDir string
	var bestTS time.Time
	found := false
	for _, r := range repos {
		if r.Path == "" {
			continue
		}
		dir := filepath.Join(r.Path, outDir)
		ts, ok := snapshotGeneratedAt(dir)
		if !ok {
			continue
		}
		if !found || ts.After(bestTS) {
			bestTS, bestDir, found = ts, dir, true
		}
	}
	return bestDir, found
}

// snapshotGeneratedAt reads the generated_at timestamp from a snapshot dir,
// preferring snapshot.meta.json and falling back to receipt.json.
func snapshotGeneratedAt(dir string) (time.Time, bool) {
	for _, name := range []string{"snapshot.meta.json", "receipt.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var m struct {
			GeneratedAt string `json:"generated_at"`
		}
		if err := json.Unmarshal(data, &m); err != nil || m.GeneratedAt == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, m.GeneratedAt); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
