package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// exploreFixture holds more facts under one directory than the 500 the directory
// summary used to sample, in several files, with a module to explore.
func exploreFixture() []facts.Fact {
	ff := []facts.Fact{
		{Kind: facts.KindModule, Name: "pkg/big", File: "pkg/big", Props: map[string]any{"language": "go"}},
		{Kind: facts.KindModule, Name: "pkg/big/inner", File: "pkg/big/inner"},
		{Kind: facts.KindDependency, Name: "pkg/big -> fmt", File: "pkg/big/f0.go", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "fmt"}}},
		{Kind: facts.KindDependency, Name: "pkg/big -> os", File: "pkg/big/f1.go", Relations: []facts.Relation{{Kind: facts.RelImports, Target: "os"}}},
		{Kind: facts.KindSymbol, Name: "cmd.main", File: "cmd/main.go", Line: 5, Relations: []facts.Relation{
			{Kind: facts.RelCalls, Target: "pkg/big.Fn000"}, {Kind: facts.RelDeclares, Target: "cmd"},
		}},
	}
	for i := range 600 {
		ff = append(ff, facts.Fact{
			Kind: facts.KindSymbol, Name: fmt.Sprintf("pkg/big.Fn%03d", i), File: fmt.Sprintf("pkg/big/f%d.go", i%7), Line: 600 - i,
			Props: map[string]any{"symbol_kind": facts.SymbolFunc, "exported": true},
			Relations: []facts.Relation{
				{Kind: facts.RelDeclares, Target: "pkg/big"},
				{Kind: facts.RelCalls, Target: fmt.Sprintf("pkg/big.Fn%03d", (i+1)%600)},
			},
		})
	}
	return ff
}

// storeOf holds ff as given, or with the facts and each fact's relations reversed.
func storeOf(ff []facts.Fact, reverse bool) *facts.Store {
	s := facts.NewStore()
	if !reverse {
		s.Add(ff...)
	} else {
		for i := len(ff) - 1; i >= 0; i-- {
			f := ff[i]
			rels := make([]facts.Relation, len(f.Relations))
			for j, r := range f.Relations {
				rels[len(rels)-1-j] = r
			}
			f.Relations = rels
			s.Add(f)
		}
	}
	s.BuildGraph()
	return s
}

// A store just extracted and the same store read back from disk hold their facts in
// different orders. explore must describe one repository the same way from both.
func TestExplore_SameTextWhateverTheStoreOrder(t *testing.T) {
	ff := exploreFixture()
	srv := newTestServer(nil)
	renders := map[string]func(store *facts.Store, sb *strings.Builder) bool{
		"directory":      func(st *facts.Store, sb *strings.Builder) bool { return srv.exploreDirectory(st, "pkg/big", sb) },
		"file depth 2":   func(st *facts.Store, sb *strings.Builder) bool { return srv.exploreFile(st, "pkg/big/f3.go", 2, sb) },
		"symbol depth 2": func(st *facts.Store, sb *strings.Builder) bool { return srv.exploreSymbol(st, "Fn00", 2, sb) },
		"module":         func(st *facts.Store, sb *strings.Builder) bool { return srv.exploreModule(st, "pkg/big", 1, "", sb) },
		"module compact": func(st *facts.Store, sb *strings.Builder) bool {
			return srv.exploreModule(st, "pkg/big", 2, modeCompact, sb)
		},
		"module substring": func(st *facts.Store, sb *strings.Builder) bool {
			return srv.exploreModuleSubstring(st, "big", 1, "", sb)
		},
	}
	for name, render := range renders {
		var a, b strings.Builder
		if !render(storeOf(ff, false), &a) || !render(storeOf(ff, true), &b) {
			t.Errorf("%s: nothing rendered", name)
			continue
		}
		if a.String() != b.String() {
			t.Errorf("%s: output depends on the store's order.\n--- as written:\n%.600s\n--- reversed:\n%.600s", name, a.String(), b.String())
		}
	}
}

// The directory summary sits under the full total, so it has to count every fact.
func TestExploreDirectory_CountsEveryFact(t *testing.T) {
	var sb strings.Builder
	if !newTestServer(nil).exploreDirectory(storeOf(exploreFixture(), false), "pkg/big", &sb) {
		t.Fatal("nothing rendered")
	}
	out := sb.String()
	for _, want := range []string{
		"Total facts: 603", "- Files: 8", "- Symbols: 600", "- Dependencies: 2", // the module's own directory is not under "pkg/big/"
		"| pkg/big.Fn595 | function | pkg/big/f0.go | 5 |", // first in source order: file, then line
		"... and 570 more symbols",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%.900s", want, out)
		}
	}
}

func TestExploreFile_ListsInSourceOrder(t *testing.T) {
	var sb strings.Builder
	if !newTestServer(nil).exploreFile(storeOf(exploreFixture(), true), "pkg/big/f3.go", 1, &sb) {
		t.Fatal("nothing rendered")
	}
	out := sb.String()
	// f3.go holds Fn003, Fn010, ...; line = 600-i, so the last of them comes first.
	first, second := strings.Index(out, "pkg/big.Fn598"), strings.Index(out, "pkg/big.Fn591")
	if first < 0 || second < 0 || first > second {
		t.Errorf("want Fn598 (line 2) before Fn591 (line 9):\n%.400s", out)
	}
}
