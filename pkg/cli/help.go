package cli

import (
	"fmt"
	"io"
	"strings"
)

// descColumn is the column at which a flag's or command's description starts.
// Continuation lines in a multi-line description are indented to match.
const descColumn = 22

// FlagDoc documents one flag or command. Desc may span several lines; the
// renderer indents continuations to the description column.
type FlagDoc struct {
	Flag string
	Desc string
}

// Section is a free-form trailing block of the help text (LICENSE, BUILD, …).
// Body is emitted verbatim, so it carries its own indentation.
type Section struct {
	Title string
	Body  string
}

// Example is one entry of the EXAMPLES block: a comment line and the command
// it introduces.
type Example struct {
	Comment string
	Command string
}

// Binary identifies the binary the help text is describing. It is what makes
// the shared help reusable: everything a wrapper needs to rename is in here.
type Binary struct {
	Name       string // executable name, e.g. "enola"
	CmdPackage string // go build target, e.g. "./cmd/enola"
	VersionVar string // ldflags -X target for the version string

	// BuildOutput is the artifact name a source build produces, when it differs
	// from Name (a wrapper commonly ships under a shorter one). Defaults to Name.
	BuildOutput string

	// Version is the build version of THIS binary, as its own main package knows
	// it — the value VersionVar names, already resolved.
	//
	// It is carried here rather than read from internal/version because that
	// variable is enola's, stamped by enola's release workflow. A wrapper is
	// stamped through a different -X target (typically its own `main.version`),
	// so anything reading internal/version inside a wrapper
	// reports "dev" forever: `doctor` said "enola dev", the SARIF driver claimed
	// to be enola dev, and the dashboard registered its instance as dev. Empty
	// falls back to internal/version, which is correct for the OSS binary and is
	// what keeps this field optional.
	Version string
}

// output returns the artifact name to show in the BUILD section.
func (b Binary) output() string {
	if b.BuildOutput != "" {
		return b.BuildOutput
	}
	return b.Name
}

// HelpSpec is the rendered form of `--help`. Every field is plain data, so a caller
// composing its own help starts from DefaultHelp rather than restating the shared text.
type HelpSpec struct {
	Bin       Binary
	Tagline   string    // one-line description, printed beside the binary name
	Intro     string    // short paragraph under the title
	Usage     []string  // USAGE lines, already prefixed with the binary name
	Commands  []FlagDoc // COMMANDS block; omitted when empty
	Flags     []FlagDoc // FLAGS block
	ConfigDoc string    // CONFIG_PATH block body (one line); omitted when empty
	Examples  []Example // EXAMPLES block
	Sections  []Section // trailing blocks, in order
}

// DefaultHelp returns the help shared by every enola binary. It documents what the
// engine itself provides, with the binary's own name and version substituted in.
func DefaultHelp(bin Binary) HelpSpec {
	return HelpSpec{
		Bin:     bin,
		Tagline: "MCP server for architectural snapshots",
		Intro:   "Give your AI agent a map of the codebase before it starts exploring.",
		Usage: []string{
			bin.Name + " [flags] [repo_path|config_path]",
			bin.Name + " dashboard [--open] [repo_path|config_path]",
			bin.Name + " cluster init [--dry-run] [dir]",
			bin.Name + " baseline <pin|show|clear> [repo_path|config_path]",
			bin.Name + " check [flags] [repo_path|config_path]",
			bin.Name + " constraints <lint|mine|ledger> [repo_path|config_path]",
			bin.Name + " plan [flags] [path...] [repo_path|config_path]",
			bin.Name + " coverage [flags] [repo_path|config_path]",
			bin.Name + " doctor [repo_path]",
			bin.Name + " log [flags] [repo_path|config_path]",
			bin.Name + " show [<revision>] [repo_path|config_path]",
			bin.Name + " diff <revA>..<revB> [repo_path|config_path]",
			bin.Name + " blame [flags] <pattern> [repo_path|config_path]",
			bin.Name + " gc [flags] [repo_path|config_path]",
			bin.Name + " history <push|pull|verify|gc> [store_dir] [repo_path|config_path]",
			bin.Name + " install [--hooks] [--global] [--targets=…] [repo_path]",
		},
		Commands: []FlagDoc{
			{Flag: "dashboard", Desc: "Explore the latest snapshot in a read-only local web dashboard,\nwithout starting an MCP server. Add --open to launch the browser.\nStays attached to the terminal; stop with Ctrl-C."},
			{Flag: "cluster", Desc: "Write a cluster config for a folder that holds several git\nrepositories. \"init\" lists every immediate subfolder that is a\nrepository in cluster.yaml, refusing to overwrite; pass that file\nto --generate to index them as one linked graph."},
			{Flag: "install", Desc: "Write " + bin.Name + "'s instructions into the files your coding\nagents read (Claude Code, Cursor, Copilot, Codex, Pi, opencode,\nAGENTS.md). Previews every change and asks before writing.\n  --hooks   also run the loop automatically, instead of leaving\n            the agent to remember. In Claude Code, Codex and Pi:\n            pin a baseline at session start and report the\n            architectural delta at the end, but only if the\n            change introduced a regression. In opencode, which\n            has no hooks of that shape: a plugin that points the\n            session's first searches at the index instead.\n            Opt-in, because both run code. Pi always gets its\n            extension, which serves the tools; --hooks only adds\n            the session hooks to it.\n  --targets a comma-separated subset, for narrowing a run. The\n            default is every target, and a target whose agent is\n            not installed skips itself and says so.\n  --global  configure this user rather than this repository.\n  --dry-run show what would change and write nothing."},
			{Flag: "uninstall", Desc: "Remove everything \"install\" wrote, leaving the rest of each\nfile byte-for-byte as it was."},
			{Flag: "baseline", Desc: "Manage the diff baseline — the \"before\" your changes are graded\nagainst. \"pin\" snapshots the repository and freezes it (no separate\n--generate needed), \"show\" reports what the current baseline\ndescribes, \"clear\" removes it. The baseline is stored per-repository,\nin that repo's output dir, so several repos each keep their own."},
			{Flag: "check", Desc: "Grade what a change did to the architecture against the pinned\nbaseline, and exit with a code CI can act on:\n  0 clean · 1 regression · 2 error · 3 declined (not comparable)\nRead-only by default — nothing is written, and the baseline stays\nput, so it can be run as often as you like. Run\n\"" + bin.Name + " check --help\" for the flags."},
			{Flag: "constraints", Desc: "Author the declared constraint vocabulary. \"lint\" parses each repo's\nenola-intent.yaml (and any cluster-config intent override), reports\nevery validation problem with its file context, and resolves each\ndeclared component against the current snapshot if one exists —\nso a selector that matches nothing is caught while authoring, not\nby a vacuously-passing rule. Exits 1 on validation problems.\n\"mine\" searches the snapshot's fact store for near-invariants and\nreports candidate rules with their evidence and named exceptions —\nproposals for review, never self-adopting law.\n\"ledger\" reports how much of the law you already declared is being\nEXCUSED rather than obeyed — each rule's breaches beside the\nsuppressions and exemptions that signed them away, with every\nexcuse's owner and age. Evidence for judging a rule, never a gate."},
			{Flag: "plan", Desc: "The pre-edit contract: which declared constraints govern an\nintended change (--paths, --symbols), its blast radius over the\ncurrent snapshot, and — for a --patch — the constraint verdicts\nthat WOULD appear, evaluated over a scratch copy BEFORE any edit\nlands in the tree. Nothing is written; a report, never a gate.\nRun \"" + bin.Name + " plan --help\" for the flags."},
			{Flag: "coverage", Desc: "Report which cross-repo edges were resolved and which were not,\nper service — telling a genuinely isolated service apart from one\nwhose outbound edges could not be followed. Needs two or more\nrepositories in one graph. A report, not a gate: always exits 0."},
			{Flag: "endpoint", Desc: "Report what changing an HTTP endpoint reaches: the controller\nserving it, the models that controller touches, the models\nassociated with those, the tables behind them, and the callers,\nincluding the frontend screen a calling route module implements.\nUse impact_analysis when you have a symbol; use this when what\nyou have is a URL."},
			{Flag: "log", Desc: "EXPERIMENTAL. Show what this repository's architecture has done over\ntime — one line per recorded snapshot, with what changed since the\none before it. Read-only: it reports what was observed and never\nsnapshots to fill a gap. Every snapshot is recorded as a revision\n(~450 bytes, outside the repo); set `history.enabled: false` to stop."},
			{Flag: "show", Desc: "EXPERIMENTAL. Show what ONE recorded revision did to the architecture\n— \"log\" says a revision added twelve facts, this says which twelve.\nReconstructs the revision and its predecessor out of\nthe stored history and compares them, so a past change is described in the same words it\nwas described in at the time. A revision is a snapshot id or prefix, a\ngit commit, HEAD~N, @<seq>, a ref name, or `latest` (the default)."},
			{Flag: "diff", Desc: "EXPERIMENTAL. Show the architecture delta between any two recorded\nrevisions — the question a week of work produces, where \"show\" answers\nfor a single one. Either side of the range may be empty, meaning the\noldest or newest recorded revision."},
			{Flag: "blame", Desc: "EXPERIMENTAL. Show when something entered the architecture and when\nit left — \"when did this module start importing that one?\", which a\nsnapshot cannot answer however good it is, because it is a question\nabout the past. Matches a name, a path, or both ends of an edge\nagainst the recorded facts; --findings searches findings instead,\nand --first stops at the introduction."},
			{Flag: "gc", Desc: "EXPERIMENTAL. Report what the architecture history holds — how many\nrevisions, how many can still be replayed, how much disk — and remove\nwhat it no longer needs. With no flags it removes only garbage;\n--thin-older-than and --prune-working discard things a reader could\nstill reach, so each has to be asked for."},
			{Flag: "history", Desc: "EXPERIMENTAL. Share the architecture history between machines through\na directory store — a git repository, a shared mount, an S3-synced\nfolder. Plain files, content-addressed, tamper-evident. \"push\" copies\nlocal revisions in, \"pull\" imports what other machines pushed,\n\"verify\" walks every chain and names gaps and tampering, \"gc\" applies\nretention — printed first, deleted only with --apply, recorded in the\nchain. Point it with history.shared_dir or the first argument."},
			{Flag: "doctor", Desc: "Report whether the session hooks are actually FIRING in this\nrepository, not merely configured. `install --hooks` can write a\nconfiguration your agent silently ignores — it reports success\neither way — so this asks the only question that settles it: when\ndid each hook last run, and what did it conclude? A report, not a\ngate: always exits 0."},
			{Flag: "providers", Desc: "The fact providers this binary carries itself. `providers list`\nnames each and whether it is ready; `providers fetch rubydex`\ndownloads the pinned Rubydex engine library, verifies its published\ndigest, and caches it, after which a `providers:` entry named\nrubydex with no command runs in-process. The only network access a\nprovider ever makes, and never at snapshot time."},
		},
		Flags: []FlagDoc{
			{Flag: "--generate", Desc: "Generate a snapshot and exit (do not start MCP server)"},
			{Flag: "--refresh", Desc: "With --generate on a cluster config: re-read ONE configured repository\ninto the union the last full generate produced, replacing its slice,\nand write the result to that repository and to the union's home. The\nother repositories are not re-read. Needs a prior full --generate."},
			{Flag: "--explain [path]", Desc: "Print a human-readable repository statistics report and exit.\nA directory is a repository; a file is a config, so a `repos:`\nconfig reports over the whole cluster."},
			{Flag: "--list", Desc: "List the MCP tools this build can serve"},
			{Flag: "--status", Desc: "Show MCP server status: uptime, tool usage and estimated value,\naggregated across every repo the server has served. While the server\nis running this also prints the dashboard URL."},
			{Flag: "--status --all", Desc: "Show the per-repo breakdown instead (from ~/.enola/usage/)"},
			{Flag: "--no-dashboard", Desc: "Do not start the localhost dashboard alongside the MCP server"},
			{Flag: "--no-cluster", Desc: "With --generate on a folder that holds several git repositories:\nindex it as one repository instead of as a cluster"},
			{Flag: "--version", Desc: "Print version information"},
			{Flag: "--version --json", Desc: "Print the version and the extractor version as JSON, on stdout.\nThis is the release manifest: what a build is called, and what it\nEXTRACTS LIKE. See UPDATES."},
			{Flag: "--help, -h", Desc: "Show this help message"},
		},
		ConfigDoc: "Path to the config file (default: mcp-arch.yaml). Set `repos:` in it to\n  name a multi-repo cluster; entries resolve relative to the config file, so\n  a checked-in cluster config means the same thing wherever it is run from.",
		Examples: []Example{
			{Comment: "Start the dashboard and open it (Ctrl-C to stop)", Command: bin.Name + " dashboard --open"},
			{Comment: "Start MCP server with default config", Command: bin.Name},
			{Comment: "Start MCP server with custom config", Command: bin.Name + " my-config.yaml"},
			{Comment: "Generate a snapshot and exit", Command: bin.Name + " --generate"},
			{Comment: "Index a whole cluster from one config (see CONFIG_PATH)", Command: bin.Name + " --generate cluster.yaml"},
			{Comment: "Re-read one repository of that cluster into the existing union", Command: bin.Name + " --generate --refresh ../service-a cluster.yaml"},
			{Comment: "Print a statistics report for a repository and exit", Command: bin.Name + " --explain /path/to/repo"},
			{Comment: "Report over a whole cluster", Command: bin.Name + " --explain cluster.yaml"},
			{Comment: "Generate snapshot with custom config", Command: bin.Name + " --generate my-config.yaml"},
			{Comment: "Freeze another repo's architecture before editing it (see THE GATE)", Command: bin.Name + " baseline pin /path/to/repo"},
			{Comment: "Report what your changes did to it (reports everything, fails nothing)", Command: bin.Name + " check /path/to/repo"},
			{Comment: "Report what the pinned baseline describes", Command: bin.Name + " baseline show /path/to/repo"},
			{Comment: "Tell your agents enola is here (instructions only)", Command: bin.Name + " install"},
			{Comment: "…and close the loop automatically at the end of a session", Command: bin.Name + " install --hooks"},
			{Comment: "Preview what install would change, writing nothing", Command: bin.Name + " install --hooks --dry-run"},
			{Comment: "Remove it all again", Command: bin.Name + " uninstall"},
			{Comment: "Fail on a violation of a layer order you declared", Command: bin.Name + " check --fail-on=layers"},
			{Comment: "Enforce a policy you set, but only warn this time", Command: bin.Name + " check --fail-on=layers --warn-only"},
			{Comment: "See which cross-repo edges resolved, and which did not", Command: bin.Name + " coverage cluster.yaml"},
			{Comment: "Just the blind spots", Command: bin.Name + " coverage --unresolved cluster.yaml"},
			{Comment: "Are the session hooks actually firing?", Command: bin.Name + " doctor"},
			{Comment: "Compare against the preceding snapshot instead of the pin", Command: bin.Name + " check --baseline=previous"},
			{Comment: "Fail on a layer order enola only INFERRED (never reaches 1.00)", Command: bin.Name + " check --fail-on=layers --min-confidence=0.8"},
			{Comment: "Opt into the cycle check as well", Command: bin.Name + " check --fail-on=layers,cycles"},
			{Comment: "Did the change stay where you said it would?", Command: bin.Name + " check --target=internal/auth"},
			{Comment: "…and fail if it reached anywhere else", Command: bin.Name + " check --target=internal/auth --max-spillover=0"},
			{Comment: "Check MCP server status", Command: bin.Name + " --status"},
			{Comment: "Show usage broken down per repo", Command: bin.Name + " --status --all"},
			{Comment: "Check version", Command: bin.Name + " --version"},
		},
		Sections: []Section{
			dashboardSection(bin),
			gateSection(bin),
			updatesSection(bin),
			mcpConfigSection(bin),
			buildSection(bin),
		},
	}
}

// gateSection documents the before/after loop. It is a section rather than a list of
// examples because the ORDER is the whole point: the baseline has to be pinned before the
// edit, and a reader who finds `check` first will otherwise run it against nothing.
func gateSection(bin Binary) Section {
	return Section{
		Title: "THE GATE — grading a change",
		Body: fmt.Sprintf(`  Tests tell you whether behaviour still works. This tells you whether a change
  altered the STRUCTURE of the system in a way nobody asked for — a layer crossed the
  wrong way, coupling between modules that had no business knowing about each other.

  It needs a "before" to compare against, so the order matters:

    1.  %s baseline pin /path/to/repo     # freeze how it looks NOW, before editing
    2.  …make your changes…
    3.  %s check /path/to/repo            # grade what they did

  Step 1 snapshots the repository and freezes it — no separate --generate needed.
  Omit the path to act on the current directory.

  The baseline lives in that repository's own output dir (.enola/baseline), so each
  repo keeps its own and you can hold baselines for several at once. "baseline show"
  reports what the current one describes; "baseline clear" removes it.

  Step 3 is read-only: it writes nothing and leaves the baseline in place, so it can
  be run as often as you like and re-run after a fix. It exits 0 when clean, 1 on a
  structural regression, 2 when it could not run, and 3 when it declined to grade
  because the baseline is not comparable — 3 is never a statement about your change.

  Step 3 fails nothing on its own. It reports every finding the change introduced and
  exits 0; "--fail-on=<explainer,…>" names the ones that should break the build, and
  "--max-spillover=N" bounds how far outside "--target" the change may reach. A run
  with neither says so in its output rather than printing a bare PASS.

  A path that names a DIRECTORY is a repository; one that names a FILE is a config.
`, bin.Name, bin.Name),
	}
}

// dashboardSection documents the read-only HTTP dashboard served alongside the
// MCP server.
func dashboardSection(bin Binary) Section {
	return Section{
		Title: "DASHBOARD",
		Body: fmt.Sprintf(`  Run "%s dashboard [repo_path|config_path]" to serve the latest snapshot
  without starting an MCP server; add --open to launch the browser. It stays attached
  to the terminal until Ctrl-C. The normal MCP
  server also exposes the same read-only dashboard. Data refresh is explicit, so an
  investigation is never interrupted. Run "--status" to find its URL, or pass
  "--no-dashboard" to disable it.

  In an interactive terminal, "--generate", "--refresh", and "check --write"
  each print an "Explore this snapshot" command using "dashboard --open". CI,
  redirected output, hooks, and ENOLA_NO_PROMPTS=1 suppress the hint.
`, bin.Name),
	}
}

// updatesSection documents the passive update notice, and — the part that has to be
// discoverable — how to switch it off. A tool that reaches the network on someone's
// machine owes them a documented way to stop it, in the help they already read.
func updatesSection(bin Binary) Section {
	return Section{
		Title: "UPDATES",
		Body: fmt.Sprintf(`  enola checks at most once every 12 hours, in the background, whether a newer
  release exists, and caches the answer in ~/.enola/update.json. No command ever
  waits on the network for it: the notice you see is a read of that file. When
  a newer release is found, "%s doctor" reports it and "%s upgrade"
  installs it.

  The notice says whether the EXTRACTORS changed, not what else did. That single
  bit is the one that matters for your data: it means snapshots from your build
  are missing facts a current enola would extract.

  It is silent for a dev build, and disabled entirely by:

    export ENOLA_NO_UPDATE_CHECK=1

  It also never runs when CI is set.
`, bin.Name, bin.Name),
	}
}

// mcpConfigSection documents how to register the binary with an MCP client.
func mcpConfigSection(bin Binary) Section {
	return Section{
		Title: "MCP CONFIGURATION",
		Body: fmt.Sprintf(`  Add to your MCP client's config (e.g., Cursor mcp.json):

    {
      "mcpServers": {
        "enola": {
          "command": "%s",
          "args": ["mcp-arch.yaml"]
        }
      }
    }

  Or if installed globally via "go install":

    {
      "mcpServers": {
        "enola": {
          "command": "%s"
        }
      }
    }
`, bin.Name, bin.Name),
	}
}

// buildSection documents the version ldflag for a source build.
func buildSection(bin Binary) Section {
	return Section{
		Title: "BUILD",
		Body: fmt.Sprintf(`  Version can be set at build time with ldflags:

    go build -ldflags "-X %s=0.1.0" -o %s %s
`, bin.VersionVar, bin.output(), bin.CmdPackage),
	}
}

// RenderHelp writes the help text for spec to w.
func RenderHelp(w io.Writer, spec HelpSpec) {
	var b strings.Builder

	fmt.Fprintf(&b, "%s — %s\n\n%s\n", spec.Bin.Name, spec.Tagline, spec.Intro)

	if len(spec.Usage) > 0 {
		b.WriteString("\nUSAGE\n")
		for _, u := range spec.Usage {
			fmt.Fprintf(&b, "  %s\n", u)
		}
	}
	writeDocs(&b, "COMMANDS", spec.Commands)
	writeDocs(&b, "FLAGS", spec.Flags)

	if spec.ConfigDoc != "" {
		fmt.Fprintf(&b, "\nCONFIG_PATH\n  %s\n", spec.ConfigDoc)
	}

	if len(spec.Examples) > 0 {
		b.WriteString("\nEXAMPLES\n")
		for i, ex := range spec.Examples {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "  # %s\n  %s\n", ex.Comment, ex.Command)
		}
	}

	for _, sec := range spec.Sections {
		fmt.Fprintf(&b, "\n%s\n%s", sec.Title, sec.Body)
	}

	// Help goes to a terminal stream; a write failure has nowhere useful to go.
	_, _ = io.WriteString(w, b.String())
}

// writeDocs renders a titled block of flag/command documentation, wrapping each
// description's continuation lines to the description column.
func writeDocs(b *strings.Builder, title string, docs []FlagDoc) {
	if len(docs) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", title)
	for _, d := range docs {
		lines := strings.Split(d.Desc, "\n")
		fmt.Fprintf(b, "  %-*s%s\n", descColumn-2, d.Flag, lines[0])
		for _, cont := range lines[1:] {
			fmt.Fprintf(b, "%s%s\n", strings.Repeat(" ", descColumn), cont)
		}
	}
}
