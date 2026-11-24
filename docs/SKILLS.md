# Skills

enola's MCP tools answer questions about the graph. Its skills are the workflows built on
them: short instructions an agent loads when a task calls for one, telling it which
tools to use in which order and what it must leave to you.

| Skill | Use it to |
|---|---|
| [review](#review) | Review a branch or pull request: what it did to the structure, and where a reviewer should look |
| [test-plan](#test-plan) | Plan what to test for a change: the tests and entry points that reach it, and the changed code no test reaches |
| [intent](#intent) | Write down the architecture you mean to have, as rules enola checks |
| [memory](#memory) | Keep notes about code tied to the code they describe, and find out when they stopped being true |
| [enola](#enola) | Route a structural question (what depends on this, how does A reach B) to the right tool |

Skills are plain [Agent Skills](https://agentskills.io) directories, so every agent that
reads the format can use them: Claude Code, Codex, Cursor, GitHub Copilot, Pi and
opencode.

## Get them

**With `enola install`.** One command, in the repository:

```bash
enola install --hooks
```

Besides the instructions and hooks, it puts the skills where each agent reads them:
`.claude/skills/` for Claude Code, and `.agents/skills/` for Codex, Cursor, Copilot, Pi
and opencode. `--global` does the same for every project, under your home directory. The
skills come from the binary, so they match the commands and flags it has.
[CLI.md](CLI.md#wiring-it-into-your-agents---enola-install) lists every file it writes.

**With the Claude Code plugin.** Inside Claude Code:

```text
/plugin marketplace add enola-labs/enola
/plugin install enola@enola
```

The plugin also registers enola's MCP server and its hooks. It does not install the
`enola` binary: if that is missing, run `/enola:setup`. Use one route or the other for
Claude Code, not both; when the plugin is installed, `enola install` sees it and leaves
Claude Code's skills to the plugin.

## Use them

**Ask for the task.** You do not have to name a skill: an agent reads each skill's short
description and loads the one that fits. These are the requests the skills were tested
with:

| You ask | Skill |
|---|---|
| "Review the change on this branch against main" | review |
| "What should be tested for this change? Give me a plan I can hand over" | test-plan |
| "Set up architecture rules for this repository so they are checked from now on" | intent |
| "Propose architecture rules for `../payments-api`" | intent |
| "Remember this about `X` in our notes" / "Which of our notes still hold?" | memory |
| "If I change the signature of `X`, what would I have to update?" | enola |

**Or call one by name** when you want it for certain:

| Agent | How | Example |
|---|---|---|
| Claude Code, after `enola install` | `/` and the skill's name | `/enola-review` |
| Claude Code, with the plugin | `/enola:` and the skill's name | `/enola:review` |
| Codex | `$` and the name, or pick it from `/skills` | `$enola-review` |
| Cursor, Copilot | `/` and the name | `/enola-review` |
| Pi | `/skill:` and the name | `/skill:enola-review` |
| opencode | ask for it by name | "use the enola-review skill" |

Installed skills are named `enola-review`, `enola-test-plan`, `enola-intent`,
`enola-memory` and `enola`; the plugin names them `/enola:review` and so on.

## review

Reviews a branch, a pull request or your own session's edits by comparing two snapshots
of the architecture and reporting only the difference. It does not hunt for bugs line by
line; use it beside a line-level review.

- **Needs** a git repository. The "before" is where the branch left its target, built for
  the run by `enola check --base origin/main` (or `main` without a remote), so no
  baseline has to be pinned first.
- **Will not** invent a policy. It reports the enforcement the repository already uses,
  and says when nothing enforced a finding.
- **You get** a verdict (findings introduced and resolved), up to five places to look
  with the reason for each (blast radius, rules governing the file, fix history, code
  reached outside the stated scope), what changed structurally, and what enola could not
  see.

## test-plan

Turns a change into a plan someone else can carry out. It does not run tests.

- **Needs** the change as a diff or a branch, and a snapshot of the changed code.
- **Will not** call code untested just because no test names it. A test that drives code
  through a browser, a router or a subprocess is invisible to the graph, so the plan says
  "no test reaches it by name" and asks for a check.
- **You get** the tests to run, nearest first; the entry points to exercise (routes,
  commands, screens, or for a library its public API and playgrounds); changed code no
  test reaches, with a suggested test for each; the areas with a history of fixes; and
  what the plan does not cover.

## intent

Helps you declare the architecture you mean to have, in `enola-intent.yaml`: a layer
order, components and rules, each with the reason it exists. [INTENT.md](INTENT.md) and
[CONSTRAINTS.md](CONSTRAINTS.md) describe the vocabulary.

To work on a repository other than the session's, name it: `/enola-intent
@payments-api` snapshots that repository, reads what it already declares, works out what
enola infers and what the code already almost follows, and brings back one proposal.
Nothing is written until you accept it, and then only what you accepted.

- **Needs** a snapshot, so it can show what the code does today and check that every
  rule matches real code. It takes the snapshot itself.
- **Will not** decide for you. Mined rules, recipe bindings and its own ideas are
  proposals; every rule needs a `because:` in your words; turning on enforcement
  (`--fail-on`, a `strict` rule, an exemption) is yours to do. Until you accept it, the
  draft stays out of the repository and is checked, laid over the declaration it
  would join, with `enola constraints lint --declaration draft.yaml .`.
- **You get** what is declared already, a proposal that lints clean with what each rule
  covers and what it breaches today, the questions only you can answer, and, once you
  accept, the declaration written and the CI command that would enforce it.

## memory

Keeps notes about code tied to the code they describe, in whatever you already keep
notes in: a notes file, your agent's memory, a memory service. enola stores nothing
itself; it supplies the key and the check.

- **Needs** somewhere to keep the notes. When a note is about specific code, the skill
  adds a stamp beside it: the symbol at that line and a hash of its code
  ([schema/locate.md](schema/locate.md) defines it).
- **Will not** rewrite or delete a note's content without asking. A note whose code only
  moved has its stamp updated; one whose code changed is checked against the code before
  it is trusted.
- **You get** the notes that matter to the code a task touches, found through the graph
  rather than by words, and, when you ask which notes still hold, each one marked
  current, changed, moved or gone, with the changed ones checked.

## enola

Points an agent at the right tool for a structural question instead of a text search:
`impact_analysis` for what depends on something, `find_path` for how A reaches B,
`find_orphans` for what nothing uses, and so on. Claude Code does not get it from
`enola install`, because the rule file installed beside the skills says the same and is
always loaded. Every other agent, and the plugin, does.

## Update or remove

- **Update:** run `enola install` again after upgrading enola. It rewrites its own skills
  and removes any a newer version no longer ships. With the plugin, run
  `/plugin marketplace update enola`, then `/plugin update enola@enola`, and restart
  Claude Code. A new plugin version arrives with each enola release.
- **Remove:** `enola uninstall` takes away everything `enola install` wrote, skills
  included, and nothing else.

## If a skill does not load

Start with `enola doctor`. Its Skills section lists every skill in each directory
`enola install` writes to, and marks any that are missing, differ from what your
binary would write, or are a skill of yours under enola's name.

- **It is not listed.** Restart the agent: most read skills once, at session start.
- **Pi does nothing, or waits.** Pi loads a project's skills and extensions only once the
  project is trusted: accept the prompt, run `/trust`, or start it with `pi --approve`.
- **`.claude/skills/enola` is missing.** That is intended; see [enola](#enola).
- **One of your skills has the same name.** `enola install` never overwrites a skill it
  did not write, and reports it as skipped. Rename yours, or remove it.
- **The agent answered without it.** Call it by name, as above.
- **A command in a skill fails.** The skills follow the binary that installed them. Check
  `enola --version`, upgrade, and run `enola install` again.
