# Gating a change

enola compares the graph before a change with the graph after it and reports what the change did: findings it introduced or resolved, dependencies and calls it added, symbols it added or removed. Everything that was already there stays out of the report. You choose which of those findings may fail a build; out of the box, none do.

This page is the whole contract: what a verdict contains, where the check runs, and exactly what can fail it. For every flag and exit code see [CLI.md](CLI.md); to walk the loop once on a module small enough to read, follow [FIRST-CHANGE.md](FIRST-CHANGE.md).

## One change, one verdict

A helper added to `storage` imports the delivery layer. The code builds and every test passes. The dependency now points against the layer order the repository declared:

```
FAIL — 1 structural regression introduced.

Regressions (fail):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify

Policy: fail on new findings from [layers] at confidence >= 1.00.
```

Exit code `1` lets the agent fix the regression before it reports done, or lets a commit hook or CI stop it. Enola shipped with no opinion about your layers; it graded this crossing because the repository declared the order:

```yaml
# enola-intent.yaml
layers:                          # outermost first
  - {name: delivery, paths: ["web/**", "notify/**"]}
  - {name: api,      paths: ["api/**"]}
  - {name: storage,  paths: ["storage/**"]}
```

[`examples/layers-gate/`](../examples/layers-gate/) is the complete five-package example in one command. [What fails the build](#what-fails-the-build) explains every policy; [the full verdict](#what-the-verdict-tells-you) names the symbols and edges the change added.

## What the verdict tells you

A verdict you can't act on is just a red light. Here is the `storage`/`notify` run from the top of this page in full - verbatim output, nothing trimmed:

```
FAIL — 1 structural regression introduced.

Regressions (fail):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify

Policy: fail on new findings from [layers] at confidence >= 1.00.

What changed
  symbols      +1
  dependencies +1
  edges        +4  (imports +1, calls +2, declares +1)

Added (2):
  symbol     storage.LoadPrice                            storage/storage.go:11
  dependency storage -> layersgate/notify                 storage/storage.go:3

New coupling (4):
  storage                                      --imports--> notify
  storage.LoadPrice                            --calls--> notify.SendReceipt
  storage.LoadPrice                            --calls--> storage.ReadPrice
  storage.LoadPrice                            --declares--> storage

New coupling is reported, not failed: an added call edge is what ordinary work
looks like. Inspect the list above if it is more than you expected.
```

Every line is the change, and nothing else. Five packages here, for readability - but on a 68,000-fact repository already carrying 268 findings it behaves identically, reporting the one thing this change introduced and none of the other 268. Long lists are capped at twelve entries with a `… N more` line; `--detail` prints all of them, `--json` emits the whole delta.

Note what the compiler had to say about that import: nothing. `go build ./...` is perfectly happy, `go vet` is silent, and no test fails - the file that introduces it looks entirely reasonable on its own. The defect is the edge, not the line, and the only thing it contradicts is an order you wrote down somewhere else.

Run the same change without naming a policy and you get the other half of the contract - the finding, and an explicit statement that nothing was enforced:

```
PASS — 1 new finding reported, nothing enforced: no policy set.

New findings (reported — no failure policy set):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify

No --fail-on policy is set, so nothing in this run could fail the build. These are
reported for you to judge. Enforce the ones you want enforced: --fail-on=layers
(`enola check --help` lists all 22).
```

A gate that enforces nothing has to *say* it enforces nothing. Silence there is indistinguishable from an all-clear, and that is the one failure mode a green exit code cannot report on its own.

## The loop

**Before a change**, your agent has the real structure of the codebase: a deterministic graph of modules, symbols, routes and storage, and how they depend on each other, extracted from source rather than inferred. It can look up what actually depends on the thing it's about to touch, instead of guessing from a grep.

**After a change**, enola grades what happened. It compares against the pinned graph and reports the delta: findings introduced or resolved, coupling added, symbols added and removed. It shows you everything the change actually did, and stays silent about everything that was already there.

It runs in three places, each usable on its own:

| | |
|---|---|
| **In your agent** | a hook grades each session and hands the verdict back, so the agent reports what it moved - or fixes its own regression - before telling you it's done |
| **In your shell** | `enola check` - reports the delta, and exits `1` on whatever `--fail-on` names |
| **In CI** | the same command, same exit code, on every pull request - or [`enola-action`](https://github.com/enola-labs/enola-action), which wires it to the pull-request base for you |

Add the CI gate from **[Enola Architecture Check on the GitHub Actions Marketplace](https://github.com/marketplace/actions/enola-architecture-check)**,
or start from the [complete workflow example](../examples/ci/architecture-gate.yml).

The whole loop, unedited - the change is reported and nothing fails, the same run under a stated policy fails on the layer it crossed, and the fix lets it through:

![enola check on the layers-gate example: a helper added to storage imports the delivery layer, the default run reports it and exits 0, --fail-on=layers fails the same change, and after the fix the re-run passes](images/layers-gate.gif)

<sub>Three runs of one command on the same change. `&& echo` never fires while the gate is red - and in the first run there is no gate to be red, which the output says out loud. Recorded from [`examples/layers-gate/`](../examples/layers-gate/) with [`docs/images/layers-gate.tape`](images/layers-gate.tape).</sub>

## What fails the build

Two separate things decide that, and confusing them is the fastest way to be surprised by this tool: **what enola finds**, and **what your policy fails on**. enola runs all twenty-two of its checks - it calls them **explainers** - on every single run. The policy picks which of their findings are allowed to set the exit code.

**Out of the box that policy is empty.** Every finding is reported, the run exits `0`, and the output says in as many words that nothing was enforced. Nothing breaks until you name what should break:

- code reaching across a layer order you declared, like storage talking straight to the delivery layer (`layers`)
- two modules that ended up depending on each other (`cycles`)
- a cross-repo seam nobody declared, or a declared one the graph never measured (`intent`)
- a single function or type that a large part of the codebase depends on (`god-class`)
- a function that nearly everything calls (`hotspots`)
- an import chain ten modules deep (`dependency-depth`)
- a function far more complicated than the rest of your code (`complexity-outliers`)
- a package that exports almost everything it contains, instead of a small surface (`exported-surface`)
- API routes that nothing in the code you loaded ever calls (`unused-routes`)
- outbound calls enola could not match to any route it loaded (`coverage`)
- messaging call sites without an AsyncAPI contract, and contract operations without detected code (`messaging-coverage`)
- which repositories in a cluster ended up depending on which (`crossrepo`)
- directories that look like vendored third-party code, reported so you can decide whether to ignore them (`vendored-candidates` — informational, so it can never fail a build)
- what `import yourpackage` executes in Python, and the package `__init__.py` files responsible for most of it (`import-closure` — the summary is informational; the barrels it names are gateable)

**enola holds itself to this.** This repository declares its own layer order in [`enola-intent.yaml`](../enola-intent.yaml) - six layers, entrypoint down to the fact model - and its CI runs `enola check --fail-on=layers` against it. Not `--fail-on=cycles`: enola is written in Go, where the compiler already refuses an import cycle between packages, so gating on one would enforce a rule the toolchain enforces first. The layer order is the part the compiler cannot see. Nothing but that file stops `internal/upgrade` importing `pkg/cli` today, and the build is green either way until something says otherwise.

**Why nothing is on by default.** enola used to fail on a new dependency cycle out of the box. A cycle is exactly measurable - Tarjan's SCC algorithm, no estimate anywhere - and that made it a tempting default. But *exactly measurable* is not the same as *unwanted*: Go's compiler forbids import cycles between packages outright, so a Go team's answer to the finding is usually "the compiler already has this covered"; a Rails app wires most of its graph at runtime, and two `app/` directories referencing each other is not something that community reads as a defect at all. A tool that arrives asserting otherwise spends its first impression being argued with, and the first thing those teams learn about it is which flag turns it off.

So enola states what it measured and stops there. The exception it makes for itself is the one above: an unenforced run must say it enforced nothing, because a silent green is exactly what a broken gate looks like.

**Any of the twenty-two can fail the build.** `--fail-on` takes the names above as a comma-separated list, and `--min-confidence` sets the floor within them. Two more things can fail it that are not findings at all:

- **scope spillover** - packages your change reached outside the area you declared with `--target`, gated with `--max-spillover=N`. A change can trip this with zero failing findings.
- **a gate that could not run.** A missing baseline or a bad flag exits `2`; a baseline that isn't comparable to the current code exits `3` and enola declines to grade rather than blaming your change. Neither is a judgement about the code, and neither is suppressed by `--warn-only`.

**Which of them can actually fail at the default floor: four.** `cycles`, `intent`, `constraints`, and `layers` when the order is declared in `enola-intent.yaml` are the ones enola computes with certainty, so only they reach `1.00`. Everything else is an estimate measured against your own repository - "this file has unusually many dependents *for this codebase*" - and caps below `1.00` by design ([`MaxHeuristicConfidence`](../internal/explainers/common/common.go) is `0.95`). Naming an inferred explainer in `--fail-on` and nothing else therefore changes nothing at all; it needs `--min-confidence` too.

### Changing what counts

| You want | Run |
|---|---|
| The default: report everything, fail nothing | `enola check` |
| Fail on violations of a layer order you declared | `enola check --fail-on=layers` |
| Also fail on a cross-repo seam nobody declared, a declared rule breached, and new cycles | `enola check --fail-on=layers,intent,cycles,constraints` |
| Everything above, plus every explainer enola infers rather than proves | `enola check --fail-on=layers,intent,cycles,constraints,crossrepo,coverage,unused-routes,messaging-coverage,god-class,hotspots,dependency-depth,exported-surface,complexity-outliers,domain,query-loops,entry-points,dead-methods,package-metrics,dead-code,performance,import-closure --min-confidence=0.8` |
| Fail if the change spread outside the area you named | `enola check --target=internal/auth --max-spillover=0` |
| Enforce a policy you set, but only warn this time | `enola check --fail-on=layers --warn-only` |

That fifth row is a different question from the others. `--target` is you saying *"this change is about `internal/auth`"*; enola works out which packages depend on it, then reports any package your change touched that isn't in that group - something you edited that your own description didn't cover. Two snapshots can tell you what changed; only you can say what you meant to change.

<details>
<summary><b>Four things that will bite you</b></summary>

- **A run with no `--fail-on` cannot fail.** That is the default, and it is deliberate - but it means a CI job that pins a baseline, runs `enola check` and reports green has enforced nothing. The output says so in a line; a job that only reads the exit code will not see it.
- **`--min-confidence` lowers the bar; it doesn't raise it.** The default floor is `1.00`, which is already the strictest setting there is. `--min-confidence=0.8` makes the gate fail on *more*, not less.
- **Confidence is per finding, not per explainer.** `layers` is the one that catches people: violations of a layer order you *declared* score `1.00`, violations of a pattern it *recognised* score `0.80`. So on a repo with no declared layer order, `--fail-on=layers` changes nothing until you also pass `--min-confidence=0.8`.
- **A misspelled name is not an error.** It just never matches anything, so the gate goes quiet instead of complaining. `enola check --json` prints the policy that actually ran - compare it against what you typed.

The policy lives in flags, not in `mcp-arch.yaml`, so a pre-commit hook and a CI job can deliberately hold you to different standards.

</details>

### A failure with no failing finding

Scope is graded separately from findings, so a change can break the build having violated nothing at all. This run names no `--fail-on` whatsoever - the layer violation is reported and explicitly not enforced - and it still exits `1`, because the change touched a package its author never said it was about:

```
## Scope

**Reached beyond the declared scope.** 1 of 2 package(s) touched were predicted or declared, match ratio 0.5.

Spillover — touched but neither predicted nor declared:
  - telemetry

A package here was changed by something the declaration did not describe.
That is worth reading even when every finding is clean.

Predicted but not touched (usually fine — the change was narrower than its blast radius):
  - api
  - web
FAIL — 1 structural regression introduced.

Measurements over threshold:
  - [fail] 1 package(s) reached outside the declared scope

New findings (reported — no failure policy set):
  - [layers] 1.00 — Layer violation: storage -> delivery
      import of notify

No --fail-on policy is set, so no FINDING could fail this run — only the threshold
above grades it. These are reported for you to judge; enforce the ones you want
enforced: --fail-on=layers (`enola check --help` lists all 22).
```

The `--target` you declare is a claim about intent, and this is the gate holding you to it. Nothing here is a judgement about `telemetry` - the code may be perfectly good. It is a report that the change did something its own description didn't cover.
