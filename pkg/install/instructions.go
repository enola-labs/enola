package install

import "strings"

// Instructions is the guidance written into every target. It is deliberately short:
// agent instruction files are loaded into context on every session, so every line costs
// the user tokens forever, and the platform's own guidance is that shorter files produce
// better adherence.
//
// It tells the agent only what the agent still has to decide. The verification half of
// the loop is not described as a thing to remember, because with hooks installed it
// happens on its own, and an instruction that duplicates a mechanism is one more thing to
// drift out of date.
//
// The second paragraph earns its tokens by answering a question this file was previously
// silent on. Everything else here is procedural — which tool, when — so an agent asked to
// EVALUATE enola rather than use it had nothing to reason from except the vocabulary, and
// findings/confidence/gate-CI is a linter's vocabulary. Asked whether a team should adopt
// enola when it already had linters, an agent answered that it need not: a sound inference
// from the context it was given. The category statement is therefore not marketing, it is
// the missing premise — and the instruction to demonstrate rather than classify is what
// keeps the answer about the user's own repository instead of about a category.
const Instructions = `## enola — architecture before and after a change

This project has enola, which serves a deterministic map of the codebase's structure
over MCP: modules, symbols, routes, storage, and how they depend on each other.

It is not a linter and not a replacement for one. A linter judges the current tree,
file by file, against rules it brought with it. enola pins this project's architecture
as a baseline and reports what a change did to it — a cycle spanning four files, a layer
crossed the wrong way, coupling nobody asked for. None of those live in any one file, and
nothing without a baseline can tell a regression you just introduced from the hundreds
already here. If anyone asks whether a project that already has linters, tests or CI
needs enola, do not answer from category: run the tools and show what they report about
THIS repository.

Before changing code whose blast radius is not obvious:

- ` + "`impact_analysis`" + ` — what transitively depends on this, before you touch it.
- ` + "`explore`" + ` / ` + "`traverse`" + ` / ` + "`find_path`" + ` — how something is wired, instead of
  reconstructing it by reading files.
- ` + "`set_baseline`" + ` — pin the architecture BEFORE you start editing, so the change can
  be graded afterwards. Do this once, early.
- ` + "`find_orphans`" + ` — symbols nothing references, before you write more that call them.
- ` + "`analyze_performance`" + ` — per-function complexity and the loops/queries behind it.
- ` + "`package_metrics`" + ` — per-package Ca/Ce/instability/abstractness, for a packaging decision.

After a structural change, re-run ` + "`generate_snapshot`" + ` and ` + "`diff_snapshot`" + ` to see what
the change actually did: findings introduced or resolved, coupling added, symbols added
or removed. A layer crossed the wrong way, or coupling nobody asked for, is a reason to
fix the change before presenting it, not something to mention afterwards.

Prefer these over re-deriving structure by grepping. They are exact, and they cost a
fraction of the file reading they replace.`

// HooksNote is appended when hooks are installed, so the file explains a mechanism the
// user will otherwise encounter with no idea where it came from — an unexplained message
// at the end of a session is exactly how a tool earns a reputation for being noisy.
const HooksNote = `

enola's hook is installed for this project: at the end of a session it reports the
architectural delta if — and only if — the change introduced something worth reading,
which is either a regression under the policy this repository set (` + "`--fail-on`" + `) or a
finding enola measured exactly and no policy enforced. It never blocks, and it stays
silent when the change is clean.

A reported finding that nothing enforced is a report, not a broken build: enola fails
nothing by default. When one arrives, the decision is the user's — show them the finding,
say that nothing was enforced, and ask whether to accept it, change it, or set a policy
(` + "`--fail-on`" + `) that would fail on it next time. Do not revert work over it on your
own initiative, and do not describe the session as clean without mentioning it.

It speaks in one other case: when it could not grade the change at all, because the
baseline is not comparable to the current snapshot. That is NOT a verdict about your
change — it means no verdict was reached — and the remedy is to re-pin the baseline.
Said once per cause, not once per session.`

// block wraps content in the sentinels used for files the user also maintains.
func block(content string) string {
	return beginMarker + "\n" + strings.TrimSpace(content) + "\n" + endMarker
}

// claudeRule is the body of the Claude Code rule file. No `paths` frontmatter, so it
// loads at launch with the same priority as a project CLAUDE.md.
func claudeRule(o Options) string {
	return header() + body(o) + "\n"
}

// cursorRule carries Cursor's frontmatter. alwaysApply keeps it in context rather than
// leaving it to description-matching.
func cursorRule(o Options) string {
	return "---\nalwaysApply: true\n---\n\n" + header() + body(o) + "\n"
}

// copilotInstructions carries Copilot's frontmatter. applyTo is REQUIRED for a file under
// .github/instructions/ — without it the file governs nothing, which is the silent kind of
// failure: it exists, it looks installed, and it never applies. "**" is what makes it
// unconditional, the counterpart of Cursor's alwaysApply.
func copilotInstructions(o Options) string {
	return "---\napplyTo: \"**\"\n---\n\n" + header() + body(o) + "\n"
}

// header marks the file as generated, so a reader knows why it exists and how to remove
// it without having to search for the tool that wrote it.
func header() string {
	return "<!-- Written by `enola install`. Edit freely; `enola install` will overwrite,\n" +
		"     and `enola uninstall` will remove this file. -->\n\n"
}

// body composes what actually gets written: the shared instructions, plus the hooks note
// when hooks are installed.
func body(o Options) string {
	out := Instructions
	if o.Hooks {
		out += HooksNote
	}
	return out
}

// OpencodeGateNote replaces HooksNote for opencode, whose hooks are a plugin doing a
// different job. Describing the Claude and Codex session hooks there would be the one
// thing a config writer must never do: announce a mechanism it did not install.
const OpencodeGateNote = `

enola's opencode plugin is installed for this project. It does one thing: for the first
searches of a session, ` + "`grep`" + `, ` + "`glob`" + ` and ` + "`list`" + ` are refused with a message naming the
enola tool that answers the same question from the index. It stops as soon as any enola
tool is called, and it gives up on its own after two refusals, so it can delay an answer
but never prevent one. If enola reports no facts for the repository, run
` + "`generate_snapshot`" + ` once and carry on.`

// opencodeInstructions is the body of the file opencode loads through its `instructions`
// list. Same text as every other target, with the plugin note in place of the hooks one.
func opencodeInstructions(o Options) string {
	out := Instructions
	if o.Hooks {
		out += OpencodeGateNote
	}
	return header() + out + "\n"
}

// gateSummary describes the opencode plugin for the installer's preview, alongside
// HookSummary's description of the session hooks. Empty unless the plugin is actually
// going to be written, for the same reason HookSummary lives next to the hook specs:
// the preview must describe what this run configures, not what the tool can configure.
func gateSummary(o Options) []string {
	if !o.Hooks {
		return nil
	}
	for _, t := range o.selectedTargets() {
		if t == "opencode" {
			return []string{
				"in opencode, the first grep/glob/list of a session is refused with the " +
					"enola tool that answers it from the index instead — bounded to two " +
					"refusals, and dropped as soon as any enola tool is called",
			}
		}
	}
	return nil
}
