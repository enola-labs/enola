package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/history"
)

// The history tools answer questions about the PAST, which every other tool in this server
// structurally cannot.
//
// generate_snapshot, query_facts, traverse and diff_snapshot all describe the tree as it is
// now (diff_snapshot compares two nows). "When did this coupling appear?" and "which change
// introduced this cycle?" are different questions, and an agent that cannot ask them
// re-derives the answer by reading git and guessing — expensively, and without the graph.
type historyArgs struct {
	RepoPath string `json:"repo_path,omitempty" jsonschema:"Repo or cluster (config or folder) to read. Default: loaded graph."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum revisions to return, newest kept. Default 20; 0 for all."`
	All      bool   `json:"all,omitempty" jsonschema:"Include working revisions (snapshots of uncommitted trees). Default false."`
}

type blameArgs struct {
	Pattern  string `json:"pattern" jsonschema:"A module or symbol name, a file path, or both endpoints of an edge (e.g. 'internal/server -> internal/facts'). Case-insensitive."`
	RepoPath string `json:"repo_path,omitempty" jsonschema:"Repo or cluster (config or folder) to search. Default: loaded graph."`
	Findings bool   `json:"findings,omitempty" jsonschema:"Search recorded FINDINGS instead of facts, e.g. when a cycle first appeared. Default false."`
	First    bool   `json:"first,omitempty" jsonschema:"Stop at the first appearance, for 'when was this introduced'. Default false."`
}

// defaultBlameLines bounds an architecture_blame answer. A pattern as broad as a repository
// name matches every fact in it: on a 45k-fact frontend that was 5 MB of text, which the
// client dropped, and which an agent could not have read anyway.
// The cap is not a parameter: the remedy for a pattern that matches too much is a
// narrower pattern, and every parameter is paid for in every session's tool list.
const maxBlameLines = 100

// registerHistoryTools adds the timeline tools. They are registered even when no history has
// been recorded: the tool then explains that, which is a better answer than a missing
// capability an agent cannot ask about.
func (s *Server) registerHistoryTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "architecture_history",
		Description: "How the architecture CHANGED OVER TIME: one entry per recorded snapshot, oldest first, with what moved since the one before (revision id, time taken, git commit and branch, counts of facts, edges and findings that moved). " +
			"Every other tool describes the tree as it is now; this one answers about the past, so prefer it over reading git log and guessing. " +
			"generate_snapshot records history automatically, so a repository never snapshotted has none yet.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args historyArgs) (*mcp.CallToolResult, any, error) {
		entries, _, note, err := s.readHistory(args.RepoPath)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		if !args.All {
			entries = committedOnly(entries)
		}
		limit := args.Limit
		if limit == 0 {
			limit = 20
		}
		if limit > 0 && len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		if len(entries) == 0 {
			return textResult(note + "No committed revisions recorded yet. Pass all=true to include snapshots of uncommitted trees."), nil, nil
		}
		return textResult(note + renderHistoryEntries(entries)), nil, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "architecture_blame",
		Description: "WHEN something entered or left the architecture: \"when did this module start importing that one?\", \"which snapshot introduced this cycle?\". " +
			"It reads what the graph held at each recorded point instead of inferring it from source history. " +
			"A symbol that shifted, was rewritten or moved is one change, not a removal and an addition.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args blameArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Pattern) == "" {
			return errorResult("architecture_blame needs a pattern — a module or symbol name, a file path, or an edge."), nil, nil
		}
		entries, root, note, err := s.readHistory(args.RepoPath)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		b, err := history.BlameLines(root, entries, args.Pattern, history.BlameOptions{
			Findings: args.Findings, FirstOnly: args.First,
		})
		if err != nil {
			return errorResult(fmt.Sprintf("blame failed: %v", err)), nil, nil
		}
		return textResult(note + renderBlameResult(b, maxBlameLines)), nil, nil
	})
}

// readHistory resolves the repository and loads its recorded entries. note is a line to
// put before the answer when the target is a cluster, whose history is its own: the
// snapshots of all its members together, which no member's history holds.
func (s *Server) readHistory(repoPath string) (entries []history.Entry, root, note string, err error) {
	historyDir := s.cfg.History.Dir
	var members []string
	source := ""
	if repoPath == "" && s.eng != nil {
		// A loaded multi-repo graph is a cluster whatever path its meta names.
		if paths := s.eng.RepoPaths(); len(paths) > 1 {
			for _, p := range paths {
				members = append(members, p)
			}
			sort.Strings(members)
			source = "the loaded graph"
		}
	}
	if repoPath == "" && len(members) == 0 {
		repoPath = s.currentRepoPath()
	}
	if repoPath == "" && len(members) == 0 {
		return nil, "", "", errors.New("no repository to read a history for — run generate_snapshot first, or pass repo_path")
	}
	if len(members) == 0 {
		var dir string
		members, source, dir, err = clusterMembers(repoPath)
		if err != nil {
			return nil, "", "", err
		}
		if dir != "" {
			historyDir = dir
		}
	}

	if len(members) > 0 {
		names := make([]string, len(members))
		for i, m := range members {
			names[i] = filepath.Base(m)
		}
		note = fmt.Sprintf("Cluster %s (members: %s): its own history, of snapshots of all members together. A member's path reads that member alone.\n\n",
			source, strings.Join(names, ", "))
		root, err = history.ClusterRoot(members, historyDir)
	} else {
		root, err = history.Root(repoPath, historyDir)
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("cannot locate the history: %w", err)
	}
	entries, err = history.Read(root)
	if err != nil {
		if errors.Is(err, history.ErrNoHistory) {
			if len(members) > 0 {
				return nil, "", "", fmt.Errorf("%sno cluster history recorded yet — it accumulates as generate_snapshot runs over the cluster. "+
					"Cluster snapshots from before enola kept one history per cluster were recorded into each member; pass a member's path to read them", note)
			}
			return nil, "", "", fmt.Errorf("no architecture history recorded for %s yet — it accumulates as generate_snapshot runs", repoPath)
		}
		return nil, "", "", err
	}
	// Timeline order — see history.SortedByTime.
	return history.SortedByTime(entries), root, note, nil
}

// clusterMembers returns the repositories path stands for when it is a folder of
// repositories: those its cluster config lists (with that config's history.dir), or the
// git repositories under it. Empty for a repository.
func clusterMembers(path string) (members []string, source, historyDir string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", "", nil
	}
	cfgPath, kids := workspace.Cluster(abs)
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		// A config file names its repositories itself; one without repos: is not a cluster.
		if cfg, err := config.Load(abs); err != nil || len(cfg.Repos) == 0 {
			return nil, "", "", nil
		}
		cfgPath = abs
	}
	if cfgPath != "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return nil, "", "", fmt.Errorf("reading %s: %w", cfgPath, err)
		}
		if len(cfg.Repos) == 0 {
			return nil, "", "", fmt.Errorf("%s has an empty repos list", cfgPath)
		}
		members, err := cfg.RepoPaths()
		if err != nil {
			return nil, "", "", fmt.Errorf("resolving the repositories of %s: %w", cfgPath, err)
		}
		return members, cfgPath, cfg.History.Dir, nil
	}
	for _, k := range kids {
		members = append(members, filepath.Join(abs, k))
	}
	return members, abs, "", nil
}

func committedOnly(entries []history.Entry) []history.Entry {
	out := make([]history.Entry, 0, len(entries))
	for _, e := range entries {
		if !e.Working() {
			out = append(out, e)
		}
	}
	return out
}

// renderHistoryEntries formats the timeline for an agent: one line per revision, oldest
// first, with the epoch seam called out where enola itself changed.
func renderHistoryEntries(entries []history.Entry) string {
	var b strings.Builder
	b.WriteString("Architecture history (oldest first):\n\n")
	prevEpoch := ""
	for i, e := range entries {
		if i > 0 && e.Epoch != prevEpoch {
			b.WriteString("  -- enola itself changed here (version, config or extractors); " +
				"the delta below is rebuild noise, not somebody's edit --\n")
		}
		prevEpoch = e.Epoch

		commit := e.Commit()
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fmt.Fprintf(&b, "%-7s  %s  %-12s %-24s %s", e.Short(), e.At, commit, e.Ref(), e.Summary.Headline())
		if e.Working() {
			b.WriteString("  [uncommitted tree]")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderBlameResult formats a blame for an agent, leading with the answer.
func renderBlameResult(b *history.Blame, maxLines int) string {
	var out strings.Builder

	if intro, ok := b.Introduced(); ok {
		fmt.Fprintf(&out, "First appeared in revision %s (%s", intro.Short(), intro.At)
		if c := intro.Commit(); c != "" {
			fmt.Fprintf(&out, ", commit %s", c)
		}
		out.WriteString(").\n\n")
	} else if len(b.Events) == 0 {
		fmt.Fprintf(&out, "Nothing matching %q in %d searched revision(s).\n", b.Pattern, b.Scanned)
	}

	listed, total := 0, 0
	emit := func(line string) {
		total++
		if listed < maxLines {
			out.WriteString(line)
			listed++
		}
	}
	restEvents := 0
	for _, ev := range b.Events {
		if listed >= maxLines {
			// Past the cap a revision header alone is still a line per revision; count them.
			restEvents++
			total += len(ev.Added) + len(ev.Removed) + len(ev.Changed)
			continue
		}
		fmt.Fprintf(&out, "%s  %s  %s\n", ev.Entry.Short(), ev.Entry.At, ev.Entry.Commit())
		startListed, startTotal := listed, total
		for _, l := range ev.Added {
			emit("  + " + history.ShortBlameLine(l) + "\n")
		}
		for _, l := range ev.Removed {
			emit("  - " + history.ShortBlameLine(l) + "\n")
		}
		for _, p := range ev.Changed {
			emit(fmt.Sprintf("  ~ %-9s %s\n", p.Kind, history.ShortBlamePair(p)))
		}
		if hidden := (total - startTotal) - (listed - startListed); hidden > 0 {
			fmt.Fprintf(&out, "  … %d more in this revision\n", hidden)
		}
	}

	if restEvents > 0 {
		fmt.Fprintf(&out, "… and %d later revision(s) with matching changes.\n", restEvents)
	}

	fmt.Fprintf(&out, "\nSearched %d revision(s).", b.Scanned)
	if listed < total {
		fmt.Fprintf(&out, " Listed %d of %d matching facts; narrow the pattern to a symbol, a file path, or an edge 'a -> b'.", listed, total)
	}
	if b.Skipped > 0 {
		fmt.Fprintf(&out, " %d could NOT be searched — their stored contents have aged out, so anything "+
			"that appeared and vanished within them is invisible here; treat 'not found' as 'not found in what was readable'.", b.Skipped)
	}
	out.WriteByte('\n')
	return out.String()
}
