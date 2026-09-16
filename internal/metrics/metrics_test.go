package metrics

import (
	"math"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/mcputil"
)

// TestIsGeneratedPath checks the conservative build/generated-output segment match.
func TestIsGeneratedPath(t *testing.T) {
	for _, p := range []string{"data/build/kspCaches/x", "Pods/Alamofire", "a/node_modules/b", "x/generated/y"} {
		if !mcputil.IsGeneratedPath(p) {
			t.Errorf("mcputil.IsGeneratedPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"internal/domain/courses", "buildconfig/x", "src/components/rules"} {
		if mcputil.IsGeneratedPath(p) {
			t.Errorf("mcputil.IsGeneratedPath(%q) = true, want false", p)
		}
	}
}

// TestCollectExcludesGeneratedPackages verifies modules under build/generated
// output directories are not counted as packages by the metrics collector.
func TestCollectExcludesGeneratedPackages(t *testing.T) {
	jsonl := `{"kind":"module","name":"app/domain","repo":"r"}
{"kind":"module","name":"data/build/kspCaches/gen","repo":"r"}
{"kind":"symbol","name":"app/domain.Course","file":"app/domain/course.go","props":{"symbol_kind":"struct"},"relations":[{"kind":"declares","target":"app/domain"}]}
{"kind":"symbol","name":"data/build/kspCaches/gen.Backup","file":"data/build/kspCaches/gen/b.kt","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"data/build/kspCaches/gen"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, _, _ := collect(store)
	for _, p := range pkgs {
		if mcputil.IsGeneratedPath(p.Name) {
			t.Errorf("generated package leaked into metrics: %q", p.Name)
		}
	}
	if len(pkgs) != 1 || pkgs[0].Name != "app/domain" {
		t.Fatalf("expected only app/domain, got %+v", pkgs)
	}
}

// TestCollectExcludesNonProductionRoles verifies modules tagged
// module_role=test or =tooling are excluded from the metrics population (and
// counted), while production/unknown/untagged modules are kept — and that a
// test→production import does not inflate the production package's Ca.
func TestCollectExcludesNonProductionRoles(t *testing.T) {
	jsonl := `{"kind":"module","name":"Sources/Core","repo":"r","props":{"module_role":"production"}}
{"kind":"module","name":"Sources/Feature","repo":"r","props":{"module_role":"production"}}
{"kind":"module","name":"Scripts/Localizations","repo":"r","props":{"module_role":"tooling"}}
{"kind":"module","name":"Tests/Core","repo":"r","props":{"module_role":"test"}}
{"kind":"module","name":"Sources/Legacy","repo":"r"}
{"kind":"symbol","name":"Sources/Core.Thing","file":"Sources/Core/t.swift","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"Sources/Core"}]}
{"kind":"symbol","name":"Tests/Core.ThingTests","file":"Tests/Core/t.swift","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"Tests/Core"}]}
{"kind":"dependency","name":"Sources/Feature -> Core","file":"r/Sources/Feature/f.swift","relations":[{"kind":"imports","target":"Sources/Core"}]}
{"kind":"dependency","name":"Tests/Core -> Core","file":"r/Tests/Core/t.swift","relations":[{"kind":"imports","target":"Sources/Core"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, edges, excluded := collect(store)
	if len(excluded) != 2 {
		t.Errorf("excluded count: got %d want 2 (Tests/Core + Scripts/Localizations)", len(excluded))
	}
	names := make(map[string]bool)
	for _, p := range pkgs {
		names[p.Name] = true
	}
	for _, want := range []string{"Sources/Core", "Sources/Feature", "Sources/Legacy"} {
		if !names[want] {
			t.Errorf("expected production/untagged package %q to be kept", want)
		}
	}
	for _, bad := range []string{"Tests/Core", "Scripts/Localizations"} {
		if names[bad] {
			t.Errorf("excluded module %q leaked into metrics population", bad)
		}
	}
	// The Feature→Core edge survives; the Tests/Core→Core edge is dropped, so
	// Core's Ca is 1 (from Feature only), not 2.
	m := byName(compute(pkgs, edges))
	if got := m["Sources/Core"].Ca; got != 1 {
		t.Errorf("Sources/Core Ca: got %d want 1 (test import must not count)", got)
	}
}

// TestCollectExcludesUntaggedTestPaths verifies the path-heuristic fallback for
// extractors that never set module_role (Python emits a module fact for EVERY
// directory, Go/TS emit none for role): an untagged module whose path looks like
// test/tooling is excluded (and counted) exactly like a role-tagged one, and its
// import into a production package does not inflate that package's Ca. Untagged
// production paths are still kept.
func TestCollectExcludesUntaggedTestPaths(t *testing.T) {
	jsonl := `{"kind":"module","name":"airflow/models","repo":"r","props":{"language":"python"}}
{"kind":"module","name":"devel-common/src/tests_common/test_utils","repo":"r","props":{"language":"python"}}
{"kind":"module","name":"airflow-e2e-tests/tests/e2e_test_utils","repo":"r","props":{"language":"python"}}
{"kind":"symbol","name":"airflow/models.DagModel","file":"airflow/models/dag.py","props":{"symbol_kind":"class","language":"python"},"relations":[{"kind":"declares","target":"airflow/models"}]}
{"kind":"symbol","name":"tu.Helper","file":"devel-common/src/tests_common/test_utils/h.py","props":{"symbol_kind":"class","language":"python"},"relations":[{"kind":"declares","target":"devel-common/src/tests_common/test_utils"}]}
{"kind":"dependency","name":"devel-common/src/tests_common/test_utils/h -> airflow.models","file":"r/devel-common/src/tests_common/test_utils/h.py","relations":[{"kind":"imports","target":"airflow/models"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, edges, excluded := collect(store)
	if len(excluded) != 2 {
		t.Errorf("excluded count: got %d want 2 (both untagged test-path modules)", len(excluded))
	}
	names := make(map[string]bool)
	for _, p := range pkgs {
		names[p.Name] = true
	}
	if !names["airflow/models"] {
		t.Errorf("untagged production package airflow/models should be kept")
	}
	for _, bad := range []string{"devel-common/src/tests_common/test_utils", "airflow-e2e-tests/tests/e2e_test_utils"} {
		if names[bad] {
			t.Errorf("untagged test-path module %q leaked into production population", bad)
		}
	}
	// The test_utils→models import must be dropped, so models has no phantom Ca.
	m := byName(compute(pkgs, edges))
	if got := m["airflow/models"].Ca; got != 0 {
		t.Errorf("airflow/models Ca: got %d want 0 (test import must not count)", got)
	}
}

// TestAggregateMetrics_TypelessExcludedFromAverages verifies that packages with
// no classes/interfaces (N==0, e.g. Kotlin/Compose function-only packages) are
// kept OUT of the avg I / avg D and analyzed count (their A/D are undefined and
// would otherwise skew the aggregates), while still being counted as type-less
// and still eligible to be the most-depended-upon package. The averages, the
// analyzed count, and the off-main-sequence count all describe the same N≥1
// population — the P4 consistency guarantee.
func TestAggregateMetrics_TypelessExcludedFromAverages(t *testing.T) {
	results := []PackageMetric{
		// analyzed, on the main sequence
		{Package: "a", ClassesInterfaces: 4, Ca: 2, Ce: 2, Instability: 0.5, Distance: 0.0},
		// analyzed, off the main sequence (rigid): N≥3, D>0.7, coupled
		{Package: "b", ClassesInterfaces: 5, Ca: 10, Ce: 0, Instability: 0.0, Distance: 1.0},
		// type-less: N==0 but heavily depended upon (Compose-style)
		{Package: "compose/theme", ClassesInterfaces: 0, Ca: 30, Ce: 1, Instability: 0.03, Distance: 0.97},
	}
	agg := aggregateMetrics(results)

	if agg.analyzed != 2 {
		t.Errorf("analyzed: got %d want 2 (type-less package excluded)", agg.analyzed)
	}
	if agg.typeless != 1 {
		t.Errorf("typeless: got %d want 1", agg.typeless)
	}
	// Averages over the analyzed set only: I=(0.5+0)/2=0.25, D=(0.0+1.0)/2=0.5.
	// The type-less D=0.97 must NOT drag avg D up.
	if agg.avgI != 0.25 || agg.avgD != 0.5 {
		t.Errorf("avg I/D: got %.3f/%.3f want 0.25/0.5", agg.avgI, agg.avgD)
	}
	if agg.offMain != 1 {
		t.Errorf("offMain: got %d want 1 (only b)", agg.offMain)
	}
	// Ca is meaningful without types, so the type-less package wins most-depended.
	if agg.mostCoupled.Package != "compose/theme" {
		t.Errorf("mostCoupled: got %q want compose/theme (Ca=30)", agg.mostCoupled.Package)
	}
}

// TestExcludedNote formats only the non-zero exclusion categories.
func TestExcludedNote(t *testing.T) {
	cases := []struct {
		typeless, testTooling int
		want                  string
	}{
		{0, 0, ""},
		{3, 0, " (3 type-less excluded)"},
		{0, 5, " (5 test/tooling excluded)"},
		{3, 5, " (3 type-less, 5 test/tooling excluded)"},
	}
	for _, c := range cases {
		if got := ExcludedNote(c.typeless, c.testTooling); got != c.want {
			t.Errorf("ExcludedNote(%d,%d) = %q, want %q", c.typeless, c.testTooling, got, c.want)
		}
	}
}

// TestRenderSummary_ReportsTypeless checks the headline reflects the analyzed
// (N≥1) count and surfaces the type-less exclusion.
func TestRenderSummary_ReportsTypeless(t *testing.T) {
	results := []PackageMetric{
		{Package: "a", ClassesInterfaces: 4, Ca: 2, Ce: 2, Instability: 0.5, Distance: 0.2},
		{Package: "compose/theme", ClassesInterfaces: 0, Ca: 30, Ce: 1, Instability: 0.03, Distance: 0.97},
	}
	out := renderSummary(results, results, nil, "", "")
	if !strings.Contains(out, "1 package(s) analyzed") {
		t.Errorf("summary should report 1 analyzed package (N≥1), got:\n%s", out)
	}
	if !strings.Contains(out, "1 type-less excluded") {
		t.Errorf("summary should note the type-less exclusion, got:\n%s", out)
	}
}

// TestCollectResolvesFromSideToModule verifies the importing side of a
// dependency fact is resolved to its enclosing module. Python emits a file-stem
// importer ("pkg/a/file"); Go emits a module dir ("gopkg"). Both must yield an
// internal edge, and the Go case must be unchanged (resolveToModule is a no-op).
func TestCollectResolvesFromSideToModule(t *testing.T) {
	jsonl := `{"kind":"module","name":"pkg/a","repo":"r"}
{"kind":"module","name":"pkg/b","repo":"r"}
{"kind":"module","name":"gopkg","repo":"r"}
{"kind":"module","name":"gopkg2","repo":"r"}
{"kind":"dependency","name":"pkg/a/file -> b.thing","file":"r/pkg/a/file.py","relations":[{"kind":"imports","target":"pkg/b"}]}
{"kind":"dependency","name":"gopkg -> example.com/x","file":"r/gopkg/x.go","relations":[{"kind":"imports","target":"gopkg2"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	_, edges, _ := collect(store)
	want := map[string]string{"pkg/a": "pkg/b", "gopkg": "gopkg2"}
	got := make(map[string]string)
	for _, e := range edges {
		got[e.From] = e.To
	}
	for from, to := range want {
		if got[from] != to {
			t.Errorf("edge %s -> %s missing; got edges %+v", from, to, edges)
		}
	}
	if len(edges) != 2 {
		t.Errorf("expected exactly 2 edges, got %d: %+v", len(edges), edges)
	}
}

// TestCollectCountsAbstractProp verifies a symbol flagged props.abstract=true
// (Python ABC/Protocol, Java/Kotlin abstract class) counts toward abstractness,
// and symbol_kind=interface still does too.
func TestCollectCountsAbstractProp(t *testing.T) {
	jsonl := `{"kind":"module","name":"p","repo":"r"}
{"kind":"symbol","name":"p.Concrete","file":"p/c.py","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"p"}]}
{"kind":"symbol","name":"p.AbstractBase","file":"p/a.py","props":{"symbol_kind":"class","abstract":true},"relations":[{"kind":"declares","target":"p"}]}
{"kind":"symbol","name":"p.Iface","file":"p/i.go","props":{"symbol_kind":"interface"},"relations":[{"kind":"declares","target":"p"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, _, _ := collect(store)
	if len(pkgs) != 1 {
		t.Fatalf("expected 1 package, got %+v", pkgs)
	}
	p := pkgs[0]
	if p.Classes != 1 {
		t.Errorf("Classes: got %d want 1 (only Concrete)", p.Classes)
	}
	if p.Interfaces != 2 {
		t.Errorf("Interfaces (abstract types): got %d want 2 (AbstractBase + Iface)", p.Interfaces)
	}
}

// TestCollectAbstractPropDemotesInterface verifies the `abstract` prop is
// authoritative when present: a Ruby namespace module (symbol_kind=interface with
// abstract:false) counts as a concrete class, while a Ruby mixin module
// (abstract:true) and a Go interface (no prop) count as abstract.
func TestCollectAbstractPropDemotesInterface(t *testing.T) {
	jsonl := `{"kind":"module","name":"p","repo":"r"}
{"kind":"symbol","name":"p.Namespace","file":"p/n.rb","props":{"symbol_kind":"interface","abstract":false},"relations":[{"kind":"declares","target":"p"}]}
{"kind":"symbol","name":"p.Mixin","file":"p/m.rb","props":{"symbol_kind":"interface","abstract":true},"relations":[{"kind":"declares","target":"p"}]}
{"kind":"symbol","name":"p.Iface","file":"p/i.go","props":{"symbol_kind":"interface"},"relations":[{"kind":"declares","target":"p"}]}
{"kind":"symbol","name":"p.Model","file":"p/o.rb","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"p"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, _, _ := collect(store)
	if len(pkgs) != 1 {
		t.Fatalf("expected 1 package, got %+v", pkgs)
	}
	p := pkgs[0]
	if p.Classes != 2 {
		t.Errorf("Classes: got %d want 2 (Model + demoted Namespace)", p.Classes)
	}
	if p.Interfaces != 2 {
		t.Errorf("Interfaces: got %d want 2 (Mixin via prop + Iface via kind)", p.Interfaces)
	}
}

// TestCollectExcludesEnumAndDIFromN verifies R2/R3a: Dagger DI infrastructure
// (di_component/di_module) and enums (enum:true, matching Java's symbol_kind=enum
// exclusion) do not count toward N, while data classes/records do and are tracked
// as data holders. A pure-DI package becomes type-less (N=0).
func TestCollectExcludesEnumAndDIFromN(t *testing.T) {
	jsonl := `{"kind":"module","name":"app/model","repo":"r"}
{"kind":"module","name":"app/di","repo":"r"}
{"kind":"symbol","name":"app/model.User","props":{"symbol_kind":"class","data_class":true},"relations":[{"kind":"declares","target":"app/model"}]}
{"kind":"symbol","name":"app/model.Plain","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"app/model"}]}
{"kind":"symbol","name":"app/model.Status","props":{"symbol_kind":"class","enum":true},"relations":[{"kind":"declares","target":"app/model"}]}
{"kind":"symbol","name":"app/di.AppComponent","props":{"symbol_kind":"interface","di_component":true},"relations":[{"kind":"declares","target":"app/di"}]}
{"kind":"symbol","name":"app/di.NetModule","props":{"symbol_kind":"class","di_module":true},"relations":[{"kind":"declares","target":"app/di"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, _, _ := collect(store)
	m := byName(compute(pkgs, nil))

	if got := m["app/model"].ClassesInterfaces; got != 2 {
		t.Errorf("app/model N = %d, want 2 (User + Plain; enum Status excluded)", got)
	}
	if got := m["app/model"].DataHolderRatio; !approx(got, 0.5) {
		t.Errorf("app/model data-holder ratio = %.3f, want 0.5 (User of {User,Plain})", got)
	}
	if got := m["app/di"].ClassesInterfaces; got != 0 {
		t.Errorf("app/di N = %d, want 0 (DI component + module both excluded → type-less)", got)
	}
}

// TestCollectExcludesTSInterfaceFromN verifies that a TypeScript interface is a
// structural data shape (like a `type` alias) and does NOT count toward N, so a
// props/`types.ts`-only package is type-less (N=0) instead of falsely A=1.0.
// A TS `abstract class` is the sole abstractness source for TS. A Go interface in
// another package still counts (the carve-out is language-scoped to typescript).
func TestCollectExcludesTSInterfaceFromN(t *testing.T) {
	jsonl := `{"kind":"module","name":"ui/types","repo":"r"}
{"kind":"module","name":"ui/svc","repo":"r"}
{"kind":"module","name":"go/pkg","repo":"r"}
{"kind":"symbol","name":"ui/types.Props","props":{"symbol_kind":"interface","language":"typescript"},"relations":[{"kind":"declares","target":"ui/types"}]}
{"kind":"symbol","name":"ui/types.UserId","props":{"symbol_kind":"type","language":"typescript"},"relations":[{"kind":"declares","target":"ui/types"}]}
{"kind":"symbol","name":"ui/svc.BaseService","props":{"symbol_kind":"class","abstract":true,"language":"typescript"},"relations":[{"kind":"declares","target":"ui/svc"}]}
{"kind":"symbol","name":"ui/svc.Concrete","props":{"symbol_kind":"class","language":"typescript"},"relations":[{"kind":"declares","target":"ui/svc"}]}
{"kind":"symbol","name":"go/pkg.Reader","props":{"symbol_kind":"interface","language":"go"},"relations":[{"kind":"declares","target":"go/pkg"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, _, _ := collect(store)
	m := byName(compute(pkgs, nil))

	// TS interface + TS type alias both excluded → type-less.
	if got := m["ui/types"].ClassesInterfaces; got != 0 {
		t.Errorf("ui/types N = %d, want 0 (TS interface + type alias both excluded → type-less)", got)
	}
	// TS abstract class counts as abstract; concrete class as concrete.
	if got := m["ui/svc"].ClassesInterfaces; got != 2 {
		t.Errorf("ui/svc N = %d, want 2 (BaseService + Concrete)", got)
	}
	if got := m["ui/svc"].Abstractness; !approx(got, 0.5) {
		t.Errorf("ui/svc A = %.3f, want 0.5 (abstract BaseService of {BaseService,Concrete})", got)
	}
	// Non-TS interface is unaffected by the carve-out.
	if got := m["go/pkg"].ClassesInterfaces; got != 1 {
		t.Errorf("go/pkg N = %d, want 1 (Go interface still counts)", got)
	}
	if got := m["go/pkg"].Abstractness; !approx(got, 1.0) {
		t.Errorf("go/pkg A = %.3f, want 1.0 (Go interface is abstract)", got)
	}
}

// TestDataHolderPackageNotOffMainSequence verifies R3b: a package that is mostly
// value carriers (DTOs / schema models / records) is concrete BY DESIGN, so it is
// not an off-main-sequence smell at all — it is filtered out of the finding rather
// than emitted with "extract interfaces" advice. A non-data rigid package with the
// same I/A/D is still flagged, with the contract-stability framing and confidence.
func TestDataHolderPackageNotOffMainSequence(t *testing.T) {
	// D=1.0 → base confidence 0.6+(1.0-0.7)=0.9 (capped).
	data := PackageMetric{Package: "app/model", ClassesInterfaces: 5, Ca: 10, Ce: 0, Instability: 0.0, Abstractness: 0.0, Distance: 1.0, DataHolderRatio: 0.8}
	logic := PackageMetric{Package: "app/svc", ClassesInterfaces: 5, Ca: 10, Ce: 0, Instability: 0.0, Abstractness: 0.0, Distance: 1.0, DataHolderRatio: 0.0}

	// The shared predicate must exclude the data-holder package from the count.
	if isOffMainSequence(data) {
		t.Errorf("data-holder package (ratio 0.8) should NOT be off-main-sequence")
	}
	if !isOffMainSequence(logic) {
		t.Errorf("non-data rigid package should be off-main-sequence")
	}

	ins := metricsToInsights([]PackageMetric{data, logic})
	offTitle := func(pkg string) bool {
		for _, in := range ins {
			if strings.Contains(in.Title, pkg) && strings.HasPrefix(in.Title, "Off main sequence") {
				return true
			}
		}
		return false
	}

	if offTitle("app/model") {
		t.Errorf("data-holder package should not produce an off-main insight")
	}
	if !offTitle("app/svc") {
		t.Fatalf("non-data rigid package should produce an off-main insight")
	}

	var l facts.Insight
	for _, in := range ins {
		if strings.Contains(in.Title, "app/svc") {
			l = in
		}
	}
	if !strings.Contains(l.Description, "rigid") {
		t.Errorf("non-data package should keep the rigid framing, got: %s", l.Description)
	}
	if !approx(l.Confidence, 0.9) {
		t.Errorf("non-data confidence = %.3f, want 0.9", l.Confidence)
	}
}

func byName(ms []PackageMetric) map[string]PackageMetric {
	m := make(map[string]PackageMetric, len(ms))
	for _, x := range ms {
		m[x.Package] = x
	}
	return m
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// TestComputeFullFixture builds a small three-package graph and checks the whole
// PackageMetric set against hand-computed expected values.
//
//	app  -> domain, util   (Ce=2, Ca=0)
//	domain -> util          (Ce=1, Ca=1  [app])
//	util  -> (none)         (Ce=0, Ca=2  [app, domain])
func TestComputeFullFixture(t *testing.T) {
	pkgs := []pkgInput{
		{Name: "app", Classes: 3, Interfaces: 1},    // N=4, A=0.25
		{Name: "domain", Classes: 1, Interfaces: 3}, // N=4, A=0.75
		{Name: "util", Classes: 2, Interfaces: 0},   // N=2, A=0
	}
	edges := []importEdge{
		{"app", "domain"},
		{"app", "util"},
		{"domain", "util"},
	}

	got := byName(compute(pkgs, edges))
	if len(got) != 3 {
		t.Fatalf("expected 3 packages, got %d", len(got))
	}

	tests := []struct {
		pkg       string
		n, ca, ce int
		i, a, d   float64
	}{
		// app: Ca=0, Ce=2 -> I=1.0 ; A=0.25 ; D=|0.25+1-1|=0.25
		{"app", 4, 0, 2, 1.0, 0.25, 0.25},
		// domain: Ca=1, Ce=1 -> I=0.5 ; A=0.75 ; D=|0.75+0.5-1|=0.25
		{"domain", 4, 1, 1, 0.5, 0.75, 0.25},
		// util: Ca=2, Ce=0 -> I=0.0 ; A=0.0 ; D=|0+0-1|=1.0
		{"util", 2, 2, 0, 0.0, 0.0, 1.0},
	}
	for _, tt := range tests {
		m := got[tt.pkg]
		if m.ClassesInterfaces != tt.n {
			t.Errorf("%s N: got %d want %d", tt.pkg, m.ClassesInterfaces, tt.n)
		}
		if m.Ca != tt.ca {
			t.Errorf("%s Ca: got %d want %d", tt.pkg, m.Ca, tt.ca)
		}
		if m.Ce != tt.ce {
			t.Errorf("%s Ce: got %d want %d", tt.pkg, m.Ce, tt.ce)
		}
		if !approx(m.Instability, tt.i) {
			t.Errorf("%s I: got %v want %v", tt.pkg, m.Instability, tt.i)
		}
		if !approx(m.Abstractness, tt.a) {
			t.Errorf("%s A: got %v want %v", tt.pkg, m.Abstractness, tt.a)
		}
		if !approx(m.Distance, tt.d) {
			t.Errorf("%s D: got %v want %v", tt.pkg, m.Distance, tt.d)
		}
	}
}

// TestComputeDedupAndSelfEdges verifies duplicate edges are counted once and
// self-edges are ignored.
func TestComputeDedupAndSelfEdges(t *testing.T) {
	pkgs := []pkgInput{{Name: "a", Classes: 1}, {Name: "b", Classes: 1}}
	edges := []importEdge{
		{"a", "b"},
		{"a", "b"}, // duplicate -> counted once
		{"a", "a"}, // self-edge -> ignored
		{"", "b"},  // empty source -> ignored
		{"a", ""},  // empty target -> ignored
	}
	got := byName(compute(pkgs, edges))
	if got["a"].Ce != 1 {
		t.Errorf("a Ce: got %d want 1 (dedup + self/empty ignored)", got["a"].Ce)
	}
	if got["b"].Ca != 1 {
		t.Errorf("b Ca: got %d want 1", got["b"].Ca)
	}
	if got["a"].Ca != 0 || got["b"].Ce != 0 {
		t.Errorf("unexpected couplings: a.Ca=%d b.Ce=%d", got["a"].Ca, got["b"].Ce)
	}
}

// TestComputeZeroGuards verifies divide-by-zero guards: an isolated package has
// I=0, and a package with no types has A=0.
func TestComputeZeroGuards(t *testing.T) {
	pkgs := []pkgInput{
		{Name: "isolated", Classes: 2, Interfaces: 0}, // Ca+Ce=0 -> I=0 ; A=0 ; D=|0+0-1|=1
		{Name: "empty", Classes: 0, Interfaces: 0},    // N=0 -> A=0
	}
	got := byName(compute(pkgs, nil))

	if !approx(got["isolated"].Instability, 0) {
		t.Errorf("isolated I: got %v want 0", got["isolated"].Instability)
	}
	if !approx(got["isolated"].Distance, 1) {
		t.Errorf("isolated D: got %v want 1", got["isolated"].Distance)
	}
	if !approx(got["empty"].Abstractness, 0) {
		t.Errorf("empty A: got %v want 0 (no divide-by-zero)", got["empty"].Abstractness)
	}
	if got["empty"].ClassesInterfaces != 0 {
		t.Errorf("empty N: got %d want 0", got["empty"].ClassesInterfaces)
	}
}

// TestIsOffMainSequence verifies the size/coupling floor that keeps trivial leaf
// packages out of the off-main-sequence findings.
func TestIsOffMainSequence(t *testing.T) {
	cases := []struct {
		name string
		m    PackageMetric
		want bool
	}{
		{"substantial rigid hub", PackageMetric{ClassesInterfaces: 5, Ca: 24, Ce: 0, Distance: 1.0}, true},
		{"below distance threshold", PackageMetric{ClassesInterfaces: 8, Ca: 4, Ce: 2, Distance: 0.5}, false},
		{"tiny leaf (N<min)", PackageMetric{ClassesInterfaces: 1, Ca: 1, Ce: 0, Distance: 1.0}, false},
		{"isolated (Ca+Ce=0)", PackageMetric{ClassesInterfaces: 6, Ca: 0, Ce: 0, Distance: 1.0}, false},
		{"exactly at N floor, coupled", PackageMetric{ClassesInterfaces: minPainfulTypes, Ca: 2, Ce: 0, Distance: 0.9}, true},
	}
	for _, c := range cases {
		if got := isOffMainSequence(c.m); got != c.want {
			t.Errorf("%s: isOffMainSequence = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestClassify pins the canonical zone mapping shared by the explainer and the
// dashboard. Off-main packages split on instability (pain vs useless); everything
// else is main-sequence (near the line, typed) or neutral.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		m    PackageMetric
		want Zone
	}{
		// Off-main: D>0.7, N≥3, coupled, not data-holders.
		{"pain: stable + concrete", PackageMetric{ClassesInterfaces: 10, Ca: 8, Ce: 0, Instability: 0.0, Abstractness: 0.1, Distance: 0.9}, ZonePain},
		{"useless: unstable + abstract", PackageMetric{ClassesInterfaces: 10, Ca: 0, Ce: 6, Instability: 1.0, Abstractness: 0.9, Distance: 0.9}, ZoneUseless},
		// On/near the line.
		{"main sequence (on the line)", PackageMetric{ClassesInterfaces: 8, Ca: 4, Ce: 4, Instability: 0.5, Abstractness: 0.5, Distance: 0.0}, ZoneMainSequence},
		{"main sequence (within band)", PackageMetric{ClassesInterfaces: 6, Ca: 3, Ce: 1, Instability: 0.25, Abstractness: 0.6, Distance: 0.15}, ZoneMainSequence},
		// Neither: far but not far enough, or no types.
		{"neutral: mid-distance", PackageMetric{ClassesInterfaces: 5, Ca: 3, Ce: 1, Instability: 0.25, Abstractness: 0.0, Distance: 0.7}, ZoneNeutral},
		{"neutral: type-less", PackageMetric{ClassesInterfaces: 0, Ca: 2, Ce: 3, Instability: 0.6, Abstractness: 0.0, Distance: 0.4}, ZoneNeutral},
		{"neutral: data-holder hub (concrete by design)", PackageMetric{ClassesInterfaces: 5, Ca: 10, Ce: 0, Instability: 0.0, Abstractness: 0.0, Distance: 1.0, DataHolderRatio: 0.8}, ZoneNeutral},
	}
	for _, c := range cases {
		if got := Classify(c.m); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSortMetrics checks the sort keys, including the deterministic name
// tie-breaker.
func TestSortMetrics(t *testing.T) {
	mk := func() []PackageMetric {
		return []PackageMetric{
			{Package: "b", Ca: 1, Ce: 5},
			{Package: "a", Ca: 1, Ce: 2},
			{Package: "c", Ca: 3, Ce: 0},
		}
	}

	byCa := mk()
	sortMetrics(byCa, "ca")
	if byCa[0].Package != "c" {
		t.Errorf("sort ca: got %q first, want c", byCa[0].Package)
	}
	// a and b tie on Ca=1; name tie-breaker puts a before b.
	if byCa[1].Package != "a" || byCa[2].Package != "b" {
		t.Errorf("sort ca tie-break: got %q,%q want a,b", byCa[1].Package, byCa[2].Package)
	}

	byCe := mk()
	sortMetrics(byCe, "ce")
	if byCe[0].Package != "b" {
		t.Errorf("sort ce: got %q first, want b", byCe[0].Package)
	}

	byNameSort := mk()
	sortMetrics(byNameSort, "name")
	if byNameSort[0].Package != "a" || byNameSort[2].Package != "c" {
		t.Errorf("sort name: got %q..%q want a..c", byNameSort[0].Package, byNameSort[2].Package)
	}
}

// TestFilterMetrics checks the package-substring and repo filters.
func TestFilterMetrics(t *testing.T) {
	in := []PackageMetric{
		{Package: "internal/app/courses", Repo: "golf"},
		{Package: "internal/domain/order", Repo: "golf"},
		{Package: "pkg/util", Repo: "other"},
	}
	if got := filterMetrics(in, "internal/app", ""); len(got) != 1 || got[0].Package != "internal/app/courses" {
		t.Errorf("package filter: got %+v", got)
	}
	if got := filterMetrics(in, "", "other"); len(got) != 1 || got[0].Package != "pkg/util" {
		t.Errorf("repo filter: got %+v", got)
	}
	if got := filterMetrics(in, "internal", "golf"); len(got) != 2 {
		t.Errorf("combined filter: got %d want 2", len(got))
	}
}

// TestResolveToModule checks that import targets pointing inside a module
// directory resolve to the enclosing module.
func TestResolveToModule(t *testing.T) {
	mods := map[string]struct{}{
		"internal/app":   {},
		"internal/types": {},
	}
	cases := map[string]string{
		"internal/app":                  "internal/app",   // exact
		"internal/types/tournament":     "internal/types", // nested -> ancestor
		"github.com/external/pkg":       "",               // unknown
		"internal/app/sub/deep/feature": "internal/app",   // deep nesting
	}
	for in, want := range cases {
		if got := resolveToModule(in, mods); got != want {
			t.Errorf("resolveToModule(%q): got %q want %q", in, got, want)
		}
	}
}

// TestImporterOf checks parsing of the dependency fact name.
func TestImporterOf(t *testing.T) {
	if got := importerOf("internal/app/courses -> internal/domain/order"); got != "internal/app/courses" {
		t.Errorf("importerOf: got %q", got)
	}
	if got := importerOf("no-arrow-here"); got != "" {
		t.Errorf("importerOf(malformed): got %q want empty", got)
	}
}

// --- new/57 / GAP-MC-05: the same defect fixed/56 fixed in perf, still live here ---

func metricsFixture() []PackageMetric {
	return []PackageMetric{
		{Package: "Sources/NebenanCore", Repo: "ios", ClassesInterfaces: 12},
		{Package: "Sources/NebenanUI", Repo: "ios", ClassesInterfaces: 8},
		{Package: "app/models", Repo: "rails", ClassesInterfaces: 5},
	}
}

// TestResponseTotal_IsTheFilteredCount is GAP-MC-01 in package_metrics. `total` was
// captured before filterMetrics ran and returned in the full-mode JSON beside a
// correctly-filtered `returned`, so package_metrics(package="X", output_mode=full)
// answered `total: 26, returned: 1` — a repo-wide number under the field name a
// consumer is most likely to divide by.
//
// This is the bug fixed in analyze_performance (fixed/56). It survived that run's
// survey because the survey read this tool's SUMMARY path, which is correct. The two
// tools were broken in opposite output modes.
func TestResponseTotal_IsTheFilteredCount(t *testing.T) {
	all := metricsFixture()

	filtered := filterMetrics(all, "Sources", "")
	if len(filtered) != 2 {
		t.Fatalf("fixture: filterMetrics returned %d, want 2", len(filtered))
	}

	// The contract: `total` is the population of the set the caller is shown; the
	// repo-wide count survives under a name that says so.
	resp := buildResponse(all, filtered)
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 (the filtered population, not the repo)", resp.Total)
	}
	if resp.RepoWidePackages != 3 {
		t.Errorf("RepoWidePackages = %d, want 3", resp.RepoWidePackages)
	}
	if resp.Returned != 2 {
		t.Errorf("Returned = %d, want 2", resp.Returned)
	}

	// The shape that made the bug visible: a filter matching nothing must not report
	// a non-zero total.
	empty := buildResponse(all, filterMetrics(all, "does/not/exist", ""))
	if empty.Total != 0 {
		t.Errorf("Total = %d for a filter that matched nothing, want 0 — this is the bug", empty.Total)
	}
	if empty.RepoWidePackages != 3 {
		t.Errorf("RepoWidePackages = %d, want 3 (context survives)", empty.RepoWidePackages)
	}
}

// TestRenderSummary_ExcludedIsScopedToTheFilter pins the second half. renderSummary's
// note read "(N type-less, M test/tooling excluded)" where N was computed over the
// FILTERED results and M was collect()'s repo-wide count — two numbers from different
// populations inside one parenthesis.
func TestRenderSummary_ExcludedIsScopedToTheFilter(t *testing.T) {
	excludedNames := []string{
		"Tests/NebenanCoreTests",
		"Tests/NebenanUITests",
		"spec/models",
	}
	// Under package="Sources", none of the excluded modules match, so the note must
	// not claim any test/tooling exclusions.
	if got := countExcluded(excludedNames, "Sources", ""); got != 0 {
		t.Errorf("countExcluded(package=Sources) = %d, want 0", got)
	}
	// Under package="Tests", two of them do.
	if got := countExcluded(excludedNames, "Tests", ""); got != 2 {
		t.Errorf("countExcluded(package=Tests) = %d, want 2", got)
	}
	// Unfiltered, all of them — this is what --explain reports, and it must not change.
	if got := countExcluded(excludedNames, "", ""); got != 3 {
		t.Errorf("countExcluded(unfiltered) = %d, want 3", got)
	}
}

// TestRenderSummary_NamesTheFilter — the headline states what it filtered, so a zero
// is distinguishable from a filter that silently matched nothing (mcputil.Scoped).
func TestRenderSummary_NamesTheFilter(t *testing.T) {
	all := metricsFixture()
	out := renderSummary(all, filterMetrics(all, "Sources", ""), nil, "Sources", "")
	if !strings.Contains(out, `package="Sources"`) || !strings.Contains(out, "of 3 repo-wide") {
		t.Errorf("summary does not name the filter:\n%s", out)
	}
	unfiltered := renderSummary(all, all, nil, "", "")
	if strings.Contains(unfiltered, "repo-wide") {
		t.Errorf("unfiltered summary should not qualify its count:\n%s", unfiltered)
	}
}

// TestLooksNonProduction_CoversTheTestPathSet pins the enterprise half of new/55.
// looksNonProduction is the fallback for extractors that emit no module_role (Go,
// Python, TypeScript, PHP, C/C++). It delegated to facts.ModuleRoleForPath, whose
// segment list predates facts.IsTestPath and knows nothing of `Mocks`, `__tests__`,
// `testdata`, `fixtures` or the Kotlin-Multiplatform trees — so those packages
// entered the Martin I/A/D production population.
//
// It must be the UNION: IsTestPath does not model `tooling` (Scripts/, bin/,
// fastlane/), which ModuleRoleForPath does and which must not be lost.
func TestLooksNonProduction_CoversTheTestPathSet(t *testing.T) {
	nonProd := []string{
		// Already handled by ModuleRoleForPath.
		"spec/models", "src/test/java/app", "app/androidTest/ui",
		// The tooling half — IsTestPath has no notion of these, so the union must keep them.
		"Scripts/Localizations", "bin", "fastlane",
		// Only IsTestPath knows these.
		"src/components/__tests__", "app/Mocks", "internal/testdata",
		"src/dashboard/fixtures", "shared/src/commonTest/kotlin", "tests_common",
	}
	for _, p := range nonProd {
		if !looksNonProduction(p) {
			t.Errorf("looksNonProduction(%q) = false, want true", p)
		}
	}

	// fixed/28 guard: a production package merely NAMED like a test stays in the
	// production population. Suppressing a real package skews every I/A/D metric.
	prod := []string{
		"app/models", "src/features/contest", "app/services/latest",
		"app/features/abtest", "superset/cli",
	}
	for _, p := range prod {
		if looksNonProduction(p) {
			t.Errorf("looksNonProduction(%q) = true, want false — production package suppressed", p)
		}
	}
}

// TestCollectCountsStructuralDataHolders verifies that a language whose DTOs use no
// dedicated construct still gets the data-holder exemption, via the extractor's
// structural data_holder prop. C# needs it: jellyfin declares 1,552 classes and 13
// records while 278 of those classes are property-only carriers, so reading
// data_class/record alone saw none of them and a third of this explainer's findings
// there were DTO, constant and attribute packages told to extract interfaces.
func TestCollectCountsStructuralDataHolders(t *testing.T) {
	jsonl := `{"kind":"module","name":"Api/Models/UserDtos","repo":"r"}
{"kind":"symbol","name":"Api/Models/UserDtos.UserDto","props":{"symbol_kind":"class","data_holder":true,"language":"csharp"},"relations":[{"kind":"declares","target":"Api/Models/UserDtos"}]}
{"kind":"symbol","name":"Api/Models/UserDtos.NameDto","props":{"symbol_kind":"class","data_holder":true,"language":"csharp"},"relations":[{"kind":"declares","target":"Api/Models/UserDtos"}]}
{"kind":"symbol","name":"Api/Models/UserDtos.Mapper","props":{"symbol_kind":"class","language":"csharp"},"relations":[{"kind":"declares","target":"Api/Models/UserDtos"}]}
`
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	pkgs, _, _ := collect(store)
	m := byName(compute(pkgs, nil))

	if got := m["Api/Models/UserDtos"].ClassesInterfaces; got != 3 {
		t.Errorf("N = %d, want 3", got)
	}
	// Two of the three carry data_holder; the mapper has behaviour and does not.
	if got := m["Api/Models/UserDtos"].DataHolderRatio; !approx(got, 0.667) {
		t.Errorf("data-holder ratio = %.3f, want 0.667", got)
	}
}

// TestDeclaringModuleResolvesAgainstKnownModules is the guard on symbol→package
// attribution.
//
// The old rule was "take the first `declares` target and stop", which assumes a symbol
// declares nothing but its own module. That holds for Go and fails for any language
// whose TYPES declare their own members: a Dart class emits a `declares` edge per
// method, so the first target was a method name and every class minted a phantom
// package. Measured on one repository, 1,746 "packages" against 199 real modules.
//
// The cases below deliberately put the member edge FIRST, which is what an extractor
// emitting members naturally produces. If this only passed with the module edge first,
// it would be pinning emission order rather than correctness.
func TestDeclaringModuleResolvesAgainstKnownModules(t *testing.T) {
	moduleSet := map[string]struct{}{
		"lib/data": {},
		"pkg/a":    {},
	}
	decl := func(targets ...string) facts.Fact {
		rels := make([]facts.Relation, 0, len(targets))
		for _, t := range targets {
			rels = append(rels, facts.Relation{Kind: facts.RelDeclares, Target: t})
		}
		return facts.Fact{Kind: facts.KindSymbol, Relations: rels}
	}

	for _, tc := range []struct {
		name string
		fact facts.Fact
		want string
	}{
		{
			name: "member edges first, module edge last",
			fact: decl("lib/data.Repo.save", "lib/data.Repo.load", "lib/data"),
			want: "lib/data",
		},
		{
			name: "module edge first still works",
			fact: decl("lib/data", "lib/data.Repo.save"),
			want: "lib/data",
		},
		{
			name: "the Go shape: exactly one edge, to the module",
			fact: decl("pkg/a"),
			want: "pkg/a",
		},
		{
			name: "no known module falls back to the first target",
			fact: decl("unmodelled/dir"),
			want: "unmodelled/dir",
		},
		{
			name: "no declares relation at all",
			fact: facts.Fact{Kind: facts.KindSymbol},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := declaringModule(tc.fact, moduleSet); got != tc.want {
				t.Errorf("declaringModule = %q, want %q", got, tc.want)
			}
		})
	}

	// Non-declares relations must not be mistaken for the module.
	mixed := facts.Fact{Kind: facts.KindSymbol, Relations: []facts.Relation{
		{Kind: facts.RelCalls, Target: "lib/data"},
		{Kind: facts.RelDeclares, Target: "lib/data.Repo.save"},
	}}
	if got := declaringModule(mixed, moduleSet); got != "lib/data.Repo.save" {
		t.Errorf("a calls relation must not be read as the declaring module, got %q", got)
	}
}
