package impact

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestCanonicalTarget_FileRefUsesExtensionlessDependencyNode(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindFileRef, Name: "ui/src/api/api.ts", File: "ui/src/api/api.ts"},
		facts.Fact{Kind: facts.KindDependency, Name: "ui/src/page -> ui/src/api/api", Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: "ui/src/api/api"},
		}},
	)
	got, res := canonicalTarget(store, "ui/src/api/api.ts")
	if got != "ui/src/api/api" {
		t.Fatalf("canonical target = %q", got)
	}
	if res == nil || res.Query != "ui/src/api/api.ts" || res.Matched != got {
		t.Fatalf("normalization was not disclosed: %+v", res)
	}
}

func TestCanonicalTarget_GenuineFileRefStaysPut(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindFileRef, Name: "ui/src/setup.ts", File: "ui/src/setup.ts"})
	got, res := canonicalTarget(store, "ui/src/setup.ts")
	if got != "ui/src/setup.ts" || res != nil {
		t.Fatalf("got %q, %+v", got, res)
	}
}
