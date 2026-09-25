package install

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// piExtension is the extension template. Embedded from a real .js file for the same
// reason opencode_plugin.js is: it stays readable and checkable as what it is. See
// pi_extension.js for what it does.
//
//go:embed pi_extension.js
var piExtension string

// PiExtensionFile is where the extension lives, relative to Pi's configuration root:
// `.pi/` in the repository, or the agent directory `~/.pi/agent/` for a global install.
// Exported so `doctor` can find it without restating the path.
const PiExtensionFile = "extensions/enola.js"

// PiHooksMarker is the line in the written extension that says the session hooks are
// on. `doctor` reads it rather than parsing the file.
const PiHooksMarker = "const HOOKS = true"

// piTarget configures Pi, which reads AGENTS.md but has no MCP client.
//
// Instructions alone were therefore worse than nothing: they name tools Pi cannot call.
// The extension is what makes the instructions true. It is the MCP client, starting the
// enola server and registering its tools with Pi, so it is written on every install and
// not only under --hooks; --hooks adds the session hooks it runs through Pi's events.
//
// Globally the user-level AGENTS.md block is kept alongside it, so projects nobody ran
// `enola install` in still get the guidance. Locally Pi reads the repository's
// AGENTS.md, which the `agents` target already maintains.
func piTarget(o Options, remove bool) ([]Result, error) {
	out, err := globalAgentsTarget(o, remove, "pi", filepath.Join(".pi", "agent", "AGENTS.md"))
	if err != nil {
		return nil, err
	}
	if o.Scope == ScopeLocal {
		// globalAgentsTarget's local answer is a skip explaining AGENTS.md; the extension
		// below is the local install, so that skip would misreport it.
		out = nil
	}

	root, stop := piRoot(o)
	path := filepath.Join(root, filepath.FromSlash(PiExtensionFile))
	if remove {
		r, err := removeOwnedFile(path, stop, o.DryRun)
		return append(out, r), err
	}
	if o.Scope == ScopeGlobal {
		// Same evidence rule as the AGENTS.md block: no ~/.pi, no Pi.
		if _, err := os.Stat(filepath.Join(o.HomeDir, ".pi")); os.IsNotExist(err) {
			return out, nil
		}
	}
	r, err := writeOwnedFile(path, piExtensionSource(o), o.DryRun)
	return append(out, r), err
}

// piRoot is Pi's configuration root for this scope, and the directory uninstall's
// pruning stops at. Globally that is `~/.pi`, the same bound the AGENTS.md block uses:
// its existence is the evidence Pi is installed, so enola may empty it but never remove
// it. Locally enola creates `.pi/` itself, so an empty one goes with the extension.
func piRoot(o Options) (root, stop string) {
	if o.Scope == ScopeGlobal {
		return filepath.Join(o.HomeDir, ".pi", "agent"), filepath.Join(o.HomeDir, ".pi")
	}
	return filepath.Join(o.RepoDir, ".pi"), o.RepoDir
}

// piExtensionSource fills in the binary and the hooks switch. The binary is the same
// absolute path the Claude and Codex hooks invoke, so every agent runs the enola that
// installed it.
func piExtensionSource(o Options) string {
	cmd, _ := json.Marshal(strings.Trim(o.hookCommand(), `"`))
	src := strings.Replace(piExtension, "__ENOLA_COMMAND__", string(cmd), 1)
	return strings.Replace(src, "__ENOLA_HOOKS__", strconv.FormatBool(o.Hooks), 1)
}

// InstallsPiExtension reports whether this run writes Pi's extension into the
// repository, where Pi loads it only once the user has trusted the project.
func InstallsPiExtension(o Options) bool {
	if o.Scope != ScopeLocal {
		return false
	}
	for _, t := range o.selectedTargets() {
		if t == "pi" {
			return true
		}
	}
	return false
}
