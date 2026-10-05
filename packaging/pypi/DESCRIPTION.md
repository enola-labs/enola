# enola

**Architectural regression testing for AI-assisted development.**

Catch structural regressions that builds and tests cannot see: new dependency
cycles, violated layer boundaries, undeclared service dependencies, and changes
that spread beyond their intended scope.

Enola maps your codebase before a change and compares it with the structure
afterward. The result is about *this change*, not every problem already in the
repository, and only the rules you choose can fail the build.

- **Exact, local measurement.** Parsed source and graph algorithms. No model, no
  embeddings, no upload, no account, no license check.
- **One graph across the repository.** 23 languages and formats, detected
  automatically and combined into one baseline and verdict.
- **One loop everywhere.** Your coding agent reads the graph before it edits and
  receives the verdict afterward. The same check runs from the CLI or in CI.

## Install

```bash
pip install enola-cli
```

The command is `enola`. This package ships a prebuilt binary, so there is no
Python dependency at run time and nothing is importable from Python.

Got this package with Cognee? You already have all of enola. See
<https://github.com/enola-labs/enola/blob/main/docs/COGNEE.md>.

## Try it read-only

```bash
enola --explain /path/to/your/repo
```

No baseline, config file or MCP client, and nothing is written to disk. Enola
prints the architecture it measured (patterns, cycles, layer violations,
hotspots, blast radius and structural outliers) and exits.

## Documentation

Full documentation, including the CLI reference and architecture internals,
lives at <https://github.com/enola-labs/enola>.
