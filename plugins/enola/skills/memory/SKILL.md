---
name: memory
description: Anchor memories about code to the symbols they describe with enola, so they can be found by what a task touches and checked for staleness before use. Works with any memory store, such as memory files, a notes directory or a memory service. Use when saving a memory about specific code, stamping existing memories that have no stamps yet, recalling what is known about code you are about to change, or cleaning up memories that may describe code that has since changed, moved or been deleted.
---

# Memory anchored to code

A memory about code ("this function retries twice because the API rate-limits", "the
bug in X came from Y") is only as good as its link to that code. Kept as text, it is
found by words and goes stale silently. Anchored with enola, it is keyed by the symbol it
describes, found through the graph, and checked against the code before it is trusted.

enola does not store memories. It supplies the key (a stamp) and the check. The memory
stays wherever the user keeps memories.

## The stamp

`locate` with the place the memory is about returns the symbol holding it:

```
locate(locations=["src/billing/retry.ts:42"])
```

or `enola locate src/billing/retry.ts:42` on the command line (`--json` for the fields).
Keep these fields from the symbol, plus the cited fields when one line was named:
`id`, `repo`, `name`, `symbol_kind`, `body_hash`, `inner_hash`, `decl_hash` when
present, and `cited_hash` with `cited_lines`.

A single cited line is hashed with the line before and after it, taken only from inside
the symbol, so `cited_lines` is 2 on a declaration or closing line and 3 elsewhere.

Cite the narrowest place the memory is about: one line for a bug or a gotcha, the
declaration line for a memory about the whole function. A line outside every symbol
(an import, a package-level block) gets no stamp; record the file path instead.

The stamp names the innermost symbol enola holds at that line, which can be smaller
than what a reader would call a declaration: a method of an object literal that a
function returns, a function named inside another function, a default export written as
a call. Every status below is about that symbol, not the function around it. Do not
overrule a status because the symbol is not a declaration by some other definition.

## Saving a memory

1. Write the memory as the user's memory system expects.
2. If it is about specific code, stamp each place it is about and store the stamps with
   it: in the store's metadata or frontmatter if it has any, otherwise in a block at the
   end of the memory, otherwise in a JSONL sidecar keyed by the memory's id. Keep
   `file:line` beside the stamp for a human reader.
3. Do not stamp memories that are not about code: preferences, people, decisions with
   no code location. They are recalled the usual way.

If the memory system already calls enola (cognee, for example, stores `locate` stamps),
reuse its stamps rather than adding a second set.

## Starting out: stamping what is already there

Run this when the skill is invoked with no task, or when the store holds memories about
code and none of them carries a stamp. Memories written before the skill was installed
name code by symbol, not by line, so nothing else ever stamps them.

1. Make sure a snapshot of the repository is loaded (`generate_snapshot` if `locate`
   says there are no facts).
2. List the memories about this repository's code: the ones naming its functions,
   types, methods, files or directories. Skip memories about preferences, people, tools,
   CLI flags, property names and other repositories.
3. Resolve the names with `locate(names=[...])`, every name of every memory in one call
   (`enola locate --names a,b,c --json` on the command line). A name matches a symbol
   whose qualified name ends in it, case included: `nodeFor` finds `Graph.nodeFor` and
   not `nodeForID`, and `Graph.nodeFor` narrows to that method. Each name comes back
   `one`, `several` or `none`, and every symbol in it already carries the stamp fields,
   with its declaration line cited. Do not send test functions, constants or
   package-level variables: the graph holds no symbol for them.
4. Decide per memory, not per name: two memories can mean different symbols by the same
   name. For `several`, keep the symbol the memory's text points to (its package,
   language or file); if the text does not settle it, stamp none of them and list the
   name as ambiguous. Store the stamps as in [Saving a memory](#saving-a-memory). In
   frontmatter, an `anchors:` list with one entry per stamp, `at: <file:line>` first,
   keeps it readable. A file or directory the memory is about is recorded by path, as an
   entry with `at` and `file`. Adding stamps changes no memory's content, so it needs no
   confirmation.
5. Report counts: memories stamped, names resolved, and names not found, listed under
   their memory. Keep two lists: names that were never symbols (tests, constants,
   package variables, fields, recorded by path if at all) and names the graph should
   hold but does not. Only the second is a finding: the code was renamed, moved or
   deleted after the memory was written. `git log -S<name> --oneline -1` shows the
   commit that last added or removed it. Searching for what does its job now and
   stamping that is worth doing only when the user asks.
6. A memory whose names are mostly not found describes code that is gone. Say so and
   ask the user before marking it stale or changing it.

## Recalling what is relevant

Relevance comes from the graph, not from words in the memory.

1. Name the task's symbols: `locate` on the lines you are about to change, or the
   symbols the user named.
2. Widen by one or two hops: `impact_analysis(target=<symbol>, max_depth=2,
   include_forward=true, output_mode='full', max_nodes=100)` lists what depends on it and
   what it depends on, each node with its `name`, `file` and `depth`. Use `full`: the
   compact and summary modes shorten Go names relative to the module, so they no longer
   equal a stamp's `name`, and they cut long lists short. If `stats.truncated` is true,
   say that the recall may be incomplete.
3. The relevant memories are the ones whose stamp `name` equals a node's `name`, compared
   exactly. Rank them: the symbol itself first, then one hop away, then two, then memories
   recorded only by file path whose path is a node's `file`.
4. Check only those stamps, as below. Checking every memory in the store on every task
   is cost without benefit.

## Checking before use

Pass the candidates' stamps to `locate(stamps=[...])` in one call, or `enola locate
--stamps stamps.jsonl`. Act on each status as follows, and do not re-derive a status by
reading code: enola already compared the versions.

| Status | What it means | What to do |
|---|---|---|
| `current` | Same symbol, same code | Use the memory as it is |
| `rewritten` | Same symbol, its code changed. `change: signature` means only the signature did; `change: body` means the body did | For `signature`, a memory about behaviour still holds; one about how to call it needs a look. For `body`, re-check the claim against the code (`show_symbol`) before relying on it, then re-stamp it or update it. `cited: intact` means the cited lines are still in the symbol |
| `moved` | Same code under a new id, in another file or package | Replace the stamp with the returned `symbol`'s fields. Nothing to re-check |
| `present` | Same symbol, but a version is missing on one side | Treat as unverified |
| `gone`, no candidates | The symbol is not in the graph | The code it described was deleted. Mark the memory stale and ask the user before removing it |
| `gone`, with candidates | Same name with other code, or a copy elsewhere | Probably moved and rewritten at once. If the memory matters for the task now, look at the best candidate once; otherwise mark it unresolved and move on |
| `ambiguous` | More than one symbol fits | Keep the memory and show the candidates; do not pick one |

Stamps are checked against the current snapshot. Refresh it first (`generate_snapshot`)
if the code changed since. To check against a recorded revision instead of the working
tree, use the CLI: `enola locate --at <commit> --stamps stamps.jsonl`.

## Keeping memory clean

When the user asks to tidy memories, check every stamped memory in one batch and report
counts per status, then the `gone` and `rewritten` ones by name. Update `moved` stamps
without asking. Changes to or removal of a memory's content are the user's call.
