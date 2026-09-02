package command

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/pkg/bootstrap"
	"github.com/enola-labs/enola/pkg/dashboard"
	"github.com/enola-labs/enola/pkg/status"
)

// Dashboard restores the latest snapshot and serves its read-only dashboard
// without also starting an MCP stdio server. It remains in the foreground so
// Ctrl-C has the unsurprising effect of stopping the listener.
func (r *Runner) Dashboard(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	open := fs.Bool("open", false, "open the dashboard in the default browser")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s dashboard [--open] [repo_path|config_path]\n\n", r.name())
		fmt.Fprintln(os.Stderr, "Start a local dashboard for this repository. An MCP server is not required.")
		fmt.Fprintln(os.Stderr, "The dashboard follows newer snapshots written by Enola automatically; it never regenerates source itself.")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Options:")
		fmt.Fprintln(os.Stderr, "  --open         launch the dashboard in the default browser")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "The server binds to 127.0.0.1 and stays attached until Ctrl-C.")
		fmt.Fprintln(os.Stderr, "Run `"+r.name()+" --status` in another terminal to recover its URL.")
		fmt.Fprintln(os.Stderr, "If Safari blocks local HTTP in HTTPS-Only mode, allow this local address or use another browser.")
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return
		}
		r.dashboardFatal("dashboard: %v", err)
	}
	if fs.NArg() > 1 {
		r.dashboardFatal("dashboard accepts at most one repository or config path")
	}
	arg := ""
	if fs.NArg() == 1 {
		arg = fs.Arg(0)
		if _, err := os.Stat(arg); err != nil {
			r.dashboardFatal("cannot use target %q: %v", arg, err)
		}
	}

	// Validate before binding a listener so a missing snapshot produces the useful
	// generate-first guidance without briefly registering a dashboard process.
	t := r.resolveTarget(arg)
	snapshotDir := t.engine.OutputDir(t.repoPaths[0])
	fmt.Fprintf(os.Stderr, "Loading dashboard snapshot from %s...\n", snapshotDir)
	bootstrap.LoadDashboardSnapshot(t.engine, t.engine.Config())
	// A dashboard-only process is still an active Enola session. Register it just
	// like the MCP startup path does so Activity does not claim zero sessions while
	// the process serving that very page is running. No tool callback is installed:
	// this process serves no MCP calls, so its session count truthfully stays zero.
	tracker := status.NewTracker(t.repoPaths[0])
	tracker.SetStartTime(time.Now())
	wd, _ := os.Getwd()
	tracker.SetIdentity(status.Identity{
		Binary:     r.name() + " dashboard",
		Version:    r.buildVersion(),
		ConfigPath: t.cfgPath,
		WorkDir:    wd,
	})
	tracker.SetGraphFunc(bootstrap.GraphStateFunc(t.engine))
	defer tracker.Close()

	port := dashboard.ResolveStablePort(t.engine.Config().Dashboard.Port)
	// The binary's own options — title, overlay panels, and the InsightLabels
	// admission list that decides which explainers' findings the page will show at
	// all. Tracker and StablePort are this command's to set, and are stamped over
	// whatever the callback returned: they describe THIS process, not the binary.
	opts := r.dashboardOptions(t.engine)
	opts.Tracker, opts.StablePort = tracker, port
	opts.SnapshotPath = snapshotDir
	opts.GenerateCommand = r.dashboardGenerateCommand(arg)
	opts.CurrentExtractorVersion = engine.ExtractorVersion()
	opts.Refresh = snapshotReloader(t.engine, snapshotDir)
	dash, err := dashboard.Start(t.engine, opts)
	if err != nil {
		r.dashboardFatal("starting dashboard: %v", err)
	}
	tracker.SetDashboardPort(dash.Port())
	tracker.PersistStartup()
	fmt.Fprintln(os.Stderr, "Dashboard running")
	if t.engine.Snapshot() == nil {
		fmt.Fprintf(os.Stderr, "Snapshot: none yet (watching %s)\n", snapshotDir)
		fmt.Fprintf(os.Stderr, "Generate: %s\n", opts.GenerateCommand)
	} else {
		fmt.Fprintf(os.Stderr, "Snapshot: %s\n", snapshotDir)
	}
	fmt.Fprintf(os.Stderr, "Open: %s\n", dash.URL())
	if port > 0 {
		fmt.Fprintf(os.Stderr, "Bookmarkable URL: http://127.0.0.1:%d\n", port)
	}
	fmt.Fprintln(os.Stderr, "Press Ctrl-C to stop.")
	if *open {
		if err := openBrowser(dash.URL()); err != nil {
			fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
		}
	}
	<-ctx.Done()
}

func (r *Runner) dashboardGenerateCommand(target string) string {
	cmd := r.name() + " --generate"
	if target != "" {
		cmd += fmt.Sprintf(" %q", target)
	}
	return cmd
}

// snapshotReloader turns a dashboard-only process into a live view of the
// selected persisted snapshot. Receipt writes complete after the rest of the
// artifact bundle, so its digest is the publication marker. A mutex prevents a
// manual refresh and the background browser poll from restoring concurrently.
func snapshotReloader(eng *bootstrap.Engine, snapshotDir string) func() (bool, error) {
	var mu sync.Mutex
	last := snapshotMarker(snapshotDir)
	return func() (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		next := snapshotMarker(snapshotDir)
		if next == last || next == ([sha256.Size]byte{}) {
			return false, nil
		}
		if !bootstrap.LoadDashboardSnapshot(eng, eng.Config()) {
			return false, fmt.Errorf("snapshot artifacts appeared at %s but could not be loaded", snapshotDir)
		}
		last = next
		return true, nil
	}
}

func snapshotMarker(snapshotDir string) [sha256.Size]byte {
	b, err := os.ReadFile(filepath.Join(snapshotDir, "receipt.json"))
	if err != nil {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(b)
}

func (r *Runner) dashboardFatal(format string, args ...any) {
	r.cmdFatal("dashboard", format, args...)
}

func openBrowser(url string) error {
	name, args := browserCommand(runtime.GOOS, url)
	return exec.Command(name, args...).Start()
}

func browserCommand(goos, url string) (string, []string) {
	var name string
	var args []string
	switch goos {
	case "darwin":
		// Force LaunchServices to interpret the argument as a URL. Without -u,
		// `open` may classify a localhost address as a filesystem path instead.
		name, args = "open", []string{"-u", url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	return name, args
}
