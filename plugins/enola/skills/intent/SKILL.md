---
name: intent
description: Declare and maintain a repository's intended architecture for enola, meaning its layer order, components and rules in enola-intent.yaml or enola/constraints/. Use when the user wants to write down, propose, review, tighten or clean up architecture rules for a repository (optionally naming it, as in "/enola-intent @repo"), asks which rules govern some code, or a constraint or layer finding needs a decision.
argument-hint: "[repository path]"
---

# Architecture intent with enola

enola measures what the architecture is. Intent is what it was meant to be, written down
so every snapshot compares the two. A rule nobody declared cannot fail, and a rule
declared wrong fails forever, so this skill is mostly about getting the user's decisions
onto the page accurately.

Intent lives in three places, and the first two use one schema:

- `enola-intent.yaml` at the repository root: `layers`, `components`, `rules`, and the
  repository's seams (`consumes`, `serves`) and `dependencies`.
- Files under `enola/constraints/`, for components and rules split by domain.
- An `enola_intent` block in a markdown page's frontmatter: knowledge pages, seams,
  layers and claims declared where the decision was made. Never components or rules.

## Ground rules

1. **The user decides what becomes law.** Mined candidates, recipe bindings and your own
   ideas are proposals. Present them, and write only what the user accepts.
2. **Every rule needs a `because:` in the user's words.** It is printed on every finding
   the rule produces. If you do not know why a rule exists, ask; do not invent a reason.
3. **Never turn on enforcement yourself.** Adding `--fail-on`, setting a rule's `mode`
   to `strict`, adding an `exempt:` entry or a suppression are the user's calls.
   Exemptions and suppressions name an owner: that is a person, never you.
4. **A selector that matches nothing is the failure to watch for.** It is valid and can
   never be violated. `enola constraints lint --fail-empty` exits 1 while any component
   or layer selects nothing; do not stop until it exits 0.
5. **A proposal is a complete, valid declaration.** When you present rules for the user
   to accept, every component a rule names is declared, every rule uses a real rule form,
   it lints clean laid over what the repository already declares, and you can say what
   each rule breaches today. Accepting it should take one step.
6. **Leave generated pages alone.** If a wiki's `enola_intent` blocks are produced by the
   user's own tooling, change the tooling or ask, not the generated output.

## The repository

The skill may be invoked with a repository: a path, or an @-mention of a directory, as
in `/enola-intent @payments-api`. That repository is the target of everything below.
Without one, the target is the repository the session is working in. Resolve it to an
absolute path and check that it is a directory before going on.

- Snapshot it with `generate_snapshot(repo_path=<absolute path>, fresh=true)`. The
  snapshot is written to `<repo>/.enola/`, which the CLI commands below read. Without
  the MCP server, run `enola --generate <repo>`.
- Run every `enola constraints` command from inside the repository with `.` as its
  path: `cd <repo> && enola constraints lint .`. enola reads `mcp-arch.yaml` from the
  working directory, so a run started elsewhere indexes under another repository's
  configuration.
- Keep drafts outside the repository, in a scratch directory, until the user accepts
  them.

## Proposing intent

Work through these steps, then bring the user one proposal, not a stream of questions.

1. **Read what is declared.** `enola constraints lint --fail-empty .` lists the
   declaration files, what each component and layer selects, what the layer order and
   every rule report today, and exits 1 on anything invalid or empty.
   - Nothing declared: the steps below apply in full.
   - Something declared: propose only what is missing or wrong. That is a layer order
     if there is none, modules no layer covers, selectors that select nothing, and rules
     whose breaches suggest the decision behind them changed. Do not propose again what
     is already there.
2. **See what enola infers.** `query_insights(explainer='layers')` gives the layer
   pattern it recognised, or the declared order's verdict; a recognised pattern is the
   starting point for a layer order. `enola constraints init --dry-run .` shows which
   shipped recipes bind to directories this repository has. "Nothing to bind" is a
   normal answer: ask whether the repository follows one of those patterns under other
   directory names. `package_metrics` and `query_insights(explainer='cycles')` show
   which way packages depend, which is where the boundaries are.
3. **Mine candidates** the code already almost follows:
   `enola constraints mine --min-confidence 0.98 --top 30 .`. Each carries its
   regularity (for example 712/714) and names its exceptions. Most are not intent, so
   read them before showing any:
   - keep a forbid-edge whose exceptions look like mistakes;
   - drop one whose exceptions are deliberate use, and one that only says a package is
     rarely called: "pkg/command almost never calls internal/updatecheck" describes how
     often it is used, not a decision;
   - drop anything the layer order already implies.

   On a small repository mining often proposes nothing and counts what fell under its
   support floor of 10. Lowering `--min-support` shows those, but a regularity over a
   handful of facts is weak evidence, so say so when you present one.
4. **Write the draft** to a scratch file: the layer order first, then components and
   rules for what layers cannot say (see "Declaration shapes"). Until the user answers,
   a rule's `because:` holds the evidence for it, marked as yours:
   `because: "PROPOSED: 712/714 calls from extractors avoid linkers"`. That text never
   reaches the repository.
5. **Lint the draft** laid over what the repository declares:

   ```sh
   cd <repo> && enola constraints lint --fail-empty --declaration /scratch/draft.yaml .
   ```

   The draft's components and rules are added, or replace the entry with the same name
   or id, and lint says which. A `layers:` section in the draft replaces the declared
   order; leave it out to keep it. Fix the draft until lint exits 0. Its "Rule verdicts"
   section is what each rule breaches today, and "Declared layer order against the
   snapshot" counts the imports that run against the order.
6. **Present the proposal** in this shape:

   ```
   Target: <repo>, snapshot of <date>, <N> modules
   Already declared: <summary, or "nothing">

   Proposed layer order, outermost first
     <layer>   <paths>   <modules>
     Today: <N> imports inward, <M> against: <the ones against>

   Proposed rules
     <id>: <the rule in plain words>
       evidence: <regularity, or where the idea came from>
       today: <holds | N breaches: the first few>

   Questions only you can answer
     1. Why does <id> exist? (my reading: <evidence>)
     2. ...

   Left out: <candidates you dropped, one line each, and why>
   ```

   Then ask the user to accept all of it, accept some by id, change it, or reject it.
   Breaches a rule has today are accepted with it: under the default mode, `ratchet`,
   they stay silent and only new ones fail.
7. **Write what was accepted, and only that.**
   - Merge it into the repository's declaration: `enola-intent.yaml`, created if it is
     absent, or the `enola/constraints/` file for that domain if the repository already
     splits rules that way. Keep everything already there, comments included.
   - Replace every `PROPOSED:` reason with the user's words. A rule whose reason the
     user did not give is not accepted yet.
   - `cd <repo> && enola constraints lint --fail-empty .` must exit 0.
   - Regenerate the snapshot and show what the declaration says about today's code:
     `query_insights(explainer='constraints')`, `query_insights(explainer='layers')`.
   - Offer enforcement and stop. Show the command the user would put in CI, for example
     `enola check --fail-on=layers,constraints`, and leave the decision to them. Do not
     commit unless asked.

## Declaration shapes

The layer order is the single most useful declaration: a declared order is graded at
confidence 1.00, an inferred one caps at 0.80, so only a declared one can gate.
Outermost first; each layer may depend on itself and on layers below:

```yaml
layers:
  - {name: handlers, paths: ["app/handlers/**"]}
  - {name: domain,   paths: ["app/domain/**"]}
  - {name: storage,  paths: ["app/storage/**"]}
```

A `paths` entry is an exact module path or a `prefix/**` subtree, nothing else. An order
between packages belongs here, not in one `forbid` rule per pair of packages. Group by
role, not by directory tree, and keep a comment beside each boundary saying why it is
there.

Components and rules say what layers cannot:

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

`match` takes an exact path, a `prefix/**` subtree or a `**/name` basename glob. `via` is
one of `calls`, `depends_on`, `implements`, `imports`. For the other rule forms (`allow`,
`protect`, `private`, `cap`, `require`, `forbid_cycles` and the rest), read
`enola help constraints` and the reference linked at the end.

## Before editing code that rules govern

- `plan_check` (or `enola plan --paths <files>`) lists the rules governing the files you
  are about to touch and their blast radius. A path that does not exist yet still
  answers.
- `enola plan --patch change.diff` evaluates a diff on a scratch copy and names the rule
  any new edge would breach, before the edit lands.
- `constraints_for` gives one file's or fact's components, the rules binding them, and
  the violations already naming it.
- `governing_intent` names the decision pages whose anchors cover the code, and the
  code a page governs.

Shape the change to satisfy the rules. If a rule is in the way, say so; do not edit the
rule to make your change pass.

## When a breach is reported

Present it with its `because:` and help the user choose, strongest first:

1. **Fix the code.** The rule stands and the change was wrong.
2. **Exempt it** with an `exempt:` entry on the rule, naming the exact witness, an
   owner, a `because:` and a `since:` date. For carve-outs decided to be out of scope
   for good.
3. **Suppress it** in `.enola/suppressions.yaml` with owner, reason and date. Temporary:
   the violation is real but must not block a `strict` rule while it is worked off.
4. **Leave it to the ratchet.** Only for breaches that predate the rule. Nobody decided
   anything, which is why anything decided deserves 2 or 3.

`enola constraints explain <file>` answers "why does this file belong to that component":
the selector that admitted it, its edges, and what would change if it moved.

## Keeping the law honest

`enola constraints ledger` lists every rule beside its breaches and the exemptions and
suppressions excusing them, with each excuse's owner and age. Bring three things to the
user:

- a rule whose breaches are mostly excused, which is a rule to reconsider;
- old excuses, whose reasons have probably stopped being true;
- excuses that matched nothing, which can be deleted today.

The ledger reads the snapshot, so regenerate first if the declaration changed.

## Reference

- `enola help constraints` and `enola help constraints <lint|mine|init|explain|ledger>`
- Rule forms, selectors, modes, recipes: https://github.com/enola-labs/enola/blob/main/docs/CONSTRAINTS.md
- Layers, seams, dependencies and page intent: https://github.com/enola-labs/enola/blob/main/docs/INTENT.md
