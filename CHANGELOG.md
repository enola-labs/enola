# Changelog

Every released version, newest first: the headline it shipped under and, where one was
written, its summary. Dates are the GitHub release dates.

The full per-release change lists — every Added, Changed and Fixed line — are on
[enola.tech/changelog](https://enola.tech/changelog). This file is the same history at
the resolution a reader of the repository needs.

## v0.4.19 — 2026-09-12

**The Stop hook reports once per session instead of at every stop**

A Stop hook's output prevents the turn from ending: the harness feeds it back to the
model and stops again, setting `stop_hook_active` on that second stop. enola did not
read that flag, and deduplicated only its incomparable-baseline report, so a standing
finding re-emitted the identical report at every stop until the harness overrode the
hook at its consecutive-block cap. The flag is now read before grading, so a suppressed
run costs no snapshot, and the once-per-report rule also covers the regression and
unenforced-finding reports, keyed by the findings themselves and the session id. A new
session, a different finding, or a recurrence after a clean grade is reported again.

## v0.4.18 — 2026-09-10

**install.sh runs under dash and on Windows, and upgrades get their own checksum**

`curl … | sh` failed wherever `sh` is dash, as on Debian and Ubuntu, because dash rejects
`set -o pipefail`; install.sh is now POSIX sh. It also installs the native `enola.exe`
from Git Bash, MSYS2 and Cygwin instead of refusing them, moving a running copy aside so a
re-run can replace it. `enola upgrade` now verifies against a new
`enola-<version>-<os>-<arch>.upgrade.sha256`, the same digest as `.sha256` under a name
only the updater fetches, so release download counts can tell upgrades from fresh
installs. Integrations should keep verifying against `.sha256`. Nothing changed for pip
installs, and this version is not on PyPI.

## v0.4.17 — 2026-09-09

**Refresh loads a newer snapshot, and the dashboard opens on findings**

`enola dashboard` re-reads its snapshot directory on Refresh, rather than only
re-rendering what the process already held. The graph does not move until you ask for it,
so a regeneration in another terminal cannot change the page under an open investigation.
The replacement is published atomically and one request reads store, snapshot and receipt
from a single publication. A repository launch restores that repository's own snapshot; a
multi-repository graph needs an explicit cluster config.

The tabs are now Findings, Architecture, Snapshots, Lifetime usage, Diagnostics and
Quality: processes, ports and paths moved out of the usage tab into Diagnostics. The
module graph condenses each dependency cycle into one node and curves the back edges.
Change summaries read the configured `history.dir`.

Python calls now bind to symbols a package defines in its own `__init__.py`, not only the
ones it re-exports. `from pkg import helper` emitted the target `pkg.helper`, whose prefix
names a directory rather than a module, so absolute resolution failed and the call bound
to nothing.

## v0.4.16 — 2026-09-08

**Constraint checks now report cases they previously skipped**

Three fixes address checks that returned incomplete or misleading results.
`require_defines` now checks both `class` and `struct` symbols; previously, a rule over
Go or another language with struct method owners could select a component without
evaluating its members. `constraints lint` now includes facts compiled from knowledge
pages when resolving components. Its `governed_by` results now agree with the explainer
and constraint check. The intent explainer now emits one notice per policy page when its
anchors name a repository that is not in the snapshot. The anchors remain unchecked,
not dangling, because the referenced repository was not measured. The notice is useful
for clones,
worktrees and CI checkouts whose directory name differs from the repository label in the
policy page.

Yarn lockfile resolution now handles descriptors whose range contains an `@`, including
npm aliases and `git+ssh` ranges. The parser previously split these descriptors at the
last `@`, derived the wrong package name and reported a dependency as unpinned even when
the committed `yarn.lock` pinned it. `cacheVersion` moves to `v265`, so existing
extraction caches are rebuilt and pinned baselines may show a one-time change.

The new `examples/policy-as-code` directory demonstrates a compliance boundary in a
small Go module. Policy pages define which files are in scope, constraints cite the
requirements they implement, exemptions include an owner, reason and date, and
`run.sh` shows the checks before and after three policy violations.

## v0.4.15 — 2026-09-06

**Linux wheels answer to the older tag name as well**

The linux wheels 0.4.14 published were tagged `manylinux_2_17`, which is the PEP 600
spelling, and pip only learned to read it in 20.3. RHEL 8, Rocky 8 and Amazon Linux 2
ship pip 20.2.4, so on exactly the systems the low glibc floor was chosen to reach, the
wheel was invisible and `pip install enola-cli` answered "no matching distribution
found". The linux wheels now carry a compressed tag set naming both the PEP 600 form and
the legacy `manylinux2014` alias, so one file answers to either spelling. Verified
against stock pip 20.2.4 on glibc 2.28.

Nothing else changed. Binaries and extraction are identical to 0.4.14; 0.4.14 itself
keeps the single tag, because an index will not accept a replacement file for a version
it already has.

## v0.4.14 — 2026-09-06

**enola installs with pip, and the binaries reach older systems than they used to**

enola is on PyPI as `enola-cli`, the name `enola` having been taken by someone else. The
command it installs is still `enola`. The wheel carries the same compiled binary the
release tarball does, with no Python launcher in front of it and no Python needed at run
time, so a `pip install enola-cli` costs nothing at startup that `install.sh` does not.
Wheels are published for linux, macOS and Windows on amd64 and arm64.

Because pip records the file it installed, `enola upgrade` refuses on a pip install and
names `pip install -U enola-cli` instead. Replacing the binary there would appear to work
and then be undone by the next pip operation, with the version silently reverting.

The two portability fixes this uncovered apply to `install.sh` and `enola upgrade` as
much as to the wheels, since every route ships the same binary. The linux binaries are
now compiled inside a manylinux_2_28 image rather than on the CI runner, which lowers
what they require at run time from glibc 2.34 to 2.17: RHEL and Rocky 8, Amazon Linux 2,
Ubuntu 20.04 and Debian 11 could not run a 0.4.13 binary and can run this one. The macOS
binaries now declare a deployment target of 12.0, which is Go's own oldest supported
release, rather than inheriting whatever SDK version the build machine happened to have.

## v0.4.13 — 2026-09-04

**Python imports bind to the module they name**

An absolute Python import bound to the enclosing package directory, and one naming the
importer's own package bound to nothing and read as a third-party dependency. Both now
bind to the module, the granularity relative imports always had. Imports that never run
at module-import time (function and class bodies, `if TYPE_CHECKING:`) carry a
`deferred` prop. The new `import-closure` explainer reports what importing a package
executes, including the `__init__.py` files no import statement names, and ranks
barrels by how much each one alone brings in. Informational only.

C++ call resolution binds a bare call when exactly one function in the repository
declares that short name, and resolves `obj.method()` when the receiver has an explicit
type and the method is unique; `auto`, template wrappers and missing methods stay
unresolved. Three TypeScript passes now match inside tree-sitter instead of walking the
tree in Go, cutting allocations with byte-identical output.

`cacheVersion` moves to `v264`. Python and C++ repositories re-extract once; changed
import targets and new call edges can shift findings, so re-pin baselines there.

## v0.4.12 — 2026-09-01

**GraphQL servers join the graph, so client operations have something to match**

enola read GraphQL client operations and graphql-ruby's server field DSL, which left a
schema-first Node server invisible: a TypeScript client's `Query.pageViews` crossed the
repository boundary and found nothing on the other side. Server SDL is now read for Apollo
Server, GraphQL Yoga, Mercurius, express-graphql, graphql-http, GraphQL Tools and
GraphQL.js, along with code-first root fields from NestJS GraphQL, TypeGraphQL, Nexus and
Pothos. Each root field becomes a server-role route in the same name shape the client
emits, which is what the cross-repository `graphql` signal joins on. Activation is a
repository-wide AST signal and package provenance is required, so a documentation example
and a client's codegen copy of its backend's schema both stay inert.

The client half widened to match: Relay's tag joins the `gql` tags, static operation
strings are read in graphql-request modules and plain `fetch` bodies, and tag extraction
now runs inside Vue and Nuxt `<script>` and `<script setup>`. Fixed: a Ruby client literal
holding more than one operation reported only the first. The TypeScript pass reads each
source once instead of rereading every file to build the gRPC stub index. Resolvers are
not read — which root fields exist is in the graph, which function serves one is not.

`cacheVersion` moves to `v259`: every repository re-extracts once, a repository serving
GraphQL gains server-role routes, and baselines on those repositories should be re-pinned.

## v0.4.11 — 2026-08-30

**A Vue component's template is code, and enola now reads it**

Vue and Nuxt extraction stops at the `<script>` block no longer. A Single File
Component's TEMPLATE is where a handler is wired to a button and a child component is
placed, so a component used only from a template read as an orphan and a `@click`
handler read as dead. Interpolations and directive values are now resolved against the
SFC's own declarations, and component tags through default imports, named imports, local
aliases and unambiguous Nuxt `components/` auto-import conventions. The references that
resolve become `calls` edges on the component, and `traverse`, `find_path`,
`impact_analysis` and orphan analysis see them. Resolution is narrow where it cannot be
sure: HTML text, native tags and CSS tokens are not read as code, and a tag two Nuxt
layers could both supply stays unresolved rather than binding to whichever was walked
first.

`<script setup>` compiler macros are recorded as the component's public surface.
`defineProps`, `defineEmits`, `defineSlots`, `defineModel`, `defineExpose` and
`defineOptions` land in `vue_macros` with a boolean prop per macro; the statically
declared prop, emit, slot, model and exposed names land in the matching `vue_*_names`
lists, and generic declaration text is kept in `vue_contract_types`. Macro-looking text
inside a comment or a string is ignored.

Routing covers what the frameworks actually ship. Nuxt file-based routes are emitted for
every supported page extension rather than `.vue` alone, route-group directories are
omitted from the URL, and named-view, `.client` and `.server` suffixes no longer leak
into it. Vue Router `createRouter({ routes })` records with literal paths emit page
routes, nested `children` paths compose onto their parent, and a statically or lazily
imported component becomes a `handled_by` edge; the configuration file keeps its
`router_config` marker fact. A Nuxt auto-imported `useXxx()` call is rebound to its
unique exported declaration under `composables/`, and left unresolved when two layers
export the name.

`cacheVersion` moves to `v258`. Every Vue and Nuxt repository gains facts and existing
ones change shape, so extraction caches from earlier releases are invalidated and the
first snapshot after upgrading re-extracts. Baselines taken on a Vue or Nuxt repository
should be re-pinned: the new template edges can resolve orphan findings and can surface
coupling that was previously invisible.

## v0.4.10 — 2026-08-28

**Snapshot artifacts now have a documented, versioned contract**

The three supported snapshot artifacts — `facts.jsonl`, `insights.json`, and
`receipt.json` — are documented under `docs/schema/`. The reference covers their fields,
fact and relation kinds, contract props, identity rules, and compatibility policy. CI
verifies the documented field names and vocabularies.

`receipt.json` now carries `format_version: 1`. Current writers add an `id` to every fact,
derived from `(repo, kind, name, file)`. Relations carry `target_id` when enola can
resolve the target without ambiguity, preferring candidates in the source fact's
repository. Multiple fact records can share an ID when those four identity fields are
equal.

Insight evidence can carry `fact_id`. Evidence resolution uses `symbol` when present,
otherwise `fact`, and uses `file` to narrow candidates. Unresolved relations and evidence
remain unresolved rather than selecting an arbitrary fact. The MCP and on-disk forms of
an empty insights array are now both `[]`.

The schema documentation also corrects existing assumptions: `repo` is present on
single-repository facts, `declares` points from a declared fact to its containing module,
and `(repo, kind, name)` is not a uniqueness guarantee. It also documents optional route
provenance, test and file reference relations, coupling kinds, cross-repository types,
and messaging props.

`docs/INTEGRATING.md` documents how to pin an enola release, run generation, validate the
receipt, and load the artifacts into another store.

`cacheVersion` remains `v257`, so existing extraction caches remain valid. Snapshot IDs
change after upgrading because fact IDs are part of the serialized facts. Baselines
containing the corrected insight evidence may report content-only changes; no findings
are added or removed.

## v0.4.9 — 2026-08-27

**A dashboard you can start on its own, and a change routed to the people who own what it touched**

`enola dashboard` serves the latest snapshot in the read-only local dashboard without
also starting an MCP server, with `--open` to launch the browser. It regenerates
nothing: a missing snapshot produces the generate-first guidance before any listener is
bound, so no half-started process registers itself. The command stays attached to the
terminal and stops on Ctrl-C — an earlier iteration of this release ran it in the
background with `status` and `stop` subcommands, and that was withdrawn, because a
server whose lifetime is the terminal's is the one nobody has to remember to clean up.
A dashboard-only process registers as an active session like the MCP startup path does,
so Activity does not report zero sessions while serving the very page a reader is
looking at, and it installs no tool callback: this process answers no MCP calls, so its
session count truthfully stays at zero. In an interactive terminal, `--generate`,
`--refresh` and `check --write` each print one line pointing at `dashboard --open` for
the path they just wrote; CI, redirected output, hooks and `ENOLA_NO_PROMPTS=1` suppress
it. Ctrl-C on the MCP server no longer reports the cancellation as a server error and
exits 1.

`enola check --reviewers` reports, per module the change touched, who owns it and
whether the author is a stranger to it while owning something that imports it. That
pairing is the major-minor-dependency from Bird, Nagappan, Murphy, Gall and Devanbu,
[*Don't Touch My Code!*](https://dl.acm.org/doi/10.1145/2025113.2025119) (ESEC/FSE
2011), and spotting it needs the import graph rather than the commit log alone. It is
never graded, is not in the snapshot, and is off by default: without the flag no git
author name is read, computed or printed. The paper's 5% minor-contributor line is a
correlation measured over Windows Vista binaries averaging some 900 commits, not a rule
any repository declared, and a module needs five commits in the window before its shares
are read as evidence of anything — measured on one open-source repository, the top
major-minor-dependency was a module holding its 100% on a single drive-by edit.
`--reviewer-window` sets how many recent commits authorship is measured over (500), and
`--author` names whose change it is when git's identity is not the answer.

Dated rules now grade by git blame rather than by the architecture history, which
annotates them instead. The two answer different questions — whether a finding was
present at the date, versus whether its witness line was last changed before it — and
they disagree whenever an old breach's witness was renamed, moved or reformatted.
Grading with the history made the verdict depend on a per-machine record: the same
commit graded one way on a laptop holding a local history and another in a fresh CI
clone. What decides a verdict has to be reproducible from the checkout. The history's
better answer is still reported, and never subtracted from the failures.

A repository-scoped extraction fact carries the repository's directory name in `File`
rather than a path, and `changedFiles` handed it to callers that read `File` as
something somebody edited. Reviewer routing and guidance then opened with a line about
the root module, measured over the whole repository, on any change that moved an
extractor's coverage counters. It is filtered in `changedFiles` rather than at the
source, since `File` is part of a fact's identity in every snapshot and diff.

A wrapper binary now reports its own version. `pkg/command` read `internal/version`,
which only enola's own release stamps, so a wrapper said "dev" in three places at once:
`doctor`, the SARIF driver it uploads under, and its dashboard instance record.
`cli.Binary` carries the version, and whether a binary can upgrade itself is derived
from the subcommands it dispatches for itself rather than from a new field — `upgrade`
being in that list already is the question. That also gates the update notice, since the
release manifest describes enola releases and measuring another version series against
it would report the distance between two unrelated streams as an upgrade.

Six of the seventeen subcommands — `log`, `show`, `diff`, `blame`, `gc` and `history`,
which is the whole of the architecture-history feature — had been absent from the CLI
reference for as long as they existed, with every other check in the repository passing.
A docslint test now ties the command table in `docs/CLI.md` to what the binary actually
dispatches, in both directions, matching on the table's command column rather than
anywhere in the prose, because a bare `log` or `diff` occurs in ordinary English on
nearly every page. `docs/DASHBOARD.md` and `docs/FIRST-CHANGE.md` are new, clusters and
constraints gained the sections their pages assumed, and the README was cut back.
`govulncheck` moves to v1.7.0: the pin has a floor as well as a ceiling, and v1.1.4
carried a type checker predating this module's `go` directive, refusing all 126 packages
with an error that reads nothing like a vulnerability report — the job stayed red while
reporting on nothing.

`cacheVersion` stays at `v257`. Nothing re-extracts, and a pinned baseline does not
churn.

## v0.4.8 — 2026-08-25

**Declared dependencies become facts, and the gate reports how much of the law is excused**

A `manifests` extractor reads `go.mod`, `package.json`, `Gemfile`, `Cargo.toml`,
`pubspec.yaml`, `requirements.txt` and `pyproject.toml`, emitting one fact per direct
dependency named by its Package URL, carrying the constraint as written, the version a
lockfile resolved it to, and whether it is pinned. Transitive entries are skipped: the
closure runs to tens of thousands of nodes, and top-level is the boundary the Cyber
Resilience Act draws for the products it covers from 11 December 2027.
`enola-intent.yaml` gains a `dependencies:` section with a mandatory `purpose:`, and the
`intent` explainer diffs it against the manifests — a measured package nothing declares
verdicts at `1.0`, a declared package no manifest carries at `0.8`, since a removed
dependency and an extraction miss look the same from here. The rule is H14 from Stanislav
Rumega's position paper [*Tell Your Coding Agent to Work as an Architect
First*](https://github.com/styrumg/Architect-First-Article): every external dependency
declared, pinned, and given a stated purpose.

`pinned` is three-valued. True when a lockfile resolved the package or the constraint
names one version, false when no lockfile resolved a range, and absent — with
`unresolved_lock` naming the file — when a lockfile enola cannot read sits at or above the
manifest. Answering `false` in that third case reported 12 of one TypeScript monorepo's
dependencies as unpinned when its root `yarn.lock` pins every one of them. A `where:`
selector on `pinned` does not select an unanswered package.

Six public repositories corrected the parsers. Cargo's `[dependencies.name]` table form
was skipped entirely, and an async runtime declares its Windows dependency only under
`[target.'cfg(windows)'.dependencies.…]`, so reading the list form alone lost the
dependency rather than its version. Lockfiles are searched for at or above the manifest,
nearest first, because a monorepo keeps one at the workspace root and a manifest in every
package. `yarn.lock` is read in both its formats, and `uv.lock` and `poetry.lock` in the
one they share with `Cargo.lock`. A PEP 621 `dependencies` array is read as quoted spans
rather than split on commas: a comment line interrupting the array became a package, 33 of
them in one Python project, and a single requirement containing commas was split into
fragments. PEP 508 direct references and PEP 503 name normalisation came from the same
corpus. That Python project went from 103 packages with 46 unpinned to 90 with 5; the four
repositories measured against independently parsed manifests now match exactly.

`enola check` prints the declared law's excuse rate as one line beside its census line and
carries a `law` object in `--format json`. `enola constraints ledger` prints the per-rule
table: breaches, the suppressions and exemptions that excused them, each excuse's owner
and age, and the ones that matched nothing in this snapshot. Both read the owner, reason
and date the two carriers already required. It is a report and changes no exit code.
`kind: dependency` is opt-in for components, as `test_ref` and `lint` are, so a rule that
does not name it judges what it judged before, and the shipped `supply-chain` recipe needs
no new rule form.

`cacheVersion` moves to `v257`: every repository re-extracts once, and one whose manifests
enola reads gains facts, so a pinned baseline churns once.

## v0.4.7 — 2026-08-25

**Layers scored per language cohort, and a built-in provider that stops building the facts the configuration throws away**

The layers explainer recognised one architecture per repository and scored every taxonomy
over the whole tree. A taxonomy now declares the languages it may classify and is scored
over that cohort alone; selection runs strongest first and skips a taxonomy whose cohort
another has already claimed, so an ungated one yields rather than competing everywhere. A
repository running two layer orders reports both, with the strongest kept beside them so
the JSON shape does not move. Violation titles carry the taxonomy name, because two
cohorts can produce the same layer pair — that changes finding identity, so a pinned
baseline churns once.

Three classes of false positive are gone. Dependency-injection and configuration packages
are classified but placed in no direction, since every layer both uses them and is used by
them, and matching now prefers an unordered layer, so a wiring directory nested inside an
ordered one no longer inherits the enclosing layer — `core/data/…/data/di` read as data
before. `android-clean` orders data innermost, per Android's own guidance, while
`ios-clean` keeps the opposite order on purpose: the two disagree about which way domain
and data depend, and neither is wrong. The Rails autoload rule stopped claiming `app/`
children Rails does not autoload, which is where front-end applications live. And `core`
left the hexagonal domain patterns, because it names a container rather than a layer: one
platform keeping its product under `src/Core/` had 1,049 of its 1,491 classified modules
read as domain on that segment alone, and 339 findings followed.

`php-layered`, `nuxt` and `sveltekit` are new — the PHP one gated on the language, since
there is no single PHP framework, the other two built from the frameworks' own prescribed
directory structures. Python, Rust, Swift, the routers/services/models Go layout and .NET
were each measured and deliberately left out, with what was measured recorded beside the
taxonomy table: three share no vocabulary that holds across unrelated repositories, one
would re-rank every `services` package on the evidence of a single example, and the .NET
repository that looks layered is the trap — a media server whose `MediaBrowser.Controller`
is its domain abstractions assembly, which dotted-name matching would read as fifty modules
of delivery. Confidence drops the coverage term whose denominator was the layer table
rather than the repository, and its floor applies to the pattern that won rather than at
admission, where it had dropped a thin framework-gated match and handed the repository to
an ungated hexagonal one.

Explainers stated their numbers in prose and every caller parsed them back out of the
title. An insight now carries `Metrics` beside its description, shaped like a fact's props;
layers publishes its denominators, conformance counts and layer order for recognised and
declared patterns alike, which is what lets the digest serve a repository that states its
own architecture rather than one recognised for it. Metrics mirrors rather than replaces:
the numbers stay in the prose the diff gate reads.

That digest is bounded now. Its repository map was one unbounded row per module, so a large
repository spent all 64,000 characters on an alphabetical census and the architecture
section was never reached. Above a row cap the map summarises by area, entry points group
by kind, routes by path prefix, storage by kind and dependency edges by out-degree, and no
one section may take more than its share of the budget — a section over its share is
truncated, not dropped. A large monolith's digest went from 64,000 characters holding two
sections to 28,000 holding ten, with no truncation left to do.

The provider seam drops facts about files the repository's ignore globs exclude, because
a provider walks the tree itself and cannot know what the configuration leaves out. That
holds for an external provider, a separate process handed a repository path. A built-in
runs in the engine's own address space, and its input already carries the predicate.

Rubydex is a built-in, and on a Rails monolith it built 2,155,664 facts so the seam
could discard 1,520,163 of them, each one named, located, given a relation, appended to
a slice growing into the millions and sorted with the rest. Collection now takes the
predicate: an excluded document is skipped before its definitions are read, and an
excluded reference before its fact is built. The census names both counts, so what the
exclusions cost stays visible rather than disappearing quietly. Same configuration, same
globs, 440 seconds before and 193 after, with 1,800,829 facts byte-for-byte identical.
Indexing is untouched — vendored gems and engine directories are still handed to the
indexer, because that is what lets a workspace constant resolve to a declaration outside
it. Indexing is how the graph resolves; emission is what enters it.

The provider work is [Muhamed Isabegović](https://github.com/misabegovic)'s.

## v0.4.6 — 2026-08-24

**A constant reference is never its own predecessor**

Fixed: a Ruby prefix walk that could not terminate. The walk steps back over the
segments written before a leaf on its line, so that `Foo::VERSION` reports `VERSION` as
the dependency and `Foo` as the path it was named through, and it found the previous
segment by comparing columns alone. A reference spanning several lines carries an end
column belonging to its last line, and when that number plus the separator happened to
equal its own start column, the reference was returned as its own predecessor and the
walk went round forever. One reference does this on a Rails monolith; the provider was
killed at 26 minutes holding 6.7GB with nothing written, on a tree the engine indexes in
24 seconds. A second Rails application hung the same way. A candidate now qualifies only
when it is a different reference ending on the line the walk is on, which is what the
adjacency pass twenty lines earlier already asserts.

Two things ride along: the walk appends and reverses once instead of allocating a new
slice at every step, and a declaration's name is fetched across the library boundary
once rather than per reference — that monolith resolves 1,624,360 references to 132,603
declarations. Facts are byte-for-byte identical where both builds complete.

This release is the work of [Muhamed Isabegović](https://github.com/misabegovic).

## v0.4.5 — 2026-08-23

**The verdict where CI reads it, provider facts cached, and detection that stops re-walking the tree**

Most of this release is the work of
[Muhamed Isabegović](https://github.com/misabegovic).

`enola check` has four writers over one verdict: `--format` picks text, json, sarif or
annotations, and nothing is recomputed for a writer. SARIF carries one rule per declared
rule id with the team's `because:` as its description, the evidence span as its region,
and a stable identity under `partialFingerprints`. Annotations place every positioned
finding on its file and line, as GitHub workflow commands or Buildkite markdown grouped
by file; findings with no position are counted at the end rather than placed somewhere
plausible. The host is a flag and never read from the environment, so a laptop renders
what CI rendered.

Every verdict now also says what it could not see. One `could not see:` line and a
census report what each provider contributed and where two of them overlapped, joined
per site. Two providers that spell the same receiver differently produce one edge rather
than two, with the surviving relation stamped `resolution_agreement`; receivers that
genuinely differ are both kept and counted by shape. `since` and `growth` read git blame
per witness file at author time, so a rebase does not restart the clock, and a breach
that cannot be dated names which of three things was unknown instead of defaulting to
now.

**Provider facts are cached.** A per-file provider keeps one entry per file keyed by
content; the built-in Rubydex provider keeps one whole-index entry keyed by its library
version, the Ruby file set and the lockfile. The receipt says how much was reused, so a
warm run that reuses nothing is visible rather than merely slow. On a Rails monolith of
roughly 1.7 million facts, providers were 249 seconds of a 292 second run before this.

**Language detection no longer re-walks the tree.** Every extractor used to answer
`Detect` with its own bounded walk, and every bound was a cliff a real repository falls
off — dotnet/runtime keeps all 3,270 of its C/C++ sources below the three levels the C++
detector scanned, so the extractor never ran at all. Detection is now membership over
the file list the engine already walked. Across a 20-repository polyglot corpus it
recovers 13,496 of the 13,828 files an extractor claimed and no extractor read.

`constraints explain` answers incoming edges as well as outgoing ones, and reports what
moving a file out of its part would cost: the verdicts that would appear and the ones
that would vanish. Rubydex emits one dependency per qualified read, carrying the file
that defines it, and every refusal carries a cause from a closed set.

Fixed: `constraints init` and `constraints explain` returned on success instead of
exiting, so the argument loop re-read `constraints` as an unknown command and exited `1`
after printing a correct report. Their exit codes are now documented beside `lint`'s.

Documentation: a [Rails workflow page](docs/RAILS.md), the declaration standard split
into [INTENT](docs/INTENT.md), [CONSTRAINTS](docs/CONSTRAINTS.md) and
[PROVIDERS](docs/PROVIDERS.md), an [index of the docs tree](docs/README.md), and this
changelog.

## v0.4.4 — 2026-08-23

**Laws written in Ruby, Angular, and the rule forms only a graph can state**

Constraints can be written as Ruby sentences, parsed and never executed, compiling to
the same declaration YAML produces. The rule vocabulary reaches 21 forms, including the
ones that need the whole graph — one owner per table, storage that stays home, a route
that must have a consumer, a file that must be governed by a page — plus `since` and
`growth` as a time dimension, and the smallest cut named beside every breach. Angular is
read the way the framework works: decorator-declared classes, dependency injection,
composed routes, templates, and requests through an injected `HttpClient`. Convention
recipes ship with the binary, so a repository binds `rails-conventions` in one command.
Rubydex joins Prism as a built-in Ruby fact provider.

## v0.4.3 — 2026-08-20

**Rails route parity, fewer query-loop false positives, and utoipa routes**

Rails route derivation matched Rails across a 3,500-route application. This release also adds dead-method detection and utoipa routes, improves query-loop analysis, and cuts a 22-repository cluster rerun from 891s to 224s.

## v0.4.2 — 2026-08-18

**Fact paths fixed on Windows; docs checked against the code**

A declared layer order could silently match zero modules on Windows. Every fact path is now forced to forward slashes regardless of host, and a new CI check catches documentation that has drifted from what the code actually does.

## v0.4.1 — 2026-08-18

**AsyncAPI channels and Kafka call sites, cross-service**

Messaging contracts declared in AsyncAPI now join the graph the same way HTTP routes do, so a missing consumer or an undeclared producer shows up as a finding instead of staying invisible.

## v0.4.0 — 2026-08-17

**Declared architectural law**

Architecture rules you write down are now verdicted against the measured graph, checked before an edit lands, and gateable in CI. Tools Enola does not ship can contribute facts through one seam, and the recorded history moves between machines.

## v0.3.21 — 2026-08-15

**Nothing fails until you name it**

The check gate no longer ships an opinion about which findings should break a build, and repository labels come from the Git remote so a second clone compares against its own baseline.

## v0.3.20 — 2026-08-14

**Express sub-router mounts compose across files**

A router declared in one file and mounted in another now reports its routes at the path they actually serve, which is the layout most Express services use.

## v0.3.19 — 2026-08-14

**Lower peak memory, and a verdict that says why nothing failed**

Cold-run peak heap drops by a quarter to two fifths on large repositories, and advisory findings now name the reason they did not fail the build.

## v0.3.18 — 2026-08-13

**Full Rails route tables, plus Grape**

Rails routes are now read from every routes file, not just the top-level one, and Grape APIs are detected automatically.

## v0.3.17 — 2026-08-12

**Five grammar updates and a protocol bump**

Java, Ruby, C, C++, and PHP move to newer tree-sitter grammars, the bundled MCP SDK updates to the latest protocol, and CI gets hermetic tests and pinned tool versions.

## v0.3.16 — 2026-08-11

**Vendored OpenAPI specs no longer read as your own server routes**

A mobile app vendoring the OpenAPI spec of a backend it calls was indexed as if it served those routes itself, manufacturing inbound edges and unused-route findings that don't exist.

## v0.3.15 — 2026-08-09

**The update-available notice actually gets written now**

The check for a newer enola release only ever ran from two entry points, so most installs never got the cache file it depends on. It now refreshes from any command.

## v0.3.14 — 2026-08-09

**Uninstall leaves nothing behind**

enola uninstall now removes the empty files and directories it leaves behind, instead of stripping its own configuration out and abandoning the shell.

## v0.3.13 — 2026-08-08

**Dart and Flutter extraction, gin routes, and update notices**

Enola now extracts Dart and Flutter apps — widgets, navigation, storage, and outbound calls — plus gin routes in Go, and tells you when a newer enola is available.

## v0.3.12 — 2026-08-08

**Scala extraction and native Codex hooks**

Enola now extracts Scala — call graph, routes, storage, and complexity metrics tuned for its effect-typed idioms — and enola install --hooks wires real session hooks for Codex.

## v0.3.11 — 2026-08-07

**Understand the architecture of an entire .NET solution**

Enola now reads every language in a .NET solution, not just C#, and understands how the pieces are wired together — project references, EF Core storage, outbound HTTP calls, and dependency injection.

## v0.3.10 — 2026-08-07

**C# extraction**

Enola now extracts C#: types and members, dependency injection, and both of ASP.NET Core's routing mechanisms.

## v0.3.9 — 2026-08-06

**Declared architectural intent, five new extraction kinds, and a lower memory footprint**

Repos and knowledge pages can now declare what the architecture is supposed to be, graded against every snapshot. Coverage extends to five new kinds, Ember support is complete, and large graphs use substantially less memory.

## v0.3.8 — 2026-08-04

**Ember and Glimmer extraction**

Enola now resolves routes, templates, services, and data-model relationships in Ember applications.

## v0.3.7 — 2026-08-04

**Incomplete binary releases are no longer published**

A release remains a GitHub draft until binaries and checksums exist for every supported platform.

## v0.3.6 — 2026-08-03

**Architecture history — enola log, show, diff, blame, and backfill**

## v0.3.5 — 2026-08-03

**Cross-repo linking becomes a plugin architecture**

## v0.3.4 — 2026-08-02

**Scope conformance, pluggable measurements & a shared command surface**

## v0.3.3 — 2026-08-01

**Python inheritance edges, and explainer findings that attribute to the right fact**

## v0.3.2 — 2026-07-31

**Config resolution, agent-hook reliability, and reproducibility fixes across explainers and extractors**

## v0.3.1 — 2026-07-30

**See which cross-repo edges resolved, from the CLI**

## v0.3.0 — 2026-07-30

**enola check: grade architectural regressions from the CLI or CI, wired into agent sessions**

## v0.2.9 — 2026-07-30

**Traversal cap fixed to bound output, not the walk, and staleness checks scoped to the loaded graph**

## v0.2.8 — 2026-07-29

**TypeScript server routes, fixed client-call detection, and multi-repo clusters outside MCP sessions**

## v0.2.7 — 2026-07-27

**Query credit scaled by the corpus searched, and seeded across restarts**

## v0.2.6 — 2026-07-27

**Value model re-anchored on measured corpus instead of flat call weights**

## v0.2.5 — 2026-07-26

**Python test-file isolation, package re-export resolution & generated-code dead-code signals**

## v0.2.4 — 2026-07-26

**Python FastAPI route composition, safer install script & cross-repo single-segment route linking**

## v0.2.3 — 2026-07-23

**Running-server instance registry fixes cross-process dashboard and restart state bugs**

## v0.2.2 — 2026-07-22

**Kotlin-on-Maven detection, programmatic servlet routes & Kafka topic cross-repo linking**

## v0.2.1 — 2026-07-21

**Cross-repo shared-symbol linking overhaul: verified matches replace name-only guesses**

## v0.2.0 — 2026-07-21

**CLI gains --help, --list & --status, plus a read-only live dashboard**

## v0.1.40 — 2026-07-20

**Cross-repo linker stops false-linking on Rails boilerplate, Swift XcodeGen app targets sub-divided, graph restore-on-restart**

## v0.1.39 — 2026-07-20

**TypeScript HTTP-client precision fixes & Axum nested-router prefix composition**

## v0.1.38 — 2026-07-19

**Go route-prefix composition, struct-usage edges & Rails route-extraction fixes**

## v0.1.37 — 2026-07-18

**I/O metadata for Rust, PHP & C++, module-graph fixes for nested layouts, SvelteKit route entry points & concurrency-safe snapshots**

## v0.1.36 — 2026-07-17

**Fixed dead-code false positives from macro-defined C/C++ functions, Python closures & guarded imports**

## v0.1.35 — 2026-07-15

**Rust language extractor & Java SPI dead-code fix**

## v0.1.34 — 2026-07-13

**Test-path gating for architecture explainers, findings-diff dedup & TypeScript ORM storage**

## v0.1.33 — 2026-07-12

**Loop-depth parity for Java/C/C++/PHP, Swift override & #if fixes, Flask route detection**

## v0.1.32 — 2026-07-10

**Test-ref parity for Go & TypeScript, Python shadow-guard fix & LLM-context repair**

## v0.1.31 — 2026-07-09

**Cross-repo linking, diff-pairing determinism & skip-accounting fixes**

## v0.1.30 — 2026-07-09

**Python value-ref resolution, relation-kind breakdown & Ruby test-file scoping**

## v0.1.29 — 2026-07-08

**Cross-repo coverage triage, Swift/Rails route fixes & Rails explainer tuning**

## v0.1.28 — 2026-07-08

**Graph-wide receipt for multi-repo snapshots**

## v0.1.27 — 2026-07-08

**Self-update via enola upgrade**

## v0.1.26 — 2026-07-08

**Python gRPC support & call-resolution fixes**

## v0.1.25 — 2026-07-07

**Explainer correctness & determinism fixes**

## v0.1.24 — 2026-07-07

**Python dead-code false-positive fixes & complexity signal improvements**

## v0.1.23 — 2026-07-05

**Gemfile-less Ruby repo detection**

## v0.1.22 — 2026-07-05

**gRPC support & expanded cacheVersion test coverage**

## v0.1.21 — 2026-07-05

**CLI version flag**

## v0.1.20 — 2026-07-04

**Snapshot receipts**

## v0.1.19 — 2026-07-04

**Expanded cross-repo HTTP-client detection**

## v0.1.18 — 2026-07-03

**TypeScript & Kotlin extractor accuracy improvements**

## v0.1.17 — 2026-07-03

**TypeScript monorepo aliases, Swift accuracy & Ruby on Rails graph**

## v0.1.16 — 2026-07-01

**C/C++ macro parsing & Ruby dynamic dispatch**

## v0.1.15 — 2026-06-29

**Diff tool, C support & extractor improvements**

## v0.1.14 — 2026-06-28

**PHP support**

## v0.1.13 — 2026-06-28

**Svelte support, shared MCP result helpers & snapshot refresh fix**

## v0.1.12 — 2026-06-27

**JS/JSX support, query_insights tool & doc fixes**

## v0.1.11 — 2026-06-26

**Unused route detection in cross-repo environments & extended testing**

## v0.1.10 — 2026-06-25

**Coverage insights, HTTP client extraction & extended testing**

## v0.1.9 — 2026-06-24

**Extraction pipeline performance pass**

## v0.1.8 — 2026-06-23

**Security patch — CVE-2026-27896**

## v0.1.7 — 2026-06-23

**Architectural smell detection**

## v0.1.6 — 2026-06-21

**Vue & Nuxt support, per-function complexity metadata & Python polyglot detection**

## v0.1.5 — 2026-06-19

**Adaptive TypeScript project detection & Python symbol indexing**

## v0.1.4 — 2026-06-18

**--explain flag & multi-language fixes**

## v0.1.3 — 2026-06-16

**C++, Java & Ruby tree-sitter rewrite**

## v0.1.2 — 2026-06-16

**Python tree-sitter rewrite**

## v0.1.1 — 2026-06-15

**TypeScript improvements, token economy & release tooling hardening**

## v0.1.0 — 2026-06-12

**First official release — Swift AST parser, HTTP client extraction & install script**

## v0.0.8

**Cross-repo resolution fixes & org migration**

## v0.0.7

**Graph of graphs & first-response latency**

## v0.0.6

**Rebrand to enola**

## v0.0.5

**Kotlin DI detection**

## v0.0.4

**OpenAPI integration**

## v0.0.3

**Ruby extractor & memory improvements**

## v0.0.2

**In-memory graph, query performance & Rails support**

## v0.0.1

**First public release**
