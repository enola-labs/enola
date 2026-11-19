---
name: review
description: Review what a change did to a codebase's structure with enola, for a branch, a pull request or the current session's edits. Use when asked to review a change or PR, to say where a reviewer should look, or what a change touched beyond what it claims. Complements a line-by-line bug review; it does not replace one.
---

# Structural review with enola

A diff shows lines. This review answers what the lines did to the system: which findings
the change introduced, which coupling it added, which code it reached beyond what it
said it was about, and which of the code it changed carries the most risk. It works from
two snapshots, before and after, and reports only the difference.

It is not a bug hunt. Read the diff for correctness as well, or pair this with a
line-level review.

## 1. Get the "before"

Pick the first that applies.

- **Your own session's edits, with a baseline pinned at the start** (by you with
  `set_baseline`, or by enola's session hook): use `diff_snapshot` after a
  `generate_snapshot`, or `enola check --detail`.
- **A branch or pull request:** grade it against where it left the target branch (their
  merge-base), with no baseline pinned:

  ```sh
  enola check --base origin/main --detail          # the PR's target branch
  ```

  enola exports the merge-base, snapshots it with the repository's configuration and
  removes it again; nothing is written to the repository. Use `main` (or the branch's
  local name) where there is no `origin` remote.
- **Both sides already recorded** in enola's history (`enola log` lists revisions):
  `enola diff <base>..<head>` compares them without an export.

If `enola check` declines to grade (exit 3), say so and do not present the change as
clean: the reason it gives (configuration, extractor version) is enola's setup, not the
change.

## 2. Read the verdict

From the `enola check` output, report:

- **New findings.** Each with its explainer and confidence. 1.00 is measured exactly;
  below that it is an estimate against this repository's own norms. A finding no policy
  enforces is still worth showing; say that nothing enforced it.
- **Resolved findings.** Credit what the change fixed.
- **New coupling.** Imports and calls the change added between packages. An added edge
  is normal work; one that crosses into an area the change has no business in is not.
- **Removed symbols.** Listed under "Removed", and their callers under "Removed
  coupling". For each one that was exported, search the current tree for its name: a
  caller the change did not update is the thing to look at.

enola files a hotspot that only moved in or out of its top-20 list under "Incidental
shifts", not as new or resolved. Another ranked or statistical finding about code the
diff did not touch is usually the same thing: check its file and line against the diff,
and if the change did not touch it, say so in one line and move on.

If the repository declares a policy (CI runs `enola check --fail-on=...`), run with the
same flags so the verdict matches what CI will say. Do not invent a policy.

## 3. Find where to look

1. **Map the diff to symbols.** On a diff of a few functions, reading it is enough; on a
   larger one:

   ```sh
   git diff "$(git merge-base HEAD origin/main)"...HEAD > /tmp/change.diff
   enola locate --diff /tmp/change.diff
   ```

   It lists every changed function and type with its line span. Lines "outside every
   symbol" are imports, package-level declarations and the like.
2. **Rank changed symbols by blast radius.** Run `impact_analysis` on each changed symbol
   (the MCP tool keeps the graph loaded; the CLI `enola impact` reloads it per call).
   `enola check` writes no snapshot, so if the MCP tools answer that no facts are
   loaded, call `generate_snapshot` once first. The
   dependent count, the endpoints reached and the modules holding most dependents are
   what make a one-line change in a widely used function the riskiest line of the diff.
3. **Check the rules governing the touched files.** `plan_check` or `constraints_for`
   per file names the declared rules and their `because:`. A change that follows the
   letter of a rule but works around its reason is worth a comment.
4. **Check history.** `touched(since=..., subject='(?i)^fix\b')` (or
   `enola touched --subject '(?i)^fix\b' --since <date> --top 0`) ranks the functions
   fixes changed most. A changed symbol near the top of that list has broken before.
5. **Check scope.** If the change says what it is about (a PR title, a ticket, the
   user's request), pass that as the target: `enola check --base origin/main
   --target=<package> --max-spillover=0`. Spillover lists packages the change touched
   that neither the target nor its dependents explain. It is a question for the author,
   not a defect.
6. **Suggest reviewers.** `enola check --base origin/main --reviewers` names the main
   authors of each touched module.

## 4. Write the review

Keep it short and ordered by what a reviewer should do:

1. **Verdict:** regressions introduced, resolved, and whether any policy enforced them.
2. **Where to look:** at most five places, each a symbol with file and line, and why:
   its blast radius, the rule governing it, its fix history, or spillover.
3. **What changed structurally:** new packages, new cross-package coupling, removed
   symbols with dependents.
4. **What enola could not see:** the "could not see" line of `enola check` (ignored
   files, imports outside the graph). Runtime behaviour, tests and logic are outside what
   a structural comparison measures.

Do not pad it with structure that did not change. If the change is structurally clean,
say so in one line and spend the review on the places it reaches.
