---
name: setup
description: Install or verify the Enola CLI required by this plugin. Use when Enola was just installed, its MCP server cannot start, or the enola command is missing.
disable-model-invocation: true
---

# Set up Enola

This plugin configures Claude Code to use Enola, but it does not install the Enola
executable. The executable must be available on `PATH` before its MCP server and hooks
can run.

First check whether it is already installed:

```sh
command -v enola && enola doctor
```

If `enola` is missing, explain what the following command does and ask the user before
running it. On macOS, Linux, or Windows through Git Bash/WSL, use the official installer:

```sh
curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh
```

Alternatively, where Python and pip are available—including native Windows—use:

```sh
pip install enola-cli
```

The package is named `enola-cli`, but the installed command is `enola`.

After installation, run:

```sh
enola doctor
```

If the shell still cannot find it, explain that the installation directory must be on
`PATH`. Do not modify shell startup files without the user's approval. Once `enola
doctor` succeeds, tell the user to restart Claude Code or run `/reload-plugins` so the
MCP server starts with the updated environment.

Never install or upgrade Enola without explicit user approval.
