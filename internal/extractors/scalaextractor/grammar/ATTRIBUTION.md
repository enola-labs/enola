# Third-party attribution: tree-sitter Scala grammar

This directory vendors the **tree-sitter Scala grammar** so the Scala extractor can parse
Scala source into an AST. It is third-party code, distributed under a different license
(MIT) than the rest of enola (Apache-2.0).

## Source

- **Project:** [tree-sitter-scala](https://github.com/tree-sitter/tree-sitter-scala)
- **License:** MIT — see [LICENSE](./LICENSE) (Copyright (c) 2018 Max Brunsfeld and GitHub)
- **Version vendored:** release `v0.26.2` (commit `b931fcc338390925eb893d70ad070033f5856ccf`)

## Why it is vendored rather than required

Until v0.24.1 the upstream Go module was consumed directly. Every release since v0.25.0
commits a `src/parser.c` that declares `LANGUAGE_VERSION 15`, while
`go-tree-sitter v0.24.0` accepts at most ABI 14. `SetLanguage` returns an error, every
file parses to nothing, and the result is **indistinguishable from a repository that
contains no Scala** — the same trap documented for the C# and Dart grammars, and the
reason `TestGrammarSmoke` asserts the grammar still loads.

Staying on v0.24.1 is not a neutral choice: it predates the grammar's support for Scala 3
**capture checking** (`T^`, `T^{x}`, `A ->{x} B`, `class Box[T, C^]`, `Box[T, {any}]`).
On capture-checked sources its error recovery re-syncs past the capture set and the
extractor silently drops and mis-nests symbols, with no parse error reported.
`TestCaptureCheckingSyntax` in the parent package pins the forms that failed.

## What is vendored, and how it was produced

| File(s) | Origin |
|---|---|
| `src/scanner.c` | Copied verbatim from the upstream repository at the commit above. |
| `grammar.json` | Copied verbatim from the upstream repository's `src/grammar.json`. |
| `src/parser.c`, `src/node-types.json`, `src/tree_sitter/*.h` | **Generated** locally from `grammar.json` using the tree-sitter CLI at ABI 14. The upstream committed parser is ABI 15 and therefore unusable here. |

The generated parser is a deterministic product of the upstream `grammar.json`; it was
not hand-edited. The `cgo_parser.c` / `cgo_scanner.c` shims and `binding.go` in this
directory are enola's own glue code (Apache-2.0) and are not part of the upstream
grammar.

## Regenerating `src/parser.c`

```sh
cd internal/extractors/scalaextractor/grammar
tree-sitter generate grammar.json --abi 14
```

The ABI (14) is pinned to match `github.com/tree-sitter/go-tree-sitter v0.24.0`. This
was produced with `tree-sitter-cli` v0.25.3.

## Known grammar gap at ABI 14

Upstream declares a `reserved` word set (a tree-sitter 0.25 feature). ABI 14 cannot
carry it, so keywords are not reserved in identifier position: `val val = ???`, which
upstream parses to an ERROR node, parses here as a definition. This is the only case in
upstream's test corpus that differs at ABI 14 (1 of 271), and it affects only source
the Scala compiler rejects.
