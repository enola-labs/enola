# Third-party attribution: tree-sitter C# grammar

This directory vendors the **tree-sitter C# grammar** so the dotnet extractor can parse
C# source into an AST. It is third-party code, distributed under a different license
(MIT) than the rest of enola (Apache-2.0).

## Source

- **Project:** [tree-sitter-c-sharp](https://github.com/tree-sitter/tree-sitter-c-sharp)
- **License:** MIT, see [LICENSE](./LICENSE) (Copyright (c) 2014-2023 Max Brunsfeld, Damien Guard, Amaan Qureshi, and contributors)
- **Version vendored:** master commit `8c0abe0b84a3` (Go pseudo-version
  `v0.23.6-0.20260929081004-8c0abe0b84a3`), with the patches below. No release tag
  carries the C# 14 syntax dotnet/runtime already uses (`extension` blocks, `?.`
  assignment).

## Why it is vendored rather than required

Upstream rejects syntax the corpus uses, and under error recovery one rejected form can
make the whole enclosing class an ERROR node, so the file loses its symbols. Each fix is
one file under [`patches/`](./patches/), applied to upstream's `grammar.js` in name order.
Each is pinned by a test in the parent package that fails without it, and
`TestGrammarPatchesParseClean` pins all of them at the parse level. File counts are
production `.cs` files across the benchmark corpus that the form left with a parse error.

| Patch | Upstream rejects | Test |
|---|---|---|
| `01-pointer-indirection.patch` | `*` takes only an lvalue: `*(int*)p = 1`, `*(int*)(p + 4)`, `c = *++p`, `*p++ ^= m`. dotnet/runtime's `Number.Parsing.cs` and `ManagedWebSocket.cs` produced no symbols. The widened operand keeps `*`'s own restriction, so `*(T*)p = v` is still an assignment to the dereference and `(a) * (b)` and `(a) && (b > 0)` keep their upstream parses. | `TestPointerIndirectionOperands` |
| `02-ref-partial-struct.patch` | C# 13's `ref partial struct`: `partial` after `ref`. runtime's `Utf8JsonReader` and its Asn1 value types, 86 files with a parse error. | `TestCSharp13Declarations` |
| `03-allows-ref-struct.patch` | C# 13's `allows ref struct` anti-constraint, 56 files. `allows` stays an identifier elsewhere. | `TestGrammarPatchesParseClean` |
| `04-safe-modifier.patch` | The `safe` modifier (unsafe-code evolution) runtime puts on extern methods and fields, 13 files. `safe` stays an identifier elsewhere. | `TestCSharp13Declarations` |

## What is vendored, and how it was produced

| File(s) | Origin |
|---|---|
| `src/scanner.c`, `LICENSE` | Copied verbatim from the upstream repository at the commit above. |
| `grammar.json`, `src/parser.c`, `src/node-types.json`, `src/tree_sitter/*.h` | **Generated** from upstream `grammar.js` with `patches/` applied. |
| `patches/`, `regenerate.sh` | enola's own (Apache-2.0). |

The `cgo_parser.c` / `cgo_scanner.c` shims and `binding.go` in this directory are enola's
own glue code (Apache-2.0) and are not part of the upstream grammar.

## Regenerating

```sh
./regenerate.sh           # clone upstream, apply patches/, generate, test, copy here
./regenerate.sh --check   # the same, but only verify the committed files match
```

The script pins tree-sitter-cli **0.26.5**, the version upstream's `package.json` names.
Without patches it reproduces upstream's committed `src/parser.c` byte for byte, and with
them upstream's own test corpus must still pass (the script fails otherwise). Generating
with 0.27.1 instead fails upstream's "Switch statements" test (`case int when i < 5:`),
patched or not. After regenerating, build with `go build -a`: Go's build cache does not
track the files the `cgo_*.c` shims include.

## Known gaps

Upstream grammar issues no patch here covers yet. Each makes the surrounding type or file
an ERROR node:

- `async` used as an identifier, e.g. a variable passed as `F(servers, async, name)`.
- a list pattern holding a property or positional subpattern,
  `x is [.., SyntaxToken { Kind: K } t]` and `is [.., (kind: K)]` (roslyn's parser).
  The parser commits to an element access before it reaches the pattern, so dynamic
  precedence on `list_pattern` does not help.
- a pointer to an array type, `ParameterInfo[]* p`.
- a ref-returning call as an assignment target, `Unsafe.Add(ref a, i) = v`.

v0.23.5 rejects every one of these too. What differs between grammar versions is only how
far the error spreads, which is why roslyn's `LanguageParser.cs` keeps fewer symbols
under 8c0abe0 than under v0.23.5 while dotnet/runtime as a whole gains 8,436.
