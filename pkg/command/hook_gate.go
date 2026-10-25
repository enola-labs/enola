package command

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Claude Code lists an MCP server's tools as deferred, names only, and a search tool is
// always one step closer than loading one. Told by the installed rule to use enola for
// structure, an agent still grepped a question the rule names, because grep was there and
// enola's tools were not. Three Claude-only hooks close that gap, the same three things
// the opencode plugin already does:
//
//   - at session start, the agent is told how to load enola's tools (sessionStartNote);
//   - the first searches of a session are refused with that same pointer, until an
//     enola tool has been used or the budget is spent (runPreToolHook);
//   - a use of any enola tool is recorded, which ends the refusals (runPostToolHook).
//
// The refusal is bounded on purpose. A search is often the right tool, and an agent
// that has been pointed at enola twice and searches again has decided; the gate gets out
// of the way. Every path exits 0, and every failure says nothing, as the other hooks do.

// gateBudget is how many searches a session has refused before the gate stops.
const gateBudget = 2

// enolaToolsQuery is the ToolSearch query that loads the core tools in one call.
const enolaToolsQuery = "select:mcp__enola__generate_snapshot,mcp__enola__impact_analysis,mcp__enola__traverse," +
	"mcp__enola__query_facts,mcp__enola__explore,mcp__enola__query_insights"

// sessionStartNote is what a Claude Code session is told when it starts.
const sessionStartNote = "enola is connected for this project. If its tools are listed by name only (deferred), load " +
	"the core ones with ToolSearch, query " + enolaToolsQuery + "; if they are already callable, call them directly. Use them for questions about structure — which " +
	"files or packages depend on, call, or would break if X changes; how A reaches B; what nothing uses — " +
	"before searching files: a text search misses a package that reaches X only through another, a call " +
	"through an interface, and a function passed as a value."

// gateReason is what a refused search is told.
func gateReason(left int) string {
	more := "This is the last search it refuses this session."
	if left > 0 {
		more = fmt.Sprintf("It refuses %d more search(es) this session, until an enola tool is used.", left)
	}
	return "If this search is about structure — what depends on, calls or would break with something, how " +
		"one part reaches another, what is unused — ask enola first: impact_analysis, traverse, find_path, " +
		"query_facts, query_insights. If its tools are deferred, load them with ToolSearch, query " +
		enolaToolsQuery + ". If this is a search for text and not for structure, run it again. " + more
}

// gateState is one session's record: whether it has used enola, and how many searches
// the gate has refused.
type gateState struct {
	EnolaUsed bool `json:"enola_used"`
	Refused   int  `json:"refused"`
}

// sessionIDPattern keeps a session id usable as a file name.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// gateStatePath is where a session's gate state lives: the system temporary directory,
// one small file per session, so the gate needs no repository and leaves none dirty.
func gateStatePath(session string) (string, bool) {
	if !sessionIDPattern.MatchString(session) {
		return "", false
	}
	return filepath.Join(os.TempDir(), "enola-gate", session+".json"), true
}

func loadGate(session string) gateState {
	var st gateState
	if p, ok := gateStatePath(session); ok {
		if raw, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(raw, &st)
		}
	}
	return st
}

func saveGate(session string, st gateState) {
	p, ok := gateStatePath(session)
	if !ok {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	raw, _ := json.Marshal(st)
	_ = os.WriteFile(p, raw, 0o600)
}

// treeSearches are the shell programs that search a tree for names or text whatever
// their operands; textSearches search the files they are given, and a tree only when
// told to recurse (grep) or given a directory or nothing at all (rg, ag, ack).
var (
	treeSearches = map[string]bool{"find": true, "fd": true}
	textSearches = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true}
	// recursesByDefault are the text searches that walk the working directory when
	// given no path.
	recursesByDefault = map[string]bool{"rg": true, "ag": true, "ack": true}
)

// isSearch reports whether a tool call searches files: the Grep and Glob tools, or a
// shell command that, in any part of a pipeline, searches a tree. A grep of files
// named on the command line, or of the output piped into it, reads what the agent
// already found; refusing it sends the agent nowhere enola can help.
func isSearch(tool string, input map[string]any) bool {
	switch tool {
	case "Grep", "Glob":
		return true
	case "Bash":
		cmd, _ := input["command"].(string)
		for _, c := range shellCommands(cmd) {
			words := c.words
			for len(words) > 0 && (strings.Contains(words[0], "=") || words[0] == "cd" || words[0] == "sudo") {
				if words[0] == "cd" && len(words) > 1 {
					words = words[2:]
					continue
				}
				words = words[1:]
			}
			if len(words) == 0 {
				continue
			}
			prog := filepath.Base(words[0])
			switch {
			case treeSearches[prog], prog == "git" && len(words) > 1 && words[1] == "grep":
				return true
			case textSearches[prog] && searchesTree(prog, words[1:], c.piped):
				return true
			}
		}
	}
	return false
}

// valueFlags are the grep, rg, ag and ack options whose value is the next word.
var valueFlags = map[string]bool{
	"-e": true, "-f": true, "-A": true, "-B": true, "-C": true, "-m": true, "-d": true, "-D": true,
	"-g": true, "-t": true, "-T": true, "-M": true,
	"--regexp": true, "--file": true, "--include": true, "--exclude": true, "--exclude-dir": true,
	"--context": true, "--after-context": true, "--before-context": true, "--max-count": true,
	"--glob": true, "--type": true, "--type-not": true,
}

// searchesTree reports whether a text search walks a tree rather than reading the
// files it names or its piped input. A path operand that names no file, a "." or a
// directory, counts as a tree.
func searchesTree(prog string, args []string, piped bool) bool {
	var operands []string
	pattern := true // the first operand is the pattern, unless -e or -f gave it
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			operands = append(operands, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			if a == "--recursive" || a == "--dereference-recursive" {
				return true
			}
			if a == "--regexp" || a == "--file" || strings.HasPrefix(a, "--regexp=") || strings.HasPrefix(a, "--file=") {
				pattern = false
			}
			if valueFlags[a] {
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			flags := a[1:]
			if prog != "rg" && strings.ContainsAny(flags, "rR") {
				return true
			}
			if strings.ContainsAny(flags, "ef") {
				pattern = false
			}
			// A value flag last in a cluster ("-nA" "3") takes the next word.
			if valueFlags["-"+flags[len(flags)-1:]] {
				i++
			}
		default:
			operands = append(operands, a)
		}
	}
	if pattern && len(operands) > 0 {
		operands = operands[1:]
	}
	if len(operands) == 0 {
		return recursesByDefault[prog] && !piped
	}
	for _, o := range operands {
		if !looksLikeFile(o) {
			return true
		}
	}
	return false
}

// looksLikeFile reports whether a path operand names a file rather than a directory:
// its last element carries an extension. The hook cannot stat it reliably, since the
// command may cd first.
func looksLikeFile(p string) bool {
	base := filepath.Base(p)
	dot := strings.LastIndexByte(base, '.')
	return dot > 0 && dot < len(base)-1 && !strings.ContainsAny(base, "*?[")
}

// shellCommand is one command of a command line: its words with quotes removed, and
// whether its input is piped from the command before it.
type shellCommand struct {
	words []string
	piped bool
}

// shellCommands splits a command line into the commands it runs, honouring quotes,
// so a pattern such as "a\|b" stays one word of one command.
func shellCommands(line string) []shellCommand {
	var (
		out    []shellCommand
		cur    shellCommand
		word   strings.Builder
		inWord bool
		quote  byte
	)
	endWord := func() {
		if inWord {
			cur.words = append(cur.words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCommand := func(piped bool) {
		endWord()
		if len(cur.words) > 0 {
			out = append(out, cur)
		}
		cur = shellCommand{piped: piped}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(line) {
				i++
				word.WriteByte(line[i])
			} else {
				word.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(line):
			i++
			word.WriteByte(line[i])
			inWord = true
		case c == ' ' || c == '\t':
			endWord()
		case c == ';' || c == '\n':
			endCommand(false)
		case c == '&':
			if i+1 < len(line) && line[i+1] == '&' {
				i++
			}
			endCommand(false)
		case c == '|':
			if i+1 < len(line) && line[i+1] == '|' {
				i++
				endCommand(false)
			} else {
				endCommand(true)
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCommand(false)
	return out
}

// isEnolaTool reports whether a tool name is one of enola's MCP tools, registered
// directly (`mcp__enola__…`) or by a plugin (`mcp__plugin_enola_enola__…`).
func isEnolaTool(tool string) bool {
	return strings.HasPrefix(tool, "mcp__enola__") || strings.HasPrefix(tool, "mcp__plugin_enola_")
}

// runPreToolHook refuses a session's first searches until enola has been used.
func (r *Runner) runPreToolHook() {
	in, err := readHookInput()
	if err != nil || in.SessionID == "" || !isSearch(in.ToolName, in.ToolInput) {
		return
	}
	st := loadGate(in.SessionID)
	if st.EnolaUsed || st.Refused >= gateBudget {
		return
	}
	st.Refused++
	saveGate(in.SessionID, st)
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": gateReason(gateBudget - st.Refused),
	}})
	_, _ = os.Stdout.Write(out)
}

// runPostToolHook records that a session has used enola, which ends the refusals.
func (r *Runner) runPostToolHook() {
	in, err := readHookInput()
	if err != nil || in.SessionID == "" || !isEnolaTool(in.ToolName) {
		return
	}
	st := loadGate(in.SessionID)
	if st.EnolaUsed {
		return
	}
	st.EnolaUsed = true
	saveGate(in.SessionID, st)
}

// writeSessionStartNote hands the session the note above, as Claude Code's
// SessionStart contract takes it.
func writeSessionStartNote() {
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "SessionStart",
		"additionalContext": sessionStartNote,
	}})
	_, _ = os.Stdout.Write(out)
}
