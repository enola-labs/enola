package server

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A node several symbols declare says how many, names the others, and caps the list;
// a single declaration adds nothing.
func TestCompactNodeLine_SharedName(t *testing.T) {
	n := facts.TraversalNode{
		Name: "pkg.Request.task", Kind: facts.KindSymbol, File: "Request.swift", Line: 239,
		Declarations: []facts.Declaration{
			{SymbolKind: "variable", File: "Request.swift", Line: 239},
			{SymbolKind: "method", File: "Request.swift", Line: 700},
		},
	}
	got := compactNodeLine(n)
	if !strings.Contains(got, "[2 declarations share this name, edges merged; also method Request.swift:700]") {
		t.Errorf("compactNodeLine = %q", got)
	}

	var many []facts.Declaration
	for i := 0; i < 8; i++ {
		many = append(many, facts.Declaration{SymbolKind: "method", File: "O.java", Line: 10 + i})
	}
	over := facts.TraversalNode{Name: "pkg.over", Kind: facts.KindSymbol, File: "O.java", Line: 10,
		Declarations: many, DeclarationsTotal: 12}
	if got := compactNodeLine(over); !strings.Contains(got, "[12 declarations share this name, edges merged; also method O.java:11, method O.java:12, method O.java:13, +8 more]") {
		t.Errorf("compactNodeLine = %q", got)
	}

	single := facts.TraversalNode{Name: "pkg.Single", Kind: facts.KindSymbol, File: "s.go", Line: 1}
	if got := compactNodeLine(single); strings.Contains(got, "declarations") {
		t.Errorf("single declaration: %q", got)
	}
}
