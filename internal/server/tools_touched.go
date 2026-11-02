package server

import (
	"context"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/touched"
)

type touchedArgs struct {
	RepoPath   string `json:"repo_path,omitempty" jsonschema:"default: the snapshot's"`
	Since      string `json:"since,omitempty" jsonschema:"as git log --since"`
	Until      string `json:"until,omitempty"`
	Subject    string `json:"subject,omitempty" jsonschema:"regexp, e.g. (?i)\\bfix"`
	Exclude    string `json:"exclude,omitempty"`
	MaxSymbols int    `json:"max_symbols,omitempty" jsonschema:"skip commits changing more"`
	Top        int    `json:"top,omitempty" jsonschema:"default 30"`
	Commits    bool   `json:"commits,omitempty"`
}

// touchedNote travels with the ranking because it can change who comes first, and a session
// comparing against its own count would otherwise not know why they differ.
const touchedNote = "A function nested inside another has no row: its commits count for the enclosing one."

// registerTouchedTool registers touched, the question of which code a run of commits
// changed. It reads git history and needs no snapshot of it.
func (s *Server) registerTouchedTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "touched",
		Description: "Symbols commits changed, ranked by count. From git history; long ranges take minutes.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args touchedArgs) (*mcp.CallToolResult, any, error) {
		repo := args.RepoPath
		if repo == "" {
			repo = s.currentRepoPath()
		}
		if repo == "" {
			return errorResult("no repository to walk: run generate_snapshot first, or pass repo_path"), nil, nil
		}
		opts := touched.Options{Since: args.Since, Until: args.Until, MaxSymbols: args.MaxSymbols}
		if args.Subject != "" {
			re, err := regexp.Compile(args.Subject)
			if err != nil {
				return errorResult("subject: " + err.Error()), nil, nil
			}
			opts.Subject = re
		}
		if args.Exclude != "" {
			re, err := regexp.Compile(args.Exclude)
			if err != nil {
				return errorResult("exclude: " + err.Error()), nil, nil
			}
			opts.Exclude = re
		}
		label, err := engine.RepoLabel(repo)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		res, err := touched.Run(ctx, repo, opts, func(ctx context.Context, root string, files []string) ([]facts.Fact, error) {
			return s.eng.ExtractFiles(ctx, root, label, files)
		})
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}
		top := args.Top
		if top <= 0 {
			top = 30
		}
		if len(res.Ranking) > top {
			res.Ranking = res.Ranking[:top]
		}
		// Every row's commits are most of the result's size, and a session pays for it again
		// on each later turn. The ranking is the answer; the commits are for following one up.
		if !args.Commits {
			for i := range res.Ranking {
				res.Ranking[i].Commits = nil
			}
		}
		return jsonResult(struct {
			Commits       int              `json:"commits"`
			Note          string           `json:"note"`
			RankedCommits int              `json:"ranked_commits"`
			Symbols       int              `json:"symbols"`
			Ranking       []touched.Ranked `json:"ranking,omitempty"`
		}{len(res.Commits), touchedNote, res.RankedCommits, res.Symbols, res.Ranking})
	})
}
