# Third-party attribution: tree-sitter Dart grammar

This directory vendors the **tree-sitter Dart grammar** so the Dart extractor can parse
Dart source into an AST. It is third-party code, distributed under a different license
(MIT) than the rest of enola (Apache-2.0).

## Source

- **Project:** [tree-sitter-dart](https://github.com/UserNobody14/tree-sitter-dart)
- **License:** MIT — see [LICENSE](./LICENSE) (Copyright (c) 2020-2023 UserNobody14 and others)
- **Version vendored:** Go module `github.com/UserNobody14/tree-sitter-dart`
  pseudo-version `v0.0.0-20260707040301-be07cf7118d3` (commit `be07cf7118d3`)

## Why it is vendored rather than required

The upstream module cannot be consumed as a dependency for **two independent reasons**,
either of which alone is fatal:

1. **Wrong runtime.** Its Go binding targets `github.com/smacker/go-tree-sitter`, while
   enola vendors `github.com/tree-sitter/go-tree-sitter`. The two runtimes have
   incompatible `Language` handles, so the published binding cannot be handed to
   enola's parser at all.

2. **Wrong ABI, and it fails silently.** Its committed `src/parser.c` declares
   `LANGUAGE_VERSION 15`, while `go-tree-sitter v0.24.0` accepts at most ABI 14.
   `SetLanguage` returns an error, every file parses to nothing, and the result is
   **indistinguishable from a repository that contains no Dart**. This is the same trap
   already documented for the C# and Scala grammars (both now vendored and regenerated
   at ABI 14), and it is why `TestGrammarSmoke` in the parent package asserts
   the grammar still loads rather than trusting the vendored bytes to stay put.

## What is vendored, and how it was produced

| File(s) | Origin |
|---|---|
| `src/scanner.c` | Copied verbatim from the upstream repository (the project's hand-written external scanner: block/doc comments and string-template chars). |
| `grammar.json` | Copied verbatim from the upstream repository's `src/grammar.json`. |
| `src/parser.c`, `src/node-types.json`, `src/tree_sitter/*.h` | **Generated** locally from `grammar.json` using the tree-sitter CLI at ABI 14. The upstream committed parser is ABI 15 and therefore unusable here. |

The generated parser is a deterministic product of the upstream `grammar.json`; it was
not hand-edited. The `cgo_parser.c` / `cgo_scanner.c` shims and `binding.go` in this
directory are enola's own glue code (Apache-2.0) and are not part of the upstream
grammar.

## Regenerating `src/parser.c`

```sh
cd internal/extractors/dartextractor/grammar
tree-sitter generate grammar.json --abi 14
```

The ABI (14) is pinned to match `github.com/tree-sitter/go-tree-sitter v0.24.0`. This
was produced with `tree-sitter-cli` v0.24.7 (e.g. `npx tree-sitter-cli@0.24.7`).

## Known grammar gap

The grammar does not accept Dart's **primary constructors** (`class const Foo(final
String name) { … }`), an in-development language feature behind an experiment flag.
Measured over a ten-repository corpus, the only code affected is the Dart SDK's own
`pkg/front_end`, which dogfoods it: 64 of its 307 library files, against **zero files in
every other repository**. See `enola-benchmarks/README.md` for the full measurement.
