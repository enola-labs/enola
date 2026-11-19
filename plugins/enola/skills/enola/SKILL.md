---
name: enola
description: Use Enola when investigating architecture, dependencies, blast radius, cross-repository relationships, structural regressions, or what a code change did to the system.
---

# Enola: architecture before and after a change

Enola serves a deterministic map of the codebase over MCP: modules, symbols, routes,
storage, and how they depend on each other. Ask it instead of reconstructing structure
from file searches; a text search finds where a name is written and misses a package
that reaches it through another, a call through an interface, and a function passed as
a value.

## Which tool answers what

| Question | Tool |
|---|---|
| What depends on X, and what breaks if it changes? | `impact_analysis` |
| Who calls X? What does X call? | `traverse` |
| How does A reach B? | `find_path` |
| How is this area wired? | `explore`, `query_facts`, `show_symbol` |
| What does nothing use? | `find_orphans` |
| What is already wrong here: cycles, layer violations, hotspots, unused routes? | `query_insights` |
| Which rules govern this file? | `constraints_for`, `plan_check` |
| What does changing this endpoint reach? | `endpoint_impact` |
| Which functions do fixes keep changing? | `touched` |
| Which symbol holds this line, and did a remembered one change? | `locate` |
| Packaging or performance decisions | `package_metrics`, `analyze_performance` |

## Before and after a change

Before editing code whose blast radius is not obvious, run `impact_analysis` on it, and
pin a baseline with `set_baseline` once, if the session hook has not pinned one.

After a structural change, `generate_snapshot` then `diff_snapshot` shows what moved:
findings introduced or resolved, coupling added, symbols added or removed. A layer
crossed the wrong way, or coupling nobody asked for, is a reason to fix the change
before presenting it.

## The gate: `enola check`

`enola check` is the CLI and CI form of the same comparison, with a stable exit code. Run
it after structural changes and before reporting the work complete:

```sh
enola check
```

A bare `enola check` reports the delta and fails nothing. A repository that wants
enforcement names its policy, for example `enola check --fail-on=layers`. Use the policy
the repository already declares; do not add or change `--fail-on` on the user's behalf.

enola reports movement from a pinned baseline, not every existing problem. A finding no
policy enforced is evidence to show the user, not authorization to revert or refactor
unrelated code. Do not call a change architecturally clean when the check could not run
or its baseline was not comparable.

## Task skills

For a whole task, load the matching skill instead of improvising the workflow: the
review skill for reviewing a change or pull request, the test-plan skill for what to
test, the intent skill for writing or maintaining architecture rules, and the memory
skill for memories about code.

## When the executable is missing

If `enola` is not on `PATH`, say that the Enola CLI is required, point the user to the
repository's installation instructions, and verify with `enola doctor` once it is
installed. Never install or upgrade the executable without the user's approval.
