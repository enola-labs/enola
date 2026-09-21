---
name: enola
description: Use Enola when investigating architecture, dependencies, blast radius, cross-repository relationships, structural regressions, or what a code change did to the system.
---

# Enola — architecture before and after a change

Enola serves a deterministic structural map over MCP: modules, symbols, routes,
storage, and the relationships between them. Use its tools for architectural
questions instead of reconstructing the same information through broad file searches.

Before changing code whose blast radius is unclear:

- Use `impact_analysis` to find transitive dependents.
- Use `explore`, `traverse`, or `find_path` to understand how code is wired.
- Use `set_baseline` once, before editing, if the session hook has not pinned one.
- Use `find_orphans` before adding more callers to apparently unused symbols.
- Use `package_metrics` for package-boundary decisions.
- Use `analyze_performance` for structural performance risks.

After a structural change, use `generate_snapshot` and `diff_snapshot` to inspect
what moved: findings introduced or resolved, coupling added, and symbols added or
removed.

## The gate: `enola check`

`enola check` is an important part of the workflow. The MCP tools help an agent
understand and inspect a change; `enola check` is the CLI and CI quality gate with a
stable exit-code contract. Run it after making structural changes and before reporting
the work complete:

```sh
enola check
```

A bare `enola check` reports the delta but intentionally fails nothing. A repository
that wants enforcement must name its policy, for example:

```sh
enola check --fail-on=layers
```

Do not add or change enforcement flags on the user's behalf. Use the policy already
declared by the repository or ask the user which rules should gate the build.

Enola reports movement from a pinned baseline, not every existing problem. A finding
that no policy enforced is evidence to show the user, not authorization to revert or
refactor unrelated code. Do not describe a change as architecturally clean when the
check could not run or its baseline was not comparable.

If the `enola` executable is missing, explain that this plugin requires the Enola CLI
on `PATH`, point the user to the repository installation instructions, and verify the
installation with `enola doctor`. Never install or upgrade the executable without the
user's approval.
