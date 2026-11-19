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
	Repo       string `json:"repo,omitempty" jsonschema:"label, not repo_path"`
	Since      string `json:"since,omitempty" jsonschema:"as git log --since"`
	Until      string `json:"until,omitempty"`
	Subject    string `json:"subject,omitempty" jsonschema:"regexp, e.g. (?i)\\bfix"`
	Exclude    string `json:"exclude,omitempty"`
	MaxSymbols int    `json:"max_symbols,omitempty" jsonschema:"skip commits changing more"`
	Top        int    `json:"top,omitempty" jsonschema:"default 30"`
	Commits    bool   `json:"commits,omitempty"`
}

// touchedCommit is one commit as touched lists it: its subject and what it changed.
type touchedCommit struct {
	SHA     string   `json:"sha"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
	Files   []string `json:"files,omitempty"`
	Symbols []string `json:"symbols,omitempty"`
}

// touchedNote travels with the ranking because it can change who comes first, and a session
// comparing against its own count would otherwise not know why they differ.
const touchedNote = "A nested function with no row of its own counts for the enclosing one."

// registerTouchedTool registers touched, the question of which code a run of commits
// changed. It reads git history and needs no snapshot of it.
func (s *Server) registerTouchedTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "touched",
		Description: "Symbols commits changed, ranked by count, from git history. Long ranges are slow.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args touchedArgs) (*mcp.CallToolResult, any, error) {
		repo := args.RepoPath
		if repo == "" && args.Repo != "" {
			paths := s.eng.RepoPaths()
			repo = paths[args.Repo]
			if repo == "" {
				return errorResult("repo " + args.Repo + " is not a repository of the loaded graph"), nil, nil
			}
		}
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
		extract := s.eng.ExtractFiles
		if s.roots != nil {
			if err := s.roots.Confine(repo); err != nil {
				return errorResult(err.Error()), nil, nil
			}
			// repo_path was confined by the serve middleware; the scratch tree replaying
			// its history is enola's own, in the temp directory, and kept free of links.
			opts.NoSymlinks = true
			extract = s.eng.ExtractScratchFiles
		}
		res, err := touched.Run(ctx, repo, opts, func(ctx context.Context, root string, files []string) ([]facts.Fact, error) {
			return extract(ctx, root, label, files)
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
		// With commits asked for, each commit is listed too, newest first: what a change
		// was called and which functions it changed is what someone following up on
		// "the latest change" reads first, and per-row SHAs alone cannot say that.
		var list []touchedCommit
		if args.Commits {
			for i := len(res.Commits) - 1; i >= 0 && len(list) < top; i-- {
				c := res.Commits[i]
				row := touchedCommit{SHA: c.SHA, Date: c.Date, Subject: c.Subject, Files: c.Files}
				for _, sym := range c.Symbols {
					row.Symbols = append(row.Symbols, sym.Name)
				}
				list = append(list, row)
			}
		}
		return jsonResult(struct {
			Commits       int              `json:"commits"`
			Note          string           `json:"note"`
			RankedCommits int              `json:"ranked_commits"`
			Symbols       int              `json:"symbols"`
			Ranking       []touched.Ranked `json:"ranking,omitempty"`
			CommitList    []touchedCommit  `json:"commit_list,omitempty"`
		}{len(res.Commits), touchedNote, res.RankedCommits, res.Symbols, res.Ranking, list})
	})
}
