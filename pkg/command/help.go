package command

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/enola-labs/enola/pkg/cli"
)

// HelpArgs rewrites a request for help into the form every command already answers, so
// there is one page per command and four ways to reach it:
//
//	enola help                    → enola --help
//	enola help <cmd> [sub]        → enola <cmd> [sub] --help
//	enola --help <cmd> [sub]      → enola <cmd> [sub] --help
//	enola -h <cmd> [sub]          → enola <cmd> [sub] --help
//
// Rewriting rather than rendering is the point: the page, the stream it goes to and the
// exit code are whatever `<cmd> --help` produces, so the spellings cannot drift apart.
//
// `--help` followed by something that is not a command is returned unchanged, because it
// already meant the top-level help and must go on meaning it. `help` is new, so an
// unknown word after it is an error, with the nearest command suggested.
func (r *Runner) HelpArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	word := args[0] == "help"
	if !word && args[0] != "--help" && args[0] != "-h" {
		return args, nil
	}
	rest := args[1:]
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		if word {
			return append([]string{"--help"}, rest...), nil
		}
		return args, nil
	}
	if rest[0] == "help" || !slices.Contains(r.knownSubcommands(), rest[0]) {
		if !word {
			return args, nil
		}
		if rest[0] == "help" {
			return []string{"--help"}, nil
		}
		if near := r.closestSubcommand(rest[0]); near != "" {
			return nil, fmt.Errorf("no help for %q, did you mean %q?\n\n    %s help %s", rest[0], near, r.name(), near)
		}
		return nil, fmt.Errorf("no help for %q (commands: %s)\n\nRun `%s help`", rest[0], strings.Join(r.knownSubcommands(), ", "), r.name())
	}
	return append(slices.Clone(rest), "--help"), nil
}

// subcommandsOf names the second words each command dispatches on. Help needs them to
// tell a command path from its arguments: `baseline pin --help` is help for `baseline
// pin`, and `doctor /path/to/repo --help` is help for `doctor`.
var subcommandsOf = map[string][]string{
	"baseline":    {"pin", "show", "clear"},
	"cluster":     {"init"},
	"constraints": {"lint", "mine", "init", "explain", "ledger"},
	"history":     {"push", "pull", "verify", "gc"},
	"providers":   {"list", "fetch"},
	"hook":        {"session-start", "stop"},
}

// helpPath reduces a request for help to the command path and --help, dropping every
// other argument. The flag package stops at the first positional, so a --help after
// one never reached it, and `coverage cluster.yaml --help` snapshotted instead of
// describing coverage. With the arguments gone, every command answers --help the way
// it answers it straight after its name.
func helpPath(args []string) []string {
	path := []string{args[0]}
	if len(args) > 1 && slices.Contains(subcommandsOf[args[0]], args[1]) {
		path = append(path, args[1])
	}
	return append(path, "--help")
}

// helpPage is what a command's --help adds after the page it always printed: examples,
// and the pages next to it, a command's own subcommands first. The subcommands
// themselves are not listed again: every page with any already names them.
type helpPage struct {
	examples []cli.Example
	seeAlso  []string // command paths, rendered as `<bin> help <path>`
}

// helpPages is keyed by command path as typed: "flows", "constraints mine". Every path
// --help answers has an entry; TestEveryHelpPageHasExamples holds that.
//
// Example commands are written with "enola" and rendered with the running binary's name.
var helpPages = map[string]helpPage{
	"check": {
		examples: []cli.Example{
			{Comment: "Grade the change against the pinned baseline: report everything, fail nothing", Command: "enola check"},
			{Comment: "Fail on a violation of a layer order you declared", Command: "enola check --fail-on=layers"},
			{Comment: "Opt into the cycle check as well, but only warn this time", Command: "enola check --fail-on=layers,cycles --warn-only"},
			{Comment: "Fail if the change reached outside what you meant to touch", Command: "enola check --target=internal/auth --max-spillover=0"},
			{Comment: "Compare against the preceding snapshot instead of the pin", Command: "enola check --baseline=previous"},
			{Comment: "Write the verdict as SARIF for code scanning", Command: "enola check --format sarif > enola.sarif"},
		},
		seeAlso: []string{"baseline", "plan", "impact"},
	},
	"baseline": {
		examples: []cli.Example{
			{Comment: "Freeze the architecture before you start editing", Command: "enola baseline pin"},
			{Comment: "The same for another repository", Command: "enola baseline pin /path/to/repo"},
			{Comment: "What the pinned baseline describes", Command: "enola baseline show"},
			{Comment: "Remove it", Command: "enola baseline clear"},
		},
		seeAlso: []string{"check", "doctor"},
	},
	"cluster": {
		examples: clusterExamples,
		seeAlso:  []string{"cluster init", "coverage"},
	},
	"cluster init": {
		examples: clusterExamples,
		seeAlso:  []string{"coverage"},
	},
	"constraints": {
		examples: []cli.Example{
			{Comment: "Validate the declarations and resolve every component", Command: "enola constraints lint"},
			{Comment: "Preview a first declaration before writing it", Command: "enola constraints init --dry-run"},
			{Comment: "Propose rules the code already almost follows", Command: "enola constraints mine"},
			{Comment: "Which components a file belongs to, and why", Command: "enola constraints explain internal/auth/session.go"},
			{Comment: "Which rules are excused more than obeyed", Command: "enola constraints ledger"},
		},
		seeAlso: []string{"constraints mine", "constraints init", "constraints explain", "constraints ledger", "check", "plan"},
	},
	"constraints mine": {
		examples: []cli.Example{
			{Comment: "Propose rules from the current snapshot", Command: "enola constraints mine"},
			{Comment: "Only near-certain candidates, all of them", Command: "enola constraints mine --min-confidence 0.98 --top 0"},
			{Comment: "Keep the full report as JSONL", Command: "enola constraints mine --jsonl candidates.jsonl"},
			{Comment: "Write the file-local candidates as ESLint rules instead", Command: "enola constraints mine --scaffold-eslint eslint-rules"},
		},
		seeAlso: []string{"constraints init", "constraints"},
	},
	"constraints init": {
		examples: []cli.Example{
			{Comment: "Print the declaration it would write", Command: "enola constraints init --dry-run"},
			{Comment: "Write it", Command: "enola constraints init"},
			{Comment: "Bind one shipped recipe only", Command: "enola constraints init --recipe layered"},
		},
		seeAlso: []string{"constraints", "constraints mine"},
	},
	"constraints explain": {
		examples: []cli.Example{
			{Comment: "The components a file belongs to, its edges, and what moving it would change", Command: "enola constraints explain internal/auth/session.go"},
			{Comment: "The same as JSON, without the re-evaluation", Command: "enola constraints explain --json --no-radius internal/auth/session.go"},
		},
		seeAlso: []string{"constraints", "plan"},
	},
	"constraints ledger": {
		examples: []cli.Example{
			{Comment: "Every rule beside the excuses that signed its breaches away", Command: "enola constraints ledger"},
			{Comment: "The same as JSON", Command: "enola constraints ledger --json"},
		},
		seeAlso: []string{"constraints", "check"},
	},
	"plan": {
		examples: []cli.Example{
			{Comment: "Which constraints govern a change to this file, and its blast radius", Command: "enola plan internal/auth/session.go"},
			{Comment: "A whole directory as the target", Command: "enola plan --paths=internal/auth"},
			{Comment: "The verdicts a patch would produce, before it is applied", Command: "enola plan --patch change.diff"},
		},
		seeAlso: []string{"constraints", "impact", "check"},
	},
	"coverage": {
		examples: []cli.Example{
			{Comment: "See which cross-repo edges resolved, and which did not", Command: "enola coverage cluster.yaml"},
			{Comment: "Just the blind spots, with each call site", Command: "enola coverage --unresolved cluster.yaml"},
			{Comment: "One service only", Command: "enola coverage --repo billing cluster.yaml"},
		},
		seeAlso: []string{"cluster", "endpoint"},
	},
	"endpoint": {
		examples: []cli.Example{
			{Comment: "What changing an endpoint reaches", Command: "enola endpoint /v1/users"},
			{Comment: "One verb only, across a cluster", Command: "enola endpoint 'GET /v1/users' cluster.yaml"},
			{Comment: "The same as JSON", Command: "enola endpoint --json /v1/users"},
		},
		seeAlso: []string{"impact", "coverage"},
	},
	"impact": {
		examples: []cli.Example{
			{Comment: "The blast radius of changing a symbol", Command: "enola impact SessionStore"},
			{Comment: "Name every dependent, two hops deep", Command: "enola impact --list --max-depth 2 SessionStore"},
			{Comment: "Narrow an ambiguous target with a scope prefix", Command: "enola impact 'file:internal/auth SessionStore'"},
			{Comment: "Also what the target itself depends on", Command: "enola impact --include-forward SessionStore"},
		},
		seeAlso: []string{"endpoint", "flows", "plan"},
	},
	"flows": {
		examples: []cli.Example{
			{Comment: "The ten largest flows", Command: "enola flows"},
			{Comment: "More of them", Command: "enola flows --top 50"},
			{Comment: "One flow's stages and what it reaches", Command: "enola flows --show main"},
			{Comment: "Only the flows that reach a symbol", Command: "enola flows --through SessionStore"},
		},
		seeAlso: []string{"impact", "endpoint"},
	},
	"doctor": {
		examples: []cli.Example{
			{Comment: "Are the session hooks actually firing here?", Command: "enola doctor"},
			{Comment: "For another repository, as JSON", Command: "enola doctor --json /path/to/repo"},
		},
		seeAlso: []string{"install", "hook"},
	},
	"providers": {
		examples: []cli.Example{
			{Comment: "Which built-in providers are ready", Command: "enola providers list"},
			{Comment: "Download and verify the Rubydex engine library", Command: "enola providers fetch rubydex"},
		},
	},
	"dashboard": {
		examples: []cli.Example{
			{Comment: "Serve the latest snapshot and open it (Ctrl-C to stop)", Command: "enola dashboard --open"},
			{Comment: "The same for a whole cluster", Command: "enola dashboard cluster.yaml"},
		},
	},
	"log": {
		examples: []cli.Example{
			{Comment: "The recorded revisions, oldest first", Command: "enola log"},
			{Comment: "With the branch topology, more of them", Command: "enola log --graph -n 50"},
			{Comment: "The last three days, by fact kind", Command: "enola log --stat --since 72h"},
			{Comment: "Build a timeline from merge commits, listing them first", Command: "enola log --backfill --sample merges --dry-run"},
		},
		seeAlso: []string{"show", "diff", "blame", "gc"},
	},
	"show": {
		examples: []cli.Example{
			{Comment: "What the latest revision did", Command: "enola show"},
			{Comment: "What a past commit did", Command: "enola show HEAD~3"},
			{Comment: "Only what touched one module", Command: "enola show --focus internal/auth HEAD~3"},
		},
		seeAlso: []string{"log", "diff"},
	},
	"diff": {
		examples: []cli.Example{
			{Comment: "What the last ten commits did to the architecture", Command: "enola diff HEAD~10..HEAD"},
			{Comment: "Everything since a release, up to the newest revision", Command: "enola diff v1.2.0.."},
			{Comment: "Only what touched one module", Command: "enola diff --focus internal/auth HEAD~10..HEAD"},
		},
		seeAlso: []string{"show", "log", "blame"},
	},
	"blame": {
		examples: []cli.Example{
			{Comment: "When did anything matching this enter or leave", Command: "enola blame internal/auth"},
			{Comment: "Only when it first appeared", Command: "enola blame --first SessionStore"},
			{Comment: "Search findings instead of facts", Command: "enola blame --findings cycle"},
		},
		seeAlso: []string{"log", "show"},
	},
	"gc": {
		examples: []cli.Example{
			{Comment: "What the history holds, and what would go", Command: "enola gc --dry-run"},
			{Comment: "Remove garbage only", Command: "enola gc"},
			{Comment: "Drop stored contents older than 90 days, keeping the timeline", Command: "enola gc --thin-older-than 90d"},
		},
		seeAlso: []string{"log", "history gc"},
	},
	"history": {
		examples: []cli.Example{
			{Comment: "Share this machine's history through a directory", Command: "enola history push /mnt/shared/enola-history"},
			{Comment: "Import what other machines pushed", Command: "enola history pull /mnt/shared/enola-history"},
			{Comment: "With history.shared_dir set, the store can be left out", Command: "enola history verify"},
		},
		seeAlso: []string{"history push", "history pull", "history verify", "history gc", "log"},
	},
	"history push": {
		examples: []cli.Example{
			{Comment: "Copy this machine's revisions into the store", Command: "enola history push /mnt/shared/enola-history"},
			{Comment: "Working revisions too", Command: "enola history push --all /mnt/shared/enola-history"},
		},
		seeAlso: []string{"history pull", "history verify"},
	},
	"history pull": {
		examples: []cli.Example{
			{Comment: "Import what other machines pushed", Command: "enola history pull /mnt/shared/enola-history"},
			{Comment: "Another repository's history", Command: "enola history pull --repo github.com/org/repo /mnt/shared/enola-history"},
		},
		seeAlso: []string{"history push", "log"},
	},
	"history verify": {
		examples: []cli.Example{
			{Comment: "Check every chain in the store end to end", Command: "enola history verify /mnt/shared/enola-history"},
		},
		seeAlso: []string{"history gc"},
	},
	"history gc": {
		examples: []cli.Example{
			{Comment: "Print what keeping the newest 200 revisions would remove", Command: "enola history gc --keep-last 200 /mnt/shared/enola-history"},
			{Comment: "Remove it, keeping anything younger than 90 days as well", Command: "enola history gc --keep-last 200 --keep-since 90d --apply /mnt/shared/enola-history"},
		},
		seeAlso: []string{"history verify", "gc"},
	},
	"install": {
		examples: []cli.Example{
			{Comment: "Tell your agents enola is here (instructions only)", Command: "enola install"},
			{Comment: "...and close the loop automatically with session hooks", Command: "enola install --hooks"},
			{Comment: "Preview what that would change, writing nothing", Command: "enola install --hooks --dry-run"},
			{Comment: "For every project of this user, two agents only", Command: "enola install --global --targets=claude,cursor"},
		},
		seeAlso: []string{"uninstall", "doctor"},
	},
	"uninstall": {
		examples: []cli.Example{
			{Comment: "Preview what would be removed", Command: "enola uninstall --dry-run"},
			{Comment: "Remove everything install wrote", Command: "enola uninstall"},
		},
		seeAlso: []string{"install"},
	},
	"hook": {
		examples: []cli.Example{
			{Comment: "Register the hooks with your agents", Command: "enola install --hooks"},
			{Comment: "See whether they are firing", Command: "enola doctor"},
		},
		seeAlso: []string{"install", "doctor"},
	},
	"upgrade": {
		examples: []cli.Example{
			{Comment: "Replace this binary with the latest release", Command: "enola upgrade"},
			{Comment: "Which version is running", Command: "enola --version"},
		},
		seeAlso: []string{"doctor"},
	},
}

var clusterExamples = []cli.Example{
	{Comment: "See the config it would write for a folder of repositories", Command: "enola cluster init --dry-run ~/src"},
	{Comment: "Write it", Command: "enola cluster init ~/src"},
	{Comment: "Index every repository in it as one linked graph", Command: "enola --generate ~/src/cluster.yaml"},
}

// HelpFooter renders the Examples and See also sections of path's help
// page, to print after the page itself. It starts with a blank line, so it follows
// whatever came before it; it is empty for a path with no entry.
func (r *Runner) HelpFooter(path string) string {
	page, ok := helpPages[path]
	if !ok {
		return ""
	}
	bin := r.name()
	var b strings.Builder
	if len(page.examples) > 0 {
		b.WriteString("\nExamples:\n")
		for i, ex := range page.examples {
			if i > 0 {
				b.WriteString("\n")
			}
			cmd := ex.Command
			if rest, ok := strings.CutPrefix(cmd, "enola "); ok {
				cmd = bin + " " + rest
			}
			fmt.Fprintf(&b, "  # %s\n  %s\n", ex.Comment, cmd)
		}
	}
	if len(page.seeAlso) > 0 {
		refs := make([]string, len(page.seeAlso))
		for i, p := range page.seeAlso {
			refs[i] = bin + " help " + p
		}
		// Wrapped at 80 columns, continuation lines under the first reference.
		line := "\nSee also: "
		for i, ref := range refs {
			if i < len(refs)-1 {
				ref += ","
			}
			if i > 0 && len(line)+1+len(ref) > 81 {
				b.WriteString(line + "\n")
				line = "          "
			} else if i > 0 {
				line += " "
			}
			line += ref
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// writeHelpFooter adds path's footer to a usage block, when the block is answering a
// request for help. The same usage block also answers a bad flag or a missing argument,
// and there the error stays as short as it always was.
func (r *Runner) writeHelpFooter(w io.Writer, args []string, path string) {
	if HelpRequested(args) {
		_, _ = io.WriteString(w, r.HelpFooter(path))
	}
}
