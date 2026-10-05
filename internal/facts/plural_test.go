package facts

import "testing"

func TestPluralKind(t *testing.T) {
	for kind, want := range map[string]string{
		KindSymbol: "symbols", KindModule: "modules", KindRoute: "routes", KindService: "services",
		KindDependency: "dependencies", KindStorage: "storage", KindTestRef: "test_refs", "unknown": "unknowns",
	} {
		if got := PluralKind(kind); got != want {
			t.Errorf("PluralKind(%q) = %q, want %q", kind, got, want)
		}
	}
}

// The impact summary counts kinds in words, and a count of two is not "2 dependencys".
func TestImpactSet_SummarySpellsPluralKinds(t *testing.T) {
	s := NewStore()
	s.Add(
		Fact{Kind: KindModule, Name: "core", File: "core"},
		Fact{Kind: KindDependency, Name: "a -> core", File: "a/a.go", Relations: []Relation{{Kind: RelImports, Target: "core"}}},
		Fact{Kind: KindDependency, Name: "b -> core", File: "b/b.go", Relations: []Relation{{Kind: RelImports, Target: "core"}}},
		Fact{Kind: KindStorage, Name: "orders", File: "db/schema.sql", Relations: []Relation{{Kind: RelDependsOn, Target: "core"}}},
	)
	s.BuildGraph()
	if got, want := s.Graph().ImpactSet("core", 1, 100, false).Summary, "3 total dependents — depth 1: 2 dependencies, 1 storage"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}
