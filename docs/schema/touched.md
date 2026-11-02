# touched output

The result of `enola touched --json`. Consumers must accept unknown fields.

| Field | Type | Meaning |
|---|---|---|
| `commits` | array of [commits](#commit) | The selected commits, oldest first |
| `ranked_commits` | int | How many of them the ranking counts |
| `symbols` | int | How many callables they changed, however few `ranking` returns |
| `ranking` | array of [ranked callables](#ranked-callable) | Most often changed first, limited by `--top` |

## Commit

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `sha` | string | always | The commit |
| `date` | string | always | Its committer date |
| `subject` | string | always | Its subject |
| `ranked` | bool | always | It changed at least one callable, and no more than `--max-symbols` |
| `files` | array of strings | optional | The files it changed |
| `symbols` | array | optional | [Symbols](locate.md#symbol) on its new side, each with `lines`, as `locate --diff` reports them |
| `outside` | int | optional | Changed lines no symbol holds, in files with symbols |

## Ranked callable

A function, method or getter, one row per file and name. When git sees a file renamed anywhere in
the range, commits before the rename count for the file it became. A function moved on its own to
another file, or a file rewritten past 50% similarity in its rename, starts a new row. So can a
commit on a branch that was editing the file while another branch moved it.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The fact id in the latest ranked commit. See [facts.md](facts.md#identity-and-ids) |
| `name`, `symbol_kind` | string | As the latest ranked commit saw it |
| `file` | string | Where the range leaves it |
| `moved_from` | array of strings | Other paths its commits saw it at, oldest first. Optional |
| `count` | int | Ranked commits that changed it |
| `commits` | array of strings | Those commits, oldest first. The `touched` MCP tool leaves them out unless asked |

## What it reads

Merges are skipped and every commit reachable from `--rev` is walked, including those on merged
branches, which can include work later reverted or superseded. `--first-parent` walks only the
first-parent line. Each commit's files are extracted from
that commit's tree on their own, so a symbol's id, name, kind and span are what a snapshot of that
commit holds, while nothing resolved across files or linked is there to read. A pure deletion is
credited as in [locate](locate.md#diff): only when the lines on both sides of it are in one symbol.

`--subject` keeps commits and `--exclude` then drops some of them. Both are RE2 regexps, which have
no lookahead, so "fix, but not CI" takes the two together.

A function nested inside another has no symbol, so its commits count for the enclosing one. A count
that treats nested functions as their own can rank differently. The `touched` MCP tool says this in
a `note` field beside the ranking.

`--grain member`, the default, credits the innermost symbol. `--grain declaration` credits a
symbol with `member_of` to that declaration, so a commit changing two functions in the object a
factory returns counts once, for the factory.
