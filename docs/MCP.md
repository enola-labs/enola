# MCP integration

Enola exposes its architecture graph through an MCP server. The `enola` binary must be on
`PATH` before the client starts.

| Client | Registration |
|---|---|
| Claude Code | `claude mcp add enola enola` |
| Codex | `codex mcp add enola -- enola` |
| GitHub Copilot | `code --add-mcp '{"name":"enola","command":"enola"}'` |
| opencode | `enola install --targets opencode` |
| Cursor and other clients | Add the configuration below |

```json
{
  "mcpServers": {
    "enola": {
      "command": "enola"
    }
  }
}
```

GitHub Copilot uses `servers` instead of `mcpServers` as the top-level key. To pass an explicit
configuration file, add `"args": ["/path/to/mcp-arch.yaml"]`.

## Repository instructions and hooks

```bash
enola install                         # repository instructions
enola install --global                # user-level instructions
enola install --targets=claude,cursor # selected targets
enola install --hooks                 # instructions and session hooks
enola uninstall                       # remove managed content
```

Every command previews changes before writing. Enola does not create a repository `AGENTS.md`; it
adds a marked block only when that file already exists. `uninstall` removes owned files, managed
configuration entries and marked blocks without changing surrounding content.

| Target | Repository files | User files |
|---|---|---|
| Claude Code | `.claude/rules/enola.md`, `.claude/settings.json` | `~/.claude/rules/enola.md` |
| Cursor | `.cursor/rules/enola.mdc` | — |
| GitHub Copilot | `.github/instructions/enola.instructions.md` | — |
| Codex | existing `AGENTS.md`, `.codex/hooks.json` | `~/.codex/AGENTS.md`, `~/.codex/hooks.json` |
| opencode | `.opencode/enola.md`, `opencode.json`, optional plugin | equivalents under `~/.config/opencode/` |

### Hook behavior

For Claude Code and Codex, `--hooks` installs:

- `SessionStart`: pins a baseline in a detached process.
- `Stop`: checks the change and returns a verdict when there is a policy regression or an exact,
  unenforced finding.

The hooks do not replace manually pinned baselines, serialize concurrent snapshot work, and do not
block a session when Enola cannot run. Codex requires one approval through `/hooks` before running a
new hook.

opencode uses a plugin instead of session hooks. It directs initial repository searches toward the
Enola index; `ENOLA_OPENCODE_GATE=off` disables its blocking behavior.

After a session, check hook activity and baseline compatibility with:

```bash
enola doctor
```

`doctor` is diagnostic and always exits `0`.

## Operation

The server uses the current repository and built-in defaults unless given a config path. Generate
large repositories from the shell if the client's tool timeout is too short:

```bash
enola --generate /path/to/repository
```

The MCP server reuses that snapshot and its extractor cache. Use `enola --list` for the current
tool inventory and [`CLI.md`](CLI.md) for command behavior.
