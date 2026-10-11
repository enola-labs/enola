package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	RepoPath string `json:"repo_path,omitempty" jsonschema:"Repository whose history to read. Defaults to the snapshot's repo."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum revisions to return, newest kept. Default 20; 0 for all."`
	All      bool   `json:"all,omitempty" jsonschema:"Include working revisions (snapshots of uncommitted trees). Default false."`
}

type blameArgs struct {
	Pattern  string `json:"pattern" jsonschema:"A module or symbol name, a file path, or both endpoints of an edge (e.g. 'internal/server -> internal/facts'). Case-insensitive."`
	RepoPath string `json:"repo_path,omitempty" jsonschema:"Repository whose history to search. Defaults to the snapshot's repo."`
	Findings bool   `json:"findings,omitempty" jsonschema:"Search recorded FINDINGS instead of facts, e.g. when a cycle first appeared. Default false."`
	First    bool   `json:"first,omitempty" jsonschema:"Stop at the first appearance, for 'when was this introduced'. Default false."`
}

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
		entries, _, err := s.readHistory(args.RepoPath)
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
			return textResult("No committed revisions recorded yet. Pass all=true to include snapshots of uncommitted trees."), nil, nil
		}
		return textResult(renderHistoryEntries(entries)), nil, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "architecture_blame",
		Description: "WHEN something entered or left the architecture: \"when did this module start importing that one?\", \"which snapshot introduced this cycle?\". " +
			"It reads what the graph held at each recorded point instead of inferring it from source history.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args blameArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Pattern) == "" {
			return errorResult("architecture_blame needs a pattern — a module or symbol name, a file path, or an edge."), nil, nil
		}
		entries, root, err := s.readHistory(args.RepoPath)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		b, err := history.BlameLines(root, entries, args.Pattern, history.BlameOptions{
			Findings: args.Findings, FirstOnly: args.First,
		})
		if err != nil {
			return errorResult(fmt.Sprintf("blame failed: %v", err)), nil, nil
		}
		return textResult(renderBlameResult(b)), nil, nil
	})
}

// readHistory resolves the repository and loads its recorded entries.
func (s *Server) readHistory(repoPath string) ([]history.Entry, string, error) {
	if repoPath == "" {
		repoPath = s.currentRepoPath()
	}
	if repoPath == "" {
		return nil, "", errors.New("no repository to read a history for — run generate_snapshot first, or pass repo_path")
	}
	root, err := history.Root(repoPath, s.cfg.History.Dir)
	if err != nil {
		return nil, "", fmt.Errorf("cannot locate the history: %w", err)
	}
	entries, err := history.Read(root)
	if err != nil {
		if errors.Is(err, history.ErrNoHistory) {
			return nil, "", fmt.Errorf("no architecture history recorded for %s yet — it accumulates as generate_snapshot runs", repoPath)
		}
		return nil, "", err
	}
	// Timeline order — see history.SortedByTime.
	return history.SortedByTime(entries), root, nil
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
func renderBlameResult(b *history.Blame) string {
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

	for _, ev := range b.Events {
		fmt.Fprintf(&out, "%s  %s  %s\n", ev.Entry.Short(), ev.Entry.At, ev.Entry.Commit())
		for _, l := range ev.Added {
			fmt.Fprintf(&out, "  + %s\n", l)
		}
		for _, l := range ev.Removed {
			fmt.Fprintf(&out, "  - %s\n", l)
		}
	}

	fmt.Fprintf(&out, "\nSearched %d revision(s).", b.Scanned)
	if b.Skipped > 0 {
		fmt.Fprintf(&out, " %d could NOT be searched — their stored contents have aged out, so anything "+
			"that appeared and vanished within them is invisible here; treat 'not found' as 'not found in what was readable'.", b.Skipped)
	}
	out.WriteByte('\n')
	return out.String()
}
