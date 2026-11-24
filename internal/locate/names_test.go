package locate

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A name finds the symbols whose qualified name ends in it at a boundary, case included,
// and each comes back ready to store as a stamp.
func TestNames(t *testing.T) {
	src := "// Graph is the graph.\ntype Graph struct{}\n\nfunc (g *Graph) nodeFor() {\n\treturn\n}\n\nfunc (g *Graph) nodeForID() {}\n"
	ix := NewIndex([]facts.Fact{
		sym("example.com/m/internal/facts.Graph", "internal/facts/graph.go", 2, 2, "g"),
		sym("example.com/m/internal/facts.Graph.nodeFor", "internal/facts/graph.go", 4, 6, "n"),
		sym("example.com/m/internal/facts.Graph.nodeForID", "internal/facts/graph.go", 8, 8, "i"),
		sym("example.com/m/internal/flows.Analysis.nodeFor", "internal/flows/flows.go", 10, 12, "a"),
		sym("example.com/m/internal/flows.NodeFor", "internal/flows/flows.go", 20, 22, "u"),
	})
	ix.SetSource(func(_, file string) ([]byte, bool) { return []byte(src), file == "internal/facts/graph.go" })

	got := map[string]NameResult{}
	for _, r := range ix.Names([]string{"nodeFor", "Graph.nodeFor", "internal/facts.Graph.nodeFor()", "odeFor", "missing", ""}) {
		got[r.Name] = r
	}
	if r := got["nodeFor"]; r.Match != NameSeveral || len(r.Symbols) != 2 ||
		r.Symbols[0].Name != "example.com/m/internal/facts.Graph.nodeFor" || r.Symbols[1].Name != "example.com/m/internal/flows.Analysis.nodeFor" {
		t.Errorf("a bare name finds both methods and neither nodeForID nor NodeFor: %+v", r)
	}
	for _, n := range []string{"Graph.nodeFor", "internal/facts.Graph.nodeFor()"} {
		r := got[n]
		if r.Match != NameOne || len(r.Symbols) != 1 || r.Symbols[0].Line != 4 {
			t.Fatalf("%s: a qualified name narrows to one: %+v", n, r)
		}
		s := r.Symbols[0]
		if s.ID == "" || s.BodyHash != "n" || s.CitedHash == "" || s.CitedLines != 2 {
			t.Errorf("%s: the match carries a stamp with its declaration line cited inside the symbol: %+v", n, s)
		}
	}
	for _, n := range []string{"odeFor", "missing", ""} {
		if r := got[n]; r.Match != NameNone || len(r.Symbols) != 0 {
			t.Errorf("%q: no symbol answers to it: %+v", n, r)
		}
	}
	// The cited version is the one locate reports for the declaration line, so a stamp made
	// from a name checks the same as one made from a location.
	loc := ix.Locations([]string{"internal/facts/graph.go:4"})[0]
	if c := got["Graph.nodeFor"].Symbols[0]; c.CitedHash != loc.CitedHash || c.CitedLines != loc.CitedLines {
		t.Errorf("name cited %s/%d, location cited %s/%d", c.CitedHash, c.CitedLines, loc.CitedHash, loc.CitedLines)
	}
}
