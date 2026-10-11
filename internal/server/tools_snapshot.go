package server

// The tools that produce and compare snapshots: generate_snapshot, set_baseline,
// diff_snapshot, snapshot_receipt and compare_receipts.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/conformance"
	"github.com/enola-labs/enola/internal/diff"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// generateSnapshotArgs are the arguments for the generate_snapshot tool.
type generateSnapshotArgs struct {
	RepoPath  string   `json:"repo_path,omitempty" jsonschema:"Repository to index. Defaults to the configured repo path."`
	RepoPaths []string `json:"repo_paths,omitempty" jsonschema:"Several repositories indexed in one call as one linked set, each a service node with the calls between them linked. Resets the store unless append=true. Not combined with repo_path or fresh."`
	Append    bool     `json:"append,omitempty" jsonschema:"Keep the loaded facts and add this repository, with repo-prefixed file paths. Default false."`
	Fresh     bool     `json:"fresh,omitempty" jsonschema:"Discard everything loaded and index only repo_path as a single repository, overriding auto-append. Not combined with append."`
	NoCluster bool     `json:"no_cluster,omitempty" jsonschema:"Index a folder holding several git repositories as ONE repository instead of a cluster. Default false."`
}

// quotedPaths renders dir's named subdirectories as the JSON list repo_paths takes, so
// the remedy an agent is handed is the exact call to make.
func quotedPaths(dir string, names []string) string {
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = filepath.Join(dir, n)
	}
	out, _ := json.Marshal(paths)
	return string(out)
}

// resolveRepoPaths validates repo_paths and makes each entry absolute, dropping repeats.
// Two entries with the same directory name would share one repo label and merge into
// one service node, so they are refused rather than silently conflated.
func resolveRepoPaths(args generateSnapshotArgs) ([]string, error) {
	if args.RepoPath != "" {
		return nil, fmt.Errorf("pass repo_path or repo_paths, not both")
	}
	if args.Fresh {
		return nil, fmt.Errorf("repo_paths already starts a fresh store unless append=true; drop fresh")
	}
	var out []string
	seen := map[string]bool{}
	byLabel := map[string]string{}
	for _, p := range args.RepoPaths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("invalid repo path %q: %v", p, err)
		}
		if seen[abs] {
			continue
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("repo path %s is not a directory", abs)
		}
		label := facts.RepoDirName(abs)
		if other, ok := byLabel[label]; ok {
			return nil, fmt.Errorf("%s and %s would share the repo label %q (a repository is labelled by its directory name) and merge into one service node; list only one of them", other, abs, label)
		}
		seen[abs], byLabel[label] = true, abs
		out = append(out, abs)
	}
	return out, nil
}

// registerGenerateSnapshotTool registers the generate_snapshot tool.
func (s *Server) registerGenerateSnapshotTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "generate_snapshot",
		Description: "Index a repository into queryable architecture facts (module, symbol, route, storage, dependency, service). Run it first, and again after code changes. " +
			"Supports Go, TypeScript/JavaScript (incl. Vue, Svelte, Ember), Python, Java, Kotlin, Scala, Dart/Flutter, Ruby, PHP, Swift, Rust, C/C++, .NET (C#/VB.NET/F#/Razor/XAML), Terraform/HCL, Ansible, gRPC/Protobuf, OpenAPI, AsyncAPI and GraphQL. " +
			"To verify a change, call set_baseline after this first snapshot, BEFORE editing. " +
			"A folder holding several git repositories is indexed as a linked cluster (no_cluster=true for one repository). Several repositories go in ONE call as repo_paths. " +
			"append=true adds a repository to what is loaded, and is enabled automatically when you switch repos; fresh=true discards what is loaded when you have moved to a different project.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args generateSnapshotArgs) (*mcp.CallToolResult, any, error) {
		if len(args.RepoPaths) > 0 {
			paths, err := resolveRepoPaths(args)
			if err != nil {
				return errorResult(err.Error()), nil, nil
			}
			s.genMu.Lock()
			defer s.genMu.Unlock()
			return s.generateSet(ctx, paths, args.Append)
		}

		// append adds ONE named repository to the store. Without repo_path it fell back to
		// the configured repo, which is wherever the agent was started: a folder holding
		// every repository the user has, indexed as one. That took minutes, not an answer.
		if args.Append && args.RepoPath == "" {
			return errorResult(fmt.Sprintf("append=true needs repo_path (or repo_paths): the repository to add to the current store. "+
				"Without it, enola would index the default path (%s) instead.", s.cfg.Repo)), nil, nil
		}
		repoPath := args.RepoPath
		if repoPath == "" {
			repoPath = s.cfg.Repo
		}

		absRepo, err := filepath.Abs(repoPath)
		if err != nil {
			return errorResult(fmt.Sprintf("invalid repo path: %v", err)), nil, nil
		}

		if args.Fresh && args.Append {
			return errorResult("fresh and append are mutually exclusive: fresh forces a single-repo reset; append adds to the existing store. Pick one."), nil, nil
		}

		// Serialize the whole write side against any other generate_snapshot: the
		// auto-append heuristic reads the current snapshot/store, then GenerateSnapshot
		// rebuilds and publishes, then snapshotsGenerated/WriteArtifacts/WriteGlobalReceipt
		// run — all of which must be atomic w.r.t. a second concurrent generate. Held
		// for the rest of the handler. Read-only tools do not take genMu.
		s.genMu.Lock()
		defer s.genMu.Unlock()

		// A folder of git repositories is indexed as a cluster, as --generate does. An
		// explicit append names one repository to add, so it is left alone.
		if !args.Append && !args.NoCluster {
			if repos := workspace.Clusterable(absRepo, ""); len(repos) > 0 {
				return s.generateCluster(ctx, absRepo, repos)
			}
		}

		// Auto-enable append mode when switching to a different repo while facts
		// from another repo are already loaded — but only once this session has
		// explicitly generated a snapshot. A store pre-populated solely by
		// AutoLoadSnapshot must not trigger append: an explicit/default
		// append=false resets and discards the auto-loaded state.
		//
		// fresh=true is the explicit escape hatch: it suppresses this heuristic so
		// the engine's non-append path clears the store and indexes only repo_path —
		// the way to force a single-repo snapshot when you've switched projects and
		// do NOT want the auto-append merge.
		appendMode := args.Append
		autoAppended := false
		autoAppendedFrom := ""
		if !args.Fresh && !appendMode && s.snapshotsGenerated && s.eng.Store().Count() > 0 && s.eng.Snapshot() != nil {
			prevRepo := s.eng.Snapshot().Meta.RepoPath
			if prevRepo != "" && prevRepo != absRepo {
				appendMode = true
				autoAppended = true
				autoAppendedFrom = prevRepo
				log.Printf("[server] auto-enabled append mode: switching from %s to %s", prevRepo, absRepo)
			}
		}

		// A fresh (non-append) snapshot that discards an auto-loaded store is
		// silent otherwise; log it so the reset is visible.
		if !appendMode && !s.snapshotsGenerated && s.eng.Store().Count() > 0 && s.eng.Snapshot() != nil {
			if prevRepo := s.eng.Snapshot().Meta.RepoPath; prevRepo != "" && prevRepo != absRepo {
				log.Printf("[server] discarding auto-loaded snapshot from %s; generating fresh single-repo snapshot for %s", prevRepo, absRepo)
			}
		}

		// A non-append snapshot resets the store, so the cross-repo corpus pool must
		// reset with it. Without this, the premium for joining a graph is charged
		// against repos that were discarded when the store was cleared — a repo
		// appended to a small cluster would be credited for edges against every
		// unrelated repo snapshotted earlier in the session.
		if !appendMode {
			s.resetCorpus()
		}

		// Read this repo's previous snapshot from disk before overwriting it, so the
		// value model can tell a first build from a refresh, and an unchanged
		// refresh from one that has real work to re-derive. Disk rather than the
		// in-memory snapshot because in append mode the loaded snapshot describes
		// whichever repo was indexed last, not this one.
		snapshot, err := s.indexRepo(ctx, absRepo, appendMode)
		if err != nil {
			return errorResult(fmt.Sprintf("snapshot generation failed: %v", err)), nil, nil
		}

		// Write artifacts to disk
		if err := s.eng.WriteArtifacts(absRepo); err != nil {
			log.Printf("[server] warning: failed to write artifacts: %v", err)
		}

		// Refresh the graph-wide receipt at ~/.enola/receipt.json (non-fatal).
		if err := s.eng.WriteGlobalReceipt(); err != nil {
			log.Printf("[server] warning: failed to write global receipt: %v", err)
		}

		summary := s.snapshotSummary(snapshot)

		if appendMode {
			autoNote := ""
			if autoAppended {
				autoNote = " (auto-enabled: different repo detected)"
			}
			summary += s.multiRepoSummary(snapshot, autoNote, nil)
		} else {
			// Single-repo edit-verify loop guidance, tailored to whether a baseline
			// is already pinned: nudge set_baseline before the agent edits, then
			// diff_snapshot once a baseline exists. Skipped in multi-repo mode where
			// the baseline concept is per-first-repo and would only add noise.
			summary += s.loopHint(absRepo)
		}

		// De-silence the auto-append: lead with a prominent, self-correcting warning
		// so an agent that only meant a single-repo snapshot sees the merge and the
		// exact remedy, rather than silently ending up with a polluted store.
		if autoAppended {
			summary = fmt.Sprintf(
				"⚠️ **Auto-appended to the existing multi-repo store.** You snapshotted %q while %q was loaded, without append=true, so enola merged them into one store. "+
					"If you meant a FRESH single-repo snapshot of %q, re-run with fresh=true.\n\n---\n\n",
				filepath.Base(absRepo), filepath.Base(autoAppendedFrom), filepath.Base(absRepo),
			) + summary
		}

		// A folder holding several git repositories was just indexed as one repository.
		// Lead with that and with how to snapshot them as a cluster, so an agent does not
		// read the missing cross-repo graph as a fact about the code.
		if !appendMode {
			if folded := workspace.Folded(absRepo, ""); len(folded) > 0 {
				summary = fmt.Sprintf(
					"⚠️ **%s holds %d git repositories (%s), indexed here as ONE repository.** It has no service nodes and no edges between them. "+
						"To analyse them as a cluster, index them in one call: generate_snapshot(repo_paths=%s).\n\n---\n\n",
					absRepo, len(folded), workspace.Names(folded), quotedPaths(absRepo, folded),
				) + summary
			}
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: summary},
			},
		}, nil, nil
	})
}

// registerSetBaselineTool registers the set_baseline tool.
func (s *Server) registerSetBaselineTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "set_baseline",
		Description: "Pin the current snapshot as the baseline diff_snapshot compares against. Call it once at the START of a task, after generate_snapshot. " +
			"It survives later generate_snapshot runs, so it holds across several edit rounds, unlike 'previous', which only ever holds the run before.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args setBaselineArgs) (*mcp.CallToolResult, any, error) {
		repoPath := s.currentRepoPath()
		if repoPath == "" {
			return errorResult("No snapshot available. Run generate_snapshot first, then set_baseline."), nil, nil
		}
		replaced := s.otherSessionMark(s.resolveBaselineDir(repoPath, "pinned"), engine.PinMarkFile)
		if err := s.eng.SetBaseline(repoPath); err != nil {
			return errorResult(fmt.Sprintf("could not set baseline: %v", err)), nil, nil
		}
		msg := "Baseline pinned from the current snapshot.\n\n" +
			"Now make your changes, re-run generate_snapshot, then call diff_snapshot to see what changed " +
			"(new findings, new coupling, added/removed symbols). The baseline persists across re-snapshots until you pin a new one."
		if replaced != nil {
			msg += fmt.Sprintf("\n\nThis replaced a baseline another running agent session pinned at %s; "+
				"that session's diffs now compare against this one.", replaced.At)
		}
		return textResult(msg), nil, nil
	})
}

// registerDiffSnapshotTool registers the diff_snapshot tool.
func (s *Server) registerDiffSnapshotTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "diff_snapshot",
		Description: "What a change did to the architecture: the deterministic answer that replaces re-reading files. " +
			"A DELTA, not a linter: findings that appeared or were resolved, coupling edges added or removed, symbols, modules and routes added or removed. Anything present both before and after stays silent. " +
			"Loop: generate_snapshot, set_baseline, edit, generate_snapshot, diff_snapshot. " +
			"New findings keep their original confidence and caveats; below 1 is a candidate to verify, not a verdict.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args diffSnapshotArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil || s.eng.Store().Count() == 0 {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}
		repoPath := s.currentRepoPath()

		// Current = the live engine state (what the agent just generated). Build it
		// from the store so it works even when the snapshot was auto-loaded.
		//
		// FactsRef, not All: diff.Compute only reads its inputs (it groups facts into
		// its own slices and sorts those), and this store is the published bundle,
		// which is immutable once swapped in — exactly FactsRef's contract. All would
		// copy the entire fact set, plus every relation slice, for a read: on a
		// kernel-sized graph that is gigabytes of transient garbage on a query path.
		current := &facts.Snapshot{Meta: s.eng.MetaFor(repoPath), Facts: s.eng.Store().FactsRef(), Insights: snap.Insights}

		// Resolve the baseline directory from the selector.
		sel := strings.TrimSpace(args.Baseline)
		baseline, err := engine.LoadSnapshotDir(s.resolveBaselineDir(repoPath, sel))
		if err != nil {
			switch strings.ToLower(sel) {
			case "", "pinned":
				return errorResult("No pinned baseline found. Call set_baseline after your first generate_snapshot, " +
					"then make changes, re-run generate_snapshot, and diff_snapshot again. " +
					"(Or pass baseline='previous' to compare against the immediately-preceding snapshot.)"), nil, nil
			case "previous":
				return errorResult("No previous snapshot yet — generate_snapshot must have run at least twice. " +
					"(Or use set_baseline + baseline='pinned' for a stable task baseline.)"), nil, nil
			default:
				return errorResult(fmt.Sprintf("could not load baseline from %q: %v", sel, err)), nil, nil
			}
		}

		note := s.baselineNote(s.resolveBaselineDir(repoPath, sel), sel)

		d := diff.Compute(baseline, current)
		if args.Focus != "" {
			d = d.Focused(s.normalizeToRelative(args.Focus))
		}

		// Conformance is computed only when the caller declares an intent, or when a
		// plain delta is worth checking against its own edit sites. It needs a graph over
		// the BASELINE's facts — a store assembled from a loaded snapshot has no index
		// until one is built — so it is not free, and it is not run for callers who asked
		// for a delta and nothing more.
		var conf *conformance.Report
		if args.Target != "" || len(args.ExpectedPackages) > 0 {
			baseStore := facts.NewStore()
			baseStore.Add(baseline.Facts...)
			rep := conformance.Compute(baseStore, s.eng.Store(), d, conformance.Options{
				Target:           args.Target,
				ExpectedPackages: args.ExpectedPackages,
				MaxDepth:         args.MaxDepth,
				MaxNodes:         args.MaxNodes,
			})
			conf = &rep
		}
		// Whether the CURRENT snapshot still matches the working tree is something only
		// the caller can establish, and it is the one caveat that invalidates the whole
		// delta: if the tree moved since the snapshot, this diff describes neither the
		// old state nor the new one. Checked here rather than in the freshness banner
		// because it re-hashes the repo — affordable at a deliberate decision point,
		// not on every tool call. Focused() preserves Comparability, so this is added
		// after narrowing and still renders above the delta.
		addDriftWarning(d, s.eng, repoPath, "diff_snapshot")
		if note != "" {
			d.AddWarningKind(diff.WarnOtherSession, note)
		}

		switch resolveOutputMode(args.OutputMode, modeSummary) {
		case modeFull:
			if conf == nil {
				return jsonResultCapped(d, args.MaxTokens)
			}
			// Embedded, so the delta's fields stay at the top level and a consumer that
			// never asks for conformance sees the shape it always saw.
			return jsonResultCapped(struct {
				*diff.SnapshotDiff
				Conformance *conformance.Report `json:"conformance,omitempty"`
			}{d, conf}, args.MaxTokens)
		case modeCompact:
			return textResult(capTokens(conformanceFirst(conf, d.RenderCompact()), args.MaxTokens, false)), nil, nil
		default:
			return textResult(capTokens(conformanceFirst(conf, d.RenderSummary()), args.MaxTokens, false)), nil, nil
		}
	})
}

// registerSnapshotReceiptTool registers the snapshot_receipt tool.
func (s *Server) registerSnapshotReceiptTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "snapshot_receipt",
		Description: "Proof of WHAT the current snapshot was generated over (enola version, git state, extractors, a content fingerprint) and how complete extraction was (files parsed and skipped, parse errors, unresolved cross-repo edges). " +
			"Read it before trusting an impact_analysis or a diff, and to spot thin extraction (a missed detection, a bad ignore glob, a failing extractor).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args snapshotReceiptArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil || s.eng.Store().Count() == 0 {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}
		receipt := snap.Meta.Receipt()
		if resolveOutputMode(args.OutputMode, modeSummary) == modeFull {
			return jsonResultCapped(receipt, args.MaxTokens)
		}
		return textResult(capTokens(renderReceipt(receipt), args.MaxTokens, false)), nil, nil
	})
}

// registerCompareReceiptsTool registers the compare_receipts tool.
func (s *Server) registerCompareReceiptsTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "compare_receipts",
		Description: "Check that the current snapshot and a baseline are comparable BEFORE trusting a diff between them. " +
			"Returns a comparability verdict (same repo, enola version, extractor set, ignore globs; a mismatch means diff_snapshot would report spurious churn) " +
			"and metric deltas (files parsed, parse errors, coverage gaps, unresolved edges, fact and insight counts) that flag regressions in enola's own extraction. " +
			"Also the signal to poll when improving enola's coverage.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args compareReceiptsArgs) (*mcp.CallToolResult, any, error) {
		snap := s.eng.Snapshot()
		if snap == nil || s.eng.Store().Count() == 0 {
			return errorResult("No snapshot available. Run generate_snapshot first."), nil, nil
		}
		baseDir := s.resolveBaselineDir(s.currentRepoPath(), args.Baseline)
		baseline, err := engine.LoadSnapshotDir(baseDir)
		if err != nil {
			sel := strings.ToLower(strings.TrimSpace(args.Baseline))
			switch sel {
			case "", "pinned":
				return errorResult("No pinned baseline found. Call set_baseline after your first generate_snapshot, then compare_receipts again. (Or pass baseline='previous'.)"), nil, nil
			case "previous":
				return errorResult("No previous snapshot yet — generate_snapshot must have run at least twice."), nil, nil
			default:
				return errorResult(fmt.Sprintf("could not load baseline from %q: %v", args.Baseline, err)), nil, nil
			}
		}
		// Both snapshots, not both metas: a union's members are labels on its facts,
		// and a meta-only comparison cleared a pair diff_snapshot refused. FactsRef
		// for the reason diff_snapshot gives — the comparison only reads.
		current := &facts.Snapshot{Meta: s.eng.MetaFor(s.currentRepoPath()), Facts: s.eng.Store().FactsRef()}
		rc := diff.CompareSnapshotReceipts(baseline, current)
		if resolveOutputMode(args.OutputMode, modeSummary) == modeFull {
			return jsonResultCapped(rc, args.MaxTokens)
		}
		return textResult(capTokens(rc.Render(), args.MaxTokens, false)), nil, nil
	})
}

// conformanceFirst puts the scope verdict ahead of the delta.
//
// A caller who declared an intent asked a yes/no question, and the answer must not sit
// below a page of structural tallies — the delta is the evidence, not the finding.
func conformanceFirst(conf *conformance.Report, body string) string {
	if conf == nil {
		return body
	}
	if s := conf.Render(); s != "" {
		return strings.TrimLeft(s, "\n") + "\n" + body
	}
	return body
}

// driftOf measures drift for one repository against its own snapshot meta. The published
// snapshot's meta describes the primary repository only, so another member of a
// multi-repo graph is measured against the meta in its own output directory.
func (s *Server) driftOf(repoPath string) (engine.Drift, error) {
	if snap := s.eng.Snapshot(); snap != nil && snap.Meta.RepoPath == repoPath {
		return s.eng.Drift(repoPath)
	}
	raw, err := os.ReadFile(filepath.Join(s.eng.OutputDir(repoPath), "snapshot.meta.json"))
	if err != nil {
		return engine.Drift{Unknown: true, Reason: "this repository has no snapshot of its own on disk, so whether the graph still matches its working tree cannot be verified"}, nil
	}
	var meta facts.SnapshotMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return engine.Drift{}, err
	}
	return s.eng.DriftFromMeta(repoPath, meta)
}

// otherSessionMark returns the session mark in dir when it names another agent session
// that is still running, and nil otherwise: no mark, an unknown writer, this session,
// or a session that has ended. Only a running session can still be changing the tree
// or re-pinning, so only it is worth a word.
func (s *Server) otherSessionMark(dir, name string) *engine.SessionMark {
	m := engine.ReadSessionMark(dir, name)
	me := s.eng.SessionClient()
	if m == nil || m.AgentPID <= 0 || me <= 0 || m.AgentPID == me || !status.ProcessAlive(m.AgentPID) {
		return nil
	}
	return m
}

// baselineNote says when a diff's "before" came from another running agent session:
// the pinned baseline it pinned, or the previous run it took. Empty otherwise,
// including for an explicit baseline path, which the caller chose knowingly.
func (s *Server) baselineNote(dir, selector string) string {
	switch strings.ToLower(strings.TrimSpace(selector)) {
	case "", "pinned":
		if m := s.otherSessionMark(dir, engine.PinMarkFile); m != nil {
			return fmt.Sprintf("the baseline was pinned at %s by another agent session that is still running on this repository, "+
				"so it may include that session's changes, or have replaced a baseline this session pinned", m.At)
		}
	case "previous":
		if m := s.otherSessionMark(dir, engine.RunMarkFile); m != nil {
			return fmt.Sprintf("the previous run was taken at %s by another agent session that is still running on this repository, "+
				"so this compares against that session's snapshot, not this session's last one", m.At)
		}
	}
	return ""
}

// resolveBaselineDir maps a baseline selector ('pinned'/”/'previous'/explicit
// path) to the on-disk directory holding that snapshot's artifacts — the same
// resolution diff_snapshot uses, shared so compare_receipts stays consistent.
func (s *Server) resolveBaselineDir(repoPath, selector string) string {
	return engine.ResolveBaselineDir(s.eng.OutputDir(repoPath), selector)
}

// renderReceipt produces the compact markdown summary for the snapshot_receipt
// tool: provenance first, then the extraction-quality metrics that flag thin
// extraction.
func renderReceipt(r facts.Receipt) string {
	var sb strings.Builder
	sb.WriteString("# Snapshot receipt\n\n")

	sb.WriteString("## Provenance\n\n")
	fmt.Fprintf(&sb, "- **Snapshot ID:** `%s`\n", orDashStr(r.SnapshotID))
	fmt.Fprintf(&sb, "- **enola version:** %s\n", orDashStr(r.EnolaVersion))
	fmt.Fprintf(&sb, "- **Generated:** %s (%s)\n", orDashStr(r.GeneratedAt), r.Duration)
	fmt.Fprintf(&sb, "- **Repository:** %s\n", orDashStr(r.RepoPath))
	if r.Git != nil {
		dirty := "clean"
		if r.Git.Dirty {
			dirty = "**dirty (uncommitted changes)**"
		}
		fmt.Fprintf(&sb, "- **Git:** %s @ `%s` — %s\n", orDashStr(r.Git.Ref), shortSHA(r.Git.Commit), dirty)
	} else {
		sb.WriteString("- **Git:** not a git repository\n")
	}
	fmt.Fprintf(&sb, "- **Extractors:** %s\n", strings.Join(r.Extractors, ", "))
	if r.ConfigHash != "" {
		fmt.Fprintf(&sb, "- **Config hash:** `%s`\n", r.ConfigHash)
	}
	fmt.Fprintf(&sb, "- **Facts:** %d · **Insights:** %d\n\n", r.FactCount, r.InsightCount)

	q := r.Quality
	sb.WriteString("## Extraction quality\n\n")
	fmt.Fprintf(&sb, "- **Files:** %d parsed / %d seen · %d file(s) + %d directory tree(s) skipped by ignore globs\n",
		q.FilesParsed, q.FilesSeen, q.FilesSkipped, q.DirsSkipped)
	fmt.Fprintf(&sb, "- **Parse errors:** %d\n", q.ParseErrors)
	fmt.Fprintf(&sb, "- **Heuristic insights:** %d of %d (confidence < 1.0; the rest are structural facts)\n", q.HeuristicInsights, r.InsightCount)
	if q.Coverage != nil {
		fmt.Fprintf(&sb, "- **Cross-repo coverage:** %d service(s), %d coverage gap(s), %d unresolved edge(s)\n",
			q.Coverage.ServicesTotal, q.Coverage.CoverageGaps, q.Coverage.UnresolvedEdges)
	}
	if len(q.ParseErrorSample) > 0 {
		sb.WriteString("\n**Parse-error sample:**\n")
		for _, pe := range q.ParseErrorSample {
			fmt.Fprintf(&sb, "- [%s] %s%s\n", pe.Extractor, oneLine(pe.Msg), fileSuffix(pe.File))
		}
	}
	sb.WriteString("\n_Use output_mode='full' for the complete JSON receipt (output hashes, skipped-path sample, per-metric detail)._\n")
	return sb.String()
}

// orDashStr returns s, or an em dash when empty, for receipt rendering.
func orDashStr(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// shortSHA truncates a git commit SHA to its first 12 chars for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// fileSuffix renders a " (file)" suffix when a parse error names a file.
func fileSuffix(file string) string {
	if file == "" {
		return ""
	}
	return " (" + file + ")"
}

// loopHint returns the next-step guidance appended to a single-repo
// generate_snapshot summary. It is context-aware: before any baseline is pinned
// it points the agent at set_baseline (so it pins BEFORE editing); once a baseline
// exists it points at diff_snapshot (to see what changed). This makes the
// edit-verify loop self-guiding without the agent having to know it up front.
func (s *Server) loopHint(absRepo string) string {
	baselineFacts := filepath.Join(s.eng.OutputDir(absRepo), engine.BaselineSubdir, "facts.jsonl")
	if _, err := os.Stat(baselineFacts); err == nil {
		return "\n\n**Verify your change:** a baseline is pinned — call diff_snapshot to see exactly what changed since set_baseline " +
			"(new/resolved findings, new coupling, added/removed symbols). Re-pin from here with set_baseline if you're starting a new change."
	}
	return "\n\n**About to change code?** Call set_baseline now to freeze this snapshot as the baseline, then after editing re-run " +
		"generate_snapshot and diff_snapshot to see exactly what your change did — no need to re-read files to confirm it."
}

// currentRepoPath returns the absolute repo path of the live snapshot, falling
// back to the configured repo. Empty only when neither is available.
func (s *Server) currentRepoPath() string {
	if snap := s.eng.Snapshot(); snap != nil && snap.Meta.RepoPath != "" {
		return snap.Meta.RepoPath
	}
	if abs, err := filepath.Abs(s.cfg.Repo); err == nil {
		return abs
	}
	return ""
}

// setBaselineArgs has no parameters: set_baseline always pins the current snapshot.
type setBaselineArgs struct{}

type diffSnapshotArgs struct {
	Baseline         string   `json:"baseline,omitempty" jsonschema:"'pinned' (DEFAULT, the snapshot frozen by set_baseline), 'previous' (the run before, rotated automatically), or a directory containing facts.jsonl."`
	Focus            string   `json:"focus,omitempty" jsonschema:"Only entries referencing this module, file or symbol (substring match). Use it to verify just what you touched."`
	OutputMode       string   `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, headline regressions and improvements plus a structural tally), 'compact' (adds finding descriptions, evidence, changed edges and facts), or 'full' (complete JSON)."`
	MaxTokens        int      `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
	Target           string   `json:"target,omitempty" jsonschema:"The symbol, type or package you INTENDED to change. Turns the diff into a conformance check: impact analysis on the PRE-change graph predicts the radius, and any package the change reached outside it is reported as spillover. Omit for a plain delta."`
	ExpectedPackages []string `json:"expected_packages,omitempty" jsonschema:"Packages you expected to touch beyond the predicted radius. Touched packages that are neither expected nor predicted are spillover."`
	MaxDepth         int      `json:"max_depth,omitempty" jsonschema:"Depth of the predicted radius (1-10). Default 3. Only with target or expected_packages."`
	MaxNodes         int      `json:"max_nodes,omitempty" jsonschema:"Nodes per impact traversal (1-500). Default 200. Only with target or expected_packages."`
}

type snapshotReceiptArgs struct {
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, headline provenance and quality metrics) or 'full' (complete JSON receipt)."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}

type compareReceiptsArgs struct {
	Baseline   string `json:"baseline,omitempty" jsonschema:"'pinned' (DEFAULT, the snapshot frozen by set_baseline), 'previous' (the run before), or a directory containing receipt.json or snapshot.meta.json."`
	OutputMode string `json:"output_mode,omitempty" jsonschema:"'summary' (DEFAULT, markdown: verdict, metric deltas, quality regressions) or 'full' (complete JSON)."`
	MaxTokens  int    `json:"max_tokens,omitempty" jsonschema:"Approximate token cap; output is truncated with a notice. Default: no cap."`
}
