# flows.json

A JSON object describing the ways into the program a snapshot covers, ranked by
what each reaches. `enola flows` prints the same ranking.

An entry is a symbol something outside the code invokes: the handler of a served
route, a program's `main`, or a symbol a framework calls. Its flow is every
symbol the calls from it reach. The file holds the 50 largest flows; `total`
says how many there were.

The file is written by the `flows` renderer, which is on by default. A
configuration that lists `renderers:` without it writes no `flows.json`. A
snapshot with no flows contains an empty `flows` array.

In a multi-repo run every repository's output directory receives the same file,
covering the whole linked graph; each flow names its repository.

## Document object

| Field | Type | Meaning |
|---|---|---|
| `roots` | object, mechanism to int | Entries found, per mechanism, before any was dropped. Empty when none was found |
| `leaves` | int | Entries that reach nothing the graph follows. They are entries, not flows |
| `total` | int | Flows the entries produced. `flows` holds the first of them |
| `flows` | array of Flow | The largest flows, ranked. Always an array |

## Flow object

| Field | Type | Present when | Meaning |
|---|---|---|---|
| `id` | string, 32 lowercase hex chars | always | Flow identity: `sha256(repo \0 "flow" \0 entry name \0 entry file)`, truncated like a fact ID. Stable while the entry symbol keeps its name and file |
| `name` | string | always | The entry's route, as `METHOD path`, or the entry symbol's name when no route reaches it. When several files declare an entry under one name (each program in a C directory declares a `main`), each is its own flow and the name ends with its file: `tools/perf.main (perf.c)` |
| `rank` | int | always | 1-based position in the ranking. Not stable across snapshots |
| `repo` | string | always | Repository label of the entry |
| `mechanism` | string | always | How the entry is reached: [Mechanisms](#mechanisms) |
| `side` | bool | when true | The entry is not the program: an example, a script, a migration, build or docs tooling. Decided by the `module_role` of the entry's module (`test` or `tooling`) when an extractor recorded one, by the entry's path otherwise |
| `routes` | array of Ref | the entry is a route handler | Every served route the entry handles. `name` uses the first |
| `entry` | Ref | always | The entry symbol |
| `summary` | string | always | One paragraph of presentation text built from the fields below. Not a stable identifier |
| `metrics` | Metrics | always | The numbers the flow was ranked on |
| `modules` | array of `{name, symbols}` | always | The ten modules holding most of the flow, with how many of its symbols sit in each, largest first. `metrics.modules` counts all of them |
| `stages` | array of Stage | the entry calls something the graph follows | The steps that account for most of the flow, in tree pre-order |
| `same_reach_as` | array of string | another entry reaches the same symbols | Names of the flows folded into this one |
| `nested_in` | array of string | a larger flow contains at least 90% of this one | Names of those flows |

Flows are ranked with the program's own first and `side` flows after them, then
by `metrics.reach`, `metrics.modules` and `name`.

## Ref object

| Field | Type | Present when | Meaning |
|---|---|---|---|
| `name` | string | always | The fact's name |
| `id` | string, 32 lowercase hex chars | always | The fact's ID in `facts.jsonl` — [Identity and IDs](facts.md#identity-and-ids) |
| `method` | string | on a route with a verb | The route's HTTP method |
| `file` | string | when set | The fact's file |
| `line` | int | when measured | The fact's start line |
| `doc` | string | the symbol carries a `doc` prop | The symbol's comment, as in [facts.md](facts.md#descriptive-props) |

Join a flow to the graph on these IDs: `entry.id` and each stage `id` name a
`symbol` fact, each route `id` a `route` fact.

## Metrics object

| Field | Type | Meaning |
|---|---|---|
| `reach` | int | Symbols the walk visited, the entry included |
| `modules` | int | Distinct modules those symbols are declared in |
| `max_depth` | int | The deepest hop the walk took, at most 8 |
| `performs_io` | int, when above zero | Reached symbols an extractor marked as performing I/O |
| `unresolved_calls` | int | Calls made inside the flow whose target is no symbol in the graph. See [What unresolved_calls counts](#what-unresolved_calls-counts) |
| `truncated` | bool, when true | The walk stopped at 5000 symbols; the flow is larger |

A flow is a floor, not a measurement. The walk follows `calls` relations that
resolve to a symbol and stops where one does not, so `reach` undercounts by
whatever sits behind `unresolved_calls`.

### What unresolved_calls counts

Every call inside the flow whose target names no symbol in the snapshot. That is
two different things under one number, and the first is by far the larger:

- **Calls that leave the repository.** `fmt.Sprintf`, `strings.Join`, a method
  of an imported package. The graph holds this repository's symbols, so the walk
  ends at its edge by design.
- **Calls the extractor could not name.** A method called on a local variable or
  a field whose type was not resolved, such as `f.PropString` where `f` is a
  parameter. A symbol for it may exist in the repository; the call does not say
  which.

On enola's own repository the flow entered at the binary's `main` reports 3,894. Of
those 3,422 are calls into the standard library and imported packages, 57 into
third-party packages, and 415 are calls on a value of unresolved type. Only the
last group is a gap in the graph.

A large number is therefore not a defect. Read it against `reach`: a flow that
reaches 6 symbols with 23 unresolved calls is one the graph could barely follow,
and a flow that reaches 2,000 with 3,900 is a large program that uses its
standard library.

## Stage object

A Ref, plus:

| Field | Type | Present when | Meaning |
|---|---|---|---|
| `order` | int | always | 1-based position in the array |
| `depth` | int | always | 1 for a step the entry calls, 2 for a step under it, at most 3 |
| `parent` | string | `depth` is above 1 | The `id` of the stage this one sits under, which is always earlier in the array |
| `reach` | int | always | This stage's share of the flow: the symbols it reaches, out of its parent's share, that no stage placed before it had reached |

At most four stages are listed under one parent. A callee is a stage when its
share is at least 5% of the flow.

## Mechanisms

| Value | The entry is |
|---|---|
| `route` | the handler a served route's `handled_by` relation names |
| `main` | a function named `main` |
| `registered` | a function or method an extractor marked `framework_registered` |
| `job`, `mailer`, `hook`, `task` | a method a framework invokes, as the `root` prop records it |

A `root` of `callback` is not an entry: a callback runs inside a request a route
already entered, so it is a step of that flow. Symbols in test code are never
entries. New mechanisms are additive vocabulary.

## What is not an entry

The list above is the whole of it, which decides what a repository's flows look
like:

- **A command-line program is one flow.** Its subcommands are reached by a
  dispatcher that `main` calls, so they are stages of the `main` flow and not
  flows of their own. enola itself lists one flow for its binary, with `Dispatch`
  and the largest commands as its stages.
- **A handler written as a function literal is not a symbol.** A tool or a route
  registered as `Register(name, func(...) {...})` has no symbol to enter at. Its
  calls belong to the function that registers it.
- **A served route with no bound handler is not an entry.** The route exists as
  a fact; without a `handled_by` relation naming a declared symbol there is
  nowhere to start the walk. Which frameworks bind their routes varies by
  language.
- **A library has no entry.** Nothing in it is a program or a served route, so
  `flows` is empty and `roots` is `{}`.

`roots` counts the entries that were found and `leaves` those among them that
reach nothing, so a short list can be told apart from an empty search.
