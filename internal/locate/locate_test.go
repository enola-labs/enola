package locate

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func sym(name, file string, line, end int, body string) facts.Fact {
	f := facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Line: line, EndLine: end, Repo: "r",
		Props: map[string]any{"symbol_kind": "function"}}
	if body != "" {
		f.Props[facts.PropBodyHash] = body
	}
	return f
}

// src/app.ts:
//
//	1  import x
//	2
//	3  class App {        App 3-12
//	4    a() {            App.a 4-6
//	5      return 1;
//	6    }
//	7
//	8    b() {            App.b 8-11
//	9      return 2;
//	10
//	11   }
//	12 }
//	13
//	14 function f() {     f 14-16
//	15   return 3;
//	16 }
func fixture() *Index {
	return NewIndex([]facts.Fact{
		{Kind: facts.KindModule, Name: "src", File: "src/app.ts", Line: 1, Repo: "r"},
		sym("src.App", "src/app.ts", 3, 12, "app"),
		sym("src.App.a", "src/app.ts", 4, 6, "a1"),
		sym("src.App.b", "src/app.ts", 8, 11, "b1"),
		sym("src.f", "src/app.ts", 14, 16, "f1"),
		sym("pkg.G", "pkg/g.go", 10, 0, ""),
		sym("pkg.H", "pkg/g.go", 20, 0, ""),
		sym("lib.f", "lib/app.ts", 1, 3, "zz"),
	})
}

func names(ss []Symbol) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestLocations(t *testing.T) {
	ix := fixture()
	for loc, want := range map[string]struct {
		match string
		names string
	}{
		"src/app.ts:5":       {MatchSpan, "src.App.a"},
		"src/app.ts:7":       {MatchSpan, "src.App"},
		"src/app.ts:1":       {MatchNone, ""},
		"src/app.ts:13":      {MatchNone, ""},
		"src/app.ts:5-9":     {MatchSpan, "src.App.a,src.App,src.App.b"},
		"./src/app.ts:15":    {MatchSpan, "src.f"},
		"/abs/src/app.ts:15": {MatchSpan, "src.f"},
		"pkg/g.go:15":        {MatchNearest, "pkg.G"},
		"pkg/g.go:5":         {MatchNone, ""},
		"app.ts:2":           {MatchNoFile, ""}, // two files end with it
		"nope.ts:2":          {MatchNoFile, ""},
		"src/app.ts":         {MatchNoFile, ""},
	} {
		r := ix.Locations([]string{loc})[0]
		got := ""
		for i, n := range names(r.Symbols) {
			if i > 0 {
				got += ","
			}
			got += n
		}
		if r.Match != want.match || got != want.names {
			t.Errorf("%s: %s %q, want %s %q", loc, r.Match, got, want.match, want.names)
		}
	}
	if r := ix.Locations([]string{"app.ts:2"})[0]; len(r.Candidates) != 2 {
		t.Errorf("an ambiguous path lists its candidates, got %v", r.Candidates)
	}
	if r := ix.Locations([]string{"src/app.ts:5"})[0]; r.Symbols[0].ID != facts.FactID("r", facts.KindSymbol, "src.App.a", "src/app.ts") ||
		r.Symbols[0].BodyHash != "a1" {
		t.Errorf("symbol %+v does not carry its fact id and body hash", r.Symbols[0])
	}
}

func diffCounts(t *testing.T, ix *Index, text, side string) (map[string]int, int) {
	t.Helper()
	r, err := ix.Diff(text, side)
	if err != nil {
		t.Fatal(err)
	}
	got, outside := map[string]int{}, 0
	for _, f := range r.Files {
		for _, s := range f.Symbols {
			got[s.Name] = s.Lines
		}
		outside += f.Outside
	}
	return got, outside
}

func TestDiff(t *testing.T) {
	ix := fixture()
	for name, tc := range map[string]struct {
		diff    string
		side    string
		want    map[string]int
		outside int
	}{
		"an edit inside a method": {
			diff: "diff --git a/src/app.ts b/src/app.ts\n--- a/src/app.ts\n+++ b/src/app.ts\n" +
				"@@ -4,3 +4,3 @@\n   a() {\n-    return 0;\n+    return 1;\n   }\n",
			want: map[string]int{"src.App.a": 1},
		},
		"read on the old side": {
			diff: "--- a/src/app.ts\n+++ b/src/app.ts\n" +
				"@@ -15,1 +15,1 @@\n-  return 3;\n+  return 4;\n",
			side: SideOld,
			want: map[string]int{"src.f": 1},
		},
		"a deletion inside a body": {
			diff: "--- a/src/app.ts\n+++ b/src/app.ts\n" +
				"@@ -9,3 +9,2 @@\n     return 2;\n-    log();\n \n",
			want: map[string]int{"src.App.b": 1},
		},
		"a deletion between two declarations": {
			diff: "--- a/src/app.ts\n+++ b/src/app.ts\n" +
				"@@ -12,4 +12,2 @@\n }\n-\n-// gone\n \n",
			want:    map[string]int{},
			outside: 2,
		},
		"an import": {
			diff: "--- a/src/app.ts\n+++ b/src/app.ts\n@@ -1,1 +1,2 @@\n import x\n+import y\n",
			want: map[string]int{}, outside: 1,
		},
		"a removed line that reads like a header": {
			diff: "--- a/src/app.ts\n+++ b/src/app.ts\n" +
				"@@ -9,2 +9,1 @@\n     return 2;\n--- a/x\n" +
				"--- a/pkg/g.go\n+++ b/pkg/g.go\n@@ -12,0 +13,1 @@\n+\tx()\n",
			want: map[string]int{"src.App.b": 1, "pkg.G": 1},
		},
		"a new file": {
			diff: "--- /dev/null\n+++ b/src/new.ts\n@@ -0,0 +1,1 @@\n+export {}\n",
			want: map[string]int{},
		},
	} {
		got, outside := diffCounts(t, ix, tc.diff, tc.side)
		if len(got) != len(tc.want) || outside != tc.outside {
			t.Errorf("%s: %v outside %d, want %v outside %d", name, got, outside, tc.want, tc.outside)
			continue
		}
		for n, lines := range tc.want {
			if got[n] != lines {
				t.Errorf("%s: %s %d lines, want %d", name, n, got[n], lines)
			}
		}
	}
	if _, err := ix.Diff("", "sideways"); err == nil {
		t.Error("an unknown side is an error")
	}
}

func TestCheck(t *testing.T) {
	before := fixture()
	stamp := func(name, file, body string) Stamp {
		return Stamp{ID: facts.FactID("r", facts.KindSymbol, name, file), BodyHash: body, Name: name}
	}
	after := NewIndex([]facts.Fact{
		sym("src.App.a", "src/app.ts", 30, 32, "a1"),     // shifted
		sym("src.App.b", "src/app.ts", 34, 38, "b2"),     // rewritten
		sym("core/src.f", "core/src/app.ts", 1, 3, "f1"), // moved
		sym("lib.f", "lib/app.ts", 1, 3, "zz"),           // unrelated f, other body
		sym("pkg.G", "pkg/g.go", 12, 0, ""),              // no version
	})
	_ = before
	for name, tc := range map[string]struct {
		st     Stamp
		status string
		now    string
	}{
		"shifted":    {stamp("src.App.a", "src/app.ts", "a1"), StatusCurrent, "src.App.a"},
		"rewritten":  {stamp("src.App.b", "src/app.ts", "b1"), StatusRewritten, "src.App.b"},
		"moved":      {stamp("src.f", "src/app.ts", "f1"), StatusMoved, "core/src.f"},
		"gone":       {stamp("src.App", "src/app.ts", "app"), StatusGone, ""},
		"no version": {stamp("pkg.G", "pkg/g.go", ""), StatusPresent, "pkg.G"},
		"id only":    {Stamp{ID: stamp("src.f", "src/app.ts", "f1").ID, BodyHash: "f1"}, StatusGone, ""},
	} {
		r := after.Check([]Stamp{tc.st})[0]
		now := ""
		if r.Symbol != nil {
			now = r.Symbol.Name
		}
		if r.Status != tc.status || now != tc.now {
			t.Errorf("%s: %s %q, want %s %q", name, r.Status, now, tc.status, tc.now)
		}
	}
	// Moved and rewritten at once is not guessed: it is gone, with the same-name
	// symbols listed.
	r := after.Check([]Stamp{stamp("src.f", "src/app.ts", "f9")})[0]
	if r.Status != StatusGone || len(r.Candidates) != 2 {
		t.Errorf("moved and rewritten: %s with %v, want gone with both f's", r.Status, names(r.Candidates))
	}
}
