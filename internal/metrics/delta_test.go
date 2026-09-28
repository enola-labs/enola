package metrics

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func pm(name string, n, ca, ce int, i, a, d float64) PackageMetric {
	return PackageMetric{Package: name, Repo: "r", ClassesInterfaces: n, Ca: ca, Ce: ce,
		Instability: i, Abstractness: a, Distance: d, RigidCaFloor: minRigidCa}
}

func TestDiffMetrics_UnchangedIsSilent(t *testing.T) {
	ms := []PackageMetric{pm("a", 3, 1, 1, 0.5, 0, 0.5)}
	if rows := diffMetrics(ms, append([]PackageMetric(nil), ms...)); len(rows) != 0 {
		t.Fatalf("unchanged population produced rows: %+v", rows)
	}
}

func TestDiffMetrics_ReportsMovedAddedRemoved(t *testing.T) {
	before := []PackageMetric{
		pm("moved", 3, 2, 1, 0.333, 0, 0.667),
		pm("gone", 2, 0, 1, 1, 0, 0),
		pm("typeless", 0, 1, 1, 0.5, 0, 0.5),
	}
	after := []PackageMetric{
		pm("moved", 3, 2, 3, 0.6, 0, 0.4),
		pm("new", 1, 1, 0, 0, 0, 1),
		pm("typeless", 0, 1, 4, 0.8, 0, 0.2),
	}
	rows := diffMetrics(before, after)
	got := map[string]DeltaRow{}
	for _, r := range rows {
		got[r.Package] = r
	}
	if len(rows) != 3 {
		t.Fatalf("want moved, new, gone; got %+v", rows)
	}
	if _, ok := got["typeless"]; ok {
		t.Error("a package with no types on either side has no A or D and must not be reported")
	}
	if r := got["moved"]; r.DeltaD() != -0.267 {
		t.Errorf("moved ΔD = %v, want -0.267", r.DeltaD())
	}
	if r := got["new"]; r.Before != nil || r.After == nil || r.ZoneBefore != "" {
		t.Errorf("added package: %+v", r)
	}
	if r := got["gone"]; r.After != nil || r.Before == nil || r.ZoneAfter != "" {
		t.Errorf("removed package: %+v", r)
	}
}

// A package whose own numbers are identical but whose zone flipped because the
// population's rigid floor moved is reported, marked incidental, and counted as
// neither worse nor better.
func TestDiffMetrics_FloorOnlyZoneFlipIsIncidental(t *testing.T) {
	b := pm("hub", 4, 6, 0, 0, 0, 1)
	a := b
	a.RigidCaFloor = 9
	if Classify(b) != ZonePain || Classify(a) == ZonePain {
		t.Fatalf("fixture must flip zone on the floor alone: %s → %s", Classify(b), Classify(a))
	}
	d := Delta{}.withRows(diffMetrics([]PackageMetric{b}, []PackageMetric{a}), nil)
	if len(d.Packages) != 1 || !d.Packages[0].ZoneIncidental {
		t.Fatalf("want one incidental row, got %+v", d.Packages)
	}
	if d.Worsened != 0 || d.Improved != 0 {
		t.Errorf("incidental row counted: worsened=%d improved=%d", d.Worsened, d.Improved)
	}
}

func TestDiffMetrics_ZoneChangesRankFirst(t *testing.T) {
	before := []PackageMetric{pm("big", 3, 1, 1, 0.5, 0, 0.1), pm("zone", 4, 6, 1, 0.143, 0, 0.6)}
	after := []PackageMetric{pm("big", 3, 1, 4, 0.8, 0, 0.6), pm("zone", 4, 7, 0, 0, 0, 1)}
	rows := diffMetrics(before, after)
	if len(rows) != 2 || rows[0].Package != "zone" {
		t.Fatalf("zone change must rank before a larger ΔD without one: %+v", rows)
	}
}

func TestComputeDelta_FromStores(t *testing.T) {
	const common = `{"kind":"module","name":"app/a","repo":"r"}
{"kind":"module","name":"app/b","repo":"r"}
{"kind":"module","name":"app/c","repo":"r"}
{"kind":"symbol","name":"app/a.A","file":"app/a/a.go","props":{"symbol_kind":"struct"},"relations":[{"kind":"declares","target":"app/a"}]}
{"kind":"symbol","name":"app/b.B","file":"app/b/b.go","props":{"symbol_kind":"struct"},"relations":[{"kind":"declares","target":"app/b"}]}
{"kind":"symbol","name":"app/c.C","file":"app/c/c.go","props":{"symbol_kind":"struct"},"relations":[{"kind":"declares","target":"app/c"}]}
{"kind":"dependency","name":"app/a -> app/b","file":"r/app/a/a.go","relations":[{"kind":"imports","target":"app/b"}]}
`
	base := storeFrom(t, common)
	cur := storeFrom(t, common+`{"kind":"dependency","name":"app/a -> app/c","file":"r/app/a/a.go","relations":[{"kind":"imports","target":"app/c"}]}
`)
	d := ComputeDelta(base, cur, nil)
	byPkg := map[string]DeltaRow{}
	for _, r := range d.Packages {
		byPkg[r.Package] = r
	}
	if r, ok := byPkg["app/a"]; !ok || r.Before.Ce != 1 || r.After.Ce != 2 {
		t.Errorf("app/a Ce should go 1→2: %+v", r)
	}
	if r, ok := byPkg["app/c"]; !ok || r.Before.Ca != 0 || r.After.Ca != 1 {
		t.Errorf("app/c Ca should go 0→1: %+v", r)
	}
	if _, ok := byPkg["app/b"]; ok {
		t.Error("app/b did not move and must not be reported")
	}
	if d.Before.Analyzed != 3 || d.After.Analyzed != 3 {
		t.Errorf("aggregates: %+v → %+v", d.Before, d.After)
	}

	focused := ComputeDelta(base, cur, FocusFilter("APP/C"))
	if len(focused.Packages) != 1 || focused.Packages[0].Package != "app/c" {
		t.Errorf("focus must narrow rows case-insensitively: %+v", focused.Packages)
	}
	if focused.Before != d.Before || focused.After != d.After {
		t.Error("focus must not narrow the aggregates")
	}
}

func storeFrom(t *testing.T, jsonl string) *facts.Store {
	t.Helper()
	s := facts.NewStore()
	if err := s.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	return s
}
