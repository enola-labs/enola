package check

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func pkgModule(name, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, File: file, Repo: "r"}
}

func pkgType(module, name, file string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindSymbol, Name: module + "." + name, File: file, Repo: "r",
		Props:     map[string]any{"symbol_kind": "struct"},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: module}},
	}
}

func pkgImport(from, to, file string) facts.Fact {
	return facts.Fact{
		Kind: facts.KindDependency, Name: from + " -> " + to, File: file, Repo: "r",
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: to}},
	}
}

func storeOf(ff []facts.Fact) *facts.Store {
	s := facts.NewStore()
	s.Add(ff...)
	return s
}

func goPair() (base, current []facts.Fact) {
	base = []facts.Fact{
		pkgModule("app/a", "app/a/a.go"), pkgModule("app/b", "app/b/b.go"),
		pkgType("app/a", "A", "app/a/a.go"), pkgType("app/b", "B", "app/b/b.go"),
	}
	current = append(append([]facts.Fact(nil), base...), pkgImport("app/a", "app/b", "app/a/a.go"))
	return base, current
}

func TestAttachPackageMetrics_ReportsTheMove(t *testing.T) {
	base, current := goPair()
	v := AttachPackageMetrics(Verdict{Status: StatusClean}, storeOf(base), storeOf(current), nil, nil, nil, "")
	if v.PackageMetrics == nil || len(v.PackageMetrics.Packages) != 2 {
		t.Fatalf("want app/a and app/b moved, got %+v", v.PackageMetrics)
	}
	out := v.Render()
	for _, want := range []string{"Package metrics (2 moved", "reported, never graded", "app/a  Ca 0  Ce 0→1"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	raw, err := v.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["package_metrics"]; !ok {
		t.Error("JSON verdict carries no package_metrics")
	}
}

func TestAttachPackageMetrics_SkipsUngradedVerdicts(t *testing.T) {
	base, current := goPair()
	for _, s := range []Status{StatusIncomparable, StatusUsageError} {
		v := AttachPackageMetrics(Verdict{Status: s}, storeOf(base), storeOf(current), nil, nil, nil, "")
		if v.PackageMetrics != nil {
			t.Errorf("%s: a verdict that graded nothing must carry no metrics delta", s)
		}
	}
}

func TestAttachPackageMetrics_UnchangedRendersNothing(t *testing.T) {
	base, _ := goPair()
	v := AttachPackageMetrics(Verdict{Status: StatusClean}, storeOf(base), storeOf(base), nil, nil, nil, "")
	if v.PackageMetrics == nil {
		t.Fatal("a graded verdict must say it measured, even when nothing moved")
	}
	if strings.Contains(v.Render(), "Package metrics") {
		t.Error("nothing moved, so the text verdict must stay silent")
	}
}

// On a partial verdict the metrics are scored over the producers both sides share,
// so a language whose extractor ran on one side only does not appear as added.
func TestAttachPackageMetrics_PartialScoresSharedProducersOnly(t *testing.T) {
	base, current := goPair()
	current = append(current,
		pkgModule("py/svc", "py/svc/__init__.py"),
		pkgType("py/svc", "Service", "py/svc/service.py"),
		pkgType("py/svc", "Repo", "py/svc/repo.py"),
	)
	owners := testOwnership()
	owners["python"] = func(rel string) bool { return strings.HasSuffix(rel, ".py") }
	excluded := []ExcludedProducer{{Name: "python", Kind: ProducerExtractor, LackedBy: SideBaseline, CurrentFactsExcluded: 3}}
	v := Verdict{
		Status:       StatusPartialClean,
		Intersection: &IntersectionGrading{SharedExtractors: []string{"go"}, Excluded: excluded},
	}
	v = AttachPackageMetrics(v, storeOf(base), storeOf(current),
		&facts.Snapshot{Facts: base}, &facts.Snapshot{Facts: current}, owners, "")
	if v.PackageMetrics == nil {
		t.Fatal("partial verdict carries no metrics delta")
	}
	for _, r := range v.PackageMetrics.Packages {
		if strings.HasPrefix(r.Package, "py/") {
			t.Errorf("excluded producer's package reported: %+v", r)
		}
	}
	if len(v.PackageMetrics.Packages) != 2 {
		t.Errorf("shared producers' moves must still be reported: %+v", v.PackageMetrics.Packages)
	}
	if got := v.Intersection.Excluded[0].CurrentFactsExcluded; got != 3 {
		t.Errorf("rescoring must not recount the verdict's exclusions: got %d, want 3", got)
	}
}
