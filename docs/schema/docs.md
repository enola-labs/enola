# docs.jsonl

JSON Lines: one line per symbol or module that carries a `doc` in
[facts.jsonl](facts.md), holding the whole comment its author wrote on it.

`doc` in facts.jsonl is the comment's first paragraph, cut at 300 bytes, because
every reader of the graph pays for what a fact holds. This file is for a reader
that indexes the comments themselves: a search index, an embedding store, a
documentation site. Join a line to its fact by `id`.

The file is written by the `docs` renderer, which is on by default. A
configuration that lists `renderers:` without it writes no `docs.jsonl`. A
snapshot with nothing documented writes the file empty. `receipt.json` lists it
in `output_hashes` when this run wrote it, so a reader can tell the file from one
an earlier run left behind.

In a multi-repo run every repository's output directory receives the same file,
covering the whole linked graph; each line names its repository.

## Line object

| Field | Type | Present when | Meaning |
|---|---|---|---|
| `id` | string, 32 lowercase hex chars | always | The fact's `id` in facts.jsonl. Ids are identities, not unique keys: two overloads share one, and each is its own line |
| `repo` | string | the fact has a repository label | Repository label |
| `kind` | string | always | `symbol` or `module` |
| `name` | string | always | The fact's name |
| `file` | string | always | The fact's file; for a module, its directory |
| `line` | int | the fact has a line | 1-based line of the declaration |
| `doc` | string | always | The whole comment: its lines without the indentation they share, one blank line between paragraphs and none at either end, comment markers removed. Cut at a word and ended with `...` past 8 KiB. Equal to the fact's `doc` when the comment is one paragraph |

Lines are sorted by `id`, then `line`, then `doc`. A line repeated exactly is
written once.

What counts as a comment, per language, is the same as for `doc`: see the `doc`
prop in [facts.md](facts.md).
