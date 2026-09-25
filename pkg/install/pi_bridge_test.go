package install

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// piHarness plays Pi: it loads the extension the installer wrote, hands it a fake `pi`
// that records what it registers, and fires the events a real session would. It
// imports the exact file the installer wrote, which is the file Pi loads.
const piHarness = `
const ext = (await import(process.argv[2])).default
const cwd = process.argv[3]
const tools = new Map(), on = {}, messages = []
ext({
  registerTool: (t) => tools.set(t.name, t),
  on: (e, h) => ((on[e] ??= []).push(h)),
  sendMessage: (m, o) => messages.push({ content: m.content, triggerTurn: !!o?.triggerTurn }),
})
const ctx = { cwd, hasUI: false, sessionManager: { getSessionId: () => "pi-bridge" } }
const fire = async (e, ev) => { let r; for (const h of on[e] ?? []) r = (await h(ev, ctx)) ?? r; return r }
const call = async (name, params) => {
  try {
    const r = await tools.get(name).execute("id", params, undefined, undefined, ctx)
    return { ok: true, text: r.content.map((c) => c.text ?? "").join("") }
  } catch (e) { return { ok: false, text: String(e.message) } }
}

const out = {}
await fire("session_start", { reason: "startup" })
out.tools = [...tools.keys()]
out.noProperties = [...tools.values()].filter((t) => typeof t.parameters?.properties !== "object").map((t) => t.name)
out.prompt = (await fire("before_agent_start", { systemPrompt: "BASE" }))?.systemPrompt ?? ""
out.snapshot = await call("enola_generate_snapshot", {})
out.missingParam = await call("enola_explore", {})

// Stop: the first agent_end grades the change the Go side made before this ran; the
// second is the end of the turn that report started, and must stay silent.
await fire("agent_end", {})
out.afterFirstStop = messages.length
await fire("agent_end", {})
out.messages = messages
await fire("session_shutdown", { reason: "quit" })
console.log(JSON.stringify(out))
`

// TestPiExtension_BridgesToARealServer — the extension against a real enola build, with
// no Pi and no model: the MCP client, the tool registration, error mapping, and the
// stop hook's report and loop breaker. What it cannot cover is Pi itself honouring
// these calls; that was checked against Pi 0.87.1 by hand.
func TestPiExtension_BridgesToARealServer(t *testing.T) {
	if testing.Short() {
		t.Skip("builds enola and runs node")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's paths and the harness assume POSIX")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	work := t.TempDir()
	// Built before HOME moves, so `go build` keeps using the real module cache.
	enola := buildEnola(ctx, t, work)
	// The server restores ~/.enola/receipt.json on start; the real one must not leak in.
	t.Setenv("HOME", filepath.Join(work, "home"))
	repo := writeCyclePendingRepo(t, work)
	runCLI(ctx, t, repo, enola, "baseline", "pin", repo)

	if _, err := Install(Options{
		Scope: ScopeLocal, RepoDir: repo, HomeDir: filepath.Join(work, "home"),
		Hooks: true, HookCommand: enola, Targets: []string{"pi"},
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Close the cycle the Stop hook should report.
	writeFile(t, filepath.Join(repo, "a", "a.go"), `package a

import "example.com/e2e/b"

// A now calls back into b, closing a cycle.
func A() string { return "a" + b.B() }
`)

	harness := filepath.Join(work, "harness.mjs")
	writeFile(t, harness, piHarness)
	cmd := exec.CommandContext(ctx, node, harness, filepath.Join(repo, ".pi", "extensions", "enola.js"), repo)
	cmd.Dir = repo
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("harness: %v\n%s", err, raw)
	}
	var got struct {
		Tools        []string
		NoProperties []string
		Prompt       string
		Snapshot     struct {
			OK   bool
			Text string
		}
		MissingParam struct {
			OK   bool
			Text string
		}
		AfterFirstStop int
		Messages       []struct {
			Content     string
			TriggerTurn bool
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("harness output: %v\n%s", err, raw)
	}

	for _, want := range []string{"enola_explore", "enola_impact_analysis", "enola_generate_snapshot"} {
		if !contains(got.Tools, want) {
			t.Errorf("tool %s not registered; got %v", want, got.Tools)
		}
	}
	// OpenAI-compatible endpoints reject a tool schema without `properties`, and fail the
	// whole request over it; set_baseline's argument-less schema did exactly that.
	if len(got.NoProperties) > 0 {
		t.Errorf("tools registered without a properties object: %v", got.NoProperties)
	}
	if !strings.HasPrefix(got.Prompt, "BASE") || !strings.Contains(got.Prompt, "enola_explore") {
		t.Errorf("system prompt does not name the prefixed tools: %q", got.Prompt)
	}
	if !got.Snapshot.OK || got.Snapshot.Text == "" {
		t.Errorf("generate_snapshot through the bridge failed: %+v", got.Snapshot)
	}
	if got.MissingParam.OK {
		t.Errorf("a tool error came back as success: %+v", got.MissingParam)
	}
	if got.AfterFirstStop != 1 || len(got.Messages) != 1 {
		t.Fatalf("stop reports = %d after the first agent_end, %d in total; want 1 and 1",
			got.AfterFirstStop, len(got.Messages))
	}
	if m := got.Messages[0]; !m.TriggerTurn || !strings.Contains(strings.ToLower(m.Content), "cycle") {
		t.Errorf("stop report did not start a turn about the cycle: %+v", m)
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
