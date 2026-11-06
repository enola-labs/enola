# Third-party attribution: tree-sitter C# grammar

This directory vendors the **tree-sitter C# grammar** so the dotnet extractor can parse
C# source into an AST. It is third-party code, distributed under a different license
(MIT) than the rest of enola (Apache-2.0).

## Source

- **Project:** [tree-sitter-c-sharp](https://github.com/tree-sitter/tree-sitter-c-sharp)
- **License:** MIT, see [LICENSE](./LICENSE) (Copyright (c) 2014-2023 Max Brunsfeld, Damien Guard, Amaan Qureshi, and contributors)
- **Version vendored:** master commit `8c0abe0b84a3` (Go pseudo-version
  `v0.23.6-0.20260929081004-8c0abe0b84a3`), with the patch below. No release tag carries
  the C# 14 syntax dotnet/runtime already uses (`extension` blocks, `?.` assignment).

## Why it is vendored rather than required

Upstream's `*` (`_pointer_indirection_expression`) accepts only an `lvalue_expression`.
That rejects three forms common in unsafe code:

- a cast under `*`: `*(int*)p = 1`, `*(Vector<byte>*)p ^= m`, `*(int*)(p + 4)`
- a pointer stepped first: `c = *++p`
- a pointer stepped after: `*p++ ^= m`

Each is a syntax error, and under error recovery one of them can make the whole enclosing
class an ERROR node. dotnet/runtime's `Number.Parsing.cs` and `ManagedWebSocket.cs`
produced no symbols at all, and PowerShell's `Variant.cs` lost half of its own.
[`pointer-indirection.patch`](./pointer-indirection.patch) widens the operand to those
three forms, each restricted the way `*`'s own operand is, so `*(T*)p = v` still parses
as an assignment to the dereference and `(a) * (b)` and `(a) && (b > 0)` keep their
upstream parses. `TestPointerIndirectionOperands` in the parent package pins the forms.

## What is vendored, and how it was produced

| File(s) | Origin |
|---|---|
| `src/scanner.c`, `LICENSE` | Copied verbatim from the upstream repository at the commit above. |
| `grammar.json`, `src/parser.c`, `src/node-types.json`, `src/tree_sitter/*.h` | **Generated** from upstream `grammar.js` with `pointer-indirection.patch` applied. |

The `cgo_parser.c` / `cgo_scanner.c` shims and `binding.go` in this directory are enola's
own glue code (Apache-2.0) and are not part of the upstream grammar.

## Regenerating

```sh
git clone https://github.com/tree-sitter/tree-sitter-c-sharp && cd tree-sitter-c-sharp
git checkout 8c0abe0b84a3
git apply <enola>/internal/extractors/dotnetextractor/grammar/pointer-indirection.patch
npx tree-sitter-cli@0.26.5 generate --abi 15
npx tree-sitter-cli@0.26.5 test
cp src/grammar.json <this directory>/grammar.json
cp src/parser.c src/node-types.json <this directory>/src/
cp src/tree_sitter/*.h <this directory>/src/tree_sitter/
```

Use tree-sitter-cli **0.26.5**, the version upstream's `package.json` names. Without the
patch it reproduces upstream's committed `src/parser.c` byte for byte. With the patch,
upstream's test corpus passes 179 of 179. Generating with 0.27.1 instead fails
upstream's own "Switch statements" test (`case int when i < 5:`), patched or not. After
replacing the sources, build with `go build -a`: Go's build cache does not track the files
the `cgo_*.c` shims include.

## Known gaps

Upstream grammar issues the patch does not touch. Each makes the surrounding type or file
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
