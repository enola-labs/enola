# A finding is not a verdict

Tarjan's strongly-connected-components algorithm is textbook. It is older than most of
the languages enola parses, it fits on a page, and enola's implementation of it is not
where the work went.

The work went into the graph it runs on, and into what comes back. Both are worth
separating from the algorithm, because they are what a finding actually rests on.

**The graph is derived, not retrieved.** An import edge exists because a route prefix
was composed across a function and package boundary, a Kotlin `import a.b.C` was
resolved to the module `a/b`, a C++ header was merged with the source that defines its
methods, or an ActiveRecord association was deliberately *excluded* so that a
bidirectional Rails relation would not manufacture a cycle that isn't there. Run a
textbook algorithm over a graph assembled carelessly and you get textbook answers to
the wrong question.

**A finding is a computed claim, with evidence and confidence.** For a cycle at
confidence `1.0`, that claim is a structural conclusion over the measured graph. For an
outlier or recognised convention below `1.0`, it is a candidate to inspect. In both
cases enola returns the result rather than asking the agent to reconstruct it from a set
of files. Nothing in that chain is retrieved by similarity or guessed by a model; see
[ARCHITECTURE.md → The idea](../ARCHITECTURE.md#the-idea) for why that is the founding
constraint rather than an optimisation.

That makes a finding reproducible. It does not make every finding certain or enforceable;
the rest of this document explains that distinction.

Across the 91 public repositories in the current [benchmark sweep](BENCHMARKS.md),
enola's eighteen explainers produced **12,027 findings**. The volume is the problem: a
repository-wide report is useful for investigation, but too broad to describe one
change.

At extractor version v197 the total was 29,633, including 23,775 `hotspots` findings.
Capping that explainer at its top 20 per repository reduced noise, but did not make a
full repository report specific to the change in front of you.

So the question that decides whether any of this is worth running is not *what can you
find*. It is **what happens to a finding after you have found it** — and that depends
entirely on the thing [SNAPSHOTS.md](SNAPSHOTS.md) describes: whether your graph is a
value you can compare against another one, or a picture of right now.

## Twenty-two explainers: three proofs and sixteen estimates

An explainer reads the fact graph and emits **findings** — a claim, a confidence, and
the entities the claim is about. There are nineteen, and they fall into six kinds:

- **The proofs.** `cycles` runs Tarjan's SCC over the resolved import edges — a cycle
  either exists or it does not. `intent` diffs DECLARED architecture (a repo's
  enola-intent.yaml, or a cluster config's intent block) against the measured
  cross-repo edges: an unexpected or mis-mechanism seam is set difference between
  stated and measured, which either holds or it does not. Its estimating findings — a
  declared seam the graph never measured, a page relation to an uncompiled page, a
  measurable code anchor no fact touches, a seam covered only by superseded intent —
  are capped below 1.0, because each absence can be drift or an extraction miss.
  It asks the same two-sided question of a repository's DECLARED DEPENDENCIES:
  a package the manifests carry that the declaration does not name is a set
  difference and is reported at 1.0, while a declaration naming a package no
  manifest carries is 0.8 — removed and gone stale, or a manifest form the
  extractor does not read, and the facts cannot tell those apart.
  `constraints` is the third: it evaluates the declared components-and-rules vocabulary
  against the measured graph — a component resolves to the facts its match patterns
  select, and a rule states one of 21 enforceable forms over components (`forbid`,
  `forbid_reach`, `allow`, `protect`, `private`, and the rest). A breach is set
  membership over measured edges, so it is proof-class; the one place it estimates is
  a `forbid_reach` membership too large to walk, which degrades to a single `0.4`
  advisory rather than guessing.
- **Outlier tests.** `god-class`, `hotspots` and `complexity-outliers` compute a
  distribution over your repository and flag what sits above `mean + 2σ`.
- **Graph shape.** `dependency-depth` measures the longest transitive import chain;
  `exported-surface` flags large modules that export nearly everything; `import-closure`
  measures what a Python package executes when it is imported, and which of its own
  `__init__.py` files account for most of that. It is the one that reads a graph of its
  own — the same dependency facts at FILE granularity, since the module graph resolves
  both ends of an edge up to the enclosing package and so cannot say which modules
  beneath one were paid for.
- **Convention matching.** `layers` recognises an architecture by matching module paths
  against fourteen known taxonomies, then flags imports that run the wrong way through it.
  Each taxonomy is scored over the modules whose LANGUAGE it describes, and one is
  reported per language cohort — so a Rails monolith with an Ember front end is named as
  both, over disjoint modules, rather than having one half judged by the other's layer
  order. Every statement names its cohort and that cohort's share of the repository, and
  every violation names the taxonomy that judged it.
  The statement it makes carries its own denominators — modules classified, how many of
  those sit in an ordered layer, and how many measured imports run inward against how
  many run against the order — because "hexagonal, 66% confidence" and "the names say
  hexagonal and 340 imports run against it" are the same snapshot and only the second is
  worth reading. Confidence is `graded / scanned` — classified modules in a non-neutral
  layer over distinct non-test modules — where it was previously
  `0.6·coverage + 0.4·(matched layers / taxonomy size)`, a second term whose denominator
  was the layer table rather than the repository. A taxonomy whose `classified / scanned`
  is under 0.20 makes no statement at all: a thin match is a wrong claim rather than a
  tentative one.
  A taxonomy may also mark a layer NEUTRAL: classified, but sitting in no dependency
  direction, so no import to or from it can be a violation. Wiring is what this is for —
  a Hilt `di` package and a Spring `@Configuration` package are referenced by every layer
  they wire and reference every layer they wire, so any rank given to them makes half of
  those edges a violation. It is the same judgement the Go layout makes about
  `internal`/`pkg` and the Rails one about `lib`, applied to a directory that is a real
  thing with no place in an order rather than one that holds every layer.
  A repo that DECLARES its layer order is evaluated against the declaration as well:
  that pattern is stated rather than guessed, so it and its violations are proof-class.
  It sits beside the recognised pattern, not in place of it — recognition scores itself
  over the whole snapshot, so it has no per-repo off switch. Both patterns are reported
  as findings that merely NAME the architecture, marked informational and never graded:
  they are exact, and gating on them would fail the change that declared an order for
  saying so. Only the violations under them can fail a build.
- **Reporters.** `crossrepo`, `coverage`, `unused-routes` and `messaging-coverage` compute nothing of their
  own; they summarise what the cross-repo linker already resolved — which repositories
  depend on which, where enola failed to follow a call, and which routes no loaded
  client calls, and where messaging contracts and detected Kafka call sites do not match.
  `unused-routes` is worth naming for the question it actually answers: every server
  route is an inbound message the repo promises to serve, and the reporter asks which
  of those promises no loaded client ever sends — a completeness check over the seam
  rather than over the code. It only means anything over a snapshot holding the backend
  AND its clients: a single-repo snapshot has no service nodes, so it reports nothing at
  all rather than reporting every route as unused, and past 80% unmatched the finding
  says the client set is incomplete instead of listing endpoints to delete.
- **The declaration-shaped ones.** Every explainer above keys off symbols, modules and
  their dependency edges, which left the route, storage and association facts the
  extractors emit feeding nothing. `domain` asks the questions those facts answer —
  what a Rails application's declarations say about its data and its API, rather than
  about the shape of its code. `query-loops` reports a database query issued once per
  iteration of a data-sized loop, and deliberately reports only the shape it can prove:
  the detector everyone asks for — `record.association` inside a loop — funnels to zero
  true positives on a large monolith, because the graph has no receiver type inference
  for Ruby and `candidate.posts` and `client.post` are the same string to it.
  `entry-points` marks the symbols a framework invokes directly — a routed controller
  action, a job a queue drains, a mailer, a rake task — so that reachability has roots
  at all. It stops at marking them: reachability *from* them reports 86% of a
  monolith's symbols unreachable, which is the receiver-typing gap showing through
  rather than a finding about the monolith, so that verdict is not shipped.
  `dead-methods` asks the narrower question that gap leaves open: it looks a Ruby
  method's bare name up in every call edge the graph holds and reports the names
  nothing uses, and the names only specs use. A name-based index under-reports, so
  what it lists is a candidate to delete, never a verdict.

  `vendored-candidates` is the one that reports so it will not have to act. It names
  directories that look like in-tree copies of another project — a licence file of
  their own, under a parent conventionally used for dependencies — and stops there.
  An earlier version of this idea excluded them automatically and deleted a
  repository's own source along with the dependencies, which nothing reported
  because the fact count moving DOWN is what a successful exclusion looks like too.
  A heuristic wired to an irreversible action has no way to be wrong safely. So this
  one hands the reader the evidence and the `ignore:` globs, and the config — which
  the reader writes and can read back — stays the only thing that excludes anything.

  `import-closure` is the one that measures a cost rather than a shape. It answers what
  `import yourpackage` actually executes — including the package `__init__.py` files no
  import statement names, which Python runs anyway. That makes visible a barrel written
  for convenience: each import in the chain is individually reasonable, so no
  file-by-file check can see that importing the public API also loads a scraping
  library. It reports the initialisations responsible for the most of it, because that
  is the part splitting one would give back.

  `package-metrics`, `dead-code` and `performance` are the three that ask about a
  whole repository rather than a framework's conventions. `package-metrics` is
  arithmetic over edges the graph already holds — Ca, Ce, instability, abstractness,
  and distance from Martin's main sequence — so the numbers are exact and only the
  claim that a large distance is a problem is an opinion. `dead-code` asks what
  nothing references, in every language, and answers in three confidence tiers that
  encode what the graph can actually see rather than how sure it feels: functions
  high, types medium, everything else low. `performance` estimates where cost grows
  with the input, from loop-nesting and call-in-loop props the parsers record; its
  Big-O labels are a structural worst case, not a measurement, and nothing here
  knows whether a loop is hot.

  Two of them defer. `dead-code` says nothing about a method `dead-methods` has
  claimed, and `performance` files no `call-in-loop` against a symbol `query-loops`
  has claimed. In both pairs the narrow explainer answers from a type the graph
  actually knows while the broad one answers from a keyword across ten languages,
  and reporting both would be the same loop counted twice.

What each one computes, every threshold it uses and what it deliberately ignores is in
[ARCHITECTURE.md → Insights](../ARCHITECTURE.md#insights-explainers). The distinction
that matters here is smaller and blunter: **three of the twenty-two prove something. The
other sixteen estimate.** A cycle is a fact about your import graph. A god class is an opinion
about your repository, expressed as a number, and reasonable people can disagree with
it.

That distinction is carried in the confidence score, and it is exact rather than
decorative: `1.0` means proven, and only `cycles`, `intent`'s set-difference verdicts,
declared-layer violations and declared-constraint breaches ever reach it. Every explainer that
computes a saturating score — a fan-in ratio, a coverage share — clamps strictly below
`1.0`, so a statistical outlier can never present itself as a certainty.

## A list of everything wrong is not a list of anything you did

Here is the shape of the problem in the current delta corpus. Twenty repositories,
before anyone changed a line:

> **1,620 pre-existing findings.** Up to 235 in a single repository.

Now make a change. Add a feature, fix a bug, let an agent refactor a package. Two
questions look similar and are not:

1. *What is wrong with this repository?* — 1,620 answers, none of them yours.
2. *What did my change do to it?* — the question you can actually act on.

Almost every one of those 1,620 findings was there before you opened the editor. You did
not write them, you are not going to fix them today, and a tool that reports them
alongside your change has buried the one thing you needed in existing findings. That
signal-to-noise ratio is why broad analysis is often disabled in CI.

Answering question 2 requires something the analysis alone cannot give you. It requires
the state of the repository *before* your change — still intact, still queryable, after
the change has landed.

## Why the snapshot matters

[SNAPSHOTS.md](SNAPSHOTS.md) makes the case that enola's graph is a **value**: computed
when you ask, named by a fingerprint of its contents, kept rather than overwritten. This
is what that buys.

Because the *before* still exists as a thing you can hold, a finding can be located in
time. And because every finding carries the entities it is about — the modules in the
cycle, the symbol with the fan-in, the dependency edge that crossed a layer — a finding
can be checked against what your change actually touched.

Put those together and the report inverts. Instead of *1,620 findings, one of which might
be new*, you get:

> **FAIL — 1 structural regression introduced.**

Measured on those same twenty repositories: an injected dependency cycle was reported
as **exactly one regression, and not one of the 1,620 pre-existing findings was repeated**
([BENCHMARKS.md § 2](BENCHMARKS.md#2-delta-precision--the-ratchet)). Revert the change
and it goes quiet again. The verdict is a function of the tree, not of history.

The explainers did not change between those runs. The difference comes from comparing
their findings across two snapshots.

## Three outcomes when snapshots are compared

Comparing findings across two snapshots gives three outcomes, not two:

- **Regression introduced** — the finding is new, *and* it cites something your change
  touched.
- **Improvement** — the finding was there and is gone, for the same reason.
- **Incidental shift** — the finding appeared or cleared, but nothing it cites was
  touched by your change.

The third bucket prevents statistical movement from being attributed to the change.

Most of the twenty-two explainers are relative to your repository. `mean + 2σ` moves when the
population moves. A ranked top-N list has fixed membership size, so when a worse
offender is deleted the next module rises into the window — and a finding "appears" for
a module nobody edited. Both are real effects of statistics, not of your work.

A tool that reports those as regressions misattributes statistical movement to your
work. enola lists them as **incidental finding shifts**, with the reason, and does not
grade them.

## What can fail the build

At the default confidence floor of `1.0`, only proof-class findings are eligible:
dependency cycles, `intent` set differences, violations of a declared layer order, and
breaches of declared constraints. Heuristic findings are capped below `1.0`.

Eligibility is not enforcement. A finding fails the build only when `--fail-on` names
its explainer. Lowering `--min-confidence` can include advisory findings such as god
classes, deep dependency chains, large exported surfaces and complexity outliers when a
team deliberately chooses to enforce them.

### Why a finding list stops at fifty

An explainer that reports a large repository one finding at a time buries every
other explainer. On a large Rails application `dead-methods` produces 955 and
`query-loops` 419; uncapped, the performance analyzer produced 1,254 of enola's own
1,385. So an explainer reports at most fifty findings individually and the rest as a
single rollup naming the number. The tool behind it still returns everything.

The budget is per repository rather than per snapshot. A snapshot can hold several,
and one budget spent in rank order is spent by whichever repository ranks first: a
cluster where one repository carries 200 dead methods and another carries 3 would
report nothing at all about the second, and a reader would take that for a clean
repository rather than an unexamined one.

The cap is not free, and what it costs decides who may use it. A diff identifies a
finding by its explainer and its title, so a finding past the cap is invisible to
`diff_snapshot`, and therefore to `check --fail-on`, except that the rollup's own
number moves. For a **candidate** that is an acceptable trade: it was going to be
verified before anyone acted on it. For a **proof** it is not — a repository
carrying 231 dependency cycles would rank a newly introduced one past the cap, and
a gate set on cycles would never fire. `cycles`, declared `layers`, `intent` and
`constraints` are therefore never capped, however many they report.

Some explainers buy their way past the cap with a second channel: props are diffed
as fact attributes rather than as insights, so `dead-code` stamps `orphan_class` on
the symbol, `performance` stamps `perf_risk`, and `package-metrics` stamps the
Martin measures on the module. A change past the cap still shows up on the fact it
is about, with a name attached, rather than only as a counter moving.

`vendored-candidates` is the deliberate exception in the other direction. It lists
every candidate uncapped, because it reports directories that look like somebody
else's code and excludes nothing: a truncated report there is the same failure as a
silent exclusion, in a quieter form.

### Measured precision of the newest explainers

`package-metrics`, `dead-code` and `performance` are gateable like any other inferred
explainer. What follows is what they measure at on a twelve-repository cross-language
sample, at the tier a gate would actually fire on. Four defects were found this way and
all four are fixed; the numbers below are after.

**`dead-code`, high-confidence tier: 37 of 37 on a Go service.** It was 37 of 41. Every
one of the four misses was the same thing: a package-local function whose name collides
with a Go builtin. A repository carrying its own `min` helper, written before Go 1.21
had one, got no incoming call edge for `min(a, b)` at all — the call was attributed to
the builtin — so a function called seventeen times read as referenced by nothing, at the
tier documented as safest to delete. A package's own declaration now shadows the
predeclared identifier, as it does in Go itself.

**`dead-code` no longer spends its budget on other people's code.** On a C++ repository
20 of 51 candidates sat under `contrib/`, third-party solvers the reader is not going to
delete. Candidates under a directory conventionally holding other projects' code now
rank last, so the capped list is the reader's own code first. They are ranked, not
excluded: such a directory routinely holds first-party code too, and `find_orphans`
still returns every candidate.

**`performance`, high-severity tier.** On Rails applications these are ActiveRecord
reads in loops (`find_by`, `where`, `save!`) and are what they say they are. Rust was
the exception and is now the fifth ecosystem with its own expensive-call gate, beside
Swift, the JVM, TypeScript and Dart, for the same reason each of those has one: an async
runtime spells its in-memory primitives with the generic list's I/O verbs. A `send` on
an mpsc channel is not a database write. On tokio that cut 42 high-severity findings to
3 — a `file_type` per directory entry, a socket `connect` in a loop, and a
`spawn_blocking` per iteration, which are the three that were real. Cargo's `benches/`
tree also counts as test code now, as `tests/` already did; it accounted for 11 of the
42.

**Deference is worth what it costs.** On a large Rails application, `query-loops` claims
333 symbols and suppresses 265 of the `call-in-loop` findings this explainer would
otherwise file — a quarter of them — and `dead-methods` claims 268 symbols, suppressing
184 dead-code candidates. Those were the same loops and the same methods, reported twice
from a weaker signal.

## What it looks like when it works

The point of all this machinery is a single moment: an agent finishes a task, and
something asks *what did that do to the structure?* before it reports done.

In the headless A/B in [BENCHMARKS.md § 6](BENCHMARKS.md#6-agent-ab--does-the-agent-ship-the-regression),
an agent with no structural tooling shipped a dependency cycle on **3 of 3** trials —
TypeScript compiled it, every test passed, nothing complained. With the session hooks
installed, **0 of 3** did. From one transcript, unprompted:

> The regression was `src/domain ↔ src/store`: my first attempt made the domain
> `import { listOrders }` from the store, which closed a loop with the store's
> pre-existing `import type { Order }` from the domain. I removed the offending edge.

The agent introduced the cycle, was handed the verdict, and fixed it before saying it
was finished. Nine runs is a demonstration rather than a statistic, and the section says
so — but the mechanism is the argument, and the mechanism is the delta.

## What this does not give you

- **It will not tell you what to fix first.** Confidence is comparable *within* an
  explainer, not across them. A coverage gap at `0.9` and a layer violation at `0.8` are
  not ranked against each other, and enola does not pretend to a severity model.
- **96.3% of findings never stop anything.** If you want a god class to fail your build
  you have to say so explicitly. The default is deliberately narrow, and narrow means
  most of what enola finds is advisory.
- **Pre-existing problems stay silent by design.** The ratchet reports movement, not
  state. A repository can carry 159 findings forever and every `enola check` will pass.
  That is the right default for a gate and the wrong tool for paying down debt — for
  that, read the findings directly rather than the delta.
- **The heuristics are repo-relative.** A uniformly complex codebase reports nothing
  remarkable, because nothing in it is remarkable *for that codebase*. Findings compare
  you to yourself, never to an industry baseline.
- **The bulk of the volume is one explainer.** 23,775 of the 29,633 findings were
  hotspots, measured before that explainer was capped; it now reports at most its 20
  highest-scoring pinch points, like its siblings. "No new findings" is a statement
  about your change, never a certificate that a repository is clean.

---

Measured numbers: **[docs/BENCHMARKS.md](BENCHMARKS.md)** ·
What the words mean: **[docs/GLOSSARY.md](GLOSSARY.md)** ·
Why the graph is a snapshot: **[docs/SNAPSHOTS.md](SNAPSHOTS.md)** ·
Commands and flags: **[docs/CLI.md](CLI.md)** ·
How the engine works: **[ARCHITECTURE.md](../ARCHITECTURE.md)**
