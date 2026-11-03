# locate output

The result of `enola locate --json` and of the `locate` MCP tool. Both return the same object,
except that a [stamp check](#stamp-check) from the tool leaves the stamp out, holding whichever of these the request asked for:

| Field | Request | Contents |
|---|---|---|
| `locations` | `file:line` or `file:start-end` | One [location](#location) per request |
| `diff` | `--diff`, or the tool's `diff` | The symbols a [unified diff](#diff) touched |
| `stamps` | `--stamps`, or the tool's `stamps` | One [check](#stamp-check) per stamp, in order |

Consumers must accept unknown fields and unknown `status` or `match` values, because new ones
are additive. A field marked optional is left out when it has no value.

## Symbol

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `id` | string | always | The fact id. See [facts.md](facts.md#identity-and-ids). Declarations can share it |
| `repo` | string | optional | Repository label |
| `name` | string | always | Fact name |
| `symbol_kind` | string | optional | `function`, `method`, `getter` and so on |
| `file` | string | always | File as the graph names it |
| `repo_prefixed` | bool | optional | A multi-repo snapshot put `repo` and a slash at the start of `file`; the rest is the path relative to the repository. Absent otherwise. See [facts.md](facts.md#identity-and-ids) |
| `line` | int | always | The declaration's line |
| `end_line` | int | optional | Last line of the span. Absent for F# and VB.NET |
| `body_hash` | string | optional | Version of the symbol's code. See [facts.md](facts.md#body_hash) |
| `decl_hash` | string | optional | Which declaration, when several share the `id`. See [facts.md](facts.md#decl_hash) |
| `inner_hash` | string | optional | Version of the code after the signature. See [facts.md](facts.md#inner_hash) |
| `member_of` | string | optional | The declaration this symbol is a part of. See [facts.md](facts.md#member_of) |
| `span_start` | int | optional | First line of the code when decorators start it above `line` |

## Location

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `location` | string | always | The location as requested |
| `file` | string | optional | The graph's name for the file |
| `match` | string | always | How it matched, below |
| `symbols` | array of [symbols](#symbol) | optional | Innermost symbol at a line; each distinct one in a range |
| `candidates` | array of strings | optional | Graph files the path matched, when more than one |
| `nested` | array | optional | For an `enclosing` match, each [nested function](#nested-functions) without a symbol the location is in: `name`, `line`, `end_line` |
| `cited_hash` | string | optional | Version of the cited lines. Needs the source |
| `cited_lines` | int | optional | Non-blank lines `cited_hash` covers. A single line is taken with one line either side |
| `error` | string | optional | The location could not be read |

| `match` | Meaning |
|---|---|
| `span` | The innermost symbol whose span holds the line. Exact |
| `enclosing` | As `span`, but the line is in a named function nested in that symbol that has no symbol of its own, which `nested` names. See [nested functions](#nested-functions) |
| `nearest` | The declaration above the line, for a file with no spans (F#, VB.NET). A line past a function's end is credited to it too |
| `none` | No symbol holds the line: imports, top-level code, blank lines |
| `no_file` | The graph has no symbols for this file, or the path matched several (`candidates`) |

### Nested functions

A nested function is located as itself when the graph has a symbol for it. When it has none and
it is named, the match is `enclosing`: `symbols` holds the function around it and `nested` names
the nested one by that symbol and the path down to it (`Client.send.onClose`), with its lines. A
nested symbol of its own carries `member_of` when it is a part of a declaration.

| Language | A symbol of its own | Named, without a symbol (`enclosing`) |
|---|---|---|
| Python | A function or `async def` inside a function or method, at any depth and in a class declared there | Not measured: a lambda bound to a name |
| TypeScript, JavaScript | A function in an object literal a top-level function returns | A local function, a variable bound to a function or arrow, a named function expression, and a method or function-valued key of an object literal or local class |
| Go | None | A function literal bound to a name (`onClose := func() {...}`, `var onClose = func() {...}`) |
| Rust | A fn item inside a fn | A closure a `let` binds to a plain name |
| Others | None | Not measured: a `span` match may be on an enclosing function |

Anonymous functions are not named and are located as the symbol around them in every language:
callbacks, lambdas, closures passed straight to a call. So are classes declared inside a function.
TypeScript and JavaScript inside Vue, Svelte and other component files are not measured.

`touched --grain` does not change any of this. It decides how commits are counted for a ranking:
`member` credits the symbol `locate` returns, `declaration` credits the `member_of` declaration
instead. The ids and spans `locate` returns are the same under either.

## Diff

| Field | Type | Meaning |
|---|---|---|
| `side` | string | `new` or `old`: the side of the diff the snapshot is of |
| `symbols` | int | Distinct symbols across all files |
| `files` | array | One per file of the diff, below |

| File field | Type | Presence | Meaning |
|---|---|---|---|
| `path` | string | always | The path in the diff |
| `file` | string | optional | The graph's name for the file |
| `match` | string | always | As for a location |
| `symbols` | array | optional | [Symbols](#symbol), each with `lines`: how many of the diff's lines fell in it |
| `outside` | int | optional | Changed lines no symbol holds |
| `candidates` | array of strings | optional | Graph files the path matched, when more than one |

## Stamp

What a consumer stores when it writes a record, and sends back to check it. Only `id` is
required. Each other field gives enola more to go on, and all of them come from a location's
result.

| Field | Comes from | Without it |
|---|---|---|
| `id` | symbol | Required |
| `body_hash` | symbol | The status is `present`, or `ambiguous` if the id is shared |
| `decl_hash` | symbol, when present | A rewritten declaration under a shared id is found by kind, or is `ambiguous` |
| `inner_hash` | symbol, when present | A `rewritten` result has no `change` |
| `name` | symbol | A moved symbol cannot be found: `gone` with no candidates |
| `symbol_kind` | symbol | Same-name candidates are not narrowed by kind |
| `cited_hash`, `cited_lines` | location | No `cited` result |

## Stamp check

| Field | Type | Presence | Meaning |
|---|---|---|---|
| `stamp` | object | CLI | The stamp as sent |
| `i` | int | MCP tool | The stamp's position in the call, instead of `stamp` |
| `status` | string | always | What became of it, below |
| `symbol` | [symbol](#symbol) | `current`, `rewritten`, `present`, `moved` | Where it is now |
| `cited` | string | optional | `intact` or `changed`. See below |
| `change` | string | optional | For `rewritten`: `signature` when the code after the signature is unchanged, `body` when it changed. Absent when either side has no `inner_hash` |
| `candidates` | array of [symbols](#symbol) | `ambiguous`; optional with `gone` | What it may be |

| `status` | Meaning | What to do with the record |
|---|---|---|
| `current` | Same symbol, same code | It still describes the code |
| `rewritten` | Same symbol, its code changed | Re-check it. `cited: intact` says the lines it cites are still there |
| `moved` | A new id, in another file or package, with the same declared name and code | Follow `symbol`, and store its new id |
| `present` | Same symbol, but one side has no version, so a change cannot be told | Treat as unverified |
| `gone` | Not in the graph. `candidates` holds symbols of the same name with other code | It may have moved and been rewritten at once. Look, do not assume |
| `ambiguous` | Cannot be tied to one symbol. No `symbol`; `candidates` holds at least one | Keep it and show the candidates. Do not choose one |

`ambiguous` comes from three situations, told apart by the candidates:

- **The candidates share the stamp's `id`**, and none has its body, or its signature, or (when
  neither was stamped) one kind. Declarations with one signature, such as `#[cfg]` variants,
  end here once rewritten.
- **One candidate shares the `id`, and it differs from the stamp's `decl_hash` or kind.** Either
  the stamped declaration was deleted and this sibling survived, or it was given a new signature.
- **The candidates have other ids and the stamp's `body_hash`.** Two copies of the moved symbol
  exist, and a move cannot be told from a copy.

`cited` is `intact` when the cited lines are still inside the symbol, wherever in it, and
`changed` when they are not. It is left out when that cannot be told: no cited version in the
stamp, no source to read, a symbol with no span, or a status with no symbol to look in.

## What needs a working tree

`--facts` (a stored snapshot) and `--at` (a recorded revision) have no source that matches the
graph, so they return no `cited_hash` or `cited_lines` and no `cited`. There, a stamp's `decl_hash`
is compared only where the snapshot carries one, so a surviving sibling of the same kind reads as
`rewritten` rather than `ambiguous`.
