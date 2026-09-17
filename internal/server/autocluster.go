package server

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// generateCluster indexes a folder of git repositories as one linked cluster, the way
// --generate does: the cluster config already in dir, or one written listing names, and
// every repository appended into a fresh store with linking run once, on the last.
// The caller holds genMu.
func (s *Server) generateCluster(ctx context.Context, dir string, names []string) (*mcp.CallToolResult, any, error) {
	path, existing, writeErr := workspace.EnsureClusterConfig(dir, names)
	var repoPaths []string
	if existing {
		cfg, err := config.Load(path)
		if err != nil {
			return errorResult(fmt.Sprintf("reading the cluster config in %s: %v", dir, err)), nil, nil
		}
		if repoPaths, err = cfg.RepoPaths(); err != nil {
			return errorResult(fmt.Sprintf("resolving the repositories of %s: %v", path, err)), nil, nil
		}
	} else {
		for _, n := range names {
			repoPaths = append(repoPaths, filepath.Join(dir, n))
		}
	}

	s.resetCorpus()
	defer s.eng.SetDeferLinking(false)
	var snapshot *facts.Snapshot
	for i, repo := range repoPaths {
		s.eng.SetDeferLinking(i < len(repoPaths)-1)
		snap, err := s.indexRepo(ctx, repo, i > 0)
		if err != nil {
			return errorResult(fmt.Sprintf("snapshot generation failed for %s: %v", repo, err)), nil, nil
		}
		snapshot = snap
	}
	for _, repo := range repoPaths {
		if err := s.eng.WriteArtifacts(repo); err != nil {
			log.Printf("[server] warning: failed to write artifacts for %s: %v", repo, err)
		}
	}
	if err := s.eng.WriteGlobalReceipt(); err != nil {
		log.Printf("[server] warning: failed to write global receipt: %v", err)
	}

	labels := make([]string, len(repoPaths))
	for i, p := range repoPaths {
		labels[i] = filepath.Base(p)
	}
	var lead strings.Builder
	fmt.Fprintf(&lead, "**%s holds %d git repositories (%s), indexed as a cluster.** Each one is a service node and the calls between them are linked.",
		dir, len(names), workspace.Names(names))
	switch {
	case existing:
		fmt.Fprintf(&lead, " Repositories were read from the cluster config already there, %s.", path)
	case writeErr != nil:
		fmt.Fprintf(&lead, " No cluster config could be written (%v).", writeErr)
	default:
		fmt.Fprintf(&lead, " Wrote %s listing them.", path)
	}
	fmt.Fprintf(&lead, " To index %s as one repository instead, call generate_snapshot(repo_path=%q, no_cluster=true).\n\n---\n\n", dir, dir)

	summary := lead.String() + s.snapshotSummary(snapshot) +
		fmt.Sprintf("\n\n- Repositories indexed: %s", strings.Join(labels, ", ")) +
		s.multiRepoSummary(snapshot, " (folder of repositories)")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: summary}}}, nil, nil
}

// indexRepo snapshots one repository into the store and records what the snapshot was
// worth. The caller holds genMu and writes the artifacts.
func (s *Server) indexRepo(ctx context.Context, absRepo string, appendMode bool) (*facts.Snapshot, error) {
	// Read this repo's previous snapshot from disk before overwriting it, so the
	// value model can tell a first build from a refresh, and an unchanged
	// refresh from one that has real work to re-derive. Disk rather than the
	// in-memory snapshot because in append mode the loaded snapshot describes
	// whichever repo was indexed last, not this one.
	prevID, prevHashes := previousSnapshot(absRepo)
	priorCorpus := s.priorCorpusTokens(absRepo)

	snapshot, err := s.eng.GenerateSnapshot(ctx, absRepo, appendMode)
	if err != nil {
		return nil, err
	}
	s.snapshotsGenerated = true

	corpusTokens := int(snapshot.Meta.SourceBytes / charsPerToken)
	s.rememberCorpus(absRepo, corpusTokens)
	recordSnapshot(ctx, absRepo, status.SnapshotValue{
		CorpusTokens:      corpusTokens,
		PriorCorpusTokens: priorCorpus,
		Append:            appendMode,
		Unchanged:         prevID != "" && prevID == snapshot.Meta.SnapshotID,
		ChangedFraction:   changedFraction(prevHashes, snapshot.Meta.FileHashes),
	})
	return snapshot, nil
}

// snapshotSummary is the part of a generate_snapshot answer every snapshot gets.
func (s *Server) snapshotSummary(snapshot *facts.Snapshot) string {
	summary := fmt.Sprintf(
		"Snapshot generated successfully.\n\n"+
			"- Repository: %s\n"+
			"- Facts: %d\n"+
			"- Insights: %d\n"+
			"- Artifacts: %d\n"+
			"- Duration: %s\n"+
			"- Extractors: %v\n"+
			"- Explainers: %v\n\n"+
			"Fetch the computed findings with query_insights (e.g. query_insights(explainer='unused-routes') for HTTP routes no loaded client calls); use query_facts or explore to inspect the raw facts.",
		snapshot.Meta.RepoPath,
		snapshot.Meta.FactCount,
		snapshot.Meta.InsightCount,
		len(snapshot.Artifacts),
		snapshot.Meta.Duration,
		snapshot.Meta.Extractors,
		snapshot.Meta.Explainers,
	)
	// A declared client nothing reads leaves its calls unresolved, so say so where the
	// agent reading the snapshot will see it, not only on the server's stderr.
	if notice := clientspec.UnsupportedNotice(s.cfg.Clients); notice != "" {
		summary += "\n\n**Declared clients skipped.** " + notice
	}
	if warning := unresolvedImportWarning(snapshot); warning != "" {
		summary += "\n\n" + warning
	}
	return summary
}

// unresolvedImportWarning promotes a suspiciously incomplete import graph into the
// snapshot response. The concentration requirement avoids warning on a healthy project
// that merely imports many unrelated third-party packages.
func unresolvedImportWarning(snapshot *facts.Snapshot) string {
	if snapshot == nil || snapshot.Meta.Unseen == nil {
		return ""
	}
	u := snapshot.Meta.Unseen
	total := u.OutsideGraph[facts.RelImports]
	if total < 25 || snapshot.Meta.FactCount > 0 && total*20 < snapshot.Meta.FactCount {
		return ""
	}
	prefix, count := "", 0
	for p, n := range u.OutsideGraphPrefixes {
		if n > count || n == count && p < prefix {
			prefix, count = p, n
		}
	}
	if prefix == "" || count*2 < total {
		return ""
	}
	local := false
	for _, f := range snapshot.Facts {
		file := strings.TrimPrefix(filepath.ToSlash(f.File), snapshot.Meta.Label()+"/")
		if strings.HasPrefix(file, prefix+"/") {
			local = true
			break
		}
	}
	if !local {
		return ""
	}
	return fmt.Sprintf(
		"**Graph coverage warning.** %d import targets are outside the graph; %d begin with `%s/`, which also exists as a local source path. This usually means a TypeScript path alias was not resolved. Impact and dependency answers for that area may be incomplete.",
		total, count, prefix,
	)
}

// multiRepoSummary is the part of a generate_snapshot answer a multi-repository store
// gets: the label to query by and the cross-repo graph.
func (s *Server) multiRepoSummary(snapshot *facts.Snapshot, autoNote string) string {
	// What the facts are ACTUALLY tagged with: this string is handed to the agent as
	// the value to pass to query_facts(repo=…), so a guess here sends it querying a
	// label nothing is stored under.
	repoLabel := snapshot.Meta.Label()
	summary := fmt.Sprintf(
		"\n\n**Multi-repo mode active%s.** Repo label: %q\n"+
			"- Filter by repo: query_facts(repo=%q)\n"+
			"- File paths are prefixed: e.g. %s/src/...\n"+
			"- Generate additional repos with append=true (sequentially, not in parallel).",
		autoNote, repoLabel, repoLabel, repoLabel,
	)

	// Report the cross-repo "graph of graphs" links derived from this set.
	crossEdges, _ := s.eng.Store().QueryAdvanced(facts.QueryOpts{
		Kind: facts.KindDependency, Prop: "type", PropValue: facts.TypeCrossRepo, Limit: 500,
	})
	services := s.eng.Store().ByKind(facts.KindService)
	summary += fmt.Sprintf(
		"\n- **Cross-repo graph:** %d service node(s), %d cross-repo dependency edge(s). "+
			"Traverse between repos with traverse(start=%q) / find_path, list edges with "+
			"query_facts(kind=\"service\") or query_facts(prop=\"type\", prop_value=\"cross_repo\").",
		len(services), len(crossEdges), repoLabel,
	)
	// Shared-code pairs are NOT edges (no depends_on relation, so traversal never
	// follows them). Surfaced separately so the signal is discoverable without being
	// mistaken for a dependency.
	sharedCode, _ := s.eng.Store().QueryAdvanced(facts.QueryOpts{
		Kind: facts.KindDependency, Prop: "type", PropValue: facts.TypeCrossRepoSharedCode, Limit: 500,
	})
	if len(sharedCode) > 0 {
		summary += fmt.Sprintf(
			"\n- **Shared code:** %d repo pair(s) declare many of the same type names with no "+
				"import or call between them, a maintenance signal rather than a dependency, so they carry "+
				"no graph edge. List them with query_facts(prop=\"type\", prop_value=%q).",
			len(sharedCode), facts.TypeCrossRepoSharedCode,
		)
	}
	return summary
}
