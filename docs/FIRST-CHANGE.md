# Your first graded change

*The loop, end to end, on a repository small enough to read in one screen.*

[RAILS.md](RAILS.md) walks this same loop through a Rails application. This guide uses a
plain module because the loop does not depend on a framework. It starts in Go, then
shows the same declared violation in TypeScript.

Everything below is runnable. The module is
[`examples/layers-gate/`](../examples/layers-gate/) in this repository, and
[`examples/layers-gate/run.sh`](../examples/layers-gate/run.sh) performs the whole
walkthrough in a throwaway copy.

---

## The module

Five packages. Four are assigned to three layers; `telemetry` sits outside the
declaration:

```
web/         delivery — the outermost layer
notify/      delivery
api/         may depend on storage
storage/     the innermost layer; depends on nothing above it
telemetry/   (nobody has said anything about this one yet)
```

That rule is written down, in `enola-intent.yaml` at the repository root:

```yaml
layers:
  - {name: delivery, paths: ["web/**", "notify/**"]}
  - {name: api,      paths: ["api/**"]}
  - {name: storage,  paths: ["storage/**"]}
```

Outermost first. Each layer may depend on the ones below it and on its own peers;
depending upwards is the violation.

Declaring the order turns an inferred pattern into a rule enola can enforce at the
default confidence floor. An inferred order is reported at no more than `0.80`; a
declared one is reported at `1.00`, the default floor for `enola check`.

---

## 1. Freeze the "before"

```
$ enola baseline pin .
enola baseline: regenerating, layers-gate holds no snapshot
Baseline pinned for /path/to/layers-gate
  Generated: 2026-08-25T19:29:19Z
  Facts:     24 · Insights: 1
  Snapshot:  sha256:a80fcac38b9574d29eac616ffcfd4c14415be5221b47a414ad0d87b4f85f6fc2

Now make your changes, then run:
    enola check /path/to/layers-gate
```

There was no snapshot yet, so `pin` took one. It would have done the same had the tree
moved since the last one — the "before" is always the tree you were looking at when you
typed the command. The full behaviour, including clusters and `--baseline=previous`, is
in [CLI.md → The gate](CLI.md#the-gate---enola-check).

## 2. Make a change

Add a reasonable-looking change that crosses the layer boundary:

```go
// LoadPrice emails the buyer a receipt — from inside the storage layer.
func LoadPrice(item, buyer string) int {
	price := ReadPrice(item)
	notify.SendReceipt(buyer, item)
	return price
}
```

The code compiles and behaves as intended, but it makes the innermost layer depend on
the outermost one. That relationship is easy to miss when reading the diff alone.

## 3. Grade it

```
$ enola check .
PASS — 1 new finding reported, nothing enforced: no policy set.

New findings (reported — no failure policy set):
  - [layers] 1.00 — Layer violation: storage -> delivery (declared)
      import of notify

No --fail-on policy is set, so nothing in this run could fail the build. These are
reported for you to judge. Enforce the ones you want enforced: --fail-on=layers
(`enola check --help` lists all 22).

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
```

Exit `0`. A bare `enola check` cannot fail. The output states that no policy was set, so
the report cannot be mistaken for an enforced gate.

Read the two halves separately. The **finding** is the judgement: one declared layer
order, crossed, at full confidence, naming the import that did it. The **delta** below it
is not a judgement at all — an added call edge is what ordinary work looks like. It is
there so you can notice when a change coupled more than you expected it to.

## 4. Enforce what you meant to enforce

```
$ enola check --fail-on=layers .
FAIL — 1 structural regression introduced.

Regressions (fail):
  - [layers] 1.00 — Layer violation: storage -> delivery (declared)
      import of notify

Policy: fail on new findings from [layers] at confidence >= 1.00.
```

Exit `1`. Same change and same finding; only the policy changed. enola reports everything
it found and fails only what you asked it to fail.

## 5. Hold the change to the scope you claimed

The flags above grade the delta. `--target` grades it against your *intent*: enola runs
reverse-dependency impact analysis on the pre-change graph, and any package the change
reached outside that predicted radius is **spillover**.

Add a second edit, in `telemetry/` — a package nothing in the description mentioned:

```
$ enola check --fail-on=layers --target=storage --max-spillover=0 .

## Scope

**Reached beyond the declared scope.** 1 of 2 package(s) touched were predicted or declared, match ratio 0.5.

Spillover — touched but neither predicted nor declared:
  - telemetry

A package here was changed by something the declaration did not describe.
That is worth reading even when every finding is clean.

Predicted but not touched (usually fine — the change was narrower than its blast radius):
  - api
  - web

FAIL — 2 structural regressions introduced.

Measurements over threshold:
  - [fail] 1 package(s) reached outside the declared scope
```

Spillover is graded separately from findings. It says the edit exceeded its declared
target, not that the architecture itself is invalid.

---

## The same loop, in TypeScript

Nothing above is Go-specific. Here is the same rule on a small TypeScript package: three
layers, one declaration and the same violation.

```yaml
# enola-intent.yaml
layers:
  - {name: delivery, paths: ["src/notify/**"]}
  - {name: api,      paths: ["src/api/**"]}
  - {name: storage,  paths: ["src/storage/**"]}
```

```ts
// src/storage/storage.ts — the same mistake, in another language
import { sendReceipt } from "../notify/notify.js";

export function loadPrice(item: string, buyer: string): number {
  const price = readPrice(item);
  sendReceipt(buyer, item);
  return price;
}
```

```
$ enola check --fail-on=layers .
FAIL — 1 structural regression introduced.

Regressions (fail):
  - [layers] 1.00 — Layer violation: storage -> delivery (declared)
      import of src/notify

Policy: fail on new findings from [layers] at confidence >= 1.00.

What changed
  symbols      +1
  dependencies +2
  file_refs    +1
  edges        +9  (imports +2, calls +6, declares +1)

Added (4):
  symbol     src/storage.loadPrice                        src/storage/storage.ts:7
  dependency module-edge: src/storage -> src/notify       src/storage
  dependency src/storage -> src/notify/notify.js          src/storage/storage.ts:1
  file_ref   src/storage/storage.ts                       src/storage/storage.ts:1
```

Both examples produce the same kind of finding because every extractor emits the same
fact vocabulary. Languages differ in what can be extracted from them; the per-language
coverage and limits are documented in [extraction/](extraction/README.md). Once those
facts enter the graph, a change spanning a Go service and its TypeScript client receives
one verdict rather than two.

---

## Where to go next

| If you want | Read |
|---|---|
| The same loop with an agent closing it for you | [CLI.md → Wiring it into your agents](CLI.md#wiring-it-into-your-agents---enola-install) |
| Every flag, exit code and output format | [CLI.md → The gate](CLI.md#the-gate---enola-check) |
| What the rest of the explainers look for | [EXPLAINERS.md](EXPLAINERS.md) |
| Rules richer than a layer order | [CONSTRAINTS.md](CONSTRAINTS.md) |
| Two services instead of one | [CLUSTERS.md](CLUSTERS.md) |
| This walkthrough, but Rails | [RAILS.md](RAILS.md) |
