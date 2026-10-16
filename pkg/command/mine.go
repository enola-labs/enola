package command

import (
	"flag"
	"fmt"
	"os"

	"github.com/enola-labs/enola/internal/eslintscaffold"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/mining"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
)

func (r *Runner) ConstraintsMine(args []string) {
	fs := flag.NewFlagSet("constraints mine", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	defaults := mining.DefaultConfig()
	minSupport := fs.Int("min-support", defaults.MinSupport, "smallest denominator a candidate may rest on; families below it are suppressed and counted")
	minConfidence := fs.Float64("min-confidence", defaults.MinConfidence, "smallest regularity ratio a candidate may carry (0..1]")
	maxExceptions := fs.Int("max-exceptions", defaults.MaxExceptions, "most named exceptions a candidate may carry; families above it are suppressed and counted")
	includeTautologies := fs.Bool("include-tautologies", false, "also print candidates whose statement holds by construction; suppressed and counted by default")
	top := fs.Int("top", 50, "how many ranked candidates the text report prints (0 = all)")
	jsonlPath := fs.String("jsonl", "", "also write the full report as a JSONL artifact to this path")
	scaffoldDir := fs.String("scaffold-eslint", "", "write ESLint rule scaffolds (rule, RuleTester test, index.js) for the candidates a file-local rule can express into this directory; the rest are listed with the reason they stay constraint proposals")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" constraints mine [flags] [repo_path|config_path]\n\n"+
			"Mine the current snapshot's fact store for near-invariants — high-regularity\n"+
			"properties with named exceptions — and report them as candidate constraint\n"+
			"declarations. Every candidate carries its regularity as numerator/denominator,\n"+
			"names every exception, and renders a would-be rule `constraints lint` accepts.\n"+
			"Candidates are proposals for operator review: mining never writes a\n"+
			"declaration, never touches enola/constraints/, and never feeds the check path.\n"+
			"With --scaffold-eslint DIR, candidates whose statement is a file-local syntactic\n"+
			"check (a naming regularity over JS/TS declarations, a forbidden import between\n"+
			"two directories) are also written as ESLint rule scaffolds under DIR, so an\n"+
			"AST-shaped rule starts life in the linter and a graph-shaped one in enola.\n\n"+
			"Exit codes:\n"+
			"  0  report produced (even with zero candidates)\n"+
			"  2  the command could not run (no snapshot, bad flag)\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "constraints mine")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}
	if *minConfidence <= 0 || *minConfidence > 1 {
		r.constraintsFatal("--min-confidence %v is outside (0..1] — a floor of nothing admits everything, and one above 1 admits nothing", *minConfidence)
	}
	if *minSupport < 1 {
		r.constraintsFatal("--min-support %d must be at least 1", *minSupport)
	}
	if *maxExceptions < 0 {
		r.constraintsFatal("--max-exceptions %d must not be negative", *maxExceptions)
	}

	var arg string
	if rest := fs.Args(); len(rest) > 0 {
		arg = rest[0]
	}
	tgt := r.resolveTarget(arg)
	fmt.Fprintf(os.Stderr, r.name()+" constraints mine: %s\n", tgt.configNote)
	tgt.engine.SetPersistCache(false)

	outDir := tgt.engine.OutputDir(tgt.repoPaths[0])
	snap, err := bootstrap.LoadSnapshotDir(outDir)
	if err != nil {
		r.constraintsFatal("no snapshot at %s — mining reads an existing snapshot and never generates one; run `%s --generate` first (%v)", outDir, r.name(), err)
	}
	store := facts.NewStore()
	for _, f := range snap.Facts {
		if f.Kind == facts.KindIntent {
			continue
		}
		store.Add(f)
	}

	report := mining.Mine(store, mining.Config{
		MinSupport:         *minSupport,
		MinConfidence:      *minConfidence,
		MaxExceptions:      *maxExceptions,
		IncludeTautologies: *includeTautologies,
	})
	_, _ = fmt.Fprintf(os.Stdout, "Snapshot: %s\n", outDir)
	report.WriteText(os.Stdout, *top)

	if *jsonlPath != "" {
		f, err := os.Create(*jsonlPath)
		if err != nil {
			r.constraintsFatal("writing %s: %v", *jsonlPath, err)
		}
		if err := report.WriteJSONL(f); err != nil {
			_ = f.Close()
			r.constraintsFatal("writing %s: %v", *jsonlPath, err)
		}
		if err := f.Close(); err != nil {
			r.constraintsFatal("writing %s: %v", *jsonlPath, err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "\nJSONL artifact: %s\n", *jsonlPath)
	}
	if *scaffoldDir != "" {
		res, written, err := eslintscaffold.Write(*scaffoldDir, report.Candidates)
		if err != nil {
			r.constraintsFatal("writing ESLint scaffolds under %s: %v", *scaffoldDir, err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "\nESLint scaffolds: %d rule(s) under %s (%d files)\n", len(res.Scaffolds), *scaffoldDir, len(written))
		for _, sc := range res.Scaffolds {
			_, _ = fmt.Fprintf(os.Stdout, "  %s  <- %s\n", sc.RuleID, sc.Candidate.Statement)
		}
		if len(res.Skipped) > 0 {
			_, _ = fmt.Fprintf(os.Stdout, "Left as constraint proposals: %d\n", len(res.Skipped))
			for _, sk := range res.Skipped {
				_, _ = fmt.Fprintf(os.Stdout, "  %s: %s\n", sk.Identity, sk.Reason)
			}
		}
	}
	os.Exit(0)
}
