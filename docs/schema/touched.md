# touched output

The result of `enola touched --json`. Consumers must accept unknown fields.

| Field | Type | Meaning |
|---|---|---|
| `commits` | array of [commits](#commit) | The selected commits, oldest first |
| `ranked_commits` | int | How many of them the ranking counts |
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

A function, method or getter, identified by its fact id, so a move to another file starts a
new entry.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The fact id. See [facts.md](facts.md#identity-and-ids) |
| `name`, `symbol_kind`, `file` | string | As the first ranked commit saw it |
| `count` | int | Ranked commits that changed it |
| `commits` | array of strings | Those commits, oldest first. The `touched` MCP tool leaves them out unless asked |

## What it reads

Merges are skipped and the first-parent line is followed. Each commit's files are extracted from
that commit's tree on their own, so a symbol's id, name, kind and span are what a snapshot of that
commit holds, while nothing resolved across files or linked is there to read. A pure deletion is
credited as in [locate](locate.md#diff): only when the lines on both sides of it are in one symbol.

`--grain member`, the default, credits the innermost symbol. `--grain declaration` credits a
symbol with `member_of` to that declaration, so a commit changing two functions in the object a
factory returns counts once, for the factory.
