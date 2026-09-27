# Declared constraints — the repo's law

> The constraints program — this vocabulary, its evaluator, the provider seam it reads,
> and the Ruby surface for writing laws as sentences — was contributed by
> [Muhamed Isabegović](https://github.com/misabegovic), who also maintains the Ruby and
> Rails integration gems enola-labs endorses.

[INTENT.md](INTENT.md) states what an architecture *is meant to look like*;
constraints state what it is **not allowed to do**, and the
`constraints` explainer verdicts them against the measured graph on
every snapshot. Two sections carry the whole vocabulary: `components`
name sets of measured facts, `rules` state enforcement over them.

Constraints are **file-level only**: they live in `enola-intent.yaml`
or under `enola/constraints/` (or a cluster config's `intent:` entry,
which overrides the repo's files wholesale) — never on a page. A page
carrying `components:` or `rules:` is a validation error, not a
merge: constraints are the repo's declared desired architecture,
reviewed beside the code they govern, and a decision page references
rule ids rather than carrying the rules themselves.

Some of what a rule can select is measured by a **provider** rather than an
extractor — `ancestor:` reads a resolved ancestry chain, and a rule may verdict over
a runtime observation or a declared signature. See [PROVIDERS.md](PROVIDERS.md).

## Start here

The rest of this page is a reference: 21 rule forms, the selector
dialect, recipes, modes, exemptions. You do not need any of it to
write your first rule. The loop is four commands, and it is worth
running once on a repository you know before reading further.

**1. Ask what enola can bind for you.** `constraints init` writes a
first declaration, binding every shipped recipe whose required roles
resolve to directories your repository actually has. It refuses to
overwrite an existing declaration and it guesses nothing — which
means the honest answer on many repositories is that it binds
nothing:

```
$ enola constraints init .
clean                not bound: no directory for frameworks, interface-adapters, use-cases, entities
layered              not bound: no directory for presentation, application, domain, infrastructure
ports-and-adapters   not bound: no directory for core, ports, adapters
rails-conventions    not bound: no directory for controllers, jobs, models, mailers, policies, …
nothing to bind: no shipped recipe's roles resolve to a directory here
```

That is not a failure. A recipe is a named pattern with required
roles ([Recipes](#recipes--named-patterns-as-instantiable-bundles)),
and a repository laid out its own way has none of them. Read the list
as a question — *is my repository one of these, under different
directory names?* If it is, bind the recipe by naming the roles
yourself. If it is not, write the rule directly, which is step 2.

**2. Write one rule, by hand.** Two components and one rule is a
complete declaration. This goes in `enola-intent.yaml`, beside the
`layers:` you may already have:

```yaml
components:
  - name: storage
    match: ["storage/**"]
  - name: notify
    match: ["notify/**"]

rules:
  - id: storage-sends-nothing
    forbid: storage
    to: notify
    via: imports
    because: "persistence must not decide what the buyer is told"
```

`because:` is mandatory, and it is not decoration: every finding this
rule produces carries it, so a violation says why the rule exists
rather than only that it was broken.

**3. Check that it means what you think.** `constraints lint`
validates the vocabulary and — when a snapshot exists — resolves each
component against it, so you find out that a selector matches nothing
*before* a verdict silently passes:

```
$ enola constraints lint .
Component resolution against the snapshot at ./.enola:
  notify                   2 member(s)
                             match notify/**
  storage                  2 member(s)
                             match storage/**

OK — every declaration is valid.
```

A component with `0 member(s)` is the failure mode to watch for. It
is valid YAML, it lints clean, and it can never be violated.

**4. Enforce it.** The `constraints` explainer verdicts declared
rules on every snapshot at confidence `1.00`, so the gate can fail on
them:

```
$ enola check --fail-on=constraints .
FAIL — 1 structural regression introduced.
law: 1 rule · 1 breach · none excused

Regressions (fail):
  - [constraints] 1.00 — Constraint storage-sends-nothing violated: storage -> notify via imports
      forbidden imports edge: storage must not reach notify
      storage/storage.go:3
      import "layersgate/notify"
```

**Then: what is the law actually doing?** As a declaration grows,
the question stops being "does it pass" and becomes "how much of it
is being excused rather than obeyed". `constraints ledger` answers
that — every rule beside its breaches, its suppressions and its
exemptions, each with an owner, a reason and an age:

```
$ enola constraints ledger .
law: 1 rule · 1 breach · none excused
read from the snapshot generated 2026-08-25T19:30:54Z — a rule declared since then is not counted here

storage-sends-nothing [ratchet] — 1 breach reported, 0 excused · declared in enola-intent.yaml
    because: persistence must not decide what the buyer is told
```

`constraints mine` is the fifth command, and it belongs after these
four rather than before them: it proposes candidate rules out of the
regularities your snapshot already shows, which is useful once you
know what a rule means and can judge a proposal. It never
self-adopts.

From here, the reference. [Components](#components) is the selector
dialect, [The 21 rule forms](#the-21-rule-forms) is everything a rule
can say, [Modes](#modes) is how strictly each is enforced,
[Exemptions](#exemptions--declared-carve-outs) is how a breach is
signed off, and [`plan` / `plan_check`](#plan--plan_check--the-pre-edit-contract)
answers which rules govern a change *before* you make it.
## The constraints directory

A repo whose law outgrows one file splits it into per-domain files
under `enola/constraints/*.yaml` — visible source at the repo root,
never under `.enola/`. Each file carries the same two sections the
inline declaration does (`components:` and/or `rules:`), and loading
merges every file's entries after the inline ones, in sorted filename
order, so the resolved sets are deterministic; inline stays legal for
small repos. Validation runs over the merged set — a rule in
`billing.yaml` may name a component declared in `domains.yaml` or
inline — but one component name or rule id declared twice across
sources is an error naming both declaring files, and each compiled
fact cites the file that declared it, so a verdict names
`billing.yaml` rather than the merged whole. The split is what makes
CODEOWNERS work: each domain's file routes to the team that owns that
domain's law.

## Components

A component names the facts its selector matches:

```yaml
components:
  - name: domain                 # lowercase token
    match: ["app/domain/**"]     # exact path, prefix/** subtree, or **/name
                                 # basename glob — nothing else
    kind: module                 # optional: module, route, storage, symbol —
                                 # plus test_ref, file_ref, lint and dependency,
                                 # which a component acquires ONLY by naming them
    name_pattern: "*Serializer"  # optional: a fact-name family — an exact
                                 # name, one prefix*, or one *suffix
    service: billing             # optional: one repo of a multi-repo snapshot,
                                 # by exact repo label
    where: { framework: rails }  # optional: a predicate over measured fact props
    owns: methods                # optional: a concept's members' methods are
                                 # theirs — the one field that widens, and the
                                 # one a rule may override for its own reach
```

`match` speaks a bounded glob dialect of three forms: an exact
repo-relative path, a `prefix/**` subtree, and a `**/name` basename
glob. Any other glob form is rejected at parse time, so a selector the
evaluator would silently fail to match is an error the author sees
instead. Membership is exact — path equality, declared subtree, or
final-segment name over a fact's file, an optional kind narrowing, an
optional fact-name narrowing — and a fact with no file matches
nothing. A component whose selector matches nothing surfaces as a
standing 0.4 advisory, so vacuous compliance never reads as
compliance.

The basename form exists because some conventions live in a filename
rather than a directory, and the files obeying one are routinely
spread across trees that share no prefix. Stimulus is the case that
asked for it: a Rails monolith keeps controllers in
`app/javascript/controllers/**` and beside their view components in
`app/components/**`, so `**/*_controller.js` is the only spelling of
"every Stimulus controller" — no prefix reaches both, and the whole
repository reaches far too much.

`**/` means "at any depth", including the repository root, and what
follows it is one path segment carrying at most one `*` around a
non-empty literal: `**/*_controller.js`, `**/Gemfile`, `**/schema.*`.
The `*` never crosses a `/`, and it is matched against the file's
final segment only — a directory named `x_controller.js` does not put
the files under it in the component. Everything else stays out
deliberately: no `?`, no character class, no brace set, no escape, no
`**` between segments, and no second `*`. `**/*` is malformed rather
than a spelling of "everything", for the same reason `name_pattern: *`
is. A malformed pattern is a named error at declaration time; a
well-formed one that matches nothing is the dead-selector advisory,
which is a different report and reads differently.

Declared layers keep the first two forms and not the third: a layer is
a place, and a filename that appears in several places is not one.

`name_pattern` narrows membership to a family of fact names, and it
speaks the bounded name dialect `require_name`'s `pattern` and
`require`'s `when_edge_to` speak: an exact name, one trailing `*`
matching a prefix, or one leading `*` matching a suffix. Nothing else
— no second star, no `?`, no character class — for the reason the
`match` dialect is bounded too, and screened by the same
`ValidNamePattern` the evaluator's `MatchBoundedName` implements, so a
family a declaration may write is a family the evaluator recognizes. A
pattern with no star is plain string equality, which is what
`name_pattern` always was. It narrows and never selects on its own: a
component carrying one still needs a `match`, a `service` or a
`where`. The screen costs something and the cost is deliberate — an
exact name carrying a glob metacharacter, Ruby's `Config#[]`, is not
declarable, because admitting it would mean admitting a pattern the
matcher has a second reading of.

Because it is a name narrowing rather than a `where:` predicate, a
name-patterned component is legal in every rule role, edge forms
included: `forbid: constructors, to: fetchers, via: calls` is a
declaration this dialect makes writable. What such a component does
not gain is grounding by file. A path-granular edge target resolves to
a file, and a file cannot show which of the facts measured in it the
edge landed on, so a name-narrowed component is joined to no path
target — exactly as it was when the narrowing could only be one name.

`service` scopes the selector to one repository of a multi-repo
(append-mode) snapshot, by the exact repo label every appended fact
carries. It ANDs with the other narrowings — members are facts of
that repo — and it is the one selector that makes `match` optional: a
component with a `service` and no patterns is the whole service, and
then it also contains the synthetic service node the cross-repo
linker emits, so service-to-service `depends_on` edges are walkable
like any other. A fact with no repo label matches no service, fail
closed.

### Selecting by concept: `where`

`where` selects members by what the measured facts **carry** instead of
by where their files sit, so a rule can name an enforceable concept
rather than a directory. It is a **membership** selector and only that.
The forms that read a member's own props take it as it stands; the forms
that walk edges take it once the component declares what it OWNS — see
*A concept in an edge role* below for the declaration, the precedence,
and the two pairings that stay refused.

```yaml
components:
  - name: view-components
    where: { superclass: "ViewComponent::Base" }
  - name: ember-components
    where: { framework: ember, symbol_kind: class }
  - name: models
    where: { kind: storage, storage_kind: model }
  - name: hairy-methods
    where: { symbol_kind: method, cyclomatic: ">=20" }
```

Every key names something the extractors measured. That is the whole
vocabulary: **a predicate can only reach a concept the facts already
carry**. Two worked examples of the limit, both measured on a
production Rails+Ember monolith (153,252 facts, 2026-08-13):

- `{ framework: ember, symbol_kind: class }` selects 2,200 Ember
  component classes. It works because the TypeScript extractor puts
  `framework: ember` on the class.
- There is **no predicate in this vocabulary that names a Stimulus
  controller.** `framework: stimulus` is set on the `static
  targets`/`values` field members, never on the class
  (`tsextractor/ts.go`), so it returns 52 method facts and
  `{ framework: stimulus, symbol_kind: class }` returns 0. Selecting on
  the base class instead depends on what the TypeScript extractor emits
  in the snapshot you are reading, which is a moving target and not a
  property of this vocabulary: ask the snapshot
  (`query_facts kind=symbol prop=superclass`) rather than this page.
  What does not move is that 42 of the 50 controllers are written
  `export default class extends Controller`, anonymously, so a
  name-based selector reaches at most 8 of them however the base class
  is measured.

- **Conjunction only.** Every pair must hold. There is no `or` and no
  negation: a disjunction is two components, which reads better than a
  nested boolean, and a negation asks the snapshot to answer for facts
  it may simply never have measured.
- **`kind` is the one reserved key**; every other key is a fact property
  name. `kind` narrows the fact kind, the same narrowing the component's
  own `kind:` field carries — declaring both spellings is an error
  rather than a silent precedence rule.
- **`superclass:` is one level, and only one.** The extractor records
  `superclass` exactly as the source wrote it, so
  `superclass: ViewComponent::Base` selects the classes that name that
  parent *directly* and nothing written underneath them: on the monolith
  that is 269 of the 357 classes whose ancestry reaches it, 310 of 531
  for `ApplicationRecord`. This vocabulary has no transitive spelling —
  a rule that must cover a hierarchy names each level, or widens the
  component another way.

  A component whose members are named as the parent by classes it does
  not contain gets a 0.4 advisory carrying the count and the classes —
  the case neither the dead-selector nor the unmeasured-property
  advisory could see, because the selector worked and simply reached
  less than the concept it names. Its witnesses are lexical, like the
  property, and the count is **neither a floor nor a ceiling**. It
  misses: a subclass that spelled its parent relatively — the
  unqualified `Base` inside a module — writes text no member's fact name
  equals. It over-attributes: the index is keyed on the parent as
  written and read by the member's resolved fact name, so a
  module-scoped `Base` and a top-level `Base` are one key, and
  `Widgets::Card < Base` is named as a subclass of a member it does not
  inherit from. Both are the same fact about `superclass` — it is source
  text, and no reading of it is transitive or namespace-aware without a
  resolution pass the extractor did not make.
- **`ancestor:` is the transitive spelling, and it is a separate key.**
  A component declaring `ancestor: ApplicationRecord` holds every class
  from which a chain of *resolved* `implements` edges reaches that name:
  the grandchild that spelled its parent as `Base` inside a module, the
  class that got there through a mixin, all of them with names already
  qualified. The root itself is not a member, the same as `superclass:`.
  The chain comes from a resolving provider (the Rubydex provider emits
  it), so when the snapshot holds no resolved ancestry at all the
  component is **unevaluable** with a named cause, every rule naming it
  stays silent, and a 0.4 finding says which provider would settle it.
  It is a new key rather than a new reading of `superclass:` because the
  same declaration must not select 269 classes on one machine and 357 on
  another depending on which gem is installed; the two keys are two
  claims, and a declaration may carry both.

  ```yaml
  components:
    - name: records
      ancestor: ApplicationRecord
    - name: view-components
      match: ["app/components/**"]
      ancestor: "ViewComponent::Base"
  ```
- **Values match one whole member at a time**, the same containment the
  `require` form's `when_prop_contains` reads set props with. For the
  space-joined set props (`columns`, `fk_constraints`, `decorators`)
  that is containment; for a scalar prop — which decomposes into a
  single token — it is exactly equality. One semantic, not two, and
  never a substring: `company_id` is not satisfied by
  `parent_company_id`.
- **Numeric props take thresholds**: `">=20"`, `"<=2"`, `">0"`,
  `"<3.5"`. Quote them — a bare `>=` opens a folded scalar in YAML.

  The grammar is one ASCII comparator and one decimal number: an
  optional `-`, digits, an optional fractional part. Nothing else
  parses, and everything else is a named error rather than a literal
  string nothing will ever equal. `"=>30"`, the hash-rocket
  transposition, is rejected. So are Go's other numeric literal forms,
  each of which meant something no reader of the YAML would guess:
  `"<=Inf"` validated clean and selected every fact carrying the
  property as a number, `">=1_0"` means ten, `">=0x1fp0"` means
  thirty-one. So are the comparators a rendered document leaves behind —
  `"≥30"`, `"≤30"`, `"≫30"`, `"﹥30"`, `"⇒30"`, `"❯30"` — which arrive
  by exactly the route `"=>30"` does and select nothing. That screen is
  an **allowlist**, not a list of named runes: enumerating the ones that
  look like a relation has no edge, and every one left off compiled to a
  literal token. A value may open with any ASCII rune, or with a letter,
  a digit or a combining mark — the alphabets an identifier is written
  in. A non-ASCII symbol or punctuation rune opens nothing this grammar
  can mean.

  A threshold against a property no measured fact carries as a number is
  a 1.0 finding for the same reason an unmeasured property is — it can
  never hold.
- **A value carries no whitespace, of any kind.** The screen reads the
  same alphabet the decoder splits on, so a non-breaking space pasted
  out of a rendered document is refused exactly as a plain one is. The
  compiled form percent-escapes whitespace so the round trip is
  lossless, and a compiled predicate that does not decode back into
  property tests selects nothing and says so. A declaration never
  compiles to a predicate different from what it says — in either
  direction.
- **`where` ANDs with `match`, `service`, `kind` and `name_pattern`**,
  for the same reason every SELECTOR on a component narrows: a
  component carrying both a path scope and a predicate is
  their intersection, which is how a path scope you trust gets
  sharpened rather than replaced. A `where` alone needs no `match`: the
  predicate is the selector.

  The AND holds wherever a component is joined to a file, including the
  edge TARGET join: a file-granular import target names no fact, so it
  is resolved against the component's `match` globs, and for a
  predicate component that join additionally requires the component to
  have measured a member in the resolved file. A path component keeps
  the plain glob join — its globs ARE its claim about files. The edge
  target join is unreachable for a predicate component today, because
  an edge form naming one is refused at declaration time (below); the
  AND is written where the join is defined rather than where it is
  called, so relaxing that refusal cannot silently widen it.
- **An unmeasured property fails closed and loudly.** A `where` naming
  a property no measured fact in the snapshot carries is a validation
  problem in `constraints lint` (exit 1, with near-miss suggestions)
  and a 1.0 finding from the explainer, and every rule naming that
  component emits **no verdict** — because an empty component makes
  every rule over it hold, and that reads exactly like compliance. This
  is distinct from the 0.4 dead-selector advisory, which covers a
  measured property whose value happens to match nothing.

  The census is scoped to the component's **own service**: a component
  reading `service: billing` is judged against billing's facts, never
  against the union's. A component naming a service the snapshot does
  not contain is unasked before the question is asked — the 0.4
  absent-service advisory, never a 1.0 measurement claim about a repo
  that was never loaded.
- **A constraint finding is never incidental.** The gate's ratchet
  files a finding as incidental when the change touched nothing it
  cites, which is right for a moving mean+2σ threshold and wrong for a
  declared rule: the fail-closed findings cite the COMPONENT, and a
  component is exactly what does not change when the code moves out
  from under its selector. Constraint findings are graded on their own
  terms, so the 1.0 "selector cannot be evaluated" finding fails
  `check` when a snapshot stops measuring what a declaration reads.
- **A breach that stopped being reported is not automatically a breach
  that was fixed.** `check` prints two further sections rather than
  folding either into "Resolved by this change". *No longer verdicted* —
  the code the breach named is still measured and no longer selected by
  the component its rule binds; changing a class's superclass silences
  every rule that named it exactly this way. *No longer declared* — the
  rule was deleted, re-formed under a preserved id, or the witness was
  carved out by an exemption, with the breaching code untouched.
  Neither is graded, both are legitimate acts, and neither reads as good
  news.

  A third section, *not attributable to this change*, covers what the
  pair of snapshots has no standing to judge at all: the repository a
  witness was measured in left a union snapshot (which reads exactly
  like deleted code, and which `WarnDifferentRepo` cannot see because it
  keys on the snapshot's own identity rather than on the union's
  members — there is a `union_membership` warning for it now), or the
  baseline carried the finding without the declaration that produced it.

  The inverse matters as much and is more ordinary, because it steals
  credit for work someone did. A rule's declaration identity now
  excludes its bookkeeping — `source`, `recipe`, `instance` and
  `because` — so moving a rule between constraints files or relabelling
  a recipe instance no longer files every breach the same change fixed
  under "the law stopped asking". Exemptions are compared **per
  witness** rather than as one blob, so adding a carve-out for witness X
  and fixing witness Y in one change credits Y to the change that fixed
  it.

### A concept in an edge role: `owns`, and the basis a verdict states

A predicate selects the facts that CARRY a property, and every property
this vocabulary can test — `superclass`, `symbol_kind`, `storage_kind`,
`framework`, `cyclomatic`, `decorators` — is measured on the **class**.
The call graph connects **methods**. In Ruby a class's calls ride its
`Owner#method` and `Owner.method` facts, which carry none of those props
and therefore cannot be members of the component. Whether those methods'
edges count as *the class's* edges is a statement about what a component
MEANS, and no selector makes it. Five rounds encoded an answer in code
while the verdict printed a different one — worst case 269 breaches at
full confidence against every member of a 269-member component.

**So ownership is declared.** A component says what it owns, and a rule
may override that for its own reach:

```yaml
components:
  - name: exceptions
    where: { superclass: StandardError }
    owns: methods           # its members' methods are the member's
rules:
  - id: exceptions-avoid-the-database
    forbid: exceptions
    to: models
    via: calls
    because: an exception carries a message, never a query
    owns:                   # optional: this rule's reach only
      - component: exceptions
        owns: nothing
```

`owns` takes `methods` or `nothing`, and **absent is not `nothing`**:
an absent ownership is a component whose meaning at an edge is unstated,
and an edge role over it is refused. An explicit `nothing` is a
declaration — a member's own facts and nothing else — which compiles.

**`methods` reaches the member's methods, and nothing else.** What it
adds is exactly the facts the graph's `has_method` edges reach, which the
graph wires for `method`, `function` and `getter` symbols. It does not
reach the rest of a member's body: a **constant**, a **nested class** or
an `attr_accessor` variable written inside a member carries no
`has_method` edge and is **not owned**. So an edge landing on
`TimeoutError::CODE` lands *outside* a concept owning `methods`, and a
rule forbidding that landing reports a true breach — the component never
claimed the constant. Lexical enclosure is a larger semantic than this
vocabulary states, and stating it would need its own measurement.

**The precedence is stated once**: the rule's override wins over the
component's declaration, and a component neither declares is undeclared.
Two overrides for one component in one rule have no precedence between
them and are a named error rather than a last-one-wins. A test pins the
precedence in both directions, permissive and strict, so a later change
cannot quietly invert it.

`owns` is the **one component field that widens**, and it is not a
selector: membership is exactly what the selectors chose, so `cap`
counts the same set and `constraints lint` prints the same numbers. What
it widens is what a rule may WALK. That distinction is what keeps the
exception from reading as licence to widen a selector — a field that
changed membership would have to narrow, and this one cannot change
membership at all.

**A verdict states the basis it reached each end on**, in one three-state
vocabulary used at both: *exact* (the fact is a member), *owned* (it is a
member's method and the declaration says that is the member's), or
*grounded* (it names no fact and joins through the measured file). Every
edge form words both ends, so no sentence can state one end and leave the
other to be read as exactly measured.

**Edge forms re-open only where the basis can be stated.** Two refusals
survive the ownership declaration, both statements about what the
vocabulary can say rather than about any snapshot:

| role resolves | over | refused because |
| --- | --- | --- |
| the SOURCE of the edge (`forbid`, `forbid_reach`, `allow`, `protocol`, an outbound `require_edge`, `owners:`, `except:`, an inbound `require_edge`'s `to:`) | `imports` | every imports edge rides a dependency fact, which carries none of the props a predicate tests, and no ownership reaches a file's dependency facts |
| the TARGET of the edge (`protect`, `private`, an inbound `require_edge`, `to:`, `only:`, `steps:`) | `imports` | an imports target names a path, so it reaches a component only through the measured file grounding joins to `match` globs — which needs globs and refuses a `name_pattern` |

Both are refused at declaration time, so no such rule compiles into a
fact. Note that `private` and a `via`-less `forbid_reach` walk every
rule-via kind, `imports` among them, so a concept in either is refused
whichever role it fills.

**And what the declaration cannot see, the snapshot answers.** A concept
may declare an ownership honestly and still reach nothing — an estate
that measures no methods for its members, members carrying no edge of
the kind the rule walks. The reach question is asked **per role and on
one side**: a source-side role asks only whether the component's edge
sources carry such an edge, a target-side role only whether such an edge
resolves onto it. The previous machinery ORed three arms belonging to
different directions, so deleting an unrelated INBOUND edge flipped an
`owners:` rule from a false breach to a correct refusal. A role that
resolves nothing silences its rule with a 1.0 finding naming the role,
the side and the edge kind; a role whose empty resolution is no verdict,
unreachable on some but not all of a multi-kind rule's edges, gets a 0.4
note instead — refusing there would delete enforcement that worked.

Two roles are deliberately not asked: `require_edge` and `protocol`
decide their subject's measurability from the extraction census, and for
the existential form an empty target resolution IS the breach it exists
to report. Asking the reach question there silences exactly the total
violation, which is how a round shipped reporting zero breaches against a
total one.

Every combination above is covered by a matrix over every rule form and
every role, run through the real extractors over an edge kind riding
member facts (`calls`) and one riding dependency facts (`imports`), and
driven from the schema's own form table so a form added later fails the
matrix rather than defaulting into a column.

The file-hosting carrier and the `inherits:` closure remain held out
(PR #94, not merged): the first because its guard is blind to symbol
kinds outside a small set, the second because its lookup is keyed on
written parent text while the walk keys on fact names.

The pre-edit contract answers for a raw path when the snapshot carries
a member in it — the arm `plan --paths` needs, since a `where`-only
component has no match patterns for a path to join. A file nobody has
written yet is still refused: nothing has been measured about it, and
that is exactly what a predicate cannot answer for.

## The 21 rule forms

Every rule has a lowercase-token `id`, unique per declaration, and a
mandatory `because:` — the rationale every resulting finding surfaces,
so a violation always says why the rule exists, not only that it was
broken. The edge forms (`forbid`, `allow`, `protect`) also need a
`via:` from the closed edge vocabulary: `calls`, `depends_on`,
`implements`, `imports`. `implements` walks inheritance and mixin
inclusion — the include/extend/prepend edges the Ruby extractor
emits — so who-may-include rules need no form of their own.

```yaml
rules:
  - id: domain-stays-pure        # forbid: this component must not reach that one
    forbid: domain
    to: adapters
    via: depends_on
    because: "the domain must not know its delivery mechanisms"

  - id: web-through-services     # allow: edges may land only in the named components
    allow: web
    only: [services]
    via: calls
    because: "controllers orchestrate; they never reach storage directly"

  - id: billing-owned            # protect: only the named owners may reach this one
    protect: billing
    owners: [payments]
    via: calls
    because: "billing invariants are enforced at the payments boundary"

  - id: pack-internals           # private: non-exported members stay inside
    private: billing
    except: [payments]           # optional: components also allowed to reach in
    because: "only the pack's public surface is a contract"

  - id: no-legacy-helpers        # forbid_fact: this component must be empty
    forbid_fact: legacy
    because: "app/legacy is frozen; new code lands in app/domain"

  - id: bounded-public-api       # cap: membership must not exceed a count
    cap: public_api
    max_members: 20
    because: "every exported surface here is a compatibility promise"

  - id: company-fk               # require: members must carry a prop value
    require: tables
    when_prop_contains: {prop: columns, value: company_id}
    must_prop_contain: {prop: fk_constraints, value: company_id->companies}
    because: "tenant isolation rides the company FK; a bare company_id column is a leak"

  - id: promise-getters-cached   # require + when_edge_to: an edge selects, a prop is demanded
    require: component-getters
    when_prop_contains: {prop: symbol_kind, value: getter}
    when_edge_to: ["*.reactiveUnwrap", "*.getPromiseState"]  # literals, never components
    via: calls                   # which edge kind the antecedent reads
    must_prop_contain: {prop: decorators, value: cached}
    because: "a getter that unwraps a promise recomputes on every read unless it memoizes"

  - id: jobs-perform             # require_defines: class members must define a method
    require_defines: jobs
    method: perform
    because: "the queue calls perform on every job"

  - id: jobs-named-job           # require_name: member names must match a convention
    require_name: jobs
    pattern: "*Job"              # prefix*, *suffix, or an exact name — nothing else
    because: "the scheduler discovers jobs by their suffix"

  - id: no-getter-prefixes       # forbid_name: member names must not match a pattern
    forbid_name: models
    pattern: "get_*"             # the same dialect require_name speaks
    surface: exported            # optional: judge exported members only
    because: "a reader is a noun; get_ says the class is a bag of fields"

  - id: every-event-consumed     # require_edge: every member must have an edge
    require_edge: events
    to: handlers                 # optional: omit to accept the edge from anywhere
    via: calls
    direction: inbound           # inbound: someone points at the member
                                 # outbound: the member points somewhere
    because: "an event nobody consumes is dead weight or a silent contract break"

  - id: checkout-protocol        # protocol: members conform to an ordered step list
    protocol: checkout-callers   # the component whose members must conform
    steps:                       # ordered step components, first step first
      - validate-cart
      - reserve-stock
      - charge-payment
    via: calls
    because: "Charging without reserving oversells; reserving without validating reserves garbage."
```

`require_defines` verdicts protocol: every class-kind member symbol of
the component must have a measured method symbol of the declared name,
in either qualified shape the extractors emit — `<Class>#<method>`
(instance) or `<Class>.<method>` (class-level). A class that inherits,
includes or extends anything is **out of the rule's scope**, not in
breach of it: the definition could ride composition (a superclass, an
included concern) the store does not resolve through, and fail-closed
means never guessing a composed definition absent. The form therefore
verdicts exactly the classes whose omission is visible.

**Class-kind means `class` or `struct`**, because the owner of a method
is not spelled the same way in every language: Ruby, Python, TypeScript,
Java, Kotlin and PHP write `class`, and Go, Rust, C++ and C# write
`struct`. Composition is excluded on both — a Go struct that embeds
another carries the same `implements` relation an inheriting Ruby class
does, so it leaves the rule's scope by the fail-closed rule above rather
than by its kind. An `interface` is excluded because it declares
signatures rather than defining them, and a `type` alias because it
defines nothing. While the gate admitted `class` alone, a rule written
over Go structs resolved its members, verdicted nothing and raised no
advisory, which is the vacuous compliance the rest of this vocabulary
exists to make loud.

`require_name` verdicts convention: every member fact's name must
match the declared pattern. The dialect is deliberately bounded —
`prefix*`, `*suffix`, or an exact name, never a general glob or
regex — for the same reason `match` patterns are: a convention the
evaluator would silently mis-apply must be impossible to declare.
Every member is in scope; a name always exists.

`forbid_name` is its negative: every member fact's name must *not* match
the declared pattern, in the same bounded dialect and through the same
matcher, so a pattern means one thing whichever way it is read; for a
method the pattern is also tried against the bare method name after its
owner, so `get_*` reaches `Order#get_total`. With
`surface: exported` only members whose measured `exported` prop is true
are judged, because a private helper is not the surface a naming
convention governs; without it every member is.

`private` verdicts visibility: members of the component whose measured
`exported` prop is `false` may be reached only from inside the
component (or from an `except:` component), over **every** rule-via
edge kind at once — privacy is about any measured reach, so the form
carries no `via:` of its own. Non-exported is the extractor's own
measurement (Go capitalization, Ruby `private` markers and packwerk
public dirs, TypeScript `export`, Python underscore prefixes, …); a
member with no boolean `exported` prop, or whose facts disagree about
visibility, is out of the rule's scope, fail closed.

`require_edge` verdicts existence — the one form that demands an edge
rather than forbidding one. For **every** member of the component, at
least one measured edge of the `via` kind must exist in the declared
`direction`: `inbound` means some source points at the member,
`outbound` means the member points somewhere. With a `to:` the demand
narrows to edges whose counterpart is one of that component's members;
without one, any measured via-edge satisfies. A member with zero such
edges is a violation with the member as its witness — `OrderPlaced has
no inbound calls edge from handlers` — at the same proof-class `1.0`
every decided rule verdicts at. Measurability fails closed on the
snapshot's own extraction census: an absence only verdicts where facts
of the searched side's file kinds demonstrably source `via`-kind edges
elsewhere in the snapshot. A member whose absence the census cannot
back — the member's own file kind sources no such edges anywhere
(outbound), or the searched sources include a file kind that sources
other edge kinds but never this one (inbound) — is **skipped** with a
named count in one 0.4 advisory per rule, never silently compliant and
never falsely violated, the same honest degrade the reach skip and the
dead-selector advisory set the shape for.

`protocol` verdicts ordered obligation — **structurally, never
temporally**. The steps are an ordered list of components; for every
member of the `protocol` component, a measured `via` edge into step
K's members obliges measured `via` edges into every step 1..K-1's
members. A caller that reaches `charge-payment` without
`reserve-stock` is a violation with the member as its witness —
`OrderFlow calls charge-payment without reserve-stock`, naming the
highest skipped step — while a member touching every prerequisite,
touching only step 1, or touching no step at all stays silent: the
protocol binds participants, not bystanders. See "Protocol ordering"
below for what this form can and cannot honestly claim.

`require` verdicts what a member fact *carries* rather than what edges
it makes: every member matching the optional `when_prop_contains` gate
(every member, when the gate is omitted) must satisfy
`must_prop_contain`. Containment is whole-member over the fact's
space-separated set prop — `columns contains company_id` is never
satisfied by `parent_company_id` — and a member whose gated prop was
never measured is out of the rule's scope, not in breach of it. The
census props the company-FK example reads (`columns`,
`fk_constraints`) are measured from whichever schema dump the project
keeps — `db/structure.sql` or `db/schema.rb`, the SQL one winning where
both exist — in the same shape either way.

`when_edge_to` is the form's second antecedent, for the conventions
whose criterion is a call rather than a property: "a getter that works
with promises carries the caching decorator" is selected by the calls
themselves, and no extractor prop should have to be invented to name
one organisation's helpers. Each entry is a **literal** matched against
the edge target in the bounded dialect `require_name` speaks —
`prefix*`, `*suffix`, or an exact name, with `ValidNamePattern` and
`MatchBoundedName` shared between the validator and the evaluator so
what may be declared and what is matched cannot drift. A target carries
no whitespace of any kind — the compiled rule holds the set as one
whitespace-separated prop, and the screen is the same `unicode.IsSpace`
the split that reads it back uses, so a target cannot validate as one
name and evaluate as two. The suffix form
is what fits a real graph: `*.reactiveUnwrap` matches
`ember_app/app/utils.reactiveUnwrap` without the declaration having to
know where the helper lives. `via:` says which edge kind is read and is
**required** — every form whose verdict turns on one kind of edge names
it, and only `forbid_reach`, which is deliberately about any path,
omits one. A rule may declare both antecedents; they narrow together,
exactly as every other field of a selector does, so a member must
satisfy each declared clause to be in the rule's scope.

Nothing in the edge antecedent resolves a second component. The near
end is the member fact, the far end is the string the declaration
wrote, and only relations riding the member fact itself are read —
dependency carriers, whose edges belong to a *file*, are deliberately
not folded in, because attributing a file's edges to each member of
that file is precisely the ownership claim this form must not make.
That restraint has one honest cost, and the form pays it out loud: when
the antecedent selects **no** member of the component, the rule emits
one `0.4` advisory — `require rule <id> skipped: no member of
<component> makes a calls edge the antecedent selects` — instead of a
clean report. Two readings reach that state and the advisory names
both, because the facts cannot tell them apart: nobody makes the call,
or the selector and the edges live on different facts (a Ruby class's
calls ride its `Owner#method` facts, and the class fact carries only
what its class body called — an `include`, an `attr_reader`, a
`validates`). Either way the rule looked and found nothing, and a rule
that holds because it looked at nothing must never read as compliance.
The advisory is read off the antecedent's own answers, on the same
representative fact per member the verdict is evidenced from, so a
relation on some other fact cannot certify a component the antecedent
never asked.

The boundary is all-or-nothing, deliberately: a component where some
members answer the antecedent and others are blind to it gets no
advisory, because telling a blind member from one that simply makes no
such call needs a notion of which fact owns which edge that these facts
do not carry.

A breach is a decided-rule finding at confidence `1.0` — the rule is
declared and each membership is either an exact fact name or a target
grounded on the measured file it names, and the verdict says which —
with the rule's `because` in the description. Target resolution fails
closed: an edge whose target names nothing measured is skipped, never
guessed into a violation.

## Decorator discipline — the cached-getter example

The TypeScript extractor records every decorated class member's (and
class's) decorators as a sorted, deduped, space-separated `decorators`
set prop, marks `get` accessors with `symbol_kind: getter`, and counts
each getter's distinct outgoing call edges into `getter_calls` —
emitted even at 0, so measured-cheap and unmeasured never look the
same. That makes a caching convention like "expensive getters carry
`@cached`" declarable over measured facts:

```yaml
components:
  - name: component-getters
    kind: symbol
    match: ["app/components/**"]
rules:
  - id: expensive-getters-carry-cached
    require: component-getters
    when_prop_contains: {prop: symbol_kind, value: getter}
    must_prop_contain: {prop: decorators, value: cached}
    mode: advisory
    because: "mined 2026-08-11 over a large Rails monolith: 106 of 10283
      getters carry @cached (60 of 6992 in components), and even of getters
      with >=5 outgoing calls only 7 of 290 carry it — while every one of the
      106 @cached getters skews expensive (52% loop vs 17% of the uncached).
      The revealed convention is weak: @cached is deliberate, not ambient, so
      the rule is advisory and judged-cheap getters are absorbed by
      witness-named exemptions, never by silencing the rule."
```

Where the expense boundary IS a call the convention names, say so with
`when_edge_to` and let the graph decide who is in scope:

```yaml
rules:
  - id: promise-getters-are-cached
    require: component-getters
    when_prop_contains: {prop: symbol_kind, value: getter}
    when_edge_to: ["*.reactiveUnwrap", "*.getPromiseState"]
    via: calls
    must_prop_contain: {prop: decorators, value: cached}
    because: "a getter that unwraps a promise recomputes on every read unless it
      memoizes; measured 2026-08-13 over a large Ember app: 478 of 6883 component
      getters call one of the two helpers and 463 of them carry no @cached"
```

The antecedent is the criterion itself rather than a proxy for it, and
it needs no new extractor prop: the call relation is already measured,
so a convention about *those two helper names* stays in the
declaration that cares about them instead of entering a general tool's
vocabulary.

Two honesty boundaries the form imposes. The expense boundary itself
is not expressible as a gate — `getter_calls` is a count and
`when_prop_contains` is set membership — so the boundary lives in the
mined evidence carried by `because:` and in exemptions: a getter
judged cheap gets a witness-named exemption, never a weaker rule. And
a member whose `symbol_kind` was never measured (a file class the
extractor cannot classify) is out of the when clause's scope, not in
breach of it. Template read fan-in is deliberately absent from the
expense signals: no template->member edge exists to derive it from —
the .hbs scanner refuses bare `{{name}}` as ambiguous and strict-mode
`.gts` tokens resolve against imports only — and a guessed fan-in is
worse than an absent one.

## Concern rules

Concern discipline composes from the edge forms — no dedicated form
exists because none is needed. "Concerns must not depend on their
includers" is a `forbid … via: calls`; "only models may include model
concerns" is a `protect … via: implements` over the measured include
edges; both sides are named by ordinary path components:

```yaml
components:
  - name: model-concerns
    match: ["app/models/concerns/**"]
  - name: models
    match: ["app/models/**"]
rules:
  - id: concerns-off-their-includers
    forbid: model-concerns
    to: models
    via: calls
    because: "a concern calling its includer is an inheritance cycle in disguise"
  - id: only-models-include
    protect: model-concerns
    owners: [models]
    via: implements
    because: "model concerns assume an ActiveRecord includer"
```

An include whose constant resolves to nothing measured verdicts
nothing — fail closed, like every other target resolution here.

## Existential edges — the first recipe primitive

Everything above forbids: edges that must not exist, members that must
not exist, names and props that must not deviate. `require_edge` is
the vocabulary's first **existential** primitive — the building block
recipes for whole architectural styles compose from — because an
event-driven, plugin, or pub/sub architecture is not defined by what
its parts must avoid but by what must be *wired*: every event has a
handler, every job class is enqueued somewhere, every route is called
by some client, every interface has an implementor. Before this form,
an orphaned event was invisible — nothing forbidden happened; nothing
happened at all — and the only existence check in the system was the
unused-routes explainer's routes-versus-clients census, one hardwired
special case of exactly this shape.

The worked event-driven pair:

```yaml
components:
  - name: events
    match: ["app/events/**"]
    kind: symbol
  - name: handlers
    match: ["app/handlers/**"]
rules:
  - id: every-event-consumed     # an event nothing calls is dead weight
    require_edge: events
    to: handlers
    via: calls
    direction: inbound
    because: "an event nobody consumes is dead weight or a silent contract break"
  - id: every-handler-subscribes # a handler that calls no event is wiring debt
    require_edge: handlers
    to: events
    via: calls
    direction: outbound
    because: "a handler consuming nothing is dead wiring the bus will never invoke"
```

`OrderPlaced` with a measured handler call stays silent;
`OrderCancelled` with none verdicts at `1.0` with the member as its
stable witness. Modes, `exempt:` (witness is the member identity, e.g.
`OrderCancelled has no inbound calls edge from handlers`), the check
gate's delta scoping, and `constraints_for`/plan's obligation
statements (`members of events must have an inbound calls edge from
handlers`) all apply exactly as they do to every other law form.

## Parts that may not depend on each other in a circle: `forbid_cycles`

A rule names a set of parts and holds when no dependency cycle runs among
them. `forbid_cycles` names the first part and `among` the rest, every one
a declared component:

```yaml
rules:
  - id: parts-never-cycle
    forbid_cycles: jobs
    among: [models, mailers]
    because: "parts that reach each other in a circle cannot be taken apart"
```

The reading contracts the module graph to one node per part, admits the
reference and rollup edges (between declared parts a constant reference
is a dependency, and on Ruby it is the only kind there is; associations
stay out, as everywhere), drops self-edges, and reports every strongly
connected component of two or more parts as one finding naming the parts
in the circle and the module edges that close it. A cycle inside one
part is not what the rule states. The repository-wide `cycles` explainer
is unchanged: it excludes those edge kinds because estate-wide they merge
everything, and a declared set is small and named. On the Ruby surface
the law reads `jobs.must_not_cycle_with :models, :mailers`.

A `to_name` literal naming a bare method matches the method of a chained
or receiver-qualified call target as well: `update_all` is the call
whether the extractor recorded it as `update_all`, `where.update_all` or
`Order.update_all`, which is how a law about the mutating persistence
methods holds against a query that reaches them through a relation
chain. A literal carrying a receiver (`Order.update_all`) stays exact.

## Five small spellings

**A naming pair.** `require_name` takes `requires`, a template with one
`*`: a member matching the pattern must have a sibling in the same
component named by the template with the captured base substituted, so
`pattern: "with_*"` with `requires: "without_*"` asks `Room#with_guests`
for `Room#without_guests`. The base is read on the member's own part of
the name, so a method on another class never satisfies it. On the Ruby
surface: `chat.names_must_match "with_*", requires: "without_*"`.

**A public surface by path.** A component takes `public`, a list of
bounded globs naming the files that are its visible surface. The
`private` form then decides visibility by path: inside those files a
member is the surface, outside them it is internal, whatever the
language's own keyword says. Ruby marks every method exported, so this
is how a Ruby component states a surface at all. On the Ruby surface:
`part :billing, files: "app/billing/**", public: "app/billing/public/**"`.

**A receiver-qualified literal.** A `forbid` with `to_name` takes
`receiver: none` to match only call targets with no receiver part, so
`params` alone is named and `request.params` is not; the default, `any`,
matches bare, chained and receiver-qualified forms alike. On the Ruby
surface: `models.must_not_call "params", receiver: :none`.

**Why a file belongs where it belongs.** `enola constraints explain
<path>` names the components whose selectors admit a fact in the file,
the selector that did it, and the edges the file's facts make, read off
the same membership the evaluator verdicts on, so the sentence and the
verdict cannot disagree. `--json` prints the same as data.

**A strict Rails arrangement.** `rails-strict` ships as a recipe: the
Rails laws, the request API kept out of models and services with
`receiver: none`, no circle among the parts, and concerns that stay
independent of their includers over an optional `concerns` role.

## A module never reaches the classes that include it: `independent`

```yaml
rules:
  - id: mixins-stay-independent
    independent: concerns
    because: "a mixin that knows its includer is half a class in hiding"
```

For each member module, the includers are the classes whose **resolved**
ancestry includes it, read off the ancestry a provider emitted (the
Rubydex provider does). The member's own edges, the edges of the methods
it encloses and the edges its files carry are walked over every rule-via
kind; one landing on an includer or on an includer's member is one
finding. When the snapshot holds no resolved ancestry the rule emits one
0.4 finding saying which provider would settle it and no verdict, the
same refusal the `ancestor:` key makes. It takes no `via`. On the Ruby
surface: `concerns.must_not_reach_includers`.

## A protocol satisfied by one of several methods: `any_of`

`require_defines` takes `any_of` beside `method`, exclusive with it: a
class member satisfies the rule by defining at least one of the named
methods, and the finding names the whole list.

```yaml
rules:
  - id: entry-point
    require_defines: services
    any_of: [call, run]
    because: "a service answers to one of two doors"
```

On the Ruby surface: `services.must_define_one_of :call, :run`.

## Protocol ordering — structural conformance, never runtime order

`protocol` closes the last gap in the rule vocabulary's expressiveness
table: ordered interaction sequences. It does so with a form that is
honest about what a static fact graph can and cannot verify.

**What a static graph cannot verify:** temporal ordering. "validate is
CALLED before charge at runtime" is a claim about execution sequence,
and no dependency snapshot — however complete — can back it. A tool
that graded runtime order from static edges would be lying about its
own evidence, so this form does not.

**What it mechanically verifies instead:** structural protocol
conformance. A member of the protocol component that makes a measured
`via` edge into step K's surface without measured `via` edges into
every earlier step's surface is a caller that *structurally skips a
mandatory step* — it demonstrably wires the later step and demonstrably
does not wire the prerequisite. That absence is decidable from the
graph, verdicted at the same proof-class `1.0` every decided rule
gets, and every violation description states the boundary in as many
words: structural protocol conformance, not runtime ordering.

The worked checkout protocol: `CompleteFlow` calling all three steps
stays silent; `OrderFlow` calling `validate-cart` and `charge-payment`
but never `reserve-stock` verdicts as `OrderFlow calls charge-payment
without reserve-stock` (the highest skipped step titles the witness;
the description lists every missing prerequisite); a file that touches
no step is a bystander the rule does not bind. Measurability rides the
same extraction census `require_edge` fails closed on: a member whose
(repo, file-extension) class never demonstrably sources `via`-kind
edges is **skipped by name** in one 0.4 advisory per rule — a wiring
file whose calls the extractor cannot see might be a step-skipping
caller, and that silence must stay visible. A step whose component
matches nothing raises the ordinary dead-selector advisory.

Ordering claims carry a **verification level** by design: every
compiled protocol rule fact carries `verification: structural`, the
only level this snapshot can honestly claim, and the schema leaves
room for a future `observed` level owned by the runtime provider —
which could capture real call sequences and verdict actual order the
way the structural level verdicts wiring. Only the structural level
exists today, and nothing in the system pretends otherwise.

Everything else composes as usual: modes, `exempt:` (the witness is
the violation identity, e.g. `OrderFlow calls charge-payment without
reserve-stock`), the check gate's delta scoping, and
`constraints_for`/plan's obligation statement (`members of
checkout-callers that reach charge-payment via calls must also reach
reserve-stock, validate-cart, in the declared order of obligation —
structural conformance, not runtime ordering`). A protocol rule in a
recipe references roles as its steps, so one declared order
instantiates per bounded context — the checkout example above is the
natural recipe body. With this form the ArchSpec parity table's
protocols family graduates from partial to **covered-structural**: the
structural half of ordered-interaction sequences is expressible and
verdictable, the runtime half remains future provider work, and the
parity re-measure belongs to the next harness run.

## Laws only a graph can state

Five forms and two component keys read what only the fact graph holds:
storage facts, the routes behind code, the seams between repositories,
the pages compiled from a knowledge base, and the history of every
snapshot. Each refuses by name when the snapshot cannot answer, so silence
never reads as compliance.

- `storage_stays_home: <component>` holds when every storage fact a
  member reaches (`calls` or `depends_on` to a model a storage fact names)
  is itself a member. The breach names the table and the model, and the
  first suggested action is the owning part's public member that already
  reaches the same table. Ruby: `billing.storage_must_stay_home`.
- `handles: [POST, PUT, PATCH, DELETE]` on a symbol component admits the
  members a route with one of those methods reaches through `handled_by`,
  so `require_edge` states "a mutating action reaches a policy" with
  nothing new. Ruby: `part :mutating_actions, files: "app/controllers/**",
  handles: [:post, :put, :patch, :delete]`.
- `cap_runtime: <component>` with `metric: queries` and `max: N` reads the
  `runtime-queries:` frames a runtime capture measured for files inside
  the component and names every frame over the budget. A snapshot with no
  capture makes the rule unevaluable with the cause `no_runtime_capture`.
  Ruby: `billing.must_keep_budget metric: :queries, max: 20`.
- `require_consumer: <route component>` breaches for every member route
  no loaded client calls, read from the cross-repository route match; a
  single-repository snapshot refuses with `no_counterparty`. Ruby:
  `api.must_have_consumer`.
- `unique_across: <component>` with `by: table` (or `name`) breaches when
  members in two different repositories share the value, naming both
  owners; members from one repository refuse with `no_counterparty`. Ruby:
  `tables.must_be_unique_across by: :table`.
- `governed_by: <page path or glob>` on a component admits the measured
  facts in files the selected pages anchor; `status:superseded` after the
  glob keeps the pages with that status, `supersedes:<page>` the pages
  that supersede it, so "the code of the superseded decision" and "the
  code of the superseding one" are two components and `forbid` states the
  law between them. Ruby: `part :old_way, governed_by: "wiki/shop/adrs/*.md
  status:superseded"`.
- `require_governed: <component>` breaches for every member file no
  compiled page anchors; a snapshot with no pages refuses with
  `no_compiled_pages`. Ruby: `old_way.must_be_governed`.

Two spellings add time. `since: YYYY-MM-DD` on any rule dates it: the
explainer verdicts as usual and stamps the date, and `check` reads the
architecture history's newest revision at or before the date, reports a
breach that revision already carried and grades one it did not; a date
before the first revision keeps every breach graded and adds a descriptive
finding naming the first revision's date. `growth: N` on `cap` lets the
count exceed the baseline's count by N before the cap fails; without a
baseline the cap alone applies. Ruby: `since "2026-08-01"` and `growth 2`
inside a law.

Where the history has no revision at or before the date, git decides
instead: `check` reads the witness line's author date with one
`git blame --porcelain -w` per witness file, reports a breach whose line
was last changed before the date and grades one changed after it, and
remembers the dates it read under `.enola/blame_cache.json` by the file's
blob hash, so an unchanged file never asks git twice. Author time, never
commit time. Three cases cannot be dated and grade as a rule without a
date would, each said once as a descriptive finding: no git or an
untracked witness file, a witness line not yet committed, and a shallow
clone whose boundary commit is newer than the date. A shallow CI checkout
that wants dated rules deepens the clone; the binary does not guess past
the boundary.

Every edge breach (`forbid`, `protect`, `private`) and every cycle breach
now leads its suggested actions with the smallest cut the graph can see:
the far part's public member with the same bare name, else its public
surface, else the part the offender's other edges mostly reach; for a
cycle, the lightest edge of the circle by module edges. When the facts
support none, the action says so rather than offering a generic sentence.

Recipe roles may carry selector defaults (`match`, `kind`, `name_pattern`,
`where`): a binding that gives none inherits the role's, key by key, and a
defaulted role is never required of the binding. A team's own recipe under
`enola/recipes/` can therefore carry its conventions with their selectors,
so the binding in `enola/constraints/` is the recipe's name and the mode
alone, and every path is overridable where a tree differs. The shipped
recipes stay framework-general; house conventions belong in the
repository's recipe, where the team that owns them reviews them.

## Recipes — named patterns as instantiable bundles

A recurring architectural pattern — event-driven, ports-and-adapters,
a migration target state — is the same handful of rules written again
and again with different paths in the component selectors. A
**recipe** names the pattern once: role slots plus parameterized
rules, instantiated per bounded context by binding paths to roles.
Two artifacts carry it:

**Recipe definitions** live in `enola/recipes/<name>.yaml`, beside
the constraints directory — visible source, never under `.enola/`.
One recipe per file: a `recipe:` name, `roles:` slots, and `rules:`
in the full existing rule vocabulary, referencing **roles** instead
of components:

```yaml
recipe: event-driven
roles:
  - name: events
  - name: bus
  - name: handlers
rules:
  - id: events-consumed
    require_edge: events
    to: handlers
    via: calls
    direction: inbound
    because: "An event nobody consumes is dead weight."
  - id: only-bus-calls-handlers
    protect: handlers
    owners: [bus]
    via: calls
    because: "Handlers are reached through the bus, never directly."
  - id: events-are-named
    require_name: events
    pattern: "*Event"
    because: "The suffix is the contract."
```

**Instantiations** live in the existing `enola/constraints/*.yaml`
files, as `use_recipe:` entries binding each role to a real component
selector (the same `match`/`service`/`kind`/`name_pattern`/`where`
narrowings a component takes, so a role a recipe only reads the props
of can be bound to a concept — `surface: { where: { superclass:
StandardError } }` — as readily as to a directory; a role some rule in
the recipe resolves against an edge cannot, and binding a `where:` to
one is refused on the EXPANDED declaration, naming the expanded
component):

```yaml
use_recipe:
  - recipe: event-driven
    as: orders-events
    bind:
      events:   { match: ["app/events/orders/**"] }
      bus:      { match: ["app/lib/event_bus.rb"] }
      handlers: { match: ["app/handlers/orders/**"] }
    mode: advisory
    exempt:
      - rule: events-consumed
        witness: "LegacyOrderMigratedEvent has no inbound calls edge from orders-events/handlers"
        owner: "dana"
        because: "Fired only by the migration backfill, consumed manually."
        since: "2026-08-11"
```

**Recipes are a compile-time concept.** Each instantiation expands
into ordinary components (`orders-events/events`) and ordinary rules
(`orders-events/events-consumed`) at load time, with role references
substituted for instance components, and the expanded set flows
through the same validation, compilation and evaluation machinery
every hand-written rule uses — the engine never sees a recipe. That
is what makes every existing capability compose for free: modes,
exemptions, guidance, lifecycle telemetry (per instance-prefixed rule
id), mining, `plan`/`constraints_for`, check rendering, the Exempted
bucket, dead-exemption warnings. The instance-wide `mode:` overrides
every expanded rule's mode; per-rule modes declared in the recipe are
the defaults when the instance declares none. Exemptions attach at
the instance, scoped to a recipe rule by its unprefixed id, because a
template cannot know concrete witnesses — a recipe rule carrying
`exempt:` is a validation error.

Validation is file-cited and fail-closed, like everything else in the
vocabulary: a recipe rule referencing an undeclared role, an
instantiation missing a binding for any role the rules reference,
binding a role the recipe does not declare, duplicate recipe names
across files (both cited), duplicate instance names across files
(both cited — the expanded ids would collide), an exemption naming a
rule the recipe lacks, and `use_recipe` inside a recipe (no recursion
in v1) are all errors. The one warning: a declared role no rule
references is a **dead role** — reported, never fatal.
`constraints lint` lists each recipe (name, roles, rule count) and
each instantiation under its declaring file (instance, recipe,
bindings, expanded rule count), and expanded rules keep their
provenance all the way to the verdict: a violation's description
traces to `rule orders-events/events-consumed (recipe event-driven,
instantiated in enola/constraints/orders.yaml)`, so a reviewer can
walk law back to pattern.

Two recipes this vocabulary is aimed at, as sketches — documented
here, not shipped files, because each repo binds its own paths:

**The vanilla Rails views recipe** (Stimulus/Hotwire, no SPA): the
extractors already measure markup wiring — `stimulus-binding` facts
from `data-controller` attributes resolved to
`app/javascript/controllers/` at `markup-declared` level, and
`turbo-frame` declaration/reference facts from `turbo_frame_tag`. A
`rails-views` recipe binds `views`, `controllers` (Stimulus), and
`components` roles per domain: `require_edge` demands every Stimulus
controller is bound by some view (inbound, so a dead controller is a
breach instead of invisible), `require_name` holds the
`*_controller.js` convention, and a `forbid` keeps views from
reaching application services directly. Instantiated once per domain
slice, the same three rules police every slice's markup wiring
without a per-domain rewrite.

**The Ember-to-Rails page-migration recipe**: the migration's target
state, declared per page. Roles for the `rails-page` (the new
views/controller subtree) and the `ember-remnant` (the legacy route's
app code); the rules are `forbid_fact` on the remnant (a migrated
page's Ember code must be gone), `require_edge` demanding the Rails
page is actually routed, and naming/reach rules for the new subtree's
conventions. Each page migration is one `use_recipe` entry — and
because every instantiation expands to rules with stable
instance-prefixed ids, the drift telemetry that trends mining runs
counts conforming pages over time: the migration's progress is the
number of instances whose rules verdict clean, measured, not
asserted.

## Cross-repo rules

With service-scoped components the rule forms reach across
repositories — the edges are the ones the cross-repo linker measures
(service-to-service and finer), so "the frontend must not touch
billing's internal surface" is one forbid:

```yaml
components:
  - name: frontend
    service: frontend
  - name: billing-internal
    service: billing
    match: ["internal/**"]
rules:
  - id: no-internal-reach
    forbid: frontend
    to: billing-internal
    via: calls
    because: "internal surfaces are not a contract"
```

The counterparty rule intentcheck's seams follow applies here too: a
component naming a service absent from the snapshot is **unasked** —
every rule naming it emits no verdicts, because a snapshot cannot
answer for a repo it does not contain — and one 0.4 advisory
(`Constraint component … names service … not present in this
snapshot`) keeps the silence visible, exactly like the dead-selector
advisory.

## Modes

- **`ratchet`** (the default): breaches verdict at `1.0` and the check
  gate fails **new** ones — pre-existing violations stay silent, the
  same delta scoping every finding gets.
- **`advisory`**: breaches report at `0.9`, deliberately below the
  gate's floor, titled `Advisory constraint … violated` — the
  declaring file chose reporting over enforcement.
- **`strict`**: breaches are titled `Strict constraint … violated` and
  fail `enola check` **even when the baseline already carried them** —
  the one deliberate exception to delta scoping, for rules decided to
  hold *now* rather than merely to stop getting worse.

A strict violation's only override is the **suppression ledger**, a
committed `.enola/suppressions.yaml` the gate only ever reads:

```yaml
entries:
  - rule: company-fk             # or finding_title_prefix: "…" — exactly one
    owner: alice
    reason: "legacy tables migrate in Q4"
    date: "2026-08-10"
```

Every entry is a signed excuse — owner, reason and date are required,
parsing is strict, and an invalid ledger rejects as a whole. A
suppressed finding is reported in the verdict's own `Suppressed`
bucket (text and JSON) and never fails; the ledger applies to ratchet
findings too. enola never writes this file.

## Exemptions — declared carve-outs

A rule may carry an `exempt:` list: declared, reasoned carve-outs
riding the law itself. Each entry names one **witness** — the exact
violation identity the rule would otherwise report, the same string a
violation is titled with (`users must have fk_constraints containing
company_id->companies`, `app/domain/billing -> app/adapters/http via
depends_on`) and the same identity the lifecycle ledger folds on —
plus who decided the carve-out, why, and when:

```yaml
rules:
  - id: company-fk
    require: tables
    when_prop_contains: {prop: columns, value: company_id}
    must_prop_contain: {prop: fk_constraints, value: company_id->companies}
    because: "tenant isolation joins through companies"
    exempt:
      - witness: "legacy_imports must have fk_constraints containing company_id->companies"
        owner: dana
        because: "legacy_imports keys company_id to the archived companies snapshot, not companies"
        since: "2026-08-01"
```

All four fields are required — an exemption without an owner, a
reason and a date is how a violation becomes permanent silently, so a
partial entry rejects at parse, cited under its declaring file.
Witnesses are matched exactly; there are no glob forms.

An exempted witness produces **no violation in any mode** — ratchet,
advisory, strict, all unchanged for every other witness. Instead it
produces one `Exempted from constraint <id>: <witness>` finding at
`0.9`: rendered in `enola check`'s own `Exempted by declaration`
bucket (on every run, not only the one that introduced it) and in the
insight listings, always carrying the owner, the date and the reason —
counted, never silent. An exemption whose witness matches nothing the
rule reports is a **dead exemption**, warned at `0.4` like the
dead-selector advisory: it either outlived its violation (delete it)
or never matched (fix the witness).

Exempted witnesses are decisions, not debt: the lifecycle ledger
keeps them out of a rule's standing-violation set and the lifecycle
report lists them separately ("N exempted by declaration").
`constraints lint` validates every entry and counts exemptions per
file; `constraints_for` and `plan_check` report each bound rule's
exemptions with their reasons, so an agent about to edit sees the
carve-out beside the law. Mining proposes no exemptions ever — it
reports reality, and a carve-out is a decision only an operator signs.

The decision hierarchy, strongest first — reach for the earliest one
that is true:

1. **Fix it** — the violation is wrong and the rule stands. No
   vocabulary needed.
2. **Exempt it** (`exempt:` on the rule) — the witness is *decided to
   be out of the rule's scope*, permanently and with a reason, and the
   decision should live beside the law it carves out of.
3. **Suppress it** (`.enola/suppressions.yaml`) — the violation is
   real and stands, but strict-mode enforcement must not block while
   it is being worked off: a temporary, signed excuse in the gate's
   own ledger, separate from the declaration.
4. **Baseline it** (ratchet's implicit merge-base baseline) — nobody
   decided anything: the violation predates the rule and the ratchet
   merely stops it getting worse. Invisible and unreasoned, which is
   exactly why anything decided deserves one of the forms above.

## Guidance rules

Everything above is law — a rule states what the architecture must
not do, and a breach is a decided finding. The `guide` form is
**steering**: "similar implementations here used X; consider it." It
names a component and carries the advice itself, with optional
exemplars pointing at prior art:

```yaml
components:
  - name: components
    match: ["app/components/**"]
rules:
  - id: getters-cached
    guide: components
    message: "Expensive derived getters here use @cached — consider it (see exemplars)"
    exemplars:
      - app/components/sortable-table.js
      - app/components/avatar-stack.js
    because: "recomputing derived state on every render is the recurring perf bug here"
```

`message` is required — the advice is what a guidance rule delivers.
`exemplars` name prior art by repo-relative file path or exact fact
name; they are shape-checked at parse time (non-empty, whitespace-free)
but **never required to exist** — prior art may move without the
advice going stale. Existence is a delivery concern: `constraints_for`
annotates each exemplar `present`/`absent` against the current
snapshot (a measured fact carrying it as its file or its exact name;
fail closed — unresolvable is absent), and `constraints lint` reports
absent exemplars as a note, never an error. Presence is a
**tri-state**: with no snapshot to measure against (`plan`'s
declarations-only mode) every exemplar is `unmeasured`, rendered
`unmeasured — no snapshot` — "absent" and "never looked" must never
read the same.

The whole point is **pre-edit** steering: a `constraints_for` query
for a target inside a guided component — including a file that does
not exist yet — returns the guidance in its own `guidance` list,
separate from the law under `rules`: message, mode, annotated
exemplars. Two modes, both non-enforcing:

- **`notify`** (the default): the contract channel only — **no
  finding, ever**.
- **`advisory`**: additionally ONE `0.9` finding per guided component,
  titled `Guidance for <component>: <rule id>` — never one per member,
  because guidance is not a violation census — so the advice rides
  `check` output visibly and can never fail anything.

The enforce-class modes (`ratchet`, `strict`) are rejected on a
guidance rule at validation — and so is `exempt:`, because guidance
emits no violations to exempt. Graduation to law means writing a law
form — a `forbid`, a `require`, a `require_name` — on the declaring
file, not hardening the guidance.

Guidance also rides the gate: **advice travels with the diff, never
gates.** When `enola check` grades a delta, every guidance rule whose
component contains a file the change touched — added, removed or
modified, derived from the fact delta — renders in its own
`Guidance for this change (N)` section: rule id, message, `because:`,
and the exemplars with their tri-state presence. The JSON verdict
carries the same entries in a `guidance` array (rule, component,
message, mode, because, exemplars, matched changed files), sorted by
rule id and stable across runs. A guidance entry is **not** a
violation, is counted by no failure policy, and never moves the exit
code in any mode combination; guidance for components the delta never
touched stays silent, so a ten-file change surfaces only the advice
those ten files selected. A partial (intersection-graded) verdict
carries guidance the same way, over its own graded delta; a declined
or errored gate carries none, because there is no trustworthy delta
for the advice to travel with.


## Laws written in Ruby

Shipped in v0.4.4.

A repository whose team writes Ruby may write its laws in Ruby. Files
ending in `.rb` in `enola/constraints/` are read beside the YAML ones,
parsed with the Ruby grammar the extractors already carry and **never
executed**, and compiled to the same declaration the YAML loader
produces: the same merge order, the same per-file provenance stamp, and
the same evaluator, lint surface and pre-edit contract. A repository may
hold one of each while a team moves.

A declaration has two levels. `part` names a piece of the application in
the team's own words; `rails` declares the conventional parts of a Rails
application from the directories Rails puts them in, so a team writes
only what is theirs. A `law` is a sentence, its reason, and optionally
its mode and its carve-outs.

```ruby
Enola.architecture "storefront" do
  rails
  part :service_objects, files: "app/services/**", kind: :symbol,
                         where: { symbol_kind: "class" }, owns: :methods

  law "background jobs never invoke controller code" do
    jobs.must_not_call controllers
    why "rendering from a job goes through ApplicationController.renderer"
    seen_in "2,552 of 2,557 call edges"
  end
end
```

Nineteen verbs cover the 21 rule forms, and a test walks the form
table and fails if any form cannot be reached from a verb, so a form
added later without a way to say it breaks the build rather than
quietly having no surface.

| Sentence | Form it compiles to |
|---|---|
| `a.must_not_call b` | `forbid` / `to`, via `calls` unless another `via` is named |
| `a.must_not_reach b` | `forbid_reach` / `to` |
| `a.may_only_call b, c` | `allow` / `only` |
| `a.is_reached_only_by b` | `protect` / `owners` |
| `a.must_be_reached_by b` | `require_edge` / `to`, inbound |
| `a.must_reach b` | `require_edge` / `to`, outbound |
| `a.stays_inside except: b` | `private` / `except` |
| `a.must_follow b, c` | `protocol` / `steps` |
| `a.must_define :call` | `require_defines` / `method` |
| `a.names_must_match "*Job"` | `require_name` / `pattern` |
| `a.names_must_not_match "get_*"` | `forbid_name` / `pattern` |
| `a.must_be_empty` | `forbid_fact` |
| `a.at_most 12` | `cap` / `max_members` |
| `a.must_carry prop: "framework", value: "rails"` | `require` / `must_prop_contain` |
| `a.advises "prefer a slot"` | `guide` / `message` |

A part is written in snake_case because that is what a Ruby file reads
like, and a component name is a lowercase token, so the underscore
becomes a dash on the way through: `part :service_objects` is the
component `service-objects`.

### A Rails and Ruby catalogue

Laws a Rails codebase can state today, each compiling to a form above.
They are written to be read and adapted rather than copied: the parts
they name come from `rails`, and the reasons are the ones a team would
actually give.

```ruby
Enola.architecture "storefront" do
  rails
  part :service_objects, files: "app/services/**", kind: :symbol,
                         where: { symbol_kind: "class" }, owns: :methods
  part :queries,         files: "app/queries/**"
  part :maintenance,     files: "app/tasks/**"
  part :public_api,      files: "app/controllers/api/**"
  part :legacy,          files: "app/legacy/**"

  # Layering: what may reach what.
  law "background jobs never invoke controller code" do
    jobs.must_not_call controllers
    why "a job that renders goes through ApplicationController.renderer"
  end

  law "models never reach controllers, however indirectly" do
    models.must_not_reach controllers
    why "a model that knows the request cannot be used off the request"
  end

  law "controllers reach the database through queries and services only" do
    controllers.may_only_call queries, service_objects
    why "a controller that builds its own scope cannot be reused or tested apart from the request"
  end

  law "the public API is reached only by controllers" do
    public_api.is_reached_only_by controllers
    why "an internal caller taking the API path skips authorization written at the controller"
  end

  # Shape: what a member must be.
  law "a service object has exactly one door" do
    service_objects.must_define :call
    why "callers never reach a second public method, so the object can change behind it"
  end

  law "every mailer action is delivered, never called" do
    mailers.must_be_reached_by jobs
    why "mail sent inline in a request makes the request wait on SMTP"
    mode :advisory
  end

  # Naming: the conventions a reviewer repeats.
  law "jobs are named for the queue that runs them" do
    jobs.names_must_match "*Job"
    why "the scheduler discovers jobs by their suffix"
  end

  law "policies are named for the model they authorize" do
    policies.names_must_match "*Policy"
    why "Pundit resolves the policy class from the record's class name"
  end

  law "maintenance tasks live in the Maintenance namespace" do
    maintenance.names_must_match "Maintenance::*"
    why "the gem resolves task constants from it; a task outside never appears in the runner"
  end

  law "no get_ prefixes on a model's public surface" do
    models.names_must_not_match "get_*", surface: :exported
    why "a reader is a noun; get_ says the class is a bag of fields"
  end

  # Size and drift.
  law "the public API surface stays reviewable" do
    public_api.at_most 40
    why "an API that grows without a decision is an API nobody decided"
    mode :advisory
  end

  law "app/legacy is frozen" do
    legacy.must_be_empty
    why "new code lands in app/domain; the directory exists only until it is empty"
  end
end
```

Each law carries its reason because every finding surfaces it: a
violation says why the rule exists rather than only that it was broken.
`seen_in` appends the measurement a law was mined from, which is what
separates a law the estate actually keeps from one somebody wished for.

A part may also be selected by ancestry: `part :records, ancestor:
"ApplicationRecord"` holds every class whose resolved chain reaches that
name, and a `bind` takes `ancestor:` the same way.

Beside the verbs, a law may carry `id` (when a finding's token must stay
stable across a rewording), `why` and `seen_in` (its reason and the
measurement behind it), `mode`, `via`, `direction`, `exemplar` (prior art
for a guidance law), `when_carrying prop:, value:` and `when_calling
"literal", via:` (the antecedents that narrow a demand to the members it
is about), and `exempt "witness", because:, owner:, since:` (a carve-out
that says who owns it and when it was taken). A far end written as a bare
name is a part this declaration selected; written as a string it is a
literal the graph recorded, which is the difference between naming
something we declared and something we merely measured.

A repository adopts a convention set it did not author by instantiating a
recipe, binding each role the recipe declares to its own parts:

```ruby
use_recipe :ember_conventions, as: :app, mode: :advisory do
  bind :components, files: "app/components/**"
  bind :fetchers, files: "app/services/**", kind: :symbol, where: { symbol_kind: "class" }
end
```

Nothing in the surface is Rails-specific except the `rails` line, which is
sugar for parts a Rails layout already names. Every other construct takes
globs, predicates and services, so a Go service, an Ember application and
a Python worker declare their laws the same way.
## Recipes that ship with enola

A convention set nobody can adopt in one line is a convention nobody
adopts, so some ship with the binary. `rails-conventions` is the first:
seven laws about where a Rails application's parts may reach, each
carrying its reason, bound to the repository's own directories at the
instantiation site.

```yaml
use_recipe:
  - recipe: rails-conventions
    as: app
    bind:
      controllers: { match: ["app/controllers/**"] }
      jobs:        { match: ["app/jobs/**"] }
      models:      { match: ["app/models/**"] }
      mailers:     { match: ["app/mailers/**"] }
      policies:    { match: ["app/policies/**"] }
      serializers: { match: ["app/serializers/**"] }
      view-components: { match: ["app/components/**"] }
```

A shipped recipe is a recipe like any other: it declares roles, its rules
carry `because:`, it is verdicted through the same evaluator, and its
findings cite `enola:recipes` as the file they came from rather than a
path that exists in no repository.

The rest describe arrangements rather than frameworks, so they apply to
any language the extractors read. `layered` names presentation,
application, domain and infrastructure, and holds the direction of the
calls between them. `ports-and-adapters` keeps a core that names ports and
never the adapters implementing them. `modular-monolith` holds a module's
internals private to it while letting its public surface be called.
`event-driven` separates publishers from handlers and asks that every
event declared has somewhere to land.

```yaml
use_recipe:
  - recipe: ports-and-adapters
    as: billing
    bind:
      core:     { match: ["lib/billing/**"] }
      ports:    { match: ["lib/billing/ports/**"] }
      adapters: { match: ["lib/billing/adapters/**"] }
```

Each one is three or four roles and three to six laws, so adopting an
arrangement is a paragraph of binding rather than a file of hand-written
rules, and the laws arrive already carrying the reason they exist.

Four more ship beside them. `vanilla-rails` is plain Rails: the extra
directories (services, forms, policies, decorators, presenters) must stay
empty, each with a stated reason, and models never reach controllers.
`clean` is four rings (frameworks, interface adapters, use cases,
entities) with every outward reach forbidden. `cqrs` splits commands,
queries and read models, and adds the one law the split exists for: a
query never calls a mutating persistence method, stated as a `to_name`
literal list over calls. `ruby-conventions` bans the `get_`, `set_` and
`is_` prefixes over whatever part the repository binds as its code.

A recipe may mark a role **optional**. A binding may leave it out, the
rules that reference it are expanded away for that instantiation, and
the lint surface names each law the binding did not take, so a recipe
can grow a role without breaking every repository that already binds it.
`rails-conventions` grew `helpers` and `services` this way: services and
models never reach helpers, services never reach controllers, and the
request API (`render`, `redirect_to`, `params`, `session`, `cookies`,
`flash`) stays out of models and services, all advisory, all in force
only where the two roles are bound.

> The rule `supply-chain` states — every external dependency declared, pinned, and
> given a stated purpose — is H14 from Stanislav Rumega's position paper
> [*Tell Your Coding Agent to Work as an Architect First*](https://github.com/styrumg/Architect-First-Article).

`supply-chain` is the odd one out, and worth reading as the shape rather
than the rule. It has one role and one law — every declared dependency
names a version — and the role binds to nothing the repository has to
name, because what an unpinned dependency IS is a property the facts
carry rather than a place in the tree:

```yaml
use_recipe:
  - recipe: supply-chain
    as: supply
    bind:
      unpinned-dependencies: {}
```

The role's own selector is `kind: dependency` with
`where: {type: package, pinned: false}`, over the package facts the
`manifests` extractor measures (see [INTENT.md](INTENT.md)), and the law is a
`forbid_fact` over it — no new rule form, because "this set must be empty" was
already one. `dependency` is an opt-in member kind for the same reason
`test_ref` and `lint` are: a component that does not name it never acquires a
package, so no existing rule changes what it judges.

One caveat before adopting it: `where:` fails closed and loudly on a property
no measured fact carries, so a repository with no manifest enola reads gets a
`1.0` *selector cannot be evaluated* finding rather than silence. A rule that
holds because it looked at nothing must never read as compliance — but it is
surprising the first time.

**A first declaration in one command.** `enola constraints init [repo]`
reads the shipped recipes, binds every role whose conventional directory
the repository has, and writes one `use_recipe` per recipe whose required
roles all resolved to `enola/constraints/recipes.yaml`, refusing to
overwrite. A recipe missing a required directory is not bound and the
output says which; nothing is guessed. `--dry-run` prints instead of
writing and `--recipe NAME` limits the binding to one recipe.

**A repository still authors its own**, under `enola/recipes/`, and a
local recipe of the same name replaces the shipped one entirely. What a
team wrote about its own codebase beats what arrived in a binary, and the
replacement is reported rather than silent, so nobody has to wonder which
one ran. The two laws in `rails-conventions` that report on a
crossing rather than a breach (jobs and models reaching a controller,
where `ApplicationController.renderer` is the sanctioned path) ship as
advisory for that reason.

## `constraints lint`

The authoring loop. `enola constraints lint` parses the declaration
(repo file, `enola/constraints/` files — each listed with its own
component and rule counts, plus its `use_recipe:` instantiations —
`enola/recipes/` definitions, and any cluster override), reports **every**
validation problem with its file context rather than dying on the first, and —
when a snapshot exists on disk — resolves each component against it so
you see what a selector actually selects before a rule built on it
verdicts anything. No snapshot degrades to a named validation-only
mode; nothing is generated or written. Exit `1` on validation
problems, `0` otherwise.

## `constraints mine`

Discovering the law instead of writing it. `enola constraints mine`
walks the current snapshot's fact store for **near-invariants** —
high-regularity properties with named exceptions — and reports each
one as a candidate constraint declaration in this vocabulary. Four
regularity families are mined:

- **Prop implications** (`require` + `when_prop_contains` /
  `must_prop_contain`): facts of one kind whose prop A contains X
  nearly always have prop B containing Y — the company-fk shape
  ("storage facts whose columns contain company_id nearly always
  have fk_constraints containing company_id->companies"), plus the
  unconditional form when a prop value holds across nearly the whole
  kind. A conditional candidate must beat the consequent's base rate,
  or the antecedent added no information and the unconditional form
  is the honest rule.
- **Naming** (`require_name` + `pattern`): facts of one kind under a
  directory subtree nearly all matching one bounded pattern
  (`prefix*` / `*suffix`), mined at word boundaries and emitted only
  where the cluster beats the whole population's match rate.
- **Edge regularities** (`forbid`/`to` and `allow`/`only`): via-edges
  leaving a directory cluster nearly never land in some other cluster
  (with the actual crossings named), or land almost entirely inside a
  small set of clusters. A forbid candidate needs at least one
  would-be violation: a zero-crossing pair is indistinguishable from
  no opportunity and would flood the report with unevidenced law.
- **Method presence** (`require_defines` + `method`): plain classes
  (no inheritance, no mixins — the same fail-closed scope the
  evaluator uses) under a cluster nearly all defining one method.

Every candidate carries its regularity as a numerator/denominator,
**names every exception** (fact and file), and renders a would-be
declaration that `constraints lint` accepts verbatim — the emitted
YAML round-trips through the real parser, and the named exceptions
are exactly the violations the rule would report if adopted. The
report is ranked by confidence x support; the support floor,
confidence floor and exception ceiling are flags
(`--min-support`, `--min-confidence`, `--max-exceptions`), printed in
the report header, and anything below a floor is suppressed **with a
count**, never silently. `--jsonl` writes the full report as an
artifact beside the ranked text.

Every candidate also carries a **stable identity** — the regularity's
semantic key, built from what the rule is *about* (family, scope, and
the rule's own parameters: the antecedent/consequent prop pair, the
cluster and pattern, the source/target clusters and via, the cluster
and method) with the parts pipe-joined and escaped. It deliberately
excludes everything that moves between snapshots: the rank, the
numerator and denominator, the exception list, and the statement text
the numbers are printed into. Mining the same repository twice
therefore names the same regularity with the same identity even as
its numbers shift, which is what makes candidates from different runs
foldable into a time series: a regularity is *the same rule observed
again*, never *the same rank re-occupied*. The identity is exported
on every candidate line of the `--jsonl` artifact.

**Candidates are proposals, never self-adopting law.** Mining reads
an existing snapshot and writes nothing: it never generates a
snapshot, never touches `enola/constraints/`, never modifies a
declaration, and never feeds the check path. Adopting a candidate is
the operator's act: copy the would-be declaration into a file under
`enola/constraints/`, rewrite `because:` into the real rationale (the
mined text is evidence, not a decision), review the mode (candidates
propose `advisory`; graduation to `ratchet` or `strict` is a
decision), reconcile its components with ones already declared, and
commit it for review like any other law. Exit `0` when a report was
produced (even an empty one), `2` when there is no snapshot to mine.

**Rules that belong in the linter start there.** Some regularities
need the graph (a call edge the linker resolved, a prop implication,
a method's presence across a cluster) and some are file-local
syntax: a naming regularity over the classes, functions and
top-level bindings declared in JavaScript or TypeScript files under
a directory, or a forbidden `import` from one directory into
another. `--scaffold-eslint DIR` writes the second kind as ESLint
rule scaffolds under `DIR`: a rule module per candidate, a
RuleTester test whose valid cases are the candidate's conforming
witnesses and whose invalid cases are its named exceptions, and an
`index.js` registering them, so the directory loads as a plugin and
each file moves into the repository's own plugin unchanged. The
TypeScript extractor qualifies a symbol with its module path
(`src/services.ApiError`) and names members through their class
(`src/commands/repo.RepoClone.description`); the scaffold cuts both
down to the declaration the rule can see, and a pattern that is only
the module path is a tautology the miner no longer ranks. Every
candidate the scaffolder leaves is listed with the reason it stays
a constraint proposal. Nothing is written to the repository's plugin
and no ESLint configuration is touched: the scaffold is a starting
point the operator reviews, like the would-be declaration.

## `constraints ledger` — how much of the law is being excused

Every excuse this vocabulary admits already demands a signature. A suppression
entry names an owner, a reason and a date, and the ledger rejects as a whole if
any is missing. An `exempt:` carve-out names the same three beside the witness
it covers. What was never asked is the question those signatures answer
together: **how much of the declared law is being excused rather than obeyed.**

`enola constraints ledger` asks it:

```
law: 34 rules (28 ratchet, 4 advisory, 2 strict) · 12 breaches · 5 excused (42%) · oldest excuse 214 days · 2 excuses matched nothing

company-fk [ratchet] — 7 breaches reported, 3 excused · declared in enola/constraints/billing.yaml
    because: tenant isolation rides the company FK
    exemption by alice (2024-11-02, 296 days ago) — users must have fk_constraints containing company_id->companies
        "legacy tables migrate in Q4"
    suppression by bob (2026-06-01, 85 days ago)
        "second signature while the migration lands"
```

Three things in that output are the point, and none of them is visible one
verdict at a time.

**The rate.** A rule most of whose breaches are signed away is a rule to
reconsider — the declaration is asking for something the codebase has decided
not to do, and the team is paying a signature per occurrence to say so. Read
one excuse at a time, that looks like thirty separate reasonable decisions. Read
as a ratio, it is one wrong rule. The gate cannot make that judgement and
should not try; what it can do is make the number available to whoever can.

**The age.** An excuse nobody has revisited in a year is a reason that has
probably stopped being true. `since:` and `date:` were mandatory long before
anything read them; this is what they were mandatory *for*.

**The ones that match nothing.** An excuse that excused nothing in this
snapshot is the row somebody can act on today: the breach it covered was fixed,
moved, or stopped being selected, and the signature is still standing. It is
marked inline and counted on the summary line. The snapshot's own census
already reported the same two facts as `unused suppression` and
`exemption matching nothing`; the ledger is where they acquire a denominator.

The report reads the **snapshot's** compiled intent facts, not the working
tree's YAML — deliberately, so both halves of every ratio come from one state
of the law. A rule declared since the snapshot was generated is therefore
absent, and the report prints the snapshot's timestamp so a reader can tell.
This is the opposite choice from `plan`, which answers governance from the
working tree precisely because its question is about an edit that has not
happened yet.

**A report, never a gate.** It exits `0` whenever it produced a report. Nothing
here changes what `enola check` fails on, and nothing here fails a build: a gate
that failed on its own unpopularity is the one nobody would leave enabled — the
false-alarm economy of [EXPLAINERS.md](EXPLAINERS.md) applied to the gate's own
record rather than to its findings.

`enola check` carries the same summary as one line beside its census line, and
as a `law` object in `--format json`. A repository that declares no rules gets
no line and no object at all: undeclared is unasked, and a zeroed ledger would
read as a law with nothing wrong with it.

## `plan` / `plan_check` — the pre-edit contract

The contract, moved into the planning loop. `enola plan` (and the
`plan_check` MCP tool, the same code path) answers, **before any edit
lands in the tree**: which declared constraints govern the intended
change, what the change's blast radius is, and — for a patch — which
constraint verdicts WOULD appear if it were applied.

Three input forms:

- `--paths a.rb,b.rb` (or positional paths): for each path, the
  declared components whose selectors cover it — a path nobody has
  written yet still answers, which is the pre-edit point — with every
  rule binding them (statement, mode, `because:`, declaring file),
  plus the path's blast radius: fan-in and fan-out over the current
  snapshot's rule-via edges, exact counts with capped, sorted samples.
- `--symbols X,Y`: the same, keyed by exact fact name. A name nothing
  measured carries is reported as unmeasured, never guessed at.
- `--patch change.diff`: the **counterfactual**. The unified diff is
  applied to a scratch copy of the repository — the working tree and
  its `.enola` are never touched — facts are regenerated over the
  scratch tree and over the unpatched tree, the constraints engine
  verdicts both, and the delta is reported in three buckets: **new**
  (violations the patch would introduce, each naming the rule, the
  would-be witness, and its `because:`), **resolved** (violations the
  patch would clear), and **unchanged**. A patch that does not apply,
  or that touches files outside the snapshot's scope, is a named
  error, never a guess.

`--json` emits the report as a stable machine-readable document —
targets with their governing rules and blast radius, the snapshot's
generation timestamp and staleness, and the counterfactual buckets —
which is the agent-facing contract.

Honesty rules, same as everywhere else in this vocabulary: an
identical plan against an identical snapshot renders byte-identically
(everything is sorted); when no rule governs a target the report says
so explicitly rather than staying silent; when the on-disk snapshot no
longer matches the working tree the report states the staleness
(generation timestamp plus the drifted files) instead of silently
answering from old facts. Governance answers from the working tree's
declarations (`enola-intent.yaml` plus `enola/constraints/`), so an
edit to the law is visible without regenerating a snapshot.

**A report, never a gate.** Like `enola check`, the verdict is for
the caller to weigh: `plan` exits `0` whenever a report was produced —
counterfactual violations included — and `2` only when it could not
run (a patch that does not apply, `--symbols` with no snapshot, an
invalid declaration). It never writes into the target tree, never
mutates the repo's `.enola`, and the counterfactual's scratch
materialization is deleted when the call returns.

The agent workflow this is built for:

1. `enola plan --paths <files you intend to touch>` (or `plan_check`)
   — read the governing rules and the blast radius before writing
   anything.
2. Shape the change so it satisfies the contract; for a concrete
   patch, `enola plan --patch change.diff` names the rule any
   violating edge would breach while the tree is still clean.
3. Make the edit.
4. `enola check` after — the gate confirms what the plan predicted.

This ordering is the point: the self-correction benchmark measures
that violations drop sharply when the contract is in reach at
planning time rather than at the CI gate, and plan-check is that
contract as a first-class query.
