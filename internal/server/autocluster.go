package server

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

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

	set, failed, err := s.indexSet(ctx, repoPaths, false)
	if err != nil {
		return errorResult(fmt.Sprintf("snapshot generation failed for %s: %v", failed, err)), nil, nil
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

	summary := lead.String() + s.setSummary(set) +
		s.multiRepoSummary(set.union(), " (folder of repositories)", set.labels())
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: summary}}}, nil, nil
}

// indexSet indexes repoPaths into the store as one linked set, linking once, on the
// last: every repository before it defers linking. appendFirst keeps what the store
// already holds; otherwise the first repository resets it. On failure it returns the
// repository that failed. Artifacts and the global receipt are written once, at the end.
// The caller holds genMu.
func (s *Server) indexSet(ctx context.Context, repoPaths []string, appendFirst bool) (indexedSet, string, error) {
	if !appendFirst {
		s.resetCorpus()
	}
	defer s.eng.SetDeferLinking(false)
	set := indexedSet{repoPaths: repoPaths}
	start := time.Now()
	for i, repo := range repoPaths {
		s.eng.SetDeferLinking(i < len(repoPaths)-1)
		snap, err := s.indexRepo(ctx, repo, appendFirst || i > 0)
		if err != nil {
			return indexedSet{}, repo, err
		}
		set.snapshots = append(set.snapshots, snap)
	}
	set.duration = time.Since(start)
	for _, repo := range repoPaths {
		if err := s.eng.WriteArtifacts(repo); err != nil {
			log.Printf("[server] warning: failed to write artifacts for %s: %v", repo, err)
		}
	}
	if err := s.eng.WriteGlobalReceipt(); err != nil {
		log.Printf("[server] warning: failed to write global receipt: %v", err)
	}
	return set, "", nil
}

// indexedSet is what indexSet produced: each repository's snapshot, in order, and the
// wall time of the whole set. The last snapshot is the linked union.
type indexedSet struct {
	repoPaths []string
	snapshots []*facts.Snapshot
	duration  time.Duration
}

func (set indexedSet) union() *facts.Snapshot { return set.snapshots[len(set.snapshots)-1] }

// labels are the labels the set's facts are actually tagged with, in order.
func (set indexedSet) labels() []string {
	out := make([]string, len(set.snapshots))
	for i, snap := range set.snapshots {
		out[i] = snap.Meta.Label()
	}
	return out
}

// summary is the snapshot part of the answer for a set. It names every repository,
// not the last one: the union's own meta describes the repository indexed last, and
// the answer used to read "Repository: <last>" with that one's duration and
// extractors, under a headline saying five were indexed.
func (s *Server) setSummary(set indexedSet) string {
	lines := make([]string, len(set.repoPaths))
	extractors := map[string]bool{}
	for i, p := range set.repoPaths {
		lines[i] = fmt.Sprintf("%s (%s)", facts.RepoDirName(p), p)
		for _, e := range set.snapshots[i].Meta.Extractors {
			extractors[e] = true
		}
	}
	used := make([]string, 0, len(extractors))
	for e := range extractors {
		used = append(used, e)
	}
	sort.Strings(used)
	repository := fmt.Sprintf("Repositories (%d): %s", len(set.repoPaths), strings.Join(lines, ", "))
	out := s.snapshotSummaryOf(set.union(), repository, set.duration.Round(time.Millisecond).String(), used)
	// The union's meta carries the last repository's import graph; each one's is its own.
	for _, snap := range set.snapshots[:len(set.snapshots)-1] {
		if warning := unresolvedImportWarning(snap); warning != "" {
			out += "\n\n" + warning
		}
	}
	return out
}

// generateSet indexes the repositories an agent listed in repo_paths, in one call. It is
// generateCluster without the folder: the list is the cluster. The caller holds genMu.
func (s *Server) generateSet(ctx context.Context, repoPaths []string, appendMode bool) (*mcp.CallToolResult, any, error) {
	set, failed, err := s.indexSet(ctx, repoPaths, appendMode)
	if err != nil {
		return errorResult(fmt.Sprintf("snapshot generation failed for %s: %v", failed, err)), nil, nil
	}
	lead := fmt.Sprintf("**Indexed %d repositories in one call (%s).** Each one is a service node and the calls between them are linked.",
		len(repoPaths), strings.Join(set.labels(), ", "))
	if appendMode {
		lead += " They were added to the repositories already loaded."
	}
	summary := lead + "\n\n---\n\n" + s.setSummary(set) + s.multiRepoSummary(set.union(), "", set.labels())
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
	return s.snapshotSummaryOf(snapshot, "Repository: "+snapshot.Meta.RepoPath, snapshot.Meta.Duration, snapshot.Meta.Extractors)
}

// snapshotSummaryOf is snapshotSummary with the repository line (label included),
// duration and extractors given, which a set of repositories reports for all of them.
func (s *Server) snapshotSummaryOf(snapshot *facts.Snapshot, repository, duration string, extractors []string) string {
	summary := fmt.Sprintf(
		"Snapshot generated successfully.\n\n"+
			"- %s\n"+
			"- Facts: %d\n"+
			"- Insights: %d\n"+
			"- Artifacts: %d\n"+
			"- Duration: %s\n"+
			"- Extractors: %v\n"+
			"- Explainers: %v\n\n"+
			"Fetch the computed findings with query_insights (e.g. query_insights(explainer='unused-routes') for HTTP routes no loaded client calls); use query_facts or explore to inspect the raw facts.",
		repository,
		snapshot.Meta.FactCount,
		snapshot.Meta.InsightCount,
		len(snapshot.Artifacts),
		duration,
		extractors,
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
//
// labels, when it names more than one repository, is the set just indexed: the answer
// lists them all and gives the first as the example, rather than presenting the
// last-indexed one as the store's label.
func (s *Server) multiRepoSummary(snapshot *facts.Snapshot, autoNote string, labels []string) string {
	// What the facts are ACTUALLY tagged with: this string is handed to the agent as
	// the value to pass to query_facts(repo=…), so a guess here sends it querying a
	// label nothing is stored under.
	repoLabel := snapshot.Meta.Label()
	labelLine := fmt.Sprintf("Repo label: %q", repoLabel)
	if len(labels) > 1 {
		repoLabel = labels[0]
		labelLine = "Repo labels: " + strings.Join(labels, ", ")
	}
	summary := fmt.Sprintf(
		"\n\n**Multi-repo mode active%s.** %s\n"+
			"- Filter by repo: query_facts(repo=%q)\n"+
			"- File paths are prefixed: e.g. %s/src/...\n"+
			"- Add more repos with generate_snapshot(repo_paths=[...], append=true), all in one call.",
		autoNote, labelLine, repoLabel, repoLabel,
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
