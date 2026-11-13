# Declared architecture intent

Enola measures what your architecture *is*. Intent declarations state
what it was *meant* to be — and every snapshot diffs the two, so
disagreement between decision and code surfaces as a finding with
evidence instead of waiting for someone to notice.

This page defines where intent lives, what enola reads, the vocabulary, and how findings
are graded. Declarations parse and compile the same way on every machine; an invalid
declaration fails the snapshot rather than being ignored.

## The input contract

Intent reaches enola through exactly three carriers, each an
enola-defined schema:

| Carrier | Where | Scope |
|---|---|---|
| `enola-intent.yaml` | a repo's root | that repo's own declaration |
| `intent:` block | a cluster config (`mcp-arch.yaml`) | per-repo declarations for an estate, overriding repo files wholesale |
| `enola_intent:` key | a markdown page's YAML frontmatter | intent declared where the *decision* lives — a wiki, a docs tree |

For markdown, the key is deliberately namespaced: a wiki's own
toolchain, linters and renderers ignore `enola_intent:`, and enola
ignores everything else — it never reads your `title:`, your tags,
your custom frontmatter, and it **never parses the page body**. Prose
stays prose. If your wiki has its own conventions (statuses, source
citations, ownership fields), your tooling can *derive* the
`enola_intent:` block from them — but that derivation is your side of
the boundary; enola's contract is the block itself.

## The repo and cluster schema

`enola-intent.yaml` at a repo's root and one entry of a cluster
config's `intent:` block are **the same schema**, which is why an
entry overrides a repo file wholesale rather than merging into it.
Every section is optional; declaring nothing is not an error:

```yaml
service:                   # this repo's declared identity
  name: backend
  description: order and payment API
consumes:                  # seams this repo intends to call
  - {repo: payments, via: http}
serves:                    # mechanisms this repo offers its callers
  - {via: http, description: public REST API}
layers:                    # this repo's layer order, outermost first
  - {name: handlers, paths: ["app/handlers/**"]}
  - {name: domain,   paths: ["app/domain/**"]}
  - {name: storage,  paths: ["app/storage/**"]}
dependencies:              # external packages this repo means to pull in
  - {name: rails, purpose: "the web framework"}
  - {name: jwt, ecosystem: rubygems, purpose: "token verification", safety_path: true}
```

In a cluster config the same document is nested one level under the
repo's label, because the file describes an estate rather than a repo:

```yaml
intent:
  backend:
    consumes:
      - {repo: payments, via: http}
```

**`layers:` here is a flat, ordered list of `{name, paths}`** — the
file already knows which repo it is about, so no `repo:` key. That is
the one place this schema and the page schema below genuinely differ,
and the difference is not cosmetic: a page can declare layers *for*
several repos, so its entries name their owner and nest the order
under `order:`. Getting it backwards is a validation error naming the
missing field, never a silently ignored section.

**What a `paths:` entry may say** is two forms and no more: an exact
module path (`src/lib`), or a `prefix/**` subtree matching the prefix
and everything under it (`src/lib/**`). There is deliberately no
basename form — a layer is a *region* of the tree, and a rule about
files named `*_controller.rb` wherever they live is a component, not a
layer. Anything else is a validation error naming the two forms, at the
moment you write it. It used to be accepted and then matched against
nothing, which is a layer order that lints clean and governs zero
modules.

Paths are repo-relative and forward-slash **on every host**. A
declaration written with backslashes is normalized rather than
refused: the same file is read on the laptop that wrote it and on the
runner that grades it, and it has to select the same code in both.

Two ways to see what an order actually selects, before it is anything
you rely on. `enola constraints lint` resolves each layer against the
snapshot on disk and prints its member count beside the measured module
paths, so a path that matches nothing is caught while you are editing
it. And every snapshot raises an advisory when a declared order
classifies no modules at all, or when one layer within it does —
reported, never gating, because the run that first declares an order is
exactly when a mistyped path shows up.

Declaring a layer order buys more than documentation. The `layers`
explainer reports a declared order at confidence `1.00` — declared,
not recognised — where a pattern it inferred for itself caps at
`0.80`. Since `enola check` gates at a `1.00` floor, only a declared
order is enforceable with `--fail-on=layers` alone; an inferred one
also needs `--min-confidence=0.8`.

`--fail-on` is the whole opt-in: enola fails nothing until it is
passed, so a declared layer order is a rule you wrote down and then
chose to enforce, in that order.

enola's own [`enola-intent.yaml`](../enola-intent.yaml) is the worked
example, and its CI runs `--fail-on=layers` against it. Two things in
it are worth copying: the layers are grouped by ROLE rather than by
directory tree (`pkg/` and `internal/` both appear at several levels,
because visibility is not a layer), and the file states in prose why
each boundary is where it is — a declaration nobody can explain is one
nobody will maintain. What it deliberately does not declare is anything
about cycles: Go's compiler already refuses those between packages, and
a declaration that restates the toolchain earns nothing.

**Declaring an order does not fail the pull request that declares it.**
The `layers` explainer emits an exact finding announcing the pattern it
matched, which is a description rather than a violation; the gate
routes those to a `Descriptive (never graded)` section and never counts
them. The first thing your new declaration reports is therefore itself,
harmlessly.

## The page schema

A page opts in by carrying `enola_intent:` in its frontmatter. Four
sections, all optional, all validated:

```yaml
enola_intent:
  page:                      # this page as a knowledge node
    type: decision           # lowercase token — your taxonomy
    status: living           # optional lowercase token
    scope: [backend]         # repos this knowledge is about
    affects: [mobile]        # repos this knowledge has consequences for
    origin: [slack, langfuse]  # channels the knowledge came from (closed set)
    relations:               # typed edges to other pages
      - {rel: part-of,       to: wiki/backend/epics/messaging.md}
      - {rel: depends-on,    to: wiki/backend/adrs/queue-choice.md}
      - {rel: supersedes,    to: wiki/backend/adrs/old-queue.md}
    anchors:                 # code locations this knowledge is about
      - {repo: backend, path: app/services/payment_processor.rb}
      - {repo: backend, path: app/handlers}
  consumes:                  # seams: who intends to call whom, and how
    - {repo: mobile, target: backend, via: graphql}
  layers:                    # a repo's declared layer order, outermost first
    - repo: backend          # named here — one page may declare layers for several repos
      order:                 # (the repo-file form above is a flat list, with no repo:)
        - {name: handlers, paths: ["app/handlers/**"]}
        - {name: domain,   paths: ["app/domain/**"]}
  claims:                    # measurable statements, re-verdicted every snapshot
    - {metric: fact-count, repo: backend, kind: route, name_prefix: "/api", value: 214}
    - {metric: seam, consumer: mobile, provider: backend, via: graphql}
```

Every fact compiled from a page carries the **page as its
provenance** — a verdict's evidence cites the decision that declared
the intent, not a config artifact. Seam and layer entries name their
owner repo explicitly (`repo:`), because a page lives where the
decision lives, not inside the repo it governs.

## The vocabularies (closed, validated, named in errors)

- **`via`** — how a seam is mechanized: `http`, `http-client`,
  `grpc`, `graphql`, `kafka`, `import`, `shared_symbols`,
  `object-storage`, `deployment`. `shared_symbols` and `object-storage`
  name coupling a call graph cannot see: shared/vendored code, and
  bucket-mediated export/import handoffs; `deployment` is a GitOps
  repository's edge to the code its releases run. A via outside this set is a parse error naming the set.
- **`dependencies`** — the external packages this repo means to depend on.
  Each entry names one and states its **purpose**, which is mandatory: the
  `manifests` extractor already measures which packages are declared, which
  are pinned and what a lockfile resolved them to, so a list of names would
  restate the manifest the repository already has. What no parser can measure
  is why a package is there, and that is the whole of what this section adds.
  `ecosystem` is optional and narrows the entry to one packaging system —
  omitted, the name is covered wherever it is measured, which is what you want
  until the day the same name is a gem and an npm package. `safety_path: true`
  marks a package the declared architecture leans on to hold an invariant; it
  produces no finding on its own, and exists so a rule can reach the set.
  File-level only, never on a page: this is the repository's own account of
  its supply chain, reviewed beside the manifest it describes.
- **`rel`** — how pages relate: `depends-on`, `supersedes`,
  `superseded-by`, `part-of`, `relates-to`. Targets are
  repo-relative markdown paths.
- **`anchors`** — where a relation joins page to page, an anchor
  joins page to code: a repo label plus a repo-relative path,
  either a file or a directory prefix. A validated shape rather
  than a closed vocabulary: both fields required, the path
  repo-relative. With anchors the reverse query — *which
  decisions govern this file?* — becomes a graph traversal
  instead of a grep through prose. That direction is the
  load-bearing one: a declared invariant naming no code location
  is a claim with nothing to check it against, so the anchor
  rather than the prose is what makes a decision enforceable —
  and an anchor that stops resolving is a citation announcing it
  went stale, instead of waiting for a reader to notice.
- **`origin`** — where knowledge came from: `slack`, `langfuse`,
  `notion`, `github`, `web`, `repo`, `other`. Channels, not source
  files: the entry names the class of system the page's evidence was
  ingested from, and your wiki keeps the mapping from its own source
  layout to these names. A new channel is a vocabulary addition
  here, never a stringly-typed leak.
- **`page.type` / `page.status`** — lowercase tokens; the taxonomy
  is yours.
- **`claims.metric`** — `fact-count` (kind + owner + optional
  file/name prefix + expected value) or `seam` (a measured
  cross-repo edge must exist).

## What compiles

Declarations become ordinary facts (`kind: intent`) at snapshot
time — snapshots carry them, diffs track them, receipts fingerprint
them. Nothing about intent lives in a side channel:

- a `page:` block → one **knowledge node** plus one **relation
  edge** per relation plus one **anchor fact** per anchor
- each `consumes:` entry → a seam-intent fact with `intent_owner`
- each `layers:` entry → per-layer facts feeding the layers explainer
- each `claims:` entry → a claim fact the explainer re-evaluates
- each `dependencies:` entry → a dependency-intent fact the explainer diffs
  against the packages the `manifests` extractor measured

## How intent findings are graded

The `intent` explainer diffs declared against measured. Confidence reflects what the
available evidence can establish:

| Finding | Confidence | Why |
|---|---|---|
| Unexpected seam (measured, not declared) | 1.0 | set difference between stated and measured — exact |
| Mis-via (right target, wrong mechanism) | 1.0 | exact |
| Failed claim (count or seam doesn't hold) | 1.0 | the claim is stated, the count is counted |
| Missing intended seam (declared, not measured) | 0.8 | could be drift *or* an extraction miss — an estimate never presents as certainty |
| Undeclared dependencies (measured, not declared) | 1.0 | set difference between the packages the manifests declare and the ones this file does — exact |
| Declared dependency not measured | 0.8 | the package was removed and the declaration went stale, *or* its manifest form eluded the extractor |
| Dangling relation (edge to an uncompiled page) | 0.8 | the target may be deleted or merely not opted in |
| Dangling code anchor (a measurable path no fact touches) | 0.8 | the code moved or died — or this one file eluded extraction |
| Anchors not checked (a page anchoring a repo the snapshot does not carry) | 0.4 | nothing was proven either way; the notice reports that the question was not asked |
| Superseded intent still measured (edge covered only by a retired page) | 0.8 | the code may lag the superseding decision — or the successor's intent is undeclared |

**Superseded pages retire from current intent.** Two signals mark a
page retired: an outgoing `superseded-by` relation (enola's own
closed vocabulary), or the status token `superseded` — the one
status token enola reads meaning into; the rest of the status
taxonomy stays yours. A retired page's seams, claims, anchors and
scope stop contributing current findings, while its relations are still evaluated
because the supersession trail itself must
not break. The one thing a retired page still *says*: a measured
seam that only a retired declaration covers surfaces as *superseded
intent still measured* — the code has not caught up with the
superseding decision, or the successor's intent is not declared
where enola can see it. That finding replaces the generic
unexpected-seam finding for such edges, because it carries the
diagnosis.

**Anchored code is traversable back to its decisions.**
`impact_analysis` on any node reports the knowledge pages whose
anchors cover the node's file — the governing decision trail,
surfaced exactly when someone is about to change the code it
governs — with each page's type and status joined from its own
declaration, and each page's outgoing relations riding along so
the trail continues past the first hop: the decision names what
it is part of, depends on, or supersedes. `show_symbol` carries
the same governing line beside the source. When the question is
governance alone, the `governing_intent` tool answers it directly
in either direction — a fact name or file path lists the pages
that govern it; a compiled page path lists its anchors with the
measured coverage under each — without computing a blast radius.
Its empty states preserve the counterparty distinction: a snapshot
with no compiled pages answers *not asked*, which is never the
same answer as *asked, none governs*.

Repos that declare nothing are unasked — adoption is per-repo, and
undeclared is not a finding. A declared seam whose counterparty is
absent from the graph is skipped, never failed; an anchor into a
repo the graph never measured is skipped the same way — and so is
a file anchor whose kind the repo's graph never measures: for a
file with an extension the kind is the extension (a README, a
doc), and for a file without one the kind is its exact basename
(a Gemfile, a Dockerfile, a version dotfile — the manifests this
rule exists for are extensionless almost by convention, and a
repo measuring extensionless scripts has not thereby measured
them). No extractor could have proven any of these either way, so
they are unasked, never dangling. Only a path the graph plausibly
measures and does not touch is dangling. An anchor
that joins is silence — and the join is the point: it is the
stale-citation check a wiki otherwise performs by hand, and it
makes every anchored file's governing decisions reachable from the
graph.

**The first of those skips is reported, because it is the one you
cannot see coming.** A page whose anchors name a repo this snapshot
does not carry raises one `0.4` notice per page — *anchors not
checked* — naming the repo it declared, the labels the snapshot does
carry, and how many anchors went unchecked. The skip itself is still
right: a citation into a repository nobody measured cannot be called
stale. What it must not do is look like a page whose citations all
held. A single-repository snapshot takes its label from that
repository's **directory name**, so a clone, a worktree or a CI
checkout under another name reaches this state with the declaration
untouched. Note the asymmetry it makes visible: `governed_by` and
`require_governed` still resolve those same anchors, because with one
repository loaded they drop the label — there is nothing else the path
could mean — while this check compares it. Two readings of one anchor,
each conservative for its own question, and now only one of them is
silent.

Scope and affects, by contrast, are **not graded**: they
speak the wiki's own repo vocabulary, and the mapping from that
vocabulary to cluster labels is the deriving toolchain's side of
the boundary (working rule 5) — a page about one name may compile
against a cluster that labels the same repo another way. Keeping
those names truthful is the wiki's job, where the mapping is
known. Declared layer
patterns are reported at 1.0 through the layers explainer, *alongside*
heuristic recognition rather than instead of it: a declaring repo gets
its declared pattern and its proof-class violations, and the
snapshot-wide heuristic pattern is still reported next to them. The
heuristic cannot be switched off per repo, because its confidence is a
ratio over every module in the snapshot — suppressing one repo's
modules would move the score, and possibly the verdict, for repos that
declared nothing.

Two consequences worth designing for. A **declared seam also earns
coverage**: unresolved client calls in a repo with exactly one
declared HTTP target attribute to it (`attributed_by_intent`) —
declarations supply what static analysis cannot, visibly, without
inventing an edge. And a declared seam **no linker can measure yet**
(e.g. `object-storage`) surfaces as a standing 0.8. Judge it once in whatever ledger your
workflow keeps and it stays acknowledged.

## What the manifests extractor measures

The declared half above only means something beside a measured half, and that
comes from the `manifests` extractor, which reads a repository's package
manifests for one thing: its **declared direct dependencies**.

| Manifest | Resolved against | Ecosystem |
|---|---|---|
| `go.mod` | itself — a `require` names an exact version | `go` |
| `package.json` | `package-lock.json`, else `yarn.lock` (classic and berry) | `npm` |
| `Gemfile` | `Gemfile.lock` | `rubygems` |
| `Cargo.toml` | `Cargo.lock` | `cargo` |
| `pubspec.yaml` | `pubspec.lock` | `pub` |
| `requirements.txt`, `pyproject.toml` | itself — `==` is pip's pin | `pypi` |
| `Chart.yaml` (or a Helm 2 `requirements.yaml`) | `Chart.lock` (`requirements.lock`) beside it, never an ancestor's | `helm` |

A lockfile is looked for **at or above** the manifest, nearest first, because
that is where package managers put it: a monorepo keeps one lock at the
workspace root and a manifest in every package. Reading only the sibling
reported five of excalidraw's dependencies as unpinned when its root
`yarn.lock` pins all of them. Helm is the exception: every chart, subcharts
included, locks its own dependencies, so a `Chart.lock` is read only beside its
own `Chart.yaml`.

Each package becomes one `kind: dependency` fact carrying `type: package`,
named by its Package URL (`pkg:gem/rails`, `pkg:npm/@scope/thing`) so a
declaration written against an advisory database or a bill of materials joins
this graph without a translation table. The props are `ecosystem`,
`package_name`, `constraint` as written, `resolved_version` from the lockfile,
`dev`, `manifest`, and `pinned`.

**`pinned` has three states, and the third is the one that matters.** It is
`true` when a lockfile resolved the package or the constraint names exactly one
version — including Cargo's `= 1.2.3` and pip's `== 1.2.3`. It is `false` when
no lockfile resolved it and the constraint is a range. And it is **absent** when
a lockfile sits beside the manifest that enola cannot read — `pnpm-lock.yaml`,
`bun.lockb` — because something resolved that dependency and enola did not look.
The fact then carries `unresolved_lock` naming the file instead. A `where:`
selector on `pinned` does not select such a package, which is the correct amount
of silence: an earlier version answered `false` anyway and turned a yarn
repository's twelve resolved dependencies into twelve false blocks, which is
precisely how a gate stops being run. Everything else fails closed — a
constraint this vocabulary does not recognise as exact is a range, because
calling an unpinned dependency pinned is the error with a consequence.

**Direct dependencies only.** The transitive closure of a lockfile runs to tens
of thousands of entries on an ordinary Node or Rust project, and every one would
enter a graph whose cost is dominated by node count. It is also the boundary the
regulation draws: the Cyber Resilience Act's Annex I asks for a bill of
materials covering "at the very least the top-level dependencies", whose
essential requirements apply from 11 December 2027. A `// indirect` line in a
`go.mod` and a nested `node_modules/x/node_modules/y` in a lock are both the
closure, and both are skipped.

**A package is a leaf.** Nothing here connects a package to the code that uses
it. Resolving an import path to a declared package is a per-ecosystem resolution
problem, and guessing it would put edges in the graph no parser proved. A
package is joined to code only by the declarations a human writes about it.

**Pinning is a rule you can state, not one enola holds.** The shipped
`supply-chain` recipe is the whole of it, and it needs no rule form of its own:

```yaml
use_recipe:
  - recipe: supply-chain
    as: supply
    bind:
      unpinned-dependencies: {}
```

which expands to a component selecting `kind: dependency` with
`where: {type: package, pinned: false}` and a `forbid_fact` over it. One caveat
worth knowing before you adopt it: `where:` fails closed and loudly on a
property no measured fact carries, so a repository with no manifest enola reads
gets a `1.0` *selector cannot be evaluated* finding rather than silence. That is
the right behaviour — a rule holding because it looked at nothing must never
read as compliance — and it is surprising the first time.

## Where the rest of it lives

This page covers where intent lives, what enola reads, the vocabularies, and how findings
are graded. Two related pages cover the parts that grew beyond this contract:

- **[CONSTRAINTS.md](CONSTRAINTS.md)** — a repository's *law*. Components and the
  selectors that resolve them, the 21 rule forms, modes, exemptions, recipes, laws
  written in Ruby, and the `constraints lint` / `mine` / `explain` and `plan` surfaces.
- **[PROVIDERS.md](PROVIDERS.md)** — measured facts from tools enola does not ship.
  The seam's fail-closed contract, and the Rubydex, runtime and RBS/Sorbet providers.

Constraints reach enola through the same three carriers described above, with one
narrowing: they are file-level only, never on a page.

## Working with intent

1. **Declare only what you know.** A declaration produces
   unexpected-seam findings over everything it measures for that
   repo — declare a repo's seams when you can stand behind the
   complete list, not for completeness's sake.
2. **Put the declaration where the decision lives.** If an ADR
   decides that the mobile app talks to the backend over GraphQL,
   that ADR's page carries the seam — and every future finding cites
   it.
3. **One seam, one deciding page.** The same seam declared on two
   pages is a compile error, not a merge.
4. **Let claims guard your load-bearing numbers.** Any count your
   documentation states as fact can be a `fact-count` claim; from
   then on the number failing is a snapshot finding, not a stale
   sentence.
5. **Derive, don't hand-maintain, what your wiki already knows.**
   If your pages carry structured metadata (kinds, statuses,
   relations, source citations), generate the `enola_intent:` block
   from it with your own tooling and check the derivation in CI —
   enola validates the block; keeping it truthful to your
   conventions is yours. Source citations that name code paths are
   anchors waiting to be derived: the richest page-to-code signal a
   wiki carries is usually already in its citation discipline.
6. **Treat vocabulary gaps as decisions.** If a real seam has no
   via, that is a proposal for this page — not a reason to shoehorn
   it into a wrong one or drop it silently.
