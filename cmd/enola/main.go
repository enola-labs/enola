package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/updatecheck"
	"github.com/enola-labs/enola/internal/upgrade"
	"github.com/enola-labs/enola/internal/version"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/cli"
	"github.com/enola-labs/enola/pkg/command"
	"github.com/enola-labs/enola/pkg/dashboard"
	"github.com/enola-labs/enola/pkg/explain"
	"github.com/enola-labs/enola/pkg/status"
)

func main() {
	log.SetOutput(os.Stderr)
	bootstrap.ConfigureRuntime()

	// A terminated server must still tidy up after itself — remove its instance
	// record and flush its counters — so cancel the server's context on a signal
	// instead of dying mid-run. Run returns, and the deferred cleanup happens.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Memory instrumentation, removed from the argument list before anything else
	// parses it. It has to come off the front because --memprofile takes a VALUE,
	// and both the subcommand dispatcher and the flag loop below are exact-match
	// switches that would read the path as a repository or reject it as a typo.
	args, memWatch := startMemWatch(os.Args[1:])

	// Subcommands are dispatched before the flag loop below, which is an exact-match
	// switch over os.Args and cannot parse `--flag=value`. Each subcommand owns its own
	// FlagSet instead.
	//
	// `upgrade` is handled here rather than in pkg/command because it is OSS-only: a
	// wrapper binary ships through its own release path and must not offer to replace
	// itself with an enola build. It is still declared to the Runner (below) so a typo
	// like `enola upgrad` is recognised as a near-miss rather than as an unknown word.
	if len(args) > 0 && args[0] == "upgrade" {
		if err := upgrade.Run(ctx, version.Version); err != nil {
			log.Fatalf("upgrade failed: %v", err)
		}
		os.Exit(0)
	}
	cmds := command.New(binary(), "upgrade")
	cmds.Dispatch(ctx, args) // returns only when this was not a subcommand

	generateMode := false
	refreshMode := false
	explainMode := false
	statusMode := false
	statusAll := false
	noDashboard := false
	cfgPath := "mcp-arch.yaml"
	repoArg := "" // optional positional repo path, for --explain and --generate

	// --json qualifies --version rather than standing alone, so it has to be known
	// BEFORE the loop below reaches --version and exits. The loop is an exact-match
	// switch that acts on the first flag it recognises, which makes it order-sensitive
	// for every flag pairing; scanning once up front is what keeps `--version --json`
	// and `--json --version` the same command.
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" {
			jsonMode = true
		}
	}

	for _, arg := range args {
		switch arg {
		case "--version":
			// The JSON form is the release manifest, byte for byte: the release
			// workflow runs this on the artifact it just built and publishes the
			// output. Generating it FROM the binary being shipped is what stops the
			// manifest from ever disagreeing with what is in the tarball. On stdout,
			// unlike the human line, because something is parsing it.
			if jsonMode {
				out, err := json.Marshal(updatecheck.Manifest{
					Version:          version.Version,
					ExtractorVersion: engine.ExtractorVersion(),
				})
				if err != nil {
					log.Fatalf("failed to encode version: %v", err)
				}
				fmt.Println(string(out))
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "enola version %s\n", version.Version)
			os.Exit(0)
		case "--json":
			// Consumed above. Listed so the catch-all does not treat it as a path.
		case "--help", "-h":
			cli.RenderHelp(os.Stderr, helpSpec())
			os.Exit(0)
		case "--list":
			fmt.Fprint(os.Stderr, cli.RenderToolList(cli.ToolListSpec{}))
			os.Exit(0)
		case "--generate":
			generateMode = true
		case "--refresh":
			refreshMode = true
		case "--explain":
			explainMode = true
		case "--status":
			statusMode = true
		case "--all":
			statusAll = true
		case "--no-dashboard":
			noDashboard = true
		default:
			// The positional argument is a REPOSITORY when it names a directory and a
			// CONFIG FILE when it names a file, so both `--explain /path/to/repo` and
			// `--explain cluster.yaml` are unambiguous — without the distinction the
			// latter would be analysed as if the YAML file were a repository.
			//
			// The same applies to --generate: pointing it at a directory is the obvious
			// way to snapshot another repo, and treating that as a config path made it
			// fall back to defaults (repo: ".") and silently snapshot the working
			// directory instead of the repository named.
			//
			// Anything else is REJECTED rather than passed on as a config path. An
			// unreadable config is only a warning inside config.Load — by design, so a
			// missing mcp-arch.yaml falls back to defaults — but combined with a
			// catch-all default: here, every typo inherited that leniency and became a
			// silent wrong action. `enola chekc .` started an MCP server; `enola
			// --generate /does/not/exist.yaml` snapshotted the working directory. Both
			// looked like they worked.
			switch {
			case command.IsDirectory(arg):
				repoArg = arg
			case command.FileExists(arg):
				cfgPath = arg
			default:
				log.Fatalf("%s", cmds.UnknownArgHelp(arg))
			}
		}
	}

	// --status reads only the recorded usage under ~/.enola/usage/, so it runs
	// before the engine is built and never touches the repo.
	if statusMode {
		if statusAll {
			status.PrintStatusAll()
		} else {
			status.PrintStatus()
		}
		os.Exit(0)
	}

	eng, cfg, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: cfgPath,
	})
	if err != nil {
		log.Fatalf("failed to create engine: %v", err)
	}

	// A positional directory names one repository and overrides the config — in
	// every mode, not just --generate: the MCP tools resolve their repo through
	// cfg.Repo, so a server launched as `enola /path/to/repo` must serve that
	// repo no matter which directory it was started from. Repos must be cleared
	// too, or it would win in RepoPaths.
	//
	// --refresh is the exception: there the directory names WHICH member of the
	// configured cluster to re-read, and the cluster must stay configured.
	if repoArg != "" && !refreshMode {
		abs, err := filepath.Abs(repoArg)
		if err != nil {
			log.Fatalf("failed to resolve repo path: %v", err)
		}
		cfg.Repo, cfg.Repos = abs, nil
	}

	// A folder holding several git repositories, given as a repository, is indexed as ONE
	// repository: the cross-repo half of enola is off and nothing in the output said so.
	// --generate in a terminal offers to write the cluster config and run with it.
	// Everything else warns and goes on as before, so scripts and agents keep working
	// and can see why the graph has no services.
	foldDir, folded := foldedRepos(cfg)
	if len(folded) > 0 && !refreshMode {
		if generateMode && cli.CanPrompt() {
			clusterPath, err := offerCluster(os.Stdin, os.Stderr, foldDir, folded)
			if err != nil {
				log.Fatalf("failed to write cluster config: %v", err)
			}
			if clusterPath != "" {
				cfgPath, repoArg, folded = clusterPath, "", nil
				eng, cfg, err = bootstrap.NewEngine(bootstrap.Options{ConfigPath: cfgPath})
				if err != nil {
					log.Fatalf("failed to create engine: %v", err)
				}
			}
		} else {
			fmt.Fprint(os.Stderr, workspace.FoldWarning(binary().Name, foldDir, folded))
		}
	}

	if explainMode {
		runExplain(ctx, eng, cfg)
		// Explicit rather than deferred: this path exits, and a deferred Report
		// would never run. Same at every os.Exit below.
		memWatch.Report(os.Stderr, factCount(eng))
		os.Exit(0)
	}

	if generateMode && refreshMode {
		runRefresh(ctx, eng, cfg, repoArg)
		memWatch.Report(os.Stderr, factCount(eng))
		os.Exit(0)
	}

	if generateMode {
		repoPaths, err := cfg.RepoPaths()
		if err != nil {
			log.Fatalf("failed to resolve repo path: %v", err)
		}

		// `--generate` is not a subcommand, so it never passes through Dispatch and would
		// otherwise be the one CLI path that reads the update cache (below) without ever
		// writing it. Started before the snapshot rather than after, so the check has the
		// whole run to complete and the notice can be current on this run, not the next.
		command.SpawnUpdateRefresh()

		var snapshot *facts.Snapshot
		for i, repoPath := range repoPaths {
			// The first repository resets the store; the rest append to it, which is
			// what makes one process produce one linked graph. Linking and the
			// explainers run once, on the last turn, over the whole union: they are
			// recomputed from scratch on every append, so running them earlier only
			// cost time (on a 22-repo cluster, twenty-one passes over a growing
			// union, and twenty-one writes of it).
			eng.SetDeferLinking(i < len(repoPaths)-1)
			snapshot, err = eng.GenerateSnapshot(ctx, repoPath, i > 0)
			if err != nil {
				log.Fatalf("snapshot generation failed for %s: %v", repoPath, err)
			}
		}
		// Every repository's output dir receives the complete linked union, written
		// once, now that it exists. Before, each received the union as of its own
		// turn and only the last was complete; a consumer reading any repo's own
		// artifacts (the CI check, enola plan) now reads the same whole graph.
		for _, repoPath := range repoPaths {
			if err := eng.WriteArtifacts(repoPath); err != nil {
				log.Fatalf("failed to write artifacts for %s: %v", repoPath, err)
			}
		}

		// Refresh the graph-wide receipt at ~/.enola/receipt.json. Non-fatal: a
		// failure here must not abort an otherwise-successful snapshot.
		if err := eng.WriteGlobalReceipt(); err != nil {
			log.Printf("warning: failed to write global receipt: %v", err)
		}

		fmt.Fprintf(os.Stderr, "\nSnapshot complete:\n")
		if len(repoPaths) > 1 {
			fmt.Fprintf(os.Stderr, "  Repositories: %d\n", len(repoPaths))
			for _, p := range repoPaths {
				fmt.Fprintf(os.Stderr, "    - %s\n", p)
			}
		} else {
			fmt.Fprintf(os.Stderr, "  Repository:  %s\n", snapshot.Meta.RepoPath)
		}
		fmt.Fprintf(os.Stderr, "  Facts:       %d\n", snapshot.Meta.FactCount)
		fmt.Fprintf(os.Stderr, "  Insights:    %d\n", snapshot.Meta.InsightCount)
		fmt.Fprintf(os.Stderr, "  Artifacts:   %d\n", len(snapshot.Artifacts))
		fmt.Fprintf(os.Stderr, "  Duration:    %s\n", snapshot.Meta.Duration)
		fmt.Fprintf(os.Stderr, "  Output:      %s\n", filepath.Join(repoPaths[len(repoPaths)-1], cfg.Output.Dir))
		if len(folded) > 0 {
			fmt.Fprintf(os.Stderr, "  Warning:     %d git repositories indexed as one; to link them: %s\n",
				len(folded), workspace.Remedy(binary().Name, foldDir))
		}
		if cli.ShowDashboardHint(os.Stderr) {
			printDashboardHint(os.Stderr, repoArg, cfgPath)
		}
		updatecheck.Fprint(os.Stderr, engine.ExtractorVersion())
		memWatch.Report(os.Stderr, snapshot.Meta.FactCount)
		os.Exit(0)
	}

	restoredCorpus := bootstrap.AutoLoadSnapshot(eng, cfg)

	srv, err := bootstrap.NewServer(eng, cfg)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// Size the restored graph before serving, so queries against it are priced
	// correctly without waiting for a snapshot this process does not need.
	srv.SeedCorpus(restoredCorpus)

	// Record per-tool usage so a later `enola --status` has something to report.
	// Per-repo counters are loaded lazily from ~/.enola/usage/ on first touch, so
	// they survive restarts; the config's repo is the fallback for calls made
	// before any snapshot is loaded. srv.StartTime() is only set once Run() is
	// called, so stamp the start time here to get a correct uptime.
	repoPath, _ := filepath.Abs(cfg.Repo)
	tracker := status.NewTracker(repoPath)
	tracker.SetStartTime(time.Now())
	// Identity + graph state make this process distinguishable in the instance
	// registry — a user typically runs one server per agent terminal, and every
	// dashboard and --status listing is built from these records.
	wd, _ := os.Getwd()
	tracker.SetIdentity(status.Identity{
		Binary:     "enola",
		Version:    version.Version,
		ConfigPath: cfgPath,
		WorkDir:    wd,
	})
	tracker.SetGraphFunc(bootstrap.GraphStateFunc(eng))
	// Deregister on the way out so the registry reflects the shutdown at once
	// rather than waiting for a reader to reap a dead PID.
	defer tracker.Close()
	srv.SetToolCallback(tracker.Record)

	// Start the localhost HTTP dashboard alongside the MCP server. It binds a
	// free loopback port — plus the shared, bookmarkable one if it can claim it —
	// and serves a read-only view of THIS server's graph and
	// activity, with links to every other server running. Non-fatal: a dashboard
	// failure must never stop the MCP server. The port is recorded on the tracker
	// BEFORE the startup write, so a separate --status invocation can print the
	// URL even before the first tool call.
	if !noDashboard {
		opts := dashboard.Options{
			Tracker:    tracker,
			StablePort: dashboard.ResolveStablePort(cfg.Dashboard.Port),
			HistoryDir: cfg.History.Dir,
		}
		if dash, err := dashboard.Start(eng, opts); err != nil {
			log.Printf("dashboard: not started: %v (continuing without it)", err)
		} else {
			tracker.SetDashboardPort(dash.Port())
			fmt.Fprintf(os.Stderr, "Dashboard: %s (refresh data explicitly)\n", dash.URL())
			if opts.StablePort > 0 {
				fmt.Fprintf(os.Stderr, "Shared URL: http://127.0.0.1:%d (whichever server holds it lists all the others)\n", opts.StablePort)
			}
		}
	}
	tracker.PersistStartup()

	if err := srv.Run(ctx); err != nil && !isNormalServerShutdown(err) {
		// Deregister explicitly: log.Fatalf exits without running deferred calls.
		tracker.Close()
		log.Fatalf("server error: %v", err)
	}

	// A server run peaks while snapshotting on behalf of a tool call, so its watch
	// spans the whole session and reports once on clean shutdown.
	memWatch.Report(os.Stderr, factCount(eng))
}

// isNormalServerShutdown distinguishes the cancellation signal.NotifyContext
// sends on Ctrl-C/SIGTERM from an actual transport failure. The MCP server
// returns context.Canceled when that context closes; reporting it through
// log.Fatalf made an orderly, fully-cleaned-up shutdown look like a crash and
// exit with status 1.
func isNormalServerShutdown(err error) bool {
	return errors.Is(err, context.Canceled)
}

// startMemWatch pulls the memory-instrumentation flags off the argument list and,
// when one is present, starts sampling. It returns the REMAINING arguments, which
// is the point: --memprofile takes a value, and every other parser in this binary
// is an exact-match switch that would read that value as a repository path.
//
//	--memstats             sample the heap, print one summary line at exit
//	--memprofile <path>    the same, plus a heap profile at the peak (<path>) and
//	                       one of the steady state (<path>.final)
//
// Both are deliberately absent from --help. They are development instruments for
// the memory work tracked in enola-benchmarks/MEMORY_IMPROVEMENTS.md, they cost a
// stop-the-world read every 150ms, and --help is already long enough that adding
// non-user-facing flags to it makes the user-facing ones harder to find.
//
// A --memprofile with no path following it is a hard error rather than a silent
// downgrade to --memstats: the caller asked for a file, and the failure mode of
// guessing is a benchmark run that looks instrumented and produced nothing.
func startMemWatch(argv []string) ([]string, *bootstrap.MemWatch) {
	rest, profilePath, enabled, err := splitMemFlags(argv)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if !enabled {
		return rest, nil
	}
	return rest, bootstrap.StartMemWatch(profilePath)
}

// splitMemFlags is the parsing half of startMemWatch, kept pure so it can be
// tested: it is the one piece of argument handling that can break flags it has
// nothing to do with, by dropping or keeping the wrong element.
func splitMemFlags(argv []string) (rest []string, profilePath string, enabled bool, err error) {
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--memstats":
			enabled = true
		case "--memprofile":
			if i+1 >= len(argv) {
				return nil, "", false, errors.New("--memprofile needs a path, e.g. --memprofile heap.pb.gz")
			}
			profilePath = argv[i+1]
			enabled = true
			i++ // consume the value too
		default:
			rest = append(rest, argv[i])
		}
	}
	return rest, profilePath, enabled, nil
}

// factCount reports how many facts the engine currently holds, or 0 when it holds
// no snapshot. Only the memory summary uses it, to derive the per-fact figures.
func factCount(eng *bootstrap.Engine) int {
	if st := eng.Store(); st != nil {
		return st.Count()
	}
	return 0
}

// helpSpec is the `--help` text for this binary: the shared engine help with
// the OSS-only `upgrade` command documented on top.
// binary identifies this build to everything that renders its name: the shared help,
// and the shared subcommands' usage lines and suggested remedies. One value, so a
// wrapper cannot end up with help that names one binary and errors that name another.
func binary() cli.Binary {
	return cli.Binary{
		Name:       "enola",
		CmdPackage: "./cmd/enola",
		VersionVar: "github.com/enola-labs/enola/internal/version.Version",
		Version:    version.Version,
	}
}

func helpSpec() cli.HelpSpec {
	spec := cli.DefaultHelp(binary())
	spec.Usage = append(spec.Usage, "enola upgrade")
	spec.Commands = append(spec.Commands, cli.FlagDoc{
		Flag: "upgrade",
		Desc: "Download and install the latest enola release, replacing the\nrunning binary in place.",
	})
	return spec
}

// runExplain indexes the configured repository (or repositories) and prints a
// human-readable statistical summary to stdout.
func runExplain(ctx context.Context, eng *bootstrap.Engine, cfg *config.Config) {
	repoPaths, err := cfg.RepoPaths()
	if err != nil {
		log.Fatalf("failed to resolve repo path: %v", err)
	}

	// --explain is a read-only, no-artifacts mode: reuse a cache if one exists,
	// but never write to .enola.
	eng.SetPersistCache(false)
	for i, repoPath := range repoPaths {
		fmt.Fprintf(os.Stderr, "Analyzing %s …\n", repoPath)
		eng.SetDeferLinking(i < len(repoPaths)-1)
		if _, err := eng.GenerateSnapshot(ctx, repoPath, i > 0); err != nil {
			log.Fatalf("snapshot generation failed for %s: %v", repoPath, err)
		}
	}

	report := explain.Compute(eng)
	fmt.Print(report.Render())
}

// runRefresh re-reads ONE member of a configured cluster into the union the last
// generate produced, replacing that repository's slice and nothing else. A full
// `--generate` walks every repository and hands each its turn's union, which for a
// large cluster costs the same twenty-odd writes and explainer passes whether one
// repository moved or all of them did. This is the shape for "only this one moved":
// restore the union, re-extract the named repository into it (append mode replaces
// a repository the union already holds), and write the result where the union
// lives and where the repository's own readers look.
func runRefresh(ctx context.Context, eng *bootstrap.Engine, cfg *config.Config, repoArg string) {
	if repoArg == "" {
		log.Fatalf("--refresh needs the repository to re-read: enola --generate --refresh <repo_path> [config]")
	}
	target, err := filepath.Abs(repoArg)
	if err != nil {
		log.Fatalf("failed to resolve repo path: %v", err)
	}
	repoPaths, err := cfg.RepoPaths()
	if err != nil {
		log.Fatalf("failed to resolve repo path: %v", err)
	}
	if len(repoPaths) < 2 {
		log.Fatalf("--refresh re-reads one repository of a configured cluster; %s configures %d", cfg.SourcePath, len(repoPaths))
	}
	member := false
	for _, p := range repoPaths {
		if p == target {
			member = true
			break
		}
	}
	if !member {
		log.Fatalf("%s is not a repository of the cluster configured by %s; add it to repos: and run a full --generate", target, cfg.SourcePath)
	}
	if restored := bootstrap.AutoLoadSnapshot(eng, cfg); restored == nil {
		log.Fatalf("no union to refresh into: nothing restored for this workspace. Run a full --generate once; refresh replaces one repository inside an existing union")
	}
	// Refuse rather than degrade. Append mode discards a union built by a
	// different extractor version and starts a single-repo snapshot; for a
	// refresh that would replace the union with one repository and call it a
	// success, which is the exact accident this command exists to prevent.
	if snap := eng.Snapshot(); snap == nil || snap.Meta.ExtractorVersion != engine.ExtractorVersion() {
		got := "none"
		if snap != nil {
			got = snap.Meta.ExtractorVersion
		}
		log.Fatalf("the restored union was built by extractor version %s and this enola is %s; a refresh cannot mix them. Run a full --generate once with this build", got, engine.ExtractorVersion())
	}
	held := false
	for _, p := range eng.RepoPaths() {
		if p == target {
			held = true
			break
		}
	}
	if !held {
		log.Fatalf("the restored union does not hold %s; run a full --generate so every configured repository is in it", target)
	}
	snapshot, err := eng.GenerateSnapshot(ctx, target, true)
	if err != nil {
		log.Fatalf("refresh failed for %s: %v", target, err)
	}
	// The repository's own directory, for the readers that look there (plan, the
	// CI check), and the union's home at the last configured repository.
	last := repoPaths[len(repoPaths)-1]
	if err := eng.WriteArtifacts(target); err != nil {
		log.Fatalf("failed to write artifacts for %s: %v", target, err)
	}
	if last != target {
		if err := eng.WriteArtifacts(last); err != nil {
			log.Fatalf("failed to write artifacts for %s: %v", last, err)
		}
	}
	if err := eng.WriteGlobalReceipt(); err != nil {
		log.Printf("warning: failed to write global receipt: %v", err)
	}
	fmt.Fprintf(os.Stderr, "\nRefresh complete:\n")
	fmt.Fprintf(os.Stderr, "  Repository:  %s (re-read into a union of %d)\n", target, len(repoPaths))
	fmt.Fprintf(os.Stderr, "  Facts:       %d\n", snapshot.Meta.FactCount)
	fmt.Fprintf(os.Stderr, "  Insights:    %d\n", snapshot.Meta.InsightCount)
	fmt.Fprintf(os.Stderr, "  Duration:    %s\n", snapshot.Meta.Duration)
	fmt.Fprintf(os.Stderr, "  Output:      %s\n", filepath.Join(last, cfg.Output.Dir))
	if cli.ShowDashboardHint(os.Stderr) {
		printDashboardHint(os.Stderr, repoArg, cfg.SourcePath)
	}
}
