package command

import (
	"fmt"
	"slices"
	"strings"
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
