package command

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/check"
	"github.com/enola-labs/enola/pkg/plan"
)

func (r *Runner) Plan(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		pathsFlag   = fs.String("paths", "", "comma-separated repo-relative paths the intended change touches")
		symbolsFlag = fs.String("symbols", "", "comma-separated exact fact names the intended change touches")
		patchFlag   = fs.String("patch", "", "a unified diff file to evaluate counterfactually over a scratch copy")
		asJSON      = fs.Bool("json", false, "emit the report as JSON instead of text")
		noRadius    = fs.Bool("no-radius", false, "skip the re-evaluation that names the verdicts a move of each path would change")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" plan [flags] [path...] [repo_path|config_path]\n\n"+
			"The pre-edit contract: which declared constraints govern an intended change,\n"+
			"its blast radius over the current snapshot, the verdicts that would appear or\n"+
			"vanish if each path left every part (one re-evaluation over the loaded\n"+
			"snapshot), and, for a --patch, the constraint verdicts that WOULD appear,\n"+
			"evaluated over a scratch copy BEFORE any edit lands in the tree. The working tree and its snapshot are never\n"+
			"written; a report, never a gate.\n\n"+
			"Positional arguments naming an existing directory or a .yaml/.yml file pick\n"+
			"the repository or config (first match wins); every other positional is a\n"+
			"path target. Use --paths for a target that is itself a directory.\n\n"+
			"Exit codes:\n"+
			"  0  the report was produced (its verdicts are for the caller to weigh)\n"+
			"  2  the command could not run (bad patch, missing snapshot for --symbols,\n"+
			"     invalid declaration)\n\nFlags:\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "plan")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}

	repoArg, paths := splitPlanPositionals(fs.Args())
	paths = append(splitList(*pathsFlag), paths...)
	symbols := splitList(*symbolsFlag)

	var patch []byte
	if *patchFlag != "" {
		var err error
		patch, err = os.ReadFile(*patchFlag)
		if err != nil {
			r.planFatal("reading the patch: %v", err)
		}
	}
	if len(paths) == 0 && len(symbols) == 0 && len(patch) == 0 {
		fs.Usage()
		os.Exit(check.StatusUsageError.ExitCode())
	}

	tgt := r.resolveTarget(repoArg)
	fmt.Fprintf(os.Stderr, r.name()+" plan: %s\n", tgt.configNote)
	eng := tgt.engine
	eng.SetPersistCache(false)
	anchor := tgt.repoPaths[0]
	label := filepath.Base(anchor)
	outDir := eng.OutputDir(anchor)

	info := plan.SnapshotInfo{}
	var measured []facts.Fact
	snap, snapErr := bootstrap.LoadSnapshotDir(outDir)
	switch {
	case snapErr != nil && len(symbols) > 0:
		r.planFatal("symbol targets resolve against a snapshot, and none exists at %s — run `%s baseline pin %s` first", outDir, r.name(), anchor)
	case snapErr != nil:
		info.Note = fmt.Sprintf("no snapshot at %s — governance answers from the declarations alone; blast radius is unmeasured", outDir)
	default:
		measured = snap.Facts
		info.GeneratedAt = snap.Meta.GeneratedAt
		if err := eng.RestoreFromDir(outDir, map[string]string{label: anchor}, label); err == nil {
			if d, dErr := eng.Drift(anchor); dErr == nil && (d.Unknown || d.Any()) {
				info.Staleness = d.Summary(5)
			}
		}
	}

	store, err := plan.ContractStore(anchor, measured, eng.Config().Intent[label])
	if err != nil {
		r.planFatal("%v", err)
	}

	deps := plan.Deps{
		RepoPath:      anchor,
		RepoLabel:     label,
		Store:         store,
		Snapshot:      info,
		OutputDirName: eng.Config().Output.Dir,
		SkipRadius:    *noRadius,
	}
	if len(patch) > 0 {
		cfgPath := tgt.cfgPath
		deps.NewEngine = func() (plan.Generator, error) {
			counterEng, _, err := r.newEngine(bootstrap.Options{ConfigPath: cfgPath})
			if err != nil {
				return nil, err
			}
			counterEng.SetPersistCache(false)
			return counterEng, nil
		}
	}

	report, err := plan.Compute(ctx, plan.Request{Paths: paths, Symbols: symbols, Patch: patch}, deps)
	if err != nil {
		r.planFatal("%v", err)
	}
	if *asJSON {
		out, err := report.JSON()
		if err != nil {
			r.planFatal("encoding the report: %v", err)
		}
		fmt.Println(string(out))
		return
	}
	fmt.Print(report.Render())
}

func splitPlanPositionals(args []string) (repoArg string, paths []string) {
	for _, a := range args {
		if repoArg == "" && (isDirectory(a) || (fileExists(a) && (strings.HasSuffix(a, ".yaml") || strings.HasSuffix(a, ".yml")))) {
			repoArg = a
			continue
		}
		paths = append(paths, a)
	}
	return repoArg, paths
}

func (r *Runner) planFatal(format string, args ...any) {
	r.cmdFatal("plan", format, args...)
}
