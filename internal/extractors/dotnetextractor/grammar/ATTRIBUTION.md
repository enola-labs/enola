# Third-party attribution: tree-sitter C# grammar

This directory vendors the **tree-sitter C# grammar** so the dotnet extractor can parse
C# source into an AST. It is third-party code, distributed under a different license
(MIT) than the rest of enola (Apache-2.0).

## Source

- **Project:** [tree-sitter-c-sharp](https://github.com/tree-sitter/tree-sitter-c-sharp)
- **License:** MIT, see [LICENSE](./LICENSE) (Copyright (c) 2014-2023 Max Brunsfeld, Damien Guard, Amaan Qureshi, and contributors)
- **Version vendored:** release `v0.23.5` (commit `cac6d5fb595f5811a076336682d5d595ac1c9e85`)

## Why it is vendored rather than required

Until v0.23.1 the upstream Go module was consumed directly. Every release since v0.23.2
commits a `src/parser.c` that declares `LANGUAGE_VERSION 15`, while
`go-tree-sitter v0.24.0` accepts at most ABI 14. `SetLanguage` returns an error, every
file parses to nothing, and the result is **indistinguishable from a repository that
contains no C#**, the same trap documented for the Scala and Dart grammars, and the
reason `TestGrammarSmoke` asserts the grammar still loads.

Staying on v0.23.1 was not a neutral choice: it predates C# 12. A body-less type
declaration (`class GroupJsonContext : JsonSerializerContext;`) or a collection expression
(`= []`) made the whole file one ERROR node, and the file produced no symbols. Across the
eight C# repositories of the benchmark corpus that was 5,254 of 62,848 `.cs` files with a
parse error and 482 that lost every symbol; with this grammar it is 2,596 and 280.
`TestCSharp12Syntax` in the parent package pins the forms that failed.

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
cd internal/extractors/dotnetextractor/grammar
tree-sitter generate grammar.json --abi 14
```

The ABI (14) is pinned to match `github.com/tree-sitter/go-tree-sitter v0.24.0`. This
was produced with `tree-sitter-cli` v0.27.1.

## Grammar behaviour at ABI 14

None differs from upstream. The grammar declares no `reserved` word set (its
`_reserved_identifier` is an ordinary rule for contextual keywords), so nothing is lost
at ABI 14. Upstream's test corpus gives the same result at ABI 14 and at ABI 15: 167 of
168 pass, and the one failure ("Switch statements") is upstream's own.

## Known gaps in v0.23.5

Measured against v0.23.1 on the corpus, after the extractor resolves preprocessor
directives (see `activeBranches` in the parent package). Each makes the surrounding
type or file an ERROR node; all are upstream grammar issues, not ABI effects:

- `async` used as an identifier, e.g. a variable passed as `F(servers, async, name)`.
  It costs runtime's `DnsResolverPal.*.cs` their symbols (about 100).
- a list pattern holding a property or positional subpattern,
  `x is [.., SyntaxToken { Kind: K } t]` and `is [.., (kind: K)]` (roslyn).
- C# 14 syntax: null-conditional assignment (`a?.B = c`) and `extension` blocks.
- a pointer dereference as an argument, `F(*(IntPtr*)p)`, and other pointer and
  ref assignments in runtime internals.
- the withdrawn C# 11 parameter null check `string key!!`, which v0.23.1 accepted.
