package locate

import (
	"slices"
	"strings"
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
		"a zero-context deletion inside a body": {
			diff: "--- a/src/app.ts\n+++ b/src/app.ts\n@@ -10,1 +9,0 @@\n-    log();\n",
			want: map[string]int{"src.App.b": 1},
		},
		"a zero-context deletion after a body's last line": {
			diff:    "--- a/src/app.ts\n+++ b/src/app.ts\n@@ -12,1 +11,0 @@\n-\n",
			want:    map[string]int{},
			outside: 1,
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

// A source is asked for the path relative to the repository, whatever a multi-repo
// snapshot prefixed, and a directory named like the repository is kept.
func TestSourceReadsTheRepositoryRelativePath(t *testing.T) {
	text := "def f():\n    pass\n"
	for _, tc := range []struct {
		fact facts.Fact
		want string
	}{
		{facts.Fact{Repo: "r", File: "r/src/a.py", RepoPrefixed: true}, "src/a.py"},
		{facts.Fact{Repo: "r", File: "r/src/a.py"}, "r/src/a.py"},
	} {
		f := tc.fact
		f.Kind, f.Name, f.Line, f.EndLine = facts.KindSymbol, "f", 1, 2
		f.Props = map[string]any{facts.PropBodyHash: facts.BodyHash([]byte(text), 1, 2)}
		ix := NewIndex([]facts.Fact{f})
		var asked string
		ix.SetSource(func(repo, file string) ([]byte, bool) { asked = file; return []byte(text), true })
		ix.Locations([]string{f.File + ":1"})
		if asked != tc.want {
			t.Errorf("%s (prefixed %v): source asked for %q, want %q", f.File, f.RepoPrefixed, asked, tc.want)
		}
	}
}

// A located symbol says whether a multi-repo snapshot prefixed its file, since the text
// cannot: a repository may hold a directory of its own name.
func TestSymbolSaysItsFileWasPrefixed(t *testing.T) {
	for _, prefixed := range []bool{true, false} {
		f := facts.Fact{Kind: facts.KindSymbol, Repo: "r", Name: "f", File: "r/src/a.py", Line: 1, EndLine: 2, RepoPrefixed: prefixed}
		res := NewIndex([]facts.Fact{f}).Locations([]string{"r/src/a.py:1"})
		if len(res) != 1 || len(res[0].Symbols) != 1 {
			t.Fatalf("prefixed %v: got %+v", prefixed, res)
		}
		if got := res[0].Symbols[0].RepoPrefixed; got != prefixed {
			t.Errorf("prefixed %v: symbol says %v", prefixed, got)
		}
	}
}

// A stamp names a function and the lines it cited. When the function is rewritten, the
// cited lines are looked for in its new body, wherever they now sit.
func TestCitedLines(t *testing.T) {
	before := "function f() {\n  setup();\n  if (x >= index) {\n    fail();\n  }\n}\n"
	ix := NewIndex([]facts.Fact{sym("src.f", "src/a.ts", 1, 6, facts.BodyHash([]byte(before), 1, 6))})
	ix.SetSource(func(repo, file string) ([]byte, bool) { return []byte(before), repo == "r" && file == "src/a.ts" })
	loc := ix.Locations([]string{"src/a.ts:3-4"})[0]
	if loc.CitedHash == "" || loc.CitedLines != 2 {
		t.Fatalf("a located range carries its cited version: %+v", loc)
	}
	stamp := Stamp{ID: loc.Symbols[0].ID, BodyHash: loc.Symbols[0].BodyHash, Name: "src.f",
		CitedHash: loc.CitedHash, CitedLines: loc.CitedLines}

	for name, tc := range map[string]struct {
		after  string
		status string
		cited  string
	}{
		"unchanged":                  {before, StatusCurrent, CitedIntact},
		"edited above the cited":     {"function f() {\n  setup();\n  log();\n\n  if (x >= index) {\n    fail();\n  }\n}\n", StatusRewritten, CitedIntact},
		"re-indented and moved down": {"\n\nfunction f() {\n  setup();\n    if (x >= index) {\n        fail();\n    }\n  done();\n}\n", StatusRewritten, CitedIntact},
		"the cited line fixed":       {"function f() {\n  setup();\n  if (x > index) {\n    fail();\n  }\n}\n", StatusRewritten, CitedChanged},
	} {
		lines := strings.Count(tc.after, "\n")
		start := 1 + strings.Count(tc.after[:strings.Index(tc.after, "function")], "\n")
		after := NewIndex([]facts.Fact{sym("src.f", "src/a.ts", start, lines, facts.BodyHash([]byte(tc.after), start, lines))})
		after.SetSource(func(string, string) ([]byte, bool) { return []byte(tc.after), true })
		r := after.Check([]Stamp{stamp})[0]
		if r.Status != tc.status || r.Cited != tc.cited {
			t.Errorf("%s: %s, cited %q; want %s, cited %q", name, r.Status, r.Cited, tc.status, tc.cited)
		}
	}

	// No source: the function's version is still checked, the cited lines are not.
	plain := NewIndex([]facts.Fact{sym("src.f", "src/a.ts", 1, 6, "other")})
	if r := plain.Check([]Stamp{stamp})[0]; r.Status != StatusRewritten || r.Cited != "" {
		t.Errorf("without a source: %s, cited %q; want rewritten, no cited version", r.Status, r.Cited)
	}
	if loc := plain.Locations([]string{"src/a.ts:3"})[0]; loc.CitedHash != "" {
		t.Errorf("without a source a location carries no cited version: %+v", loc)
	}
	// A cited range of blank lines has no version to keep.
	blank := NewIndex([]facts.Fact{sym("src.f", "src/a.ts", 1, 4, "x")})
	blank.SetSource(func(string, string) ([]byte, bool) { return []byte("function f() {\n\n\n}\n"), true })
	if loc := blank.Locations([]string{"src/a.ts:2-3"})[0]; loc.CitedHash != "" {
		t.Errorf("a blank cited range carries no version: %+v", loc)
	}
	// A single line is cited with its neighbours, so a guard added beside it counts.
	one := ix.Locations([]string{"src/a.ts:4"})[0]
	if one.CitedLines != 3 {
		t.Errorf("a single cited line covers itself and one line either side, got %d lines", one.CitedLines)
	}
	guarded := "function f() {\n  setup();\n  if (x >= index) {\n    if (ok) {\n    fail();\n    }\n  }\n}\n"
	g := NewIndex([]facts.Fact{sym("src.f", "src/a.ts", 1, 8, facts.BodyHash([]byte(guarded), 1, 8))})
	g.SetSource(func(string, string) ([]byte, bool) { return []byte(guarded), true })
	single := Stamp{ID: stamp.ID, BodyHash: stamp.BodyHash, CitedHash: one.CitedHash, CitedLines: one.CitedLines}
	if r := g.Check([]Stamp{single})[0]; r.Cited != CitedChanged {
		t.Errorf("a guard added beside a single cited line: cited %q, want changed", r.Cited)
	}
}

// Declarations sharing an id: a stamp stays on the one it named through a rewrite, and when
// nothing says which one it means, the answer says that too.
func TestCheckSharedID(t *testing.T) {
	decl := func(line, end int, body, declHash, kind string) facts.Fact {
		f := sym("src.Box.value", "src/a.ts", line, end, body)
		f.Props[facts.PropDeclHash] = declHash
		f.Props["symbol_kind"] = kind
		return f
	}
	getter := decl(3, 7, "g1", "gd", "getter")
	setter := decl(8, 12, "s1", "sd", "method")
	decl1 := func(line, end int, body, kind string) facts.Fact {
		f := sym("src.Box.value", "src/a.ts", line, end, body)
		f.Props["symbol_kind"] = kind
		return f
	}
	id := getter.Identity()

	for name, tc := range map[string]struct {
		st     Stamp
		after  []facts.Fact
		status string
		line   int
		cands  int
	}{
		"unchanged":                {Stamp{ID: id, BodyHash: "s1", DeclHash: "sd"}, []facts.Fact{getter, setter}, StatusCurrent, 8, 0},
		"stamped one rewritten":    {Stamp{ID: id, BodyHash: "s1", DeclHash: "sd"}, []facts.Fact{getter, decl(8, 13, "s2", "sd", "method")}, StatusRewritten, 8, 0},
		"sibling rewritten":        {Stamp{ID: id, BodyHash: "g1", DeclHash: "gd"}, []facts.Fact{getter, decl(8, 13, "s2", "sd", "method")}, StatusCurrent, 3, 0},
		"no decl, kind decides":    {Stamp{ID: id, BodyHash: "s1", SymbolKind: "method"}, []facts.Fact{getter, decl(8, 13, "s2", "sd", "method")}, StatusRewritten, 8, 0},
		"nothing decides":          {Stamp{ID: id, BodyHash: "s1"}, []facts.Fact{decl(3, 7, "g2", "gd", "getter"), decl(8, 13, "s2", "sd", "method")}, StatusAmbiguous, 0, 2},
		"no version at all":        {Stamp{ID: id}, []facts.Fact{getter, setter}, StatusAmbiguous, 0, 2},
		"same signature twice":     {Stamp{ID: id, BodyHash: "x", DeclHash: "same"}, []facts.Fact{decl(3, 7, "a", "same", "method"), decl(8, 12, "b", "same", "method")}, StatusAmbiguous, 0, 2},
		"signature changed":        {Stamp{ID: id, BodyHash: "s1", DeclHash: "sd"}, []facts.Fact{getter, decl(8, 13, "s2", "new", "method")}, StatusAmbiguous, 0, 2},
		"survivor of another kind": {Stamp{ID: id, BodyHash: "s1", DeclHash: "sd", SymbolKind: "method"}, []facts.Fact{decl1(3, 7, "g1", "getter")}, StatusAmbiguous, 0, 1},
		"alone, no source to read": {Stamp{ID: id, BodyHash: "s1", DeclHash: "sd", SymbolKind: "method"}, []facts.Fact{decl1(8, 13, "s2", "method")}, StatusAmbiguous, 0, 1},
		"alone, never shared":      {Stamp{ID: id, BodyHash: "s1", SymbolKind: "method"}, []facts.Fact{decl1(8, 13, "s2", "method")}, StatusRewritten, 8, 0},
	} {
		r := NewIndex(tc.after).Check([]Stamp{tc.st})[0]
		if r.Status != tc.status || len(r.Candidates) != tc.cands {
			t.Errorf("%s: %s with %d candidates, want %s with %d", name, r.Status, len(r.Candidates), tc.status, tc.cands)
		}
		if tc.line > 0 && (r.Symbol == nil || r.Symbol.Line != tc.line) {
			t.Errorf("%s: resolved to %+v, want line %d", name, r.Symbol, tc.line)
		}
		if tc.status == StatusAmbiguous && r.Symbol != nil {
			t.Errorf("%s: ambiguous, yet picked %+v", name, r.Symbol)
		}
	}
}

// A stamped setter deleted, its getter rewritten: alone under the id, the getter carries no
// decl_hash. With the source its signature is read and differs; from the facts alone the
// survivor cannot be told from the stamped setter rewritten. Neither answer binds to it.
// The stamped setter surviving its getter, rewritten, is found again when the source says so.
func TestCheckSoleSurvivorOfSharedID(t *testing.T) {
	before := []byte("class Box:\n    @property\n    def value(self):\n        return self._value\n\n    @value.setter\n    def value(self, new_value):\n        self._value = new_value\n")
	stamp := Stamp{BodyHash: facts.BodyHash(before, 6, 8), DeclHash: facts.DeclHash(before, 7, 8), SymbolKind: "function"}
	for name, tc := range map[string]struct {
		after      string
		line, end  int
		withSource bool
		status     string
		resolvedTo int
	}{
		"setter deleted, facts only":  {"class Box:\n    @property\n    def value(self):\n        return self._value + 1\n", 3, 4, false, StatusAmbiguous, 0},
		"setter deleted, with source": {"class Box:\n    @property\n    def value(self):\n        return self._value + 1\n", 3, 4, true, StatusAmbiguous, 0},
		"getter deleted, with source": {"class Box:\n    @value.setter\n    def value(self, new_value):\n        self._value = new_value * 2\n", 3, 4, true, StatusRewritten, 3},
		"getter deleted, facts only":  {"class Box:\n    @value.setter\n    def value(self, new_value):\n        self._value = new_value * 2\n", 3, 4, false, StatusAmbiguous, 0},
	} {
		content := []byte(tc.after)
		f := sym("sample.Box.value", "sample.py", tc.line, tc.end, facts.BodyHash(content, tc.line-1, tc.end))
		f.Props[facts.PropSpanStart] = tc.line - 1
		ix := NewIndex([]facts.Fact{f})
		if tc.withSource {
			ix.SetSource(func(string, string) ([]byte, bool) { return content, true })
		}
		st := stamp
		st.ID = f.Identity()
		r := ix.Check([]Stamp{st})[0]
		if r.Status != tc.status {
			t.Errorf("%s: %s, want %s", name, r.Status, tc.status)
		}
		if tc.resolvedTo > 0 && (r.Symbol == nil || r.Symbol.Line != tc.resolvedTo) {
			t.Errorf("%s: resolved to %+v, want line %d", name, r.Symbol, tc.resolvedTo)
		}
		if tc.status == StatusAmbiguous && r.Symbol != nil {
			t.Errorf("%s: ambiguous, yet picked %+v", name, r.Symbol)
		}
	}
}

// The reproduction: a setter edited below the lines a stamp cites, whichever way the
// declarations are ordered, keeps its cited lines.
func TestCitedLinesReadTheStampedDeclaration(t *testing.T) {
	src := func(extra string) string {
		return "class B {\n  get v() {\n    return 1;\n  }\n  set v(n) {\n    const a = n;\n    this.v = a;\n    return;\n" + extra + "  }\n}\n"
	}
	ix := func(content string, setterEnd int) *Index {
		cf := []byte(content)
		g := sym("s.B.v", "a.ts", 2, 4, facts.BodyHash(cf, 2, 4))
		g.Props[facts.PropDeclHash] = facts.DeclHash(cf, 2, 4)
		st := sym("s.B.v", "a.ts", 5, setterEnd, facts.BodyHash(cf, 5, setterEnd))
		st.Props[facts.PropDeclHash] = facts.DeclHash(cf, 5, setterEnd)
		out := NewIndex([]facts.Fact{g, st})
		out.SetSource(func(string, string) ([]byte, bool) { return cf, true })
		return out
	}
	before := ix(src(""), 9)
	loc := before.Locations([]string{"a.ts:7"})[0]
	s := loc.Symbols[0]
	if s.Line != 5 || s.DeclHash == "" {
		t.Fatalf("located %+v, want the setter with a decl_hash", s)
	}
	stamp := Stamp{ID: s.ID, BodyHash: s.BodyHash, DeclHash: s.DeclHash, CitedHash: loc.CitedHash, CitedLines: loc.CitedLines}
	r := ix(src("    log();\n"), 10).Check([]Stamp{stamp})[0]
	if r.Status != StatusRewritten || r.Symbol == nil || r.Symbol.Line != 5 || r.Cited != CitedIntact {
		t.Errorf("got %s on %+v, cited %q; want rewritten on the setter, cited intact", r.Status, r.Symbol, r.Cited)
	}
}

// Two copies of a moved body cannot be told from each other, which is not the same answer
// as a symbol of that name with other code.
func TestCheckMoveTie(t *testing.T) {
	st := Stamp{ID: facts.FactID("r", facts.KindSymbol, "src.f", "src/a.ts"), BodyHash: "f1", Name: "src.f"}
	tie := NewIndex([]facts.Fact{sym("lib.f", "lib/a.ts", 1, 3, "f1"), sym("core.f", "core/a.ts", 1, 3, "f1")}).Check([]Stamp{st})[0]
	if tie.Status != StatusAmbiguous || len(tie.Candidates) != 2 || tie.Symbol != nil {
		t.Errorf("two copies: %+v, want ambiguous with both", tie)
	}
	other := NewIndex([]facts.Fact{sym("lib.f", "lib/a.ts", 1, 3, "other")}).Check([]Stamp{st})[0]
	if other.Status != StatusGone || len(other.Candidates) != 1 {
		t.Errorf("same name, other code: %+v, want gone with one candidate", other)
	}
}

// A body copied into another repository is a copy, not the stamped symbol moved. A stamp
// that names its repository only moves within it; one that names none cannot tell the
// two apart and keeps the old answer.
func TestCheckMoveStaysInStampRepo(t *testing.T) {
	inRepo := func(repo, name, file, body string) facts.Fact {
		f := sym(name, file, 1, 3, body)
		f.Repo = repo
		return f
	}
	stamp := func(repo string) Stamp {
		return Stamp{ID: facts.FactID(repo, facts.KindSymbol, "src.f", "src/a.ts"), BodyHash: "f1", Name: "src.f", Repo: repo}
	}
	copyOnly := NewIndex([]facts.Fact{inRepo("lib", "pkg.f", "pkg/a.ts", "f1")})
	moveAndCopy := NewIndex([]facts.Fact{inRepo("app", "core.f", "core/a.ts", "f1"), inRepo("lib", "pkg.f", "pkg/a.ts", "f1")})

	cases := map[string]struct {
		ix         *Index
		st         Stamp
		status     string
		symbolRepo string
		candidates int
	}{
		"repo not loaded, a copy elsewhere":   {copyOnly, stamp("scratch"), StatusGone, "", 1},
		"moved in its repo, a copy elsewhere": {moveAndCopy, stamp("app"), StatusMoved, "app", 0},
		"no repo, one copy":                   {copyOnly, Stamp{ID: stamp("scratch").ID, BodyHash: "f1", Name: "src.f"}, StatusMoved, "lib", 0},
		"no repo, two copies":                 {moveAndCopy, Stamp{ID: stamp("scratch").ID, BodyHash: "f1", Name: "src.f"}, StatusAmbiguous, "", 2},
	}
	for name, tc := range cases {
		r := tc.ix.Check([]Stamp{tc.st})[0]
		gotRepo := ""
		if r.Symbol != nil {
			gotRepo = r.Symbol.Repo
		}
		if r.Status != tc.status || gotRepo != tc.symbolRepo || len(r.Candidates) != tc.candidates {
			t.Errorf("%s: %s on repo %q with %d candidates, want %s on %q with %d", name, r.Status, gotRepo, len(r.Candidates), tc.status, tc.symbolRepo, tc.candidates)
		}
	}
}

func TestCheckSaysWhatKindOfRewrite(t *testing.T) {
	const stamped = "private f(x: number) {\n  return x;\n}\n"
	version := func(src string) facts.Fact {
		f := sym("src.f", "src/a.ts", 1, 3, facts.BodyHash([]byte(src), 1, 3))
		f.Props[facts.PropInnerHash] = facts.InnerHash([]byte(src), 1, 3)
		return f
	}
	stamp := func(inner string) Stamp {
		return Stamp{ID: facts.FactID("r", facts.KindSymbol, "src.f", "src/a.ts"), Name: "src.f",
			BodyHash: facts.BodyHash([]byte(stamped), 1, 3), InnerHash: inner}
	}
	inner := facts.InnerHash([]byte(stamped), 1, 3)
	if inner == "" {
		t.Fatal("no inner hash for a function with a body")
	}
	for name, tc := range map[string]struct {
		after  string
		inner  string
		status string
		change string
	}{
		"same":               {stamped, inner, StatusCurrent, ""},
		"a modifier":         {"public f(x: number) {\n  return x;\n}\n", inner, StatusRewritten, ChangeSignature},
		"a parameter's type": {"private f(x: string) {\n  return x;\n}\n", inner, StatusRewritten, ChangeSignature},
		"the body":           {"private f(x: number) {\n  return x + 1;\n}\n", inner, StatusRewritten, ChangeBody},
		"both":               {"public f(x: number) {\n  return x + 1;\n}\n", inner, StatusRewritten, ChangeBody},
		"an old stamp":       {"public f(x: number) {\n  return x;\n}\n", "", StatusRewritten, ""},
	} {
		r := NewIndex([]facts.Fact{version(tc.after)}).Check([]Stamp{stamp(tc.inner)})[0]
		if r.Status != tc.status || r.Change != tc.change {
			t.Errorf("%s: %s %q, want %s %q", name, r.Status, r.Change, tc.status, tc.change)
		}
	}
}

// A line in a named nested function without a symbol of its own is an enclosing match that
// names the function, by the symbol and the path down to it. A line in the symbol outside
// them stays a span match.
func TestEnclosingMatchNamesTheNestedFunction(t *testing.T) {
	send := sym("Client.send", "a.ts", 2, 20, "")
	send.Props[facts.PropNested] = []string{"onClose:3-5", "onOpen:6-12", "inner:8-10"}
	ix := NewIndex([]facts.Fact{send})

	for loc, want := range map[string]struct {
		match  string
		nested []string
	}{
		"a.ts:4":     {MatchEnclosing, []string{"Client.send.onClose"}},
		"a.ts:9":     {MatchEnclosing, []string{"Client.send.onOpen.inner"}},
		"a.ts:7":     {MatchEnclosing, []string{"Client.send.onOpen"}},
		"a.ts:15":    {MatchSpan, nil},
		"a.ts:14-16": {MatchSpan, nil},
		"a.ts:4-7":   {MatchEnclosing, []string{"Client.send.onClose", "Client.send.onOpen"}},
	} {
		r := ix.Locations([]string{loc})[0]
		var got []string
		for _, n := range r.Nested {
			got = append(got, n.Name)
		}
		if r.Match != want.match || !slices.Equal(got, want.nested) {
			t.Errorf("%s: %s %v, want %s %v", loc, r.Match, got, want.match, want.nested)
		}
		if len(r.Symbols) != 1 || r.Symbols[0].Name != "Client.send" {
			t.Errorf("%s: symbols %v, want the enclosing Client.send", loc, names(r.Symbols))
		}
	}
}
