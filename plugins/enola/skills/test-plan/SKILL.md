---
name: test-plan
description: Build a test plan for a code change with enola, covering which entry points and endpoints to exercise, which existing tests reach the changed code, which changed code no test reaches, and which areas have broken before. Use when asked what to test, how to QA a change or pull request, whether a change is covered by tests, or what a change could break for users.
---

# Test plan from the graph

A change is tested by the paths that reach it: the routes, commands and jobs a user or
another service enters through, and the tests that call into it. enola knows both from
the graph, so the plan comes from what the change actually reaches, not from what its
file names suggest.

The output is a plan for a person or an agent to carry out. This skill does not run
tests or judge them.

## 1. Name the changed code

- For a diff (a branch, a PR, uncommitted work):

  ```sh
  git diff "$(git merge-base HEAD origin/main)"...HEAD > /tmp/change.diff
  enola locate --diff /tmp/change.diff
  ```

  or `locate` with the diff text. Every changed function and type comes back with its
  file and line span.
- For a planned change: the symbols or files the user names. `plan_check` with the
  paths gives their blast radius before any edit.

Make sure the snapshot matches the code you are planning for (`generate_snapshot` after
the change is in the working tree).

## 2. Entry points to exercise

- **Flows.** `enola flows --through <symbol>` lists the entry points whose calls reach a
  changed symbol: a served route's handler, a program's `main`, a subcommand, a
  framework hook. Each is something to exercise. Flows marked `side` (examples,
  scripts, migrations, build tooling) come after the program's own.
- **Endpoints.** `impact_analysis` reports "Endpoints affected" for a symbol. For each
  affected route, or each route the diff changes, `endpoint_impact` names the
  controller, the models and tables behind it, the client call sites that send it, and
  the app screens that depend on it. Screens and client calls are the end-to-end checks.
- **When nothing reaches it.** A library's code is entered by its users, and code
  injected into a browser or another runtime is entered by that runtime, so no flow or
  endpoint in the graph leads to it. Its entry points are then its public API and the
  examples, playgrounds or end-to-end specs that exercise it: search the test and example
  directories for the feature's names and fixtures.
- **Across services.** In a multi-repository snapshot, `endpoint_impact` includes
  callers in other repositories. `coverage_report` says which services have calls enola
  could not resolve; a change behind such a service may have callers the plan cannot
  list, so say so.

## 3. Existing tests that reach the change

`impact_analysis(target=<symbol>, max_depth=6, output_mode='compact')` lists test files
as `test_ref` dependents, by depth. A test at depth 1 calls the symbol directly; at depth
4 it reaches it through three other functions, and a failure there says less about this
symbol. List the tests to run, nearest first, grouped by symbol.

A changed symbol with no `test_ref` within that depth is **untested by any reference
enola can see**. That is the most useful line in the plan. Call it out with the symbol,
file and line, and propose what a test for it would call.

Tests reach code by name. A test that drives the code through an interface, a router, a
subprocess, a browser or reflection is invisible here, so "no test_ref" means "no test names a path
to it", not "certainly untested". Check such a symbol's package for integration tests
before reporting it as uncovered.

## 4. Where it has broken before

- `touched(since=<a year ago>, subject='(?i)^fix\b')` ranks the functions that fixes
  changed most. A changed symbol in that ranking has regressed before: give it a
  targeted test, not just a pass through the happy path.
- The changed symbols with the most dependents (the totals from step 3) carry the most
  risk per line changed.

## 5. Write the plan

1. **Run these tests:** existing test files, nearest first, with the command if the
   repository's test runner is obvious.
2. **Exercise these entry points:** each flow, route or screen, with what to check
   there, ordered by how directly it reaches the change.
3. **Untested changed code:** symbols no test reaches, with a one-line suggestion each.
4. **Higher risk:** symbols with fix history or a large blast radius.
5. **Not covered by this plan:** callers enola could not resolve, and anything reached
   only at runtime (configuration, reflection, message consumers without a contract).

Keep it to what the change reaches. A plan that lists every test in the repository
tells the reader nothing.
