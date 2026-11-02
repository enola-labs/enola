package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/touched"
	"github.com/enola-labs/enola/pkg/check"
)

// Touched is `enola touched`: which symbols a run of commits changed, and which of them
// changed most often. It extracts only the files each commit changed, so a long history
// costs what its commits changed and not a full snapshot per commit.
func (r *Runner) Touched(ctx context.Context, args []string) {
	os.Exit(r.runTouched(ctx, args, os.Stdout, os.Stderr))
}

func (r *Runner) runTouched(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usageError := check.StatusUsageError.ExitCode()
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, r.name()+" touched: "+format+"\n", args...)
	}

	fs := flag.NewFlagSet("touched", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		rev        = fs.String("rev", "HEAD", "the history to walk")
		since      = fs.String("since", "", "only commits after this date, as git log --since reads it")
		until      = fs.String("until", "", "only commits before this date, as git log --until reads it")
		subject    = fs.String("subject", "", "only commits whose subject matches this regular expression, for example (?i)^fix\\b")
		maxSymbols = fs.Int("max-symbols", 0, "leave a commit changing more callables than this out of the ranking (0 keeps every commit)")
		grain      = fs.String("grain", "member", "member credits the innermost symbol, declaration credits a function in a returned object to the function returning it")
		top        = fs.Int("top", 30, "how many ranked callables to report (0 for all)")
		asJSON     = fs.Bool("json", false, "emit the result as JSON, with every selected commit")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(stderr,
			"Usage: "+r.name()+" touched [flags] [repo_path|config_path]\n\n"+
				"Which symbols a run of commits changed, and which callables changed most often.\n"+
				"Merges are skipped and the first-parent line is followed. Each commit's new side\n"+
				"is mapped to symbols as locate --diff maps a diff, against the tree of that\n"+
				"commit. Nothing is written to the repository and no snapshot is taken.\n\n"+
				"Exit codes:\n"+
				"  0  the walk ran\n"+
				"  2  the command could not run\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "touched")
	}
	if err := fs.Parse(args); err != nil {
		return usageError
	}
	positional := fs.Args()
	if len(positional) > 1 || *maxSymbols < 0 || *top < 0 || (*grain != "member" && *grain != "declaration") {
		fs.Usage()
		return usageError
	}
	repoArg := ""
	if len(positional) == 1 {
		repoArg = positional[0]
	}
	opts := touched.Options{Rev: *rev, Since: *since, Until: *until, MaxSymbols: *maxSymbols, Declarations: *grain == "declaration"}
	if *subject != "" {
		re, err := regexp.Compile(*subject)
		if err != nil {
			say("--subject: %v", err)
			return usageError
		}
		opts.Subject = re
	}

	tgt := r.resolveTarget(repoArg)
	say("%s", tgt.configNote)
	if len(tgt.repoPaths) != 1 {
		say("walks one repository, and this target names %d", len(tgt.repoPaths))
		return usageError
	}
	repoPath := tgt.repoPaths[0]
	if !isGitRepo(repoPath) {
		say("%s is not a git repository, so there is no history to walk", repoPath)
		return usageError
	}
	label, err := engine.RepoLabel(repoPath)
	if err != nil {
		say("%v", err)
		return usageError
	}
	res, err := touched.Run(ctx, repoPath, opts, func(ctx context.Context, root string, files []string) ([]facts.Fact, error) {
		return tgt.engine.ExtractFiles(ctx, root, label, files)
	})
	if err != nil {
		say("%v", err)
		return usageError
	}
	if *top > 0 && len(res.Ranking) > *top {
		res.Ranking = res.Ranking[:*top]
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			say("encoding the result: %v", err)
			return usageError
		}
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "%d commit(s), %d in the ranking.\n\n", len(res.Commits), res.RankedCommits)
	for _, e := range res.Ranking {
		_, _ = fmt.Fprintf(stdout, "%5d  %s  %s\n", e.Count, e.Name, e.File)
	}
	return 0
}
