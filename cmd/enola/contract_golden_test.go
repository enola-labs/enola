package main

// Contract goldens for the command line.
//
// The subcommands print to the process's own stdout and exit with a code a script
// acts on, so the only place the whole contract exists is the built binary. These
// tests run it over fixture repos and pin, per invocation, the exit code, stdout
// and stderr against a committed golden. The unit tests in pkg/command assert
// pieces of that from inside the package; they move when the code moves. These do
// not: nothing here names a Go symbol, so a command can be split, merged or
// relocated and the goldens still say whether a user would notice.
//
// Regenerate with:
//
//	go test ./cmd/enola -run TestCLIContract -update
//
// Machine and run specifics (temp paths, timestamps, durations, content hashes,
// the engine's progress log) are normalized away.
//
// Set ENOLA_CONTRACT_COVERDIR to a directory to build the binary with coverage
// instrumentation and collect what these invocations execute there, since a
// subprocess never shows up in `go test -cover`:
//
//	ENOLA_CONTRACT_COVERDIR=/tmp/cov go test ./cmd/enola -run TestCLIContract
//	go tool covdata percent -i=/tmp/cov

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/contract")

const contractCoverDirEnv = "ENOLA_CONTRACT_COVERDIR"

var (
	coverBuildOnce sync.Once
	coverBin       string
	coverBuildErr  error
)

// contractBinary is the binary the goldens run: the shared build, or an
// instrumented one when coverage was asked for.
func contractBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv(contractCoverDirEnv) == "" {
		return enolaBinary(t)
	}
	coverBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "enola-contract-bin-")
		if err != nil {
			coverBuildErr = err
			return
		}
		coverBin = filepath.Join(dir, "enola")
		cmd := exec.Command(goTool(), "build", "-cover", "-coverpkg=github.com/enola-labs/enola/...", "-o", coverBin, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			coverBuildErr = fmt.Errorf("building instrumented enola: %v\n%s", err, out)
		}
	})
	if coverBuildErr != nil {
		t.Fatal(coverBuildErr)
	}
	return coverBin
}

// cliCase is one invocation whose exit code and output are pinned.
type cliCase struct {
	name string
	args []string
}

// cliSession runs the binary in one sandbox: a home directory nothing else
// shares and a working directory the relative arguments resolve against.
type cliSession struct {
	scenario string
	bin      string
	home     string
	dir      string
	roots    []string
}

var cliNormalizers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:\d{2})?`), "<TIME>"},
	{regexp.MustCompile(`sha256:[0-9a-f]{64}`), "sha256:<HASH>"},
	{regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|ms|s)\b`), "<DURATION>"},
	// Bumped on every extractor change; pinning it would re-record goldens for
	// changes that leave every answer alone.
	{regexp.MustCompile(`\bv\d{3,}\b`), "<EXTRACTOR_VERSION>"},
	// The per-workspace directory is keyed on a hash of the repository's absolute path.
	{regexp.MustCompile(`(\.enola/graphs/[\w.]+)-[0-9a-f]{8}\b`), "$1-<KEY>"},
	// doctor reports the machine's memory.
	{regexp.MustCompile(`soft memory limit .*`), "soft memory limit <MACHINE>"},
	{regexp.MustCompile(`"memory_limit": \d+`), `"memory_limit": "<MACHINE>"`},
	{regexp.MustCompile(`"memory_limit_from": "[^"]*"`), `"memory_limit_from": "<MACHINE>"`},
	// An excuse's age is counted from today.
	{regexp.MustCompile(`\b\d+ days?\b`), "<N> days"},
	{regexp.MustCompile(`"(age_days|oldest_excuse_days)": \d+`), `"$1": "<N>"`},
	// The history epoch fingerprints the extractor version among other things.
	{regexp.MustCompile(`"epoch": "[0-9a-f]+"`), `"epoch": "<EPOCH>"`},
}

// engineLogLine matches the tagged lines of the standard logger ("[engine] ..."):
// the progress report, timestamped and ordered by goroutine scheduling. It is
// diagnostics, not something a caller reads, so it is dropped rather than pinned.
var engineLogLine = regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \[.*\n`)

// logStamp is the logger's prefix on an untagged line. Those are the fatal
// errors, which a caller does read, so the line is kept and only the time goes.
var logStamp = regexp.MustCompile(`(?m)^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// engineLogTail is the same line when it lands after a progress indicator that
// did not end its own line, as backfill's does.
var engineLogTail = regexp.MustCompile(`(?m)\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \[.*$`)

func (s *cliSession) normalize(out string) string {
	// A progress indicator redraws its line with a carriage return.
	out = strings.ReplaceAll(out, "\r", "\n")
	out = engineLogLine.ReplaceAllString(out, "")
	out = engineLogTail.ReplaceAllString(out, "")
	out = logStamp.ReplaceAllString(out, "")
	// A history source is named after the host, with a per-machine suffix.
	if host, err := os.Hostname(); err == nil && host != "" {
		out = regexp.MustCompile(regexp.QuoteMeta(host)+`-[0-9a-f]{8}`).ReplaceAllString(out, "<HOST>")
	}
	// install writes the running binary's own path into the hook configuration.
	if resolved, err := filepath.EvalSymlinks(s.bin); err == nil && resolved != s.bin {
		out = strings.ReplaceAll(out, resolved, "<BIN>")
	}
	out = strings.ReplaceAll(out, s.bin, "<BIN>")
	for _, root := range s.roots {
		if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
			out = strings.ReplaceAll(out, resolved, "<TMP>")
		}
		out = strings.ReplaceAll(out, root, "<TMP>")
	}
	for _, n := range cliNormalizers {
		out = n.re.ReplaceAllString(out, n.with)
	}
	return out
}

// run executes one invocation and renders it the way its golden stores it.
func (s *cliSession) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.bin, args...)
	cmd.Dir = s.dir
	// CI is cleared so the answers do not depend on where the suite runs.
	cmd.Env = append(sandboxEnv(s.home), "ENOLA_NO_PROMPTS=1", "CI=", "NO_COLOR=1")
	if dir := os.Getenv(contractCoverDirEnv); dir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+dir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("enola %s: %v", strings.Join(args, " "), err)
		}
		code = exit.ExitCode()
	}
	return fmt.Sprintf("$ enola %s\nexit: %d\n--- stdout\n%s--- stderr\n%s",
		s.normalize(strings.Join(args, " ")), code, s.normalize(stdout.String()), s.normalize(stderr.String()))
}

func (s *cliSession) pin(t *testing.T, cases []cliCase) {
	t.Helper()
	for _, c := range cases {
		assertCLIGolden(t, filepath.Join(s.scenario, c.name+".golden"), s.run(t, c.args...))
	}
}

func assertCLIGolden(t *testing.T, rel, got string) {
	t.Helper()
	path := filepath.Join("testdata", "contract", rel)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s differs from its golden (run with -update if intended).\n--- want\n%s\n--- got\n%s", rel, want, got)
	}
}

func newCLISession(t *testing.T, scenario string) *cliSession {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	if os.Getenv(contractCoverDirEnv) != "" {
		if err := os.MkdirAll(os.Getenv(contractCoverDirEnv), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	home, work := t.TempDir(), t.TempDir()
	return &cliSession{scenario: scenario, bin: contractBinary(t), home: home, dir: work, roots: []string{home, work}}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, body := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const contractIntent = "components:\n" +
	"  - {name: pkg-a, match: [pkg/a/**]}\n" +
	"  - {name: pkg-b, match: [pkg/b/**]}\n" +
	"rules:\n" +
	"  - {id: a-avoids-b, forbid: pkg-a, to: pkg-b, via: imports,\n" +
	"     because: a must stay deliverable without b}\n"

// TestCLIContract_Surface pins what the binary says about itself and how it
// answers arguments it cannot act on. Nothing here needs a repository.
func TestCLIContract_Surface(t *testing.T) {
	s := newCLISession(t, "surface")

	cases := []cliCase{
		{"help", []string{"--help"}},
		{"version", []string{"--version"}},
		{"version_json", []string{"--version", "--json"}},
		{"list", []string{"--list"}},
		{"status", []string{"--status"}},
		{"status_all", []string{"--status", "--all"}},
		{"typo_subcommand", []string{"chekc"}},
		{"unknown_word", []string{"frobnicate"}},
		{"unknown_flag", []string{"--no-such-flag"}},
		{"bad_path", []string{"./no/such/path"}},
		{"baseline_no_action", []string{"baseline"}},
		{"baseline_bad_action", []string{"baseline", "frobnicate"}},
		{"constraints_bad_action", []string{"constraints", "frobnicate"}},
		{"history_bad_action", []string{"history", "frobnicate"}},
		{"providers_bad_action", []string{"providers", "frobnicate"}},
		{"providers_list", []string{"providers", "list"}},
		{"cluster_bad_action", []string{"cluster", "frobnicate"}},
		{"impact_no_target", []string{"impact"}},
		{"endpoint_no_endpoint", []string{"endpoint"}},
		{"blame_no_pattern", []string{"blame"}},
		{"diff_no_range", []string{"diff"}},
		{"check_bad_flag", []string{"check", "--no-such-flag"}},
		{"check_bad_fail_on", []string{"check", "--fail-on", "no-such-explainer"}},
		{"help_providers", []string{"providers", "--help"}},
		{"help_hook", []string{"hook", "--help"}},
		{"help_upgrade", []string{"upgrade", "--help"}},
		{"help_baseline_pin", []string{"baseline", "pin", "--help"}},
		{"help_word", []string{"help"}},
		{"help_word_flows", []string{"help", "flows"}},
		{"help_word_nested", []string{"help", "constraints", "mine"}},
		{"help_flag_flows", []string{"--help", "flows"}},
		{"help_word_typo", []string{"help", "flwos"}},
	}
	for _, sub := range []string{
		"check", "cluster", "constraints", "plan", "coverage", "endpoint", "impact", "flows", "doctor",
		"dashboard", "baseline", "log", "show", "diff", "blame", "gc", "history", "install", "uninstall",
	} {
		cases = append(cases, cliCase{"help_" + sub, []string{sub, "--help"}})
	}
	s.pin(t, cases)
}

// TestCLIContract_Repo pins the commands over one repository through the life of
// a change: no snapshot, a snapshot, a pinned baseline, an edit, the verdict.
// The repository is go_sample with a declared rule its own import cycle breaks.
func TestCLIContract_Repo(t *testing.T) {
	s := newCLISession(t, "repo")
	repo := copyFixture(t, "go_sample", filepath.Join(s.dir, "go_sample"))
	writeFiles(t, repo, map[string]string{"enola-intent.yaml": contractIntent})
	s.dir = repo

	s.pin(t, []cliCase{
		// Before any snapshot exists.
		{"00_baseline_show_none", []string{"baseline", "show", "."}},
		{"00_check_no_baseline", []string{"check", "."}},
		{"00_constraints_lint_no_snapshot", []string{"constraints", "lint", "."}},
		{"00_constraints_mine_no_snapshot", []string{"constraints", "mine", "."}},
		{"00_constraints_ledger_no_snapshot", []string{"constraints", "ledger", "."}},
		{"00_constraints_explain_no_snapshot", []string{"constraints", "explain", "pkg/a/a.go", "."}},
		{"00_log_no_history", []string{"log", "."}},
		{"00_show_no_history", []string{"show", "."}},
		{"00_gc_no_history", []string{"gc", "--dry-run", "."}},
		{"00_doctor", []string{"doctor", "."}},
		{"00_doctor_json", []string{"doctor", "--json", "."}},

		{"10_generate", []string{"--generate", "."}},
		{"10_explain", []string{"--explain", "."}},
		{"10_explain_summary", []string{"--explain", "--summary", "."}},
		{"10_explain_summary_json", []string{"--explain", "--summary", "--json", "."}},
		{"10_summary_without_explain", []string{"--summary", "."}},

		{"20_constraints_lint", []string{"constraints", "lint", "."}},
		{"20_constraints_mine", []string{"constraints", "mine", "."}},
		{"20_constraints_ledger", []string{"constraints", "ledger", "."}},
		{"20_constraints_explain_bound", []string{"constraints", "explain", "pkg/a/a.go", "."}},
		{"20_constraints_explain_unbound", []string{"constraints", "explain", "main.go", "."}},
		{"20_constraints_init_exists", []string{"constraints", "init", "."}},
		{"21_constraints_explain_part", []string{"constraints", "explain", "pkg-a", "."}},
		{"21_constraints_explain_json", []string{"constraints", "explain", "--json", "pkg/a/a.go", "."}},
		{"21_constraints_explain_no_radius", []string{"constraints", "explain", "--no-radius", "pkg/b/b.go", "."}},
		{"21_constraints_explain_no_match", []string{"constraints", "explain", "no/such/file.go", "."}},
		{"21_constraints_mine_low_floor", []string{"constraints", "mine", "--min-support", "1", "--min-confidence", "0.5", "--top", "5", "."}},
		{"21_constraints_mine_tautologies", []string{"constraints", "mine", "--min-support", "1", "--include-tautologies", "--top", "0", "."}},
		{"21_constraints_mine_jsonl", []string{"constraints", "mine", "--min-support", "1", "--jsonl", "../mined.jsonl", "."}},
		{"21_constraints_mine_scaffold", []string{"constraints", "mine", "--min-support", "1", "--scaffold-eslint", "../eslint", "."}},
		{"21_constraints_mine_bad_flag", []string{"constraints", "mine", "--min-confidence", "7", "."}},
		{"21_constraints_ledger_json", []string{"constraints", "ledger", "--json", "."}},
		{"21_providers_fetch_unknown", []string{"providers", "fetch", "no-such-provider"}},
		{"21_gc_bad_age", []string{"gc", "--thin-older-than", "soon", "."}},

		{"30_plan_path", []string{"plan", "pkg/a/new_file.go", "."}},
		{"30_plan_paths_json", []string{"plan", "--json", "--paths", "pkg/a/a.go,pkg/b/b.go", "."}},
		{"30_plan_symbols", []string{"plan", "--symbols", "pkg/b.Beta", "."}},
		{"30_plan_no_radius", []string{"plan", "--no-radius", "pkg/a/a.go", "."}},
		{"30_plan_nothing", []string{"plan", "."}},

		{"40_impact", []string{"impact", "pkg/a.Alpha", "."}},
		{"40_impact_list", []string{"impact", "--list", "--include-forward", "pkg/a.Alpha", "."}},
		{"40_impact_json", []string{"impact", "--json", "pkg/a.Alpha", "."}},
		{"40_impact_file", []string{"impact", "pkg/a/a.go", "."}},
		{"40_impact_ambiguous", []string{"impact", "a", "."}},
		{"40_impact_no_match", []string{"impact", "no-such-thing", "."}},
		{"40_flows", []string{"flows", "."}},
		{"40_flows_show", []string{"flows", "--show", "main", "."}},
		{"40_flows_json", []string{"flows", "--json", "--top", "1", "."}},
		{"40_flows_through", []string{"flows", "--through", "pkg/a.Alpha", "."}},
		{"40_flows_through_no_symbol", []string{"flows", "--through", "no-such-thing", "."}},
		{"40_flows_show_no_match", []string{"flows", "--show", "no-such-flow", "."}},
		{"40_endpoint_no_routes", []string{"endpoint", "/v1/things", "."}},
		{"40_coverage_single_repo", []string{"coverage", "."}},

		{"50_baseline_pin", []string{"baseline", "pin", "."}},
		{"50_baseline_show", []string{"baseline", "show", "."}},
		{"50_check_clean", []string{"check", "."}},
		{"50_check_clean_json", []string{"check", "--json", "."}},
		{"50_check_clean_summary", []string{"check", "--summary", "."}},
	})

	// A new package that pkg/b starts importing, and a second import from pkg/a
	// into pkg/b that the declared rule forbids.
	writeFiles(t, repo, map[string]string{
		"pkg/c/c.go":     "package c\n\n// Gamma is new since the baseline.\nfunc Gamma() string { return \"gamma\" }\n",
		"pkg/b/gamma.go": "package b\n\nimport \"example.com/gosample/pkg/c\"\n\n// UsesGamma couples pkg/b to the new package.\nfunc UsesGamma() string { return c.Gamma() }\n",
	})

	s.pin(t, []cliCase{
		{"60_check_after_edit", []string{"check", "."}},
		{"60_check_detail", []string{"check", "--detail", "."}},
		{"60_check_json", []string{"check", "--json", "."}},
		{"60_check_fail_on_dead_code", []string{"check", "--fail-on", "dead-code", "."}},
		{"60_check_fail_on_cycles", []string{"check", "--fail-on", "cycles", "."}},
		{"60_check_summary", []string{"check", "--summary", "."}},
		{"60_check_summary_json", []string{"check", "--summary", "--json", "."}},
		{"60_check_summary_fails", []string{"check", "--summary", "--fail-on", "dead-code", "--min-confidence", "0.5", "."}},
		{"60_check_summary_with_format", []string{"check", "--summary", "-format", "sarif", "."}},
		{"60_check_summary_with_detail", []string{"check", "--summary", "--detail", "."}},
		{"60_check_target", []string{"check", "--target", "pkg/b.Beta", "--max-spillover", "0", "."}},
		{"60_check_target_expected", []string{"check", "--target", "pkg/b.Beta", "--expected", "pkg/c", "--max-spillover", "0", "."}},
		{"60_check_baseline_previous", []string{"check", "--baseline", "previous", "."}},

		{"70_generate_after_edit", []string{"--generate", "."}},
		{"70_log", []string{"log", "."}},
		{"70_log_all", []string{"log", "--all", "."}},
		{"70_log_all_oneline", []string{"log", "--all", "--oneline", "."}},
		{"70_log_all_stat", []string{"log", "--all", "--stat", "."}},
		{"70_log_all_graph", []string{"log", "--all", "--graph", "."}},
		{"70_log_all_json", []string{"log", "--all", "--json", "."}},
		{"70_log_all_n1", []string{"log", "--all", "-n", "1", "."}},
		{"70_show_latest", []string{"show", "."}},
		{"70_show_latest_json", []string{"show", "--json", "."}},
		{"70_show_seq", []string{"show", "@1", "."}},
		{"70_show_focus", []string{"show", "--focus", "pkg/c", "."}},
		{"70_show_no_such_revision", []string{"show", "deadbeef", "."}},
		{"70_diff_range", []string{"diff", "@1..@2", "."}},
		{"70_diff_open_range", []string{"diff", "..", "."}},
		{"70_diff_json", []string{"diff", "--json", "@1..@2", "."}},
		{"70_diff_bad_range", []string{"diff", "nonsense", "."}},
		{"70_blame", []string{"blame", "--all", "pkg/c", "."}},
		{"70_blame_edge_first", []string{"blame", "--all", "--first", "pkg/b -> pkg/c", "."}},
		{"70_blame_findings", []string{"blame", "--all", "--findings", "cycle", "."}},
		{"70_blame_json", []string{"blame", "--all", "--json", "pkg/c", "."}},
		{"70_blame_no_match", []string{"blame", "--all", "no-such-thing", "."}},
		{"70_gc_dry_run", []string{"gc", "--dry-run", "."}},

		{"80_history_verify_no_store", []string{"history", "verify", "."}},
		{"80_history_push", []string{"history", "push", "../store", "."}},
		{"80_history_verify", []string{"history", "verify", "../store", "."}},
		{"80_history_pull", []string{"history", "pull", "../store", "."}},

		{"90_baseline_clear", []string{"baseline", "clear", "."}},
		{"90_baseline_show_cleared", []string{"baseline", "show", "."}},
		{"90_install_dry_run", []string{"install", "--dry-run", "--targets=claude", "."}},
		{"90_install_bad_target", []string{"install", "--dry-run", "--targets=no-such-agent", "."}},
		{"90_uninstall_dry_run", []string{"uninstall", "--dry-run", "--targets=claude", "."}},
		{"91_uninstall_nothing_installed", []string{"uninstall", "--yes", "--targets=claude", "."}},
		{"92_install", []string{"install", "--yes", "--targets=claude", "."}},
		{"92_install_again", []string{"install", "--yes", "--targets=claude", "."}},
		{"93_install_hooks", []string{"install", "--yes", "--hooks", "--targets=claude", "."}},
		{"93_doctor_hooks_configured", []string{"doctor", "."}},
		{"93_doctor_hooks_configured_json", []string{"doctor", "--json", "."}},
		{"94_uninstall", []string{"uninstall", "--yes", "--targets=claude", "."}},
		{"94_doctor_after_uninstall", []string{"doctor", "."}},
	})
}

// TestCLIContract_Cluster pins the cross-repo commands over a folder holding an
// api service and the consumer that calls its one route.
func TestCLIContract_Cluster(t *testing.T) {
	s := newCLISession(t, "cluster")
	cluster := filepath.Join(s.dir, "services")
	for _, name := range []string{"api", "consumer"} {
		repo := copyFixture(t, "go_httpclient_multirepo/"+name, filepath.Join(cluster, name))
		// cluster init lists the subfolders that are git repositories.
		cmd := exec.Command("git", "-C", repo, "init", "-q")
		cmd.Env = sandboxEnv(s.home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	s.dir = cluster

	s.pin(t, []cliCase{
		{"cluster_init_dry_run", []string{"cluster", "init", "--dry-run", "."}},
		{"cluster_init", []string{"cluster", "init", "."}},
		{"cluster_init_refuses_overwrite", []string{"cluster", "init", "."}},
		{"coverage_no_snapshot", []string{"coverage", "cluster.yaml"}},
		{"generate", []string{"--generate", "cluster.yaml"}},
		{"explain", []string{"--explain", "cluster.yaml"}},
		{"coverage_summary", []string{"coverage", "cluster.yaml"}},
		{"coverage_json", []string{"coverage", "--json", "cluster.yaml"}},
		{"coverage_unresolved", []string{"coverage", "--unresolved", "cluster.yaml"}},
		{"coverage_repo", []string{"coverage", "--repo", "consumer", "cluster.yaml"}},
		{"endpoint", []string{"endpoint", "GET /v1/things", "cluster.yaml"}},
		{"endpoint_json", []string{"endpoint", "--json", "/v1/things", "cluster.yaml"}},
		{"endpoint_no_match", []string{"endpoint", "/no/such/route", "cluster.yaml"}},
		{"impact_cross_repo", []string{"impact", "--list", "getThing", "cluster.yaml"}},
		{"refresh_one_repo", []string{"--generate", "--refresh", "api", "cluster.yaml"}},
		{"baseline_pin", []string{"baseline", "pin", "cluster.yaml"}},
		{"check_clean", []string{"check", "cluster.yaml"}},
		{"constraints_lint_undeclared", []string{"constraints", "lint", "cluster.yaml"}},
		{"folder_autocluster_generate", []string{"--generate", "."}},
	})
}

// TestCLIContract_GitHistory pins the history commands over a repository with
// commits: the timeline is backfilled from git, then read, shared and thinned.
// Author, committer and dates are fixed so the commit ids are the same everywhere.
func TestCLIContract_GitHistory(t *testing.T) {
	s := newCLISession(t, "githistory")
	repo := copyFixture(t, "go_sample", filepath.Join(s.dir, "go_sample"))
	s.dir = repo

	git := func(date string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false"}, args...)...)
		cmd.Env = append(sandboxEnv(s.home),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_AUTHOR_DATE="+date,
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_COMMITTER_DATE="+date,
			"GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("2024-01-01T10:00:00Z", "init", "-q", "-b", "main")
	git("2024-01-01T10:00:00Z", "add", "-A")
	git("2024-01-01T10:00:00Z", "commit", "-q", "-m", "first")
	writeFiles(t, repo, map[string]string{
		"pkg/c/c.go":     "package c\n\n// Gamma arrives in the second commit.\nfunc Gamma() string { return \"gamma\" }\n",
		"pkg/b/gamma.go": "package b\n\nimport \"example.com/gosample/pkg/c\"\n\n// UsesGamma couples pkg/b to the new package.\nfunc UsesGamma() string { return c.Gamma() }\n",
	})
	git("2024-01-02T10:00:00Z", "add", "-A")
	git("2024-01-02T10:00:00Z", "commit", "-q", "-m", "second")

	s.pin(t, []cliCase{
		{"00_log_empty", []string{"log", "."}},
		{"10_backfill_dry_run", []string{"log", "--backfill", "--dry-run", "."}},
		{"10_backfill", []string{"log", "--backfill", "."}},
		{"10_backfill_again", []string{"log", "--backfill", "."}},
		{"20_log", []string{"log", "."}},
		{"20_log_stat", []string{"log", "--stat", "."}},
		{"20_log_graph", []string{"log", "--graph", "."}},
		{"20_log_json", []string{"log", "--json", "."}},
		{"20_log_since", []string{"log", "--since", "2024-01-02T00:00:00Z", "."}},
		{"20_log_since_bad", []string{"log", "--since", "yesterday", "."}},
		{"20_show_head", []string{"show", "HEAD", "."}},
		{"20_show_head_parent", []string{"show", "HEAD~1", "."}},
		{"20_diff_commits", []string{"diff", "HEAD~1..HEAD", "."}},
		{"20_blame_edge", []string{"blame", "pkg/b -> pkg/c", "."}},
		{"20_blame_first_full", []string{"blame", "--first", "--full", "pkg/c", "."}},
		{"30_generate", []string{"--generate", "."}},
		{"30_log_after_generate", []string{"log", "."}},
		{"40_history_push", []string{"history", "push", "../store", "."}},
		{"40_history_push_again", []string{"history", "push", "../store", "."}},
		{"40_history_verify", []string{"history", "verify", "../store", "."}},
		{"40_blame_store", []string{"blame", "--store", "../store", "pkg/c", "."}},
		{"40_diff_store", []string{"diff", "--store", "../store", "HEAD~1..HEAD", "."}},
		{"40_history_gc_no_policy", []string{"history", "gc", "../store", "."}},
		// The store first and the flags after it, the order the documentation shows.
		{"40_history_gc_store_first", []string{"history", "gc", "../store", "--keep-last", "1", "."}},
		{"40_history_gc_plan", []string{"history", "gc", "--keep-last", "1", "../store", "."}},
		{"40_history_gc_apply", []string{"history", "gc", "--keep-last", "1", "--apply", "../store", "."}},
		{"40_history_verify_after_gc", []string{"history", "verify", "../store", "."}},
		{"40_history_pull", []string{"history", "pull", "../store", "."}},
		{"50_gc_dry_run", []string{"gc", "--dry-run", "."}},
		{"50_gc_thin", []string{"gc", "--thin-older-than", "1h", "."}},
		{"50_gc_prune_working", []string{"gc", "--prune-working", "."}},
		{"50_log_after_gc", []string{"log", "--all", "."}},
		{"60_doctor", []string{"doctor", "."}},
	})
}

// TestCLIContract_Excused pins the ledger and the gate over a rule whose one
// breach is signed away by an exemption, and over an exemption that excuses
// nothing.
func TestCLIContract_Excused(t *testing.T) {
	s := newCLISession(t, "excused")
	repo := copyFixture(t, "go_sample", filepath.Join(s.dir, "go_sample"))
	writeFiles(t, repo, map[string]string{"enola-intent.yaml": "components:\n" +
		"  - {name: pkg-a, match: [pkg/a/**]}\n" +
		"  - {name: pkg-b, match: [pkg/b/**]}\n" +
		"rules:\n" +
		"  - id: a-avoids-b\n" +
		"    forbid: pkg-a\n" +
		"    to: pkg-b\n" +
		"    via: imports\n" +
		"    because: a must stay deliverable without b\n" +
		"    exempt:\n" +
		"      - witness: \"pkg/a -> example.com/gosample/pkg/b -> pkg/b via imports\"\n" +
		"        owner: dana\n" +
		"        because: the cycle is scheduled for removal\n" +
		"        since: \"2026-08-01\"\n" +
		"      - witness: \"pkg/a -> example.com/gosample/pkg/z -> pkg/z via imports\"\n" +
		"        owner: dana\n" +
		"        because: excuses an edge that does not exist\n" +
		"        since: \"2026-08-01\"\n"})
	s.dir = repo

	s.pin(t, []cliCase{
		{"lint", []string{"constraints", "lint", "."}},
		{"generate", []string{"--generate", "."}},
		{"lint_resolved", []string{"constraints", "lint", "."}},
		{"ledger", []string{"constraints", "ledger", "."}},
		{"ledger_json", []string{"constraints", "ledger", "--json", "."}},
		{"explain_bound", []string{"constraints", "explain", "pkg/a/a.go", "."}},
		{"plan", []string{"plan", "pkg/a/a.go", "."}},
		{"baseline_pin", []string{"baseline", "pin", "."}},
		{"check_fail_on_constraints", []string{"check", "--fail-on", "constraints", "."}},
		{"check_summary_fail_on_constraints", []string{"check", "--summary", "--fail-on", "constraints", "."}},
	})

	// A declaration that does not parse is refused by every command that reads it.
	writeFiles(t, repo, map[string]string{"enola-intent.yaml": "components:\n  - {name: pkg-a}\nrules:\n  - {id: broken, forbid: nowhere, to: pkg-a}\n"})
	s.pin(t, []cliCase{
		{"invalid_lint", []string{"constraints", "lint", "."}},
		{"invalid_generate", []string{"--generate", "."}},
		{"invalid_check", []string{"check", "."}},
		{"invalid_plan", []string{"plan", "pkg/a/a.go", "."}},
	})
}

// TestCLIContract_Status pins `--status` over usage a real server recorded: the
// binary serves MCP, answers a few calls, and the report is read while it runs
// and after it has stopped.
func TestCLIContract_Status(t *testing.T) {
	s := newCLISession(t, "status")
	repo := copyFixture(t, "go_sample", filepath.Join(s.dir, "go_sample"))
	s.dir = repo

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	server := exec.Command(s.bin, "--no-dashboard", repo)
	server.Dir = repo
	server.Env = append(sandboxEnv(s.home), "ENOLA_NO_PROMPTS=1", "CI=")
	client := mcp.NewClient(&mcp.Implementation{Name: "enola-contract-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: server}, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = cs.Close()
		}
	}()
	// The server's pid is in every report; it is the one number that is its own.
	pid := regexp.MustCompile(`\b` + fmt.Sprint(server.Process.Pid) + `\b`)
	// Columns are padded to the longest repository path, and the token estimate is
	// derived from response sizes, which include that path. Both follow the temp
	// directory's length, so padding is collapsed and the estimate left unpinned.
	padding := regexp.MustCompile(` {2,}`)
	tokens := regexp.MustCompile(`(?m) \d+ ?$`)
	pin := func(name string, args ...string) {
		t.Helper()
		out := pid.ReplaceAllString(s.run(t, args...), "<PID>")
		out = padding.ReplaceAllString(out, "  ")
		if head, estimate, ok := strings.Cut(out, "Value Estimate (approximate):"); ok {
			out = head + "Value Estimate (approximate):" + tokens.ReplaceAllString(estimate, " <TOKENS>")
		}
		assertCLIGolden(t, filepath.Join(s.scenario, name+".golden"), out)
	}

	callTool(t, cs, "generate_snapshot", map[string]any{"repo_path": repo})
	callTool(t, cs, "query_facts", map[string]any{"kind": "module"})
	callTool(t, cs, "query_facts", map[string]any{"kind": "symbol"})
	callTool(t, cs, "explore", map[string]any{"focus": "pkg/a"})
	callTool(t, cs, "impact_analysis", map[string]any{"target": "pkg/a.Alpha"})
	callTool(t, cs, "show_symbol", map[string]any{"name": "no-such-symbol"})

	pin("running", "--status")
	pin("running_all", "--status", "--all")

	if err := cs.Close(); err != nil {
		t.Logf("closing the session: %v", err)
	}
	closed = true

	pin("stopped", "--status")
	pin("stopped_all", "--status", "--all")
}

// TestCLIContract_Dashboard pins the dashboard command: what it refuses, and, for a
// repository with a snapshot, that it serves a page until it is interrupted and then
// exits cleanly.
//
// ENOLA_DASHBOARD_PORT=off turns the shared, bookmarkable port off, so the process
// binds only an ephemeral one and cannot collide with a dashboard already running
// on this machine.
func TestCLIContract_Dashboard(t *testing.T) {
	s := newCLISession(t, "dashboard")
	repo := copyFixture(t, "go_sample", filepath.Join(s.dir, "go_sample"))
	s.dir = repo

	s.pin(t, []cliCase{
		{"too_many_targets", []string{"dashboard", ".", "."}},
		{"no_such_target", []string{"dashboard", "./no/such/path"}},
		{"bad_flag", []string{"dashboard", "--no-such-flag"}},
		{"generate", []string{"--generate", "."}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, "dashboard", ".")
	cmd.Dir = repo
	cmd.Env = append(sandboxEnv(s.home), "ENOLA_NO_PROMPTS=1", "CI=", "ENOLA_DASHBOARD_PORT=off")
	if dir := os.Getenv(contractCoverDirEnv); dir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+dir)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the dashboard: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Read the startup report up to the line that says it is serving.
	var report strings.Builder
	openURL := regexp.MustCompile(`Open: (http://127\.0\.0\.1:\d+)`)
	url := ""
	reader := bufio.NewReader(stderr)
	for {
		line, err := reader.ReadString('\n')
		report.WriteString(line)
		if m := openURL.FindStringSubmatch(line); m != nil {
			url = m[1]
		}
		if strings.Contains(line, "Press Ctrl-C to stop.") {
			break
		}
		if err != nil {
			t.Fatalf("the dashboard stopped before it reported serving: %v\n%s", err, report.String())
		}
	}
	if url == "" {
		t.Fatalf("the startup report names no URL:\n%s", report.String())
	}

	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	page, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d, want 200", url, res.StatusCode)
	}
	// The page is the dashboard package's to test; here it only has to be the
	// dashboard of this repository's snapshot.
	for _, want := range []string{"<html", "go_sample"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the served page does not contain %q", want)
		}
	}

	// Ctrl-C stops it, and stopping is a clean exit.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupting the dashboard: %v", err)
	}
	rest, _ := io.ReadAll(reader)
	report.Write(rest)
	waitErr := cmd.Wait()
	stopped = true
	code := 0
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			t.Fatalf("waiting for the dashboard: %v", waitErr)
		}
		code = exit.ExitCode()
	}

	port := regexp.MustCompile(`127\.0\.0\.1:\d+`)
	got := fmt.Sprintf("$ enola dashboard .\nexit: %d\n--- stderr\n%s", code, port.ReplaceAllString(s.normalize(report.String()), "127.0.0.1:<PORT>"))
	assertCLIGolden(t, filepath.Join(s.scenario, "serve.golden"), got)
}
