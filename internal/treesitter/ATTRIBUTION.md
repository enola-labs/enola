# Third-party attribution: go-tree-sitter runtime

This directory vendors the **tree-sitter runtime and its Go binding**, the library every
enola extractor parses with. It is third-party code under the MIT license (see
[LICENSE](./LICENSE)), not enola's Apache-2.0.

## Source

- **Project:** [go-tree-sitter](https://github.com/tree-sitter/go-tree-sitter)
- **License:** MIT, see [LICENSE](./LICENSE)
- **Version vendored:** commit `adc13ffd8b2c` (Go pseudo-version
  `v0.24.1-0.20250202211042-adc13ffd8b2c`), the code of the v0.25.0 tag upstream later
  deleted. It accepts tree-sitter ABI 13 to 15.

## What is vendored

The module's `*.go`, `*.c` and `*.h` files, `include/` and `src/`, copied verbatim
except for `src/parser.c` (below). Left out: the upstream tests, `README.md`, `copy.sh`,
`go.mod` and `go.sum`. `ATTRIBUTION.md`, `node_value.go`, `recovery_test.go` and `testdata/` are
enola's own. `node_value.go` adds value-returning child accessors and the slab allocator
`newNode` uses (below).

## Why it is vendored, and the one change

Commit adc13ff changed error recovery in `src/parser.c` relative to v0.24.0:
`MAX_COST_DIFFERENCE` went from 16 to 18 skipped trees, and the branch that restarts
recovery when a token is invalid while the parser is already in the error state was
removed. Upstream master still has both changes. A file with a syntax error that a
grammar does not support (a C macro such as `__initdata`, a C# `#if` inside an
expression, a Dart primary constructor) then recovers differently, and across the
benchmark corpus that cost about 1,700 symbols in real files, often a whole file turned
into one `ERROR` node.

This copy restores both v0.24.0 behaviours and changes nothing else, so enola keeps the
ABI 15 runtime with the recovery it was built against. The restored lines are pinned
by `recovery_test.go`.

## The other change: how a node is allocated

`newNode` in `node.go` returns `allocNode(node)` instead of `&Node{_inner: node}`. That
is the only edit to an upstream Go file. `allocNode`, in enola's `node_value.go`, carves
nodes from pooled 8 KiB slabs: every `Child`, `Parent` or `ChildByFieldName` returned a
fresh 32-byte allocation, which was 45 to 80% of all a cold index allocated in every
language. Behaviour is unchanged; a node kept alive keeps its slab alive, which the
tree's own lifetime bounds.

## Updating

Copy the new upstream files over these, re-apply the two hunks to `src/parser.c` and
the one-line `newNode` change to `node.go`, and
build and test with `-a`: Go's build cache does not track the files `tree_sitter.go`
includes from `src/`, so without it the old parser stays linked.
`TestRecovery_LinkedParserUsesTheRestoredRecovery` fails when that happens.

`testdata/recovery.cs` is an 11-line fragment of `HttpConnection.cs` from
[dotnet/runtime](https://github.com/dotnet/runtime) (MIT), cut down to the shortest
input whose root differs between the two recoveries.
