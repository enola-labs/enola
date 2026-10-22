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
const sessionStartNote = "enola is connected for this project. Its tools may be listed as deferred: load the core " +
	"ones with ToolSearch, query " + enolaToolsQuery + ". Use them for questions about structure — which " +
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

// searchCommands are the shell programs that search a tree for text or names.
var searchCommands = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true, "find": true, "fd": true}

// shellSeparators splits a command line into the commands it runs.
var shellSeparators = regexp.MustCompile(`&&|\|\||;|\||\n`)

// isSearch reports whether a tool call searches files: the Grep and Glob tools, or a
// shell command whose program, in any part of a pipeline, is a search.
func isSearch(tool string, input map[string]any) bool {
	switch tool {
	case "Grep", "Glob":
		return true
	case "Bash":
		cmd, _ := input["command"].(string)
		for _, seg := range shellSeparators.Split(cmd, -1) {
			words := strings.Fields(seg)
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
			if searchCommands[prog] || (prog == "git" && len(words) > 1 && words[1] == "grep") {
				return true
			}
		}
	}
	return false
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
