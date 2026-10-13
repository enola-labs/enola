package perf

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestCallToPortMemberIsResolvedIO(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "src/app.OrderRepository", Repo: "api", File: "src/app/ports.ts",
		Props: map[string]any{"language": "typescript", "symbol_kind": "interface", "io_members": []string{"save"}}})
	store.Add(facts.Fact{Kind: facts.KindSymbol, Name: "src/app.SaveAll.run", Repo: "api", File: "src/app/save-all.ts", Line: 5,
		Props: map[string]any{"language": "typescript", "symbol_kind": "method", "loop_depth": 1, "loop_count": 1,
			"calls_in_loop": []string{"src/app.OrderRepository.save"}},
		Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "src/app.OrderRepository.save"}}})

	got := Analyze(store)
	f, ok := findFinding(got, "src/app.SaveAll.run", "call-in-loop")
	if !ok {
		t.Fatalf("no call-in-loop on SaveAll.run; got %+v", got)
	}
	if f.Severity != "high" || len(f.Calls) != 1 || f.Calls[0].Basis != basisResolvedIO {
		t.Errorf("SaveAll.run = %s %+v, want high on a resolved I/O callee", f.Severity, f.Calls)
	}
	for _, x := range got {
		if x.Symbol == "src/app.OrderRepository.save" {
			t.Errorf("a port member was reported: %+v", x)
		}
	}
}

func TestBuiltinMethodIsNotReadThroughTheIndex(t *testing.T) {
	funcs := []funcInfo{
		{Name: "src/log.EventLog.append", File: "src/log.ts", PerformsIO: true},
		{Name: "src/fs.Files.join", File: "src/fs.ts", PerformsIO: true},
		{
			Name: "src/ui.toParams", File: "src/ui.ts", LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"sp.append", "node:path.join"},
		},
	}
	if c, ok := findFinding(analyze(funcs, nil, nil, nil), "src/ui.toParams", "call-in-loop"); ok {
		t.Errorf("URLSearchParams.append / path.join read as I/O by name: %+v", c)
	}
}

func TestFileIOAndClientVerbsStayOnTheIndex(t *testing.T) {
	funcs := []funcInfo{
		{Name: "src/api.Client.delete", File: "src/api.ts", PerformsIO: true},
		{Name: "src/fsx.Files.writeFile", File: "src/fsx.ts", PerformsIO: true},
		{
			Name: "src/run.all", File: "src/run.ts", LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"client.delete", "node:fs/promises.writeFile", "node:path.join"},
		},
	}
	c, ok := findFinding(analyze(funcs, nil, nil, nil), "src/run.all", "call-in-loop")
	if !ok || len(c.Calls) != 2 {
		t.Fatalf("run.all = %+v (ok=%v), want client.delete and writeFile, not path.join", c, ok)
	}
}
