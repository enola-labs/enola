# Glossary

The words enola uses in its own output, in the order you meet them. Standard algorithms
(Tarjan's SCC, `mean + 2σ`, longest-path) are glossed where they appear rather than
here — this page covers the vocabulary that means something specific *in enola* and that
searching the web will not tell you.

## What enola extracts

**Fact** — the unit of the graph, and the only thing enola treats as knowledge. A fact
is one typed thing that exists in your code: a `module`, a `symbol`, a `route`, a
`storage` target, a `dependency`, an `association` (a model's declared relation to
another, which is what lets a query walk from a URL to the tables behind it), or a
`service` (a whole repository, in a multi-repo graph). Every fact is derived by a parser or a deterministic algorithm — never guessed
by a model, never retrieved by similarity.

**Relation / edge** — a typed link between facts: `imports`, `calls`, `declares`,
`implements`. Relations live inside the fact that owns them, which is why two graphs can
have identical fact counts and still differ.

**Extractor** — the per-language component that turns source into facts. One per
language, each documented with the code that produces which fact in
[docs/extraction/](extraction/README.md).

**Linker** — the pass that connects facts *across* repositories: a client call site to
the route that serves it, a gRPC call to the `.proto` service behind it. It runs after
extraction and before any analysis.

## What enola computes

**Explainer** — an analysis that reads the fact graph and emits findings. Each is named
in `enola check --fail-on=` and in `query_insights(explainer=…)`, so the names below are
vocabulary you type, not internals:

| Name | What it claims |
|---|---|
| `cycles` | these modules can all reach each other — a dependency cycle, proven |
| `layers` | this repository has this layer order, and these imports run the wrong way through it |
| `constraints` | a declared component rule was breached by a measured edge |
| `intent` | the declared architecture and the measured graph disagree here |
| `crossrepo` | which repositories depend on which |
| `coverage` | where enola failed to follow a call, as distinct from there being none |
| `unused-routes` | no client in this snapshot calls this route |
| `messaging-coverage` | a messaging contract and the code implementing it do not match |
| `god-class` | this symbol is depended on by far more than the average |
| `hotspots` | this symbol is a pinch point — high fan-in *and* high fan-out |
| `dependency-depth` | this module sits at the end of an unusually long import chain |
| `exported-surface` | this module is large and exports nearly all of it |
| `complexity-outliers` | this function's cyclomatic complexity is an outlier for this repository |
| `domain` | what a framework's declarations say about the data and the API |
| `query-loops` | a database query issued once per iteration of a data-sized loop |
| `entry-points` | a framework invokes this symbol directly, so reachability has a root here |
| `dead-methods` | a Ruby method whose name no call edge in the graph uses, or only spec files use |
| `vendored-candidates` | a directory carrying its own licence under a conventional dependency parent — reported so you can decide, never excluded |
| `import-closure` | what `import yourpackage` actually executes, and the package `__init__.py` files responsible for most of it |
| `package-metrics` | how stable and how abstract a package is, and how far that puts it from Martin's main sequence |
| `dead-code` | a symbol nothing in the snapshot references, in any language, reported with a confidence tier rather than a verdict |
| `performance` | a loop, a call inside one, or a recursion whose cost grows with the input, with the Big-O that follows from the nesting |

Only the first four ever reach confidence `1.0`. The rest estimate — see
[docs/EXPLAINERS.md](EXPLAINERS.md) for what each computes and why that distinction is
the whole design.

**Finding** (also **insight**) — one claim an explainer makes, carrying a title, a
confidence, and **evidence**: the specific entities the claim is about. The evidence is
not decoration — it is what lets the diff decide whether *your* change caused the
finding.

**Confidence** — how strongly the claim is held, from `0.0` to `1.0`. It means something
exact: **`1.0` is a structural fact and nothing else reaches it.** In practice that is a
dependency cycle, the one thing enola proves rather than infers. Everything below `1.0`
is a heuristic — a candidate to look at, not a verdict. Confidence is comparable *within*
an explainer, not across them.

**Structural vs heuristic** — the same distinction as above, and the one the snapshot
receipt counts. A structural finding is computed with certainty; a heuristic one is a
statistical outlier or a convention match, and reasonable people can disagree with it.

**Coverage gap** — a service that appears to depend on nothing, but where enola *did*
detect outbound call sites it could not resolve. Reported separately from `isolated`
precisely because "depends on nothing" and "enola could not tell" look identical in a
graph, and only one of them is good news.

## What enola keeps

**Snapshot** — the published result of one analysis run: the facts, the findings, the
metadata and the receipt, written together to `.enola/`. A snapshot is a value you keep,
not a database you update — see [docs/SNAPSHOTS.md](SNAPSHOTS.md).

**Snapshot ID** — the snapshot's name: a SHA-256 fingerprint of its own contents (plus
the enola version and config). Not a random ID — anyone with the same commit and config
re-derives the same string, which is what makes "the same graph" checkable rather than
claimed.

**Receipt** — the manifest recorded alongside every snapshot: enola's version, the git
ref and whether the tree was dirty, which extractors ran, and extraction-quality
counters (files parsed vs skipped, parse errors, how many findings were heuristic). It
answers *what was this graph computed over, what was excluded, and where are its known
limits?*

**Baseline** — a snapshot you pinned as the "before" of a change (`enola baseline pin`,
kept in `.enola/baseline/`). It survives later snapshots, so it stays valid across many
rounds of edits. `previous/` is the automatic one-step-back baseline, rotated on every
run.

**Repo label** — the short name every fact is tagged with, naming the repository it came
from (`internal/facts`, `pkg/command`, … all carry one). It is the repository's own name
taken from its git remote when the indexed directory is that repository's root, and the
checkout directory name otherwise. It is an identity, not a display string: a delta
matches facts on it, so two snapshots of one repository under different labels share
nothing and enola declines to compare them rather than reporting the whole graph as
rewritten.

**Append mode / composed graph** — loading more than one repository into a single graph,
so cross-repo edges can be resolved. Some explainers (`crossrepo`, `coverage`,
`unused-routes`) only produce findings here, because there is nothing cross-repo to
report in a single-repo snapshot.

## What enola grades

**Delta** (also **diff**) — the comparison of two snapshots. It reports only what
*changed*: findings introduced or resolved, coupling added, symbols added and removed.
It never restates what was already there.

**The ratchet** — the consequence of that: enola reports **movement, not state**. A
repository can carry hundreds of pre-existing findings and still pass, because none of
them is new. This is deliberate — it is what keeps a gate worth leaving switched on —
and it is also why the delta is the wrong tool for paying down existing debt.

**Regression introduced** — a finding that is new *and* cites something your change
touched. This is the only category that can fail a build.

**Improvement** — the mirror: a finding that was there, is gone, and cites something
your change touched.

**Descriptive finding** — a finding that says what the graph *is* rather than what is
wrong with it: which architecture pattern was matched, that a cluster config overrode a
repo's own declaration. Exact, worth reporting, and **never graded** — declaring a layer
order produces one, and grading it would fail the very change that declared it.

**Incidental shift** — a finding that appeared or cleared without your change touching
anything it cites. Usually a moving statistical threshold, or a ranked list re-ordering
after a worse offender was removed. Reported separately and **never graded**, so
statistical drift cannot masquerade as something you did.

**Declared scope** — what you say a change was *supposed* to touch, via `--target` (a
symbol, type or package) or `--expected` (packages). A delta cannot infer it: two
snapshots record what changed, never what was intended.

**Predicted blast radius** — the packages a declared target's reverse dependents live in,
computed on the **pre-change** graph. What *should* have been reachable.

**Spillover** — packages the change actually touched that were neither predicted nor
declared. A package altered by something your description did not cover — which is worth
reading even when every finding is clean and the build is green. Reported by default;
fails only when `--max-spillover` says so.

**Comparability** — whether two snapshots can be meaningfully subtracted at all. Two
snapshots built over different extractor sets, ignore rules or enola versions differ in
ways that describe *how they were produced* rather than what you edited. `diff_snapshot`
warns and still shows the delta; `enola check` declines to grade and exits `3` rather
than reporting a mismatch as a failing change.

---

Why the graph is a snapshot: **[docs/SNAPSHOTS.md](SNAPSHOTS.md)** ·
What the findings are for: **[docs/EXPLAINERS.md](EXPLAINERS.md)** ·
Commands and flags: **[docs/CLI.md](CLI.md)** ·
How the engine works: **[ARCHITECTURE.md](../ARCHITECTURE.md)**
