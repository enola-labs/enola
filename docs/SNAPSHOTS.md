# Why a snapshot, not a store

enola computes the graph when you ask for it and keeps the result as a value: a complete set of
artifacts, named by a fingerprint of its own contents, that nothing later edits in place. It does
not maintain one graph that is kept current as your files change.

That is a deliberate choice, it costs something, and this document makes the argument for it and
then says where the opposite choice is the better one. It is about the graph's *lifecycle* — when it
is computed, and what happens to the one before it. What the graph actually contains is
[ARCHITECTURE.md → The idea](../ARCHITECTURE.md#the-idea).

## A verdict is a function of two states

What enola produces is not the graph. It is a verdict on a change: *this edit closed a dependency
cycle between `billing` and `invoice`, and nothing else moved.*

A verdict of that shape is a function of two states. You cannot compute it from the current
structure of the code, however accurate that structure is, because "your change made this worse" is
not a property of the code in front of you — it is a relation between the code in front of you and
the code before you started. Every question worth gating on has the same requirement: what did this
session do, what does this pull request do, what did this commit do to the architecture.

So the design question is not *how do we keep a graph current*. It is *how do we still have the
earlier one*. And there is a trap in the obvious answer: the edit you want graded is the same edit
that would update a graph maintained in place. Whatever keeps such a graph current — a watcher, a
hook, a re-index on read — fires on precisely the event whose effect you were trying to measure, and
the *before* is gone by the time you can ask about it. A graph that is updated in place has one
state, and its name is "now".

The way out is to stop treating the graph as something you are *in* and start treating it as
something you *have*: a value that gets computed, named, kept, and compared against the next one.
Everything about enola that looks unusual falls out of that one decision, and the rest of this
document is those consequences in order.

That it delivers the verdict it claims to is measured in
[BENCHMARKS.md § 2](BENCHMARKS.md#2-delta-precision--the-ratchet): across fifteen repositories
carrying **1,228 pre-existing findings**, an injected dependency cycle was reported as exactly one
regression, and not one of the 1,228 was repeated.

## So the unit is a value, and it is named by its contents

A value you intend to compare needs a name, and the name has to mean something. enola's is
`snapshot_id` — a SHA-256 over the byte-stable fact serialization plus the enola version and the
effective config hash
([ARCHITECTURE.md → Determinism](../ARCHITECTURE.md#determinism--incremental-updates)).

The property that earns its keep is not uniqueness. A UUID is unique. It is that the name is
**re-derivable by someone who was not there**: given the same commit and the same config, anyone
gets the same string, on another machine, a week later. That turns *"is this the same graph you were
looking at?"* from a matter of trust into an equality check — which is what lets `compare_receipts`
treat a matching ID as proof rather than as a coincidence.

Determinism is the precondition for that, not the payoff. Measured:
[95 of 95 repositories produced a byte-identical `snapshot_id` and a byte-identical `facts.jsonl`
across three runs each — 285 runs, 8,372,731 facts, zero drift](BENCHMARKS.md#1-reproducibility).
Cold first and then warm, deliberately, because the thing worth testing is that a cached run and a
from-scratch run agree rather than that one code path repeats itself.

The same discipline holds in memory, where nobody can check it. A snapshot is built into a brand-new
fact store off to the side and published with a single atomic swap of an immutable bundle, so a
reader that started iterating before the run keeps iterating the graph it started with instead of
watching one change underneath it ([`internal/engine/engine.go`](../internal/engine/engine.go)). A
restart restoring a multi-repo graph from disk goes through the same publish.

One qualifier, since "on demand" invites the question: the graph is computed when something asks for
it — never in the background, and never in response to a file change. A server starting up restores
the last published graph rather than re-extracting, but nothing recomputes it until you say so.

## A baseline is a value you keep, not a state you are in

`enola baseline pin` copies the current artifact set into `.enola/baseline/`, and it stays there
across as many `generate_snapshot` runs as you like — which is the point, since a task is usually
several rounds of edits and the *before* has to outlive all of them. `.enola/previous/` is the other
one: rotated automatically on every write, always exactly one step back, no pin required.

Because a baseline is a file set rather than a position in a running process, two useful properties
come almost for free. It publishes atomically — staged in a sibling temp directory, renamed into
place — so a half-written baseline is never diffed against: an absent baseline reads as "none
pinned" everywhere, while a truncated one would read as a real one and quietly produce a wrong delta
([`internal/engine/baseline.go`](../internal/engine/baseline.go)). And it is portable, because
repository identity is a normalized git remote rather than an absolute path, so a baseline pinned on
a CI runner grades a checkout that lives somewhere else entirely
([ARCHITECTURE.md → Repository identity](../ARCHITECTURE.md#repository-identity-portable-baselines)).

## Two values compare only if they were made the same way

Content-addressing gives a snapshot an identity. It does not give you permission to subtract one
from another. Two snapshots taken over different extractor sets, different ignore globs or different
enola versions differ in ways that describe *how they were produced* rather than what you edited, and
a delta that reports an entire language as removed because an extractor was switched off is worse
than no delta at all.

So provenance travels with the value. `receipt.json` is one of the four files copied into every
pinned and rotated baseline, alongside the facts, the insights and the metadata
([`internal/engine/baseline.go`](../internal/engine/baseline.go)) — which means comparability is a
check either side can perform, because both sides carry what they were made from. The kinds it
distinguishes, and why it is a spectrum rather than a boolean, are in
[ARCHITECTURE.md](../ARCHITECTURE.md#comparability-is-a-spectrum-not-a-boolean).

What is done about it differs by surface, and the difference is worth knowing before you rely on
either. `diff_snapshot` **warns above the delta** and still shows it, because an agent reading the
caveat can weigh it. `enola check` **declines to grade**, exiting `3` rather than `1`, because a gate
cannot weigh anything: reporting a mismatched pair as a failing change would be a lie, and passing it
would be worse. That includes the one caveat the diff cannot categorize at all — a working tree that
has moved since the snapshot was taken — which fails closed by design
([`internal/diff/diff.go`](../internal/diff/diff.go)).

## A composed graph is re-linked, not accumulated

The multi-repo case is the same choice applied to composition. Appending a repository does not add
its cross-repo edges to the ones already present: every previously synthesized cross-repo fact is
dropped and the link set is recomputed over the whole fact set
([`internal/engine/engine.go`](../internal/engine/engine.go)). The links therefore describe exactly
the repositories currently loaded, and unloading one cannot leave behind an edge pointing into a
graph nobody holds any more.

Stated precisely, because the distinction matters: it is the **link layer** that is recomputed. The
facts of repositories already loaded are carried forward from the published bundle rather than
re-extracted, which is what makes appending a fifth repository cheap — and it is also why a
cross-repo edge is only as current as the least recently indexed side. That consequence is spelled
out in the limits section below rather than left here as a footnote.

## What recomputation costs

None of the above is affordable unless recomputing the graph is cheap at the cadence a verdict is
needed, which is roughly twice per task: once before the work and once after.

On the sweep corpus, 71 of 81 re-snapshot warm in 5.0 seconds or less. The ten that do not are the
largest things in it — dotnet/runtime at **29.4s** (17,766 files, 397,608 facts), the Linux kernel at
24.3s (55,408 files, 1.9M facts), GitLab at 20.1s, Spark at 18.1s, roslyn at 10.3s, rust-lang/rust at
8.1s, dart-sdk at 7.4s, OpenProject at 5.4s, flutter at 5.2s, Airflow at 5.2s. Warm runs come out
1.33×–9.15× faster than cold over the 62 repositories whose cold run exceeds half a second; below
that the timing is noise
([BENCHMARKS.md § 4](BENCHMARKS.md#4-scale)).

The mechanism is the per-file content-hash cache in
[ARCHITECTURE.md](../ARCHITECTURE.md#determinism--incremental-updates), and there is one thing that
section does not say which you should know before budgeting for this: **warm cost is not proportional
to the size of your change.** Editing one file still walks the tree, re-links, and re-runs every
explainer over the full fact set. Only parsing is skipped, and only for files whose contents did not
move. The honest claim is that a re-snapshot is affordable twice per task — not that it is free, and
not that a small edit costs less than a large one.

## enola persists plenty — none of it is the source of truth

This is not an argument about writing zero bytes to disk, and it would be easy to check and find
otherwise. A snapshot leaves `facts.jsonl`, `insights.json`, `snapshot.meta.json`, `receipt.json`,
`flows.json`, `docs.jsonl` and `llm_context.md` in `.enola/`, plus the rotated `previous/` and any pinned `baseline/`. There is an
extractor cache. There is a graph receipt under `~/.enola/graphs/` whose entire job is to let a
restarted server restore a multi-repo graph without running an extractor
([ARCHITECTURE.md → Output artifacts](../ARCHITECTURE.md#output-artifacts)).

The distinction is not persistence. It is **authority**. Every one of those files is derivable from
the tree: delete all of them and the next run reproduces the same `snapshot_id`. None of them can
tell you something your source does not say, and none of them accumulates a history your source has
forgotten. The working tree is the only thing enola treats as true.

That rule has teeth, and the sharpest edge is the cache. A cache written by a different binary is
discarded wholesale rather than partially reused — the reasoning, including why the cheap staleness
check is wrong in the right direction, is in
[ARCHITECTURE.md → Cache identity](../ARCHITECTURE.md#cache-identity--why-a-snapshot-cannot-depend-on-which-binary-wrote-the-cache).
For a tool whose value rests on "a clean delta means something", a fact that depends on which binary
happened to warm the cache is the worst thing that can be in the graph.

## What this costs

Every one of the properties above is bought with the same currency: the graph is not maintained for
you. That is a real bill, and it comes due in three places.

**Freshness is your move.** A graph that refreshes itself answers *what does this look like now* with
nobody having to remember anything. enola's snapshot is exactly as current as your last
`generate_snapshot`, and when it is not, enola tells you rather than fixing it. What that buys is the
thing the whole design rests on — nothing in the graph can quietly disagree with your tree, because
nothing is allowed to update without being asked. What it costs is a step someone has to take, and
enola's own measurements are blunt about how reliably people take it: in
[BENCHMARKS.md § 6](BENCHMARKS.md#6-agent-ab--does-the-agent-ship-the-regression), **no
enola-equipped run ever called `diff_snapshot` on its own.** That is precisely why `enola install
--hooks` exists — the loop is wired to fire on session end rather than left as an instruction to
follow, because as an instruction it was not followed.

**Refresh cost tracks the repository, not the change.** Editing one file re-walks the tree, re-links,
and re-runs every explainer; only parsing is skipped. Updating just what moved would be strictly
cheaper per edit, and the gap widens with size — 33.9 seconds warm on the largest repository in the
corpus is comfortable for a gate that runs twice a task and impossible for one that runs on save.
What it buys is that every snapshot is a complete value rather than an accumulation of edits, which
is what makes any two of them comparable at all.

**Retrieval cost shows up in real use.** Across
[238 Claude Code and OpenCode sessions](BENCHMARKS.md#real-session-usage),
4,375 enola calls put 4.1 million tokens into context; the median call was about 220 to 250 tokens,
and most graph lookups completed in tens of milliseconds. In 205 of 210 sessions that took a snapshot,
the source represented by it was larger than the agent's median active context. Those are field
measurements from ordinary development. The fixed-corpus [benchmarks](BENCHMARKS.md) answer a different
question: whether the complete snapshots and the structural verdicts drawn from them are reproducible.

One number in that benchmark is easy to misread as enola's price, and is not it. The A/B's cost
column — a median \$0.31 for the bare agent against $1.12 with the loop installed — compares an agent
that checked its work against one that did not. The bare arm is cheap because it stopped early and
shipped the dependency cycle in all three trials. Nothing there isolates what enola costs, and no arm
measures the alternative that would: an agent asked to establish the same property *without* a graph,
re-deriving the module structure from source, in tokens, on every task. enola's own contribution to
that column is a call to a local binary.

### Limits that are enola's own

Two more things are worth knowing, and neither follows from the trade above.

- **Three snapshots for grading, plus an optional timeline.** The gate still works from exactly
  three: current, `previous/` one step back, and one pinned `baseline/`. Each run publishes a
  complete artifact set and rotates the old one out.
  Alongside them, enola now records each snapshot as a revision in an append-only **architecture
  history** ([HISTORY.md](HISTORY.md)) — so it can answer what your architecture looked like in
  March, and `enola log --backfill` can build that answer from your commits even for a repository it
  has never seen. That history is deliberately kept OUT of the three above: it is derived, it is
  replayable, and nothing that judges the present reads it, so deleting it changes no verdict and no
  `snapshot_id`. The duplication between `previous/`, `baseline/` and the history is the price of
  that separation, and it is the reason the paragraph below still holds.
- **A cross-repo edge is only as current as the least recently indexed side.** Appending re-links
  from scratch but does not re-extract what is already loaded, so a graph built up across an
  afternoon holds each repository as of the run that added it. This does not touch a per-repo
  verdict: `enola check` grades a repository against the baseline pinned in *that* repository's
  `.enola/`, so a sibling indexed yesterday cannot change today's answer. Where it does bite is the
  link layer — the one place two repositories' facts meet — so a client call can resolve to a route
  the server side has since removed, or miss one it has since added. That is ordinary staleness
  rather than anything peculiar to composition: the freshness check reports it per repository, and
  the graph receipt records each repository's git ref, commit and dirty state. Re-snapshot a
  repository before trusting edges that cross into it.

### How narrowly to read this

None of the underlying techniques are exotic, and it would be false to imply otherwise. Reproducible
output is a reachable invariant for anything that parses rather than guesses. Computing a delta
between two graphs is not a hard problem. A graph file kept under version control has a prior state
by construction.

The claim is not that these things are difficult. It is that a verdict on a change needs all of them
at once and as invocable surfaces — an addressable identity, a pinned baseline that survives
re-snapshots, and a comparability check that can refuse, each reachable from a shell with an exit
code. Assembling that on top of a graph maintained in place means rebuilding the *before* that the
maintenance keeps destroying. It is easier to keep it.

---

Measured numbers: **[docs/BENCHMARKS.md](BENCHMARKS.md)** ·
What the words mean: **[docs/GLOSSARY.md](GLOSSARY.md)** ·
What the findings are for: **[docs/EXPLAINERS.md](EXPLAINERS.md)** ·
Commands and flags: **[docs/CLI.md](CLI.md)** ·
How the engine works: **[ARCHITECTURE.md](../ARCHITECTURE.md)**
