# Rails — from a checkout to a graded change

This page answers **"I have a Rails application, what do I actually do?"**. It is the
workflow, end to end, with the output each step really prints.

It is not the fact reference. What a given piece of Ruby produces in the graph — route
shapes, associations, schema folding, Hotwire bindings, Packwerk — is
[docs/extraction/ruby.md](extraction/ruby.md), and this page assumes none of it.

Everything below was run against a `rails new` application on Rails 8.1.3.1 and Ruby
3.4.10, with `app/jobs`, `app/policies`, `app/serializers` and `app/components` present.
Where the output depends on which directories exist, that is called out rather than
smoothed over.

> **Driving it from Ruby instead.** Everything on this page is the `enola` binary, which
> is all you need. If you would rather stay inside Bundler:
>
> ```bash
> bundle add enola-rb
> bin/rails generate enola:install
> bin/rake enola:snapshot && bin/rake enola:check
> ```
>
> The `enola` gem wraps the released binary and forwards every command, `enola-rb` adds
> the generator and rake tasks, and `enola-guides` carries guides, a law catalogue and
> starter declarations. Both Ruby fact providers are on by default there.
>
> They are **third-party integrations, created and maintained by
> [Muhamed Isabegović](https://github.com/misabegovic)**, who also contributed the Ruby
> constraint DSL that ships in enola itself from v0.4.4. The gems live at
> [misabegovic/enola-rb](https://github.com/misabegovic/enola-rb) and
> [misabegovic/enola-guides](https://github.com/misabegovic/enola-guides); enola-labs
> mirrors both to keep the attribution visible. Issues and pull requests belong upstream.

## 1. Look at it

```bash
enola --explain .
```

Read-only, writes nothing, and needs no configuration: enola detects the languages in
the tree rather than being told. A Rails application with a JavaScript front end is two
extractors and one graph, which is usually the point.

When you want the artifacts on disk instead of a report on stdout:

```bash
enola --generate .
```

## 2. Declare the law

Rails already has conventions. enola ships them as **recipes** — named bundles of laws,
each carrying the reason it exists — so adopting them is a binding, not a file of
hand-written rules:

```bash
enola constraints init .
```

```
clean                not bound: no directory for use-cases
cqrs                 not bound: no directory for commands, queries, read-models
event-driven         not bound: no directory for publishers, events, handlers
layered              bound 4 role(s)
modular-monolith     not bound: no directory for module-public, module-internal, other-modules
ports-and-adapters   not bound: no directory for ports, adapters
rails-conventions    bound 8 role(s); optional, left for the author: services
rails-strict         bound 8 role(s); optional, left for the author: services, concerns
ruby-conventions     bound 1 role(s)
vanilla-rails        not bound: no directory for services, forms, decorators, presenters
```

It binds every shipped recipe whose **required** roles resolve to directories the
repository actually has, writes the result to `enola/constraints/recipes.yaml`, and
**never overwrites** an existing declaration. A recipe it could not bind says which
directory was missing; nothing is guessed. `--dry-run` prints instead of writing, and
`--recipe NAME` limits it to one.

Two things surprise people here, and both are visible in that output.

**A plain `rails new` does not bind the Rails recipes.** `rails-conventions` and
`rails-strict` require `policies`, `serializers` and `view-components` among their
roles, and a fresh application has none of those directories. On a bare skeleton you
get `layered` and `ruby-conventions` and nothing Rails-specific. That is the tool
declining to invent structure you have not got.

**`init` binds both Rails recipes when both resolve.** They overlap heavily, so leaving
both in place reports the same crossing twice — see §4. Pick the one you mean and
delete the other binding; the file is yours to edit.

## 3. What the Rails recipes say

`rails-conventions` is twelve laws about where a Rails application's parts may reach.
Seven are **blocking**; five are **advisory**, meaning they report without failing a
build:

| Law | Mode |
|---|---|
| `policies-only-answer` | blocking |
| `mailers-do-not-enqueue` | blocking |
| `serializers-do-not-render` | blocking |
| `view-components-do-not-enqueue` | blocking |
| `view-components-do-not-render-controllers` | blocking |
| `jobs-do-not-render` | advisory |
| `models-do-not-render` | advisory |
| `models-do-not-reach-helpers` | advisory |
| `services-do-not-reach-helpers` | advisory |
| `services-do-not-reach-controllers` | advisory |
| `request-api-stays-in-controllers` | advisory |
| `request-api-stays-out-of-services` | advisory |

The advisory ones are advisory on purpose. A job or a model that renders goes through
`ApplicationController.renderer`, which is a sanctioned path that nonetheless reads as a
crossing — so the finding is worth surfacing and not worth breaking a build over.

`rails-strict` is the same vocabulary with every law blocking, plus `parts-never-cycle`
and `concerns-stay-independent`. It is a different opinion about the same codebase, not
a newer version of one.

Each law carries its `because:`. `policies-only-answer` exists because *"authorization is
asked many times per request and sometimes speculatively, so a policy that enqueues work
makes what happens depend on how often it was asked"* — which is the sentence a
violation surfaces, so nobody has to reconstruct the intent from the rule name.

## 4. Pin, change, grade

```bash
enola baseline pin .        # the "before"
# …make the change…
enola check --fail-on=constraints .
```

A policy that enqueues a job, and a model that renders through the controller:

```
FAIL — 3 structural regressions introduced.

Regressions (fail):
  - [constraints] 1.00 — Constraint rails-conventions/policies-only-answer violated: OrderPolicy#show? -> DigestEmailJob via calls
      forbidden calls edge
      app/policies/order_policy.rb:6
        def show?
        ^^^^^^^^^
  - [constraints] 1.00 — Constraint rails-strict/models-do-not-render violated: Order#summary_html -> ApplicationController via calls
      forbidden calls edge
      app/models/order.rb:2
        def summary_html
        ^^^^^^^^^^^^^^^^

Policy: fail on new findings from [constraints] at confidence >= 1.00.

New findings (advisory — below the failure policy):
  - [constraints] 0.90 — Advisory constraint rails-conventions/models-do-not-render violated: Order#summary_html -> ApplicationController via calls
  - [constraints] 0.90 — Advisory constraint rails-conventions/request-api-stays-in-controllers violated: Order#summary_html -> renderer.render via calls
  - [layers] 0.80 — Layer violation: model -> controller
      import of app/controllers

Confidence < 1.00 is a candidate to verify, not a verdict.
```

Exit `1`, so a hook or a CI job can stop there. **Nothing fails without `--fail-on`** — a
bare `enola check` reports the same delta and exits `0`.

That output is worth reading twice, because one edit produced four findings at three
different confidences:

- **`rails-strict/models-do-not-render`, 1.00, fails.** You declared the law, and the
  graph measured exactly the edge it forbids. There is nothing to verify.
- **`rails-conventions/models-do-not-render`, 0.90, advisory.** The *same* edge under the
  other recipe. Both were bound, so the crossing is graded twice by two different
  opinions — the concrete cost of §2's warning.
- **`layers`, 0.80.** Nobody declared this one. enola *recognised* a Rails layering by
  matching module paths against known taxonomies and inferred the violation, so it never
  reaches 1.00 and never fails a build by default.

Declared law is proof. A recognised pattern is an estimate. The confidence says which
you are looking at, and the number is exact rather than a vibe — see
[docs/EXPLAINERS.md](EXPLAINERS.md).

### Where the reason went

`enola check` prints the rule, the edge and the underlined source span. The `because:`
travels on the finding itself rather than the gate's summary, so it reaches an agent
through `query_insights` and a reader through `.enola/insights.json`:

> `rails-conventions/policies` must not reach `rails-conventions/jobs` via calls, and the
> graph measures exactly this edge. The rule is declared, both memberships are exact, so
> this is a decided-rule breach, not a heuristic. **Because:** Authorization is asked many
> times per request…

## 5. Writing your own

A repository whose team writes Ruby may write its laws in Ruby. Files ending in `.rb`
under `enola/constraints/` are parsed with the same grammar the extractor uses and
**never executed**, compiling to exactly what the YAML loader produces:

```ruby
Enola.architecture "storefront" do
  rails

  law "background jobs never invoke controller code" do
    jobs.must_not_call controllers
    why "rendering from a job goes through ApplicationController.renderer"
  end
end
```

`rails` declares the conventional parts from the directories Rails puts them in, so you
write only what is yours. Nineteen verbs cover the 21 rule forms.

The full vocabulary — components and their selectors, every rule form, modes,
exemptions, recipes you author yourself, and `constraints lint` / `mine` / `explain` —
is [docs/CONSTRAINTS.md](CONSTRAINTS.md).

## 6. Beyond what the parser can see

Ruby has no compiler and no import graph, so some of what you want is not in the syntax.
enola takes it from **providers**, which contribute measured facts through a fail-closed
seam and stamp every fact with how it was resolved:

- **Rubydex** resolves constant references through Ruby's own nesting and inheritance
  rules, and emits each class's linearised ancestor chain. This is what the `ancestor:`
  component selector reads.
- **RBS/Sorbet** brings declared signatures in as claims, marking a measured symbol
  `typed: true` and naming the file that said so.
- **The runtime provider** carries observations from a booted application — the final
  route table, reflected associations — as `runtime-observed`, never as a static fact.

A declaration is a claim about the implementation, not proof of it, and the resolution
level always says who said it. Setup and the full contract are in
[docs/PROVIDERS.md](PROVIDERS.md).

## 7. In the loop

```bash
enola install --hooks
```

writes enola's instructions into the files your agents already read and adds the two
hooks that pin a baseline when a session starts and grade the tree when it ends. The
same three commands from §4 also run in a git hook or CI —
[docs/CLI.md](CLI.md) has the gate, the exit codes and the scope flags.

---

**Next:** [docs/extraction/ruby.md](extraction/ruby.md) for what Ruby produces in the
graph, [docs/CONSTRAINTS.md](CONSTRAINTS.md) for the law vocabulary,
[docs/EXPLAINERS.md](EXPLAINERS.md) for what the twenty-two explainers compute and why a
finding is not a verdict.
