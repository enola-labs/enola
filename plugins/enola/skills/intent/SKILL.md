---
name: intent
description: Declare and maintain a repository's intended architecture for enola, meaning its layer order, components and rules in enola-intent.yaml or enola/constraints/. Use when the user wants to write down, review, tighten or clean up architecture rules, asks which rules govern some code, or a constraint or layer finding needs a decision.
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
4. **A selector that matches nothing is the failure to watch for.** It is valid, lints
   clean and can never be violated. Do not stop until every component and layer resolves
   to members.
5. **A proposal is a complete, valid declaration.** When you present rules for the user
   to accept, every component a rule names is declared, every rule uses a real rule form,
   and it lints clean (see "Checking a draft" below). Accepting it should take one step.
6. **Leave generated pages alone.** If a wiki's `enola_intent` blocks are produced by the
   user's own tooling, change the tooling or ask, not the generated output.

## Starting from nothing

1. Make sure a snapshot exists (`generate_snapshot`), so lint can resolve selectors.
2. See what enola already infers: `query_insights(explainer='layers')` reports the layer
   pattern it recognised. `enola constraints init --dry-run` shows which shipped recipes
   bind to directories this repository has. "Nothing to bind" is a normal answer: ask
   whether the repository follows one of those patterns under other directory names.
3. Propose rules the code already almost follows: `enola constraints mine`. Each
   candidate carries its regularity (for example 47/48) and names its exceptions. Show
   the user the strongest few, with the exceptions, and let them pick. Use
   `--min-confidence 0.98 --top 0` when there are too many. On a small repository it
   often proposes nothing and counts what fell under its support floor of 10; lowering
   `--min-support` shows those, but a regularity over a handful of facts is weak
   evidence, so say so when you present one.
4. Write the layer order first. It is the single most useful declaration: a declared
   order is graded at confidence 1.00, an inferred one caps at 0.80, so only a declared
   one can gate. Outermost first; each layer may depend on itself and on layers below:

   ```yaml
   layers:
     - {name: handlers, paths: ["app/handlers/**"]}
     - {name: domain,   paths: ["app/domain/**"]}
     - {name: storage,  paths: ["app/storage/**"]}
   ```

   A `paths` entry is an exact module path or a `prefix/**` subtree, nothing else. An
   order between packages belongs here, not in one `forbid` rule per pair of packages.
   Group by role, not by directory tree, and keep a comment beside each boundary saying
   why it is there.
5. Then components and rules for what layers cannot say:

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

   `match` takes an exact path, a `prefix/**` subtree or a `**/name` basename glob.
   `via` is one of `calls`, `depends_on`, `implements`, `imports`. For the other rule
   forms (`allow`, `protect`, `private`, `cap`, `require`, `forbid_cycles` and the rest),
   read `enola help constraints` and the reference linked at the end.
6. Validate: `enola constraints lint`. Repeat until it reports no problems and no
   component or layer with 0 members.
7. Regenerate the snapshot and show what the declaration says about today's code:
   `query_insights(explainer='constraints')`, `query_insights(explainer='layers')`.
   Explain the default mode, `ratchet`: breaches that already exist stay silent, and only
   new ones fail. Declaring a rule does not break the build that declares it.
8. Offer enforcement and stop. Show the command the user would put in CI, for example
   `enola check --fail-on=layers,constraints`, and leave the decision to them.

## Checking a draft without adopting it

Until the user accepts it, a draft stays out of the repository. Lint it in place of the
repository's declaration, with its selectors resolved against the snapshot:

```sh
enola constraints lint --declaration /tmp/draft.yaml .
```

Fix it until it passes and every component and layer has members. Nothing is written;
files under `enola/constraints/` are still read beside the draft. Make sure a snapshot
exists first (`generate_snapshot`), or lint can only validate, not resolve.

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
