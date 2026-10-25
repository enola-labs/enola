package server

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A type listed with its members reads as one entry: the members folded under it by
// name and line, a member in another file with its file, and an unrelated name and a
// member whose owner is not listed kept on lines of their own.
func TestRenderNamesOnly_FoldsMembersUnderTheirType(t *testing.T) {
	sym := func(name, file string, line int) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Line: line}
	}
	got := renderNamesOnly([]facts.Fact{
		{Kind: facts.KindDependency, Name: "src/a -> src/starter.js", File: "src/a/app.ts", Line: 48},
		sym("common.IStarter", "starter.ts", 24),
		sym("common.IStarter.start", "starter.ts", 33),
		sym("common.IStarter.onExit", "starter.ts", 30),
		sym("common.IStarter.kill", "other.ts", 7),
		sym("common.IStarterChannel", "starter.ts", 11),
		sym("main.Starter.start", "main.ts", 90),
	}, 7)
	want := "Found 7 results (showing 7):\n\n" +
		"src/a -> src/starter.js  src/a/app.ts:48\n" +
		"common.IStarter  starter.ts:24\n" +
		"    members (3): start:33, onExit:30, kill (other.ts:7)\n" +
		"common.IStarterChannel  starter.ts:11\n" +
		"main.Starter.start  main.ts:90\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Count(got, "common.IStarter.") != 0 {
		t.Errorf("members still on lines of their own:\n%s", got)
	}
}
