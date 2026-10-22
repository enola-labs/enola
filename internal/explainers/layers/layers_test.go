package layers

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

// --- helpers ---

func makeModules(names ...string) []facts.Fact {
	var ff []facts.Fact
	for _, n := range names {
		ff = append(ff, facts.Fact{Kind: facts.KindModule, Name: n})
	}
	return ff
}

// makeModulesLang builds module facts that carry a language prop, matching what
// every extractor emits in production.
func makeModulesLang(lang string, names ...string) []facts.Fact {
	var ff []facts.Fact
	for _, n := range names {
		ff = append(ff, facts.Fact{
			Kind:  facts.KindModule,
			Name:  n,
			Props: map[string]any{"language": lang},
		})
	}
	return ff
}

// frameworkFact returns a non-module fact carrying a framework prop, mimicking
// the route/symbol facts extractors emit (e.g. framework=nextjs).
func frameworkFact(fw string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: "marker", Props: map[string]any{"framework": fw}}
}

func makeStore(modules []string, deps map[string][]string) *facts.Store {
	s := facts.NewStore()
	for _, m := range modules {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	for src, targets := range deps {
		for _, tgt := range targets {
			s.Add(facts.Fact{
				Kind: facts.KindDependency,
				File: src + "/file.go",
				Relations: []facts.Relation{
					{Kind: facts.RelImports, Target: tgt},
				},
			})
		}
	}
	return s
}

func findPattern(patterns []*archPattern, name string) *archPattern {
	for _, p := range patterns {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func fwSet(names ...string) map[string]bool {
	m := make(map[string]bool)
	for _, n := range names {
		m[n] = true
	}
	return m
}

// --- Unit tests ---

func TestMatchesLayer(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{"middle segment matches", "src/domain/user", []string{"domain"}, true},
		{"exact segment not substring", "src/domain_helper", []string{"domain"}, false},
		{"case insensitive", "Domain/UseCases", []string{"domain"}, true},
		{"no match", "src/foo/bar", []string{"domain"}, false},
		{"first segment matches", "cmd/server", []string{"cmd"}, true},
		{"last segment matches", "src/api", []string{"api"}, true},
		{"multiple patterns", "src/models", []string{"model", "models"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesLayer(tt.path, tt.patterns)
			if got != tt.want {
				t.Errorf("matchesLayer(%q, %v) = %v, want %v", tt.path, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestPresentFrameworks(t *testing.T) {
	s := facts.NewStore()
	s.Add(frameworkFact("nextjs"))
	s.Add(frameworkFact("react"))
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "x"}) // no framework prop

	fw := presentFrameworks(s.FactsRef())
	if !fw["nextjs"] || !fw["react"] {
		t.Errorf("presentFrameworks = %v, want nextjs and react", fw)
	}
	if len(fw) != 2 {
		t.Errorf("presentFrameworks size = %d, want 2", len(fw))
	}
}

// --- detection: positive cases ---

func TestDetectPatterns_Hexagonal(t *testing.T) {
	modules := makeModules("domain/entity", "application/usecase", "adapter/rest", "presentation/views")
	e := New()
	patterns := e.detectPatterns(modules, nil)

	hexPattern := findPattern(patterns, "hexagonal")
	if hexPattern == nil {
		t.Fatal("expected hexagonal pattern to be detected")
	}

	// Confidence is the share of modules sitting in a layer that carries a
	// direction — here all four — clamped at the heuristic ceiling. It used to
	// blend in the share of the taxonomy's own layer NAMES that appeared, which
	// paid a pattern for being narrow: matching all four names of the four-layer
	// .NET taxonomy across 3% of a repository scored 0.42.
	if math.Abs(hexPattern.Confidence-common.MaxHeuristicConfidence) > 0.01 {
		t.Errorf("confidence = %f, want the heuristic ceiling %f", hexPattern.Confidence, common.MaxHeuristicConfidence)
	}
	if hexPattern.Graded != 4 || hexPattern.Scanned != 4 {
		t.Errorf("graded/scanned = %d/%d, want 4/4", hexPattern.Graded, hexPattern.Scanned)
	}

	if len(hexPattern.Layers) != 4 {
		t.Errorf("matched layers = %d, want 4", len(hexPattern.Layers))
	}
}

func TestDetectPatterns_NextJS(t *testing.T) {
	modules := makeModulesLang("typescript", "app", "components", "hooks", "lib")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("nextjs"))

	if findPattern(patterns, "nextjs") == nil {
		t.Error("expected nextjs pattern to be detected when framework=nextjs is present")
	}
}

func TestDetectPatterns_GoStandard(t *testing.T) {
	modules := makeModulesLang("go", "cmd/server", "internal/auth", "pkg/utils")
	e := New()
	patterns := e.detectPatterns(modules, nil)

	if findPattern(patterns, "go-standard") == nil {
		t.Error("expected go-standard pattern to be detected")
	}
}

func TestDetectPatterns_RailsMVC(t *testing.T) {
	modules := makeModulesLang("ruby", "app/models", "app/controllers", "app/views", "app/helpers")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("rails"))

	if findPattern(patterns, "rails-mvc") == nil {
		t.Error("expected rails-mvc pattern to be detected")
	}
}

func TestDetectPatterns_AndroidClean(t *testing.T) {
	modules := makeModulesLang("kotlin", "domain", "data", "ui", "designsystem")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("android"))

	if findPattern(patterns, "android-clean") == nil {
		t.Error("expected android-clean pattern to be detected")
	}
}

func TestDetectPatterns_IOSClean(t *testing.T) {
	modules := makeModulesLang("swift", "Domain/UseCases", "Data/Network", "Screens", "DesignSystem")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("swiftui"))

	best := firstSelected(e, patterns)
	if best == nil || best.Name != "ios-clean" {
		t.Errorf("expected ios-clean to win, got %v", best)
	}
}

func TestDetectPatterns_SpringLayered(t *testing.T) {
	modules := makeModulesLang("java", "controller", "service", "repository", "entity", "config")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("spring"))

	best := firstSelected(e, patterns)
	if best == nil || best.Name != "spring-layered" {
		t.Errorf("expected spring-layered to win, got %v", best)
	}
}

func TestDetectPatterns_Django(t *testing.T) {
	modules := makeModulesLang("python", "models", "views", "serializers", "urls", "admin")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("django"))

	if findPattern(patterns, "django") == nil {
		t.Error("expected django pattern to be detected")
	}
}

// --- detection: regression cases (the reported bugs) ---

func TestDetectPatterns_PythonNotNextJS(t *testing.T) {
	// Airflow-like Python repo: generic dirs but no nextjs framework signal.
	modules := makeModulesLang("python", "api", "app", "utils", "lib", "services")
	e := New()
	patterns := e.detectPatterns(modules, nil)

	if p := findPattern(patterns, "nextjs"); p != nil {
		t.Errorf("python repo should not be detected as nextjs (got confidence %f)", p.Confidence)
	}
}

func TestDetectPatterns_RailsNotNextJS(t *testing.T) {
	// Discourse-like repo: JS-heavy Rails app, but framework is rails not nextjs.
	modules := makeModulesLang("ruby", "app", "lib", "api", "services", "components")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("rails"))

	if p := findPattern(patterns, "nextjs"); p != nil {
		t.Errorf("rails repo should not be detected as nextjs (got confidence %f)", p.Confidence)
	}
}

func TestDetectPatterns_GenericOOPNotHexagonal(t *testing.T) {
	// Swift/Kotlin-style repo with only generic dirs (no ports/adapters/usecases).
	modules := makeModulesLang("swift", "model", "view", "ui", "networking")
	e := New()
	patterns := e.detectPatterns(modules, fwSet("swiftui"))

	if p := findPattern(patterns, "hexagonal"); p != nil {
		t.Errorf("generic OOP repo should not be detected as hexagonal (got confidence %f)", p.Confidence)
	}
}

func TestDetectPatterns_SingleSignatureNotHexagonal(t *testing.T) {
	// Airflow-like: one stray "infrastructure" directory (a single adapter
	// signature layer) is not enough to call a repo hexagonal.
	modules := makeModulesLang("python", "api", "core", "utils", "infrastructure")
	e := New()
	patterns := e.detectPatterns(modules, nil)

	if findPattern(patterns, "hexagonal") != nil {
		t.Error("a single signature layer should not trigger hexagonal")
	}
}

func TestDetectPatterns_GoNotNextJS(t *testing.T) {
	// A Go repo with an "api" dir must not match nextjs (no nextjs framework, wrong language).
	modules := makeModulesLang("go", "cmd", "internal", "pkg", "api")
	e := New()
	patterns := e.detectPatterns(modules, nil)

	if findPattern(patterns, "nextjs") != nil {
		t.Error("go repo should not be detected as nextjs")
	}
	if findPattern(patterns, "go-standard") == nil {
		t.Error("go repo should still be detected as go-standard")
	}
}

func TestDetectPatterns_BelowThreshold(t *testing.T) {
	// Only 1 module matches 1 layer out of many unrelated modules
	modules := makeModules("domain", "foo", "bar", "baz", "qux", "quux", "corge", "grault", "garply", "waldo")
	e := New()
	patterns := e.detectPatterns(modules, nil)

	// "domain" is not a hexagonal signature layer, and coverage is below the
	// 0.2 threshold anyway, so hexagonal must not be reported.
	if findPattern(patterns, "hexagonal") != nil {
		t.Error("hexagonal should not be detected from a single generic module")
	}
}

// --- detection: gating helpers ---

func TestGateOK(t *testing.T) {
	tests := []struct {
		name string
		def  patternDef
		fw   map[string]bool
		want bool
	}{
		{"no gate", patternDef{}, nil, true},
		{"framework present", patternDef{frameworks: []string{"nextjs"}}, fwSet("nextjs"), true},
		{"framework absent", patternDef{frameworks: []string{"nextjs"}}, fwSet("rails"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.def.gateOK(tt.fw); got != tt.want {
				t.Errorf("gateOK = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDescribes pins which languages a taxonomy may classify. It replaced a gate
// on the repository's dominant language, which both hid a Go layout behind a
// bigger front end and let the taxonomy that won classify modules written in
// something else.
func TestDescribes(t *testing.T) {
	goStd := patternDef{appliesTo: []string{"go"}}
	if !goStd.describes("go") || goStd.describes("typescript") {
		t.Error("go-standard must describe go modules and no others")
	}
	if !(patternDef{}).describes("php") {
		t.Error("a taxonomy naming no languages describes any of them")
	}

	mods := []facts.Fact{
		{Kind: facts.KindModule, Name: "pkg/api", Props: map[string]any{"language": "go"}},
		{Kind: facts.KindModule, Name: "public/app", Props: map[string]any{"language": "typescript"}},
	}
	cohort, langs := goStd.cohort(mods)
	if len(cohort) != 1 || cohort[0].Name != "pkg/api" {
		t.Errorf("cohort = %v, want just the go module", cohort)
	}
	if len(langs) != 1 || !langs["go"] {
		t.Errorf("cohort languages = %v, want {go}", langs)
	}
}

// --- bestPattern ---

// pat builds a pattern that clears the coverage floor, for the selection tests.
func pat(name string, spec int, conf float64, langs ...string) *archPattern {
	set := map[string]bool{}
	for _, l := range langs {
		set[l] = true
	}
	return &archPattern{Name: name, Specificity: spec, Confidence: conf,
		Languages: set, Scanned: 10, Classified: 10}
}

func TestSelectPatterns_OneAnswerPerCohort(t *testing.T) {
	e := New()

	// Same cohort: one question, so only the strongest is reported.
	got := e.selectPatterns([]*archPattern{
		pat("a", 2, 0.5, "go"), pat("b", 2, 0.8, "go"), pat("c", 2, 0.3, "go"),
	})
	if len(got) != 1 || got[0].Name != "b" {
		t.Errorf("same cohort selected %v, want just b", names(got))
	}

	// Disjoint cohorts: two halves of a polyglot repository, both true. This is
	// the Rails-monolith-with-an-Ember-front-end case, where reporting one
	// statement meant the loser's modules were described by the winner's order.
	got = e.selectPatterns([]*archPattern{
		pat("ember-octane", 2, 0.8, "typescript"), pat("rails-mvc", 2, 0.5, "ruby"),
	})
	if len(got) != 2 {
		t.Errorf("disjoint cohorts selected %v, want both", names(got))
	}

	// An ungated taxonomy overlaps everything, so it yields to whatever claimed
	// those modules first even at higher confidence.
	got = e.selectPatterns([]*archPattern{
		pat("rails-mvc", 2, 0.4, "ruby"),
		{Name: "hexagonal", Specificity: 0, Confidence: 0.9, Scanned: 10, Classified: 10,
			Languages: map[string]bool{"ruby": true, "php": true}},
	})
	if len(got) != 1 || got[0].Name != "rails-mvc" {
		t.Errorf("overlapping cohorts selected %v, want just rails-mvc", names(got))
	}
}

func names(ps []*archPattern) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestSelectPatterns_PrefersSpecificity(t *testing.T) {
	e := New()

	// A generic pattern with high confidence should lose to a more specific
	// (framework-gated) pattern over the same modules, even at lower confidence.
	best := firstSelected(e, []*archPattern{
		pat("hexagonal", 0, 0.9, "ruby"),
		pat("rails-mvc", 2, 0.5, "ruby"),
	})
	if best == nil || best.Name != "rails-mvc" {
		t.Errorf("selected %v, want rails-mvc (more specific)", best)
	}
}

func TestSelectPatterns_Empty(t *testing.T) {
	if got := New().selectPatterns(nil); len(got) != 0 {
		t.Errorf("selectPatterns(nil) = %v, want none", got)
	}
}

// --- violations & Explain integration ---

func TestDetectViolations_InnerImportsOuter(t *testing.T) {
	// Include an adapter module so the hexagonal signature requirement is met.
	store := makeStore(
		[]string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"},
		map[string][]string{
			"domain/entity": {"presentation/views"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// Should find at least one layer violation
	hasViolation := false
	for _, insight := range insights {
		if insight.Title != "" && insight.Confidence == 0.8 {
			hasViolation = true
			break
		}
	}
	if !hasViolation {
		t.Error("expected a layer violation insight when domain imports presentation")
	}
}

// TestDetectViolations_ResolvesNestedSourceAndSuffixedTarget pins the fix for
// endpoint granularity: the source file sits BELOW its module directory
// (Swift/Xcode nesting) and the import target carries a trailing symbol name
// (Kotlin/Java `import a.b.C` -> "a/b/C"). Both must resolve up to their modules
// or the inner→outer violation is silently dropped (which was hiding every
// Kotlin-sourced layer violation).
func TestDetectViolations_ResolvesNestedSourceAndSuffixedTarget(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	s.Add(facts.Fact{
		Kind: facts.KindDependency,
		File: "domain/entity/nested/Thing.kt", // FileDir = domain/entity/nested (not a module)
		Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: "presentation/views/HomeView"}, // module dir + class name
		},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := countViolations(insights); got != 1 {
		t.Fatalf("want 1 violation (domain nested -> presentation class-suffixed), got %d", got)
	}
}

func TestDetectViolations_OuterImportsInner(t *testing.T) {
	store := makeStore(
		[]string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"},
		map[string][]string{
			"presentation/views": {"domain/entity"},
		},
	)

	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// No violation — outer importing inner is correct
	for _, insight := range insights {
		if insight.Confidence == 0.8 {
			t.Errorf("unexpected layer violation: %s", insight.Title)
		}
	}
}

// addDep adds a single import dependency fact from a specific file.
func addDep(s *facts.Store, file, target string) {
	s.Add(facts.Fact{Kind: facts.KindDependency, File: file,
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}}})
}

func countViolations(insights []facts.Insight) int {
	n := 0
	for _, in := range insights {
		if in.Confidence == 0.8 {
			n++
		}
	}
	return n
}

// TestDetectViolations_Dedup: two files in the same source module importing the
// same outer module produce ONE violation, not two — so the renderer's
// title-prefix count isn't inflated.
func TestDetectViolations_Dedup(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	addDep(s, "domain/entity/a.go", "presentation/views")
	addDep(s, "domain/entity/b.go", "presentation/views") // same (source module, target) pair

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := countViolations(insights); got != 1 {
		t.Errorf("duplicate import across two files should yield 1 violation, got %d", got)
	}
}

// TestDetectViolations_LevelEqualNoViolation: an import between two same-level
// layers (application -> port, both level 1) is not a violation.
func TestDetectViolations_LevelEqualNoViolation(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"application/svc", "port/iface", "adapter/rest", "domain/entity"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	addDep(s, "application/svc/a.go", "port/iface")

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := countViolations(insights); got != 0 {
		t.Errorf("same-level import should not be a violation, got %d", got)
	}
}

// TestDetectViolations_RailsDomainTier: in Rails, models calling services/jobs/
// helpers is idiomatic (all one domain tier) and must NOT be a violation; only a
// domain module reaching UP into the delivery layer (a controller/view) is flagged.
func TestDetectViolations_RailsDomainTier(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("ruby",
		"app/models", "app/services", "app/jobs", "app/helpers",
		"app/controllers", "app/views")...)
	s.Add(frameworkFact("rails"))
	// Idiomatic Rails — all domain tier, no violation.
	addDep(s, "app/models/_coupling.rb", "app/services")
	addDep(s, "app/models/_coupling.rb", "app/jobs")
	addDep(s, "app/services/_coupling.rb", "app/helpers")
	// Genuine smell — a model reaching up into a controller.
	addDep(s, "app/models/_coupling.rb", "app/controllers")

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var titles []string
	for _, in := range insights {
		if in.Confidence == 0.8 {
			titles = append(titles, in.Title)
		}
	}
	if len(titles) != 1 {
		t.Fatalf("expected exactly 1 Rails violation (model -> controller), got %d: %v", len(titles), titles)
	}
	if !strings.Contains(titles[0], "model -> controller") {
		t.Errorf("expected the model -> controller violation, got %q", titles[0])
	}
}

// TestDetectViolations_RelativeImportResolved guards the fix where a JS/TS-style
// relative import target ("../pages") is resolved against the source module
// before matching a layer — previously the raw target never matched, so the
// violation was missed.
func TestDetectViolations_RelativeImportResolved(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("typescript", "pages", "components", "lib", "hooks")...)
	s.Add(frameworkFact("nextjs"))
	// lib (level 0) imports "../pages" (level 3) via a relative path -> violation.
	addDep(s, "lib/util.ts", "../pages")

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	found := false
	for _, in := range insights {
		if in.Confidence == 0.8 && strings.Contains(in.Title, "lib -> pages") {
			found = true
		}
	}
	if !found {
		t.Errorf("relative import lib -> ../pages should resolve to a lib->pages violation; got %v", func() []string {
			out := make([]string, len(insights))
			for i, in := range insights {
				out[i] = in.Title
			}
			return out
		}())
	}
}

// TestExplain_PatternEvidenceDeterministic: the architecture-pattern insight's
// evidence used to be built by ranging a map, so its order churned run to run.
func TestExplain_PatternEvidenceDeterministic(t *testing.T) {
	store := makeStore(
		[]string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"},
		map[string][]string{},
	)
	render := func() string {
		insights, err := New().Explain(context.Background(), store)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		var b strings.Builder
		for _, ev := range insights[0].Evidence { // insights[0] is the architecture pattern
			b.WriteString(ev.Fact)
			b.WriteByte(',')
		}
		return b.String()
	}
	want := render()
	for i := 0; i < 50; i++ {
		if got := render(); got != want {
			t.Fatalf("non-deterministic pattern evidence on iteration %d:\nwant %q\ngot  %q", i, want, got)
		}
	}
}

func TestExplain_NoModules(t *testing.T) {
	store := facts.NewStore()
	e := New()
	insights, err := e.Explain(context.Background(), store)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for empty store, got %d", len(insights))
	}
}

func TestExplain_RailsDetectedNotNextJS(t *testing.T) {
	// End-to-end through Explain: a Rails repo (framework=rails) yields a
	// rails-mvc architecture insight, never nextjs.
	s := facts.NewStore()
	s.Add(makeModulesLang("ruby", "app/models", "app/controllers", "app/views")...)
	s.Add(frameworkFact("rails"))

	e := New()
	insights, err := e.Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	var arch string
	for _, in := range insights {
		const prefix = "Architecture pattern: "
		if len(in.Title) > len(prefix) && in.Title[:len(prefix)] == prefix {
			arch = in.Title[len(prefix):]
		}
	}
	if arch != "rails-mvc" {
		t.Errorf("architecture insight = %q, want rails-mvc", arch)
	}
}

// --- test-code exclusion, evidence, and append mode ---

// TestDetectViolations_SkipsTestFiles pins the gate on the importing FILE rather than
// on its module. resolveLayerModule walks UP to the nearest classified module, so a
// mock or test nested inside a production module (Sources/Foo/Mocks/X.swift) would
// otherwise have its imports attributed to the production layer and reported as a
// violation of it.
func TestDetectViolations_SkipsTestFiles(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	// Production file: a genuine violation. Test file in the same module: not.
	addDep(s, "domain/entity/a.go", "presentation/views")
	addDep(s, "domain/entity/mocks/a_mock.go", "presentation/views")

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := countViolations(insights); got != 1 {
		t.Fatalf("only the production import should violate, got %d", got)
	}
}

// TestDetectViolations_CitesEdgeEndpoints pins the evidence the snapshot diff needs.
// The importing file is never a fact name, so a violation citing only the file can
// never be attributed to a change and never reaches the gate. The dependency fact and
// the RAW import target are the two endpoints of the edge that the diff records.
func TestDetectViolations_CitesEdgeEndpoints(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	s.Add(facts.Fact{
		Kind: facts.KindDependency,
		Name: "domain/entity -> presentation/views",
		File: "domain/entity/a.go",
		Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: "presentation/views"},
		},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var got []facts.Insight
	for _, in := range insights {
		if in.Confidence == 0.8 {
			got = append(got, in)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(got))
	}

	// The file entry stays first: the dashboard and the gate both render the first
	// qualifying evidence entry.
	if got[0].Evidence[0].File != "domain/entity/a.go" {
		t.Errorf("file evidence must stay first, got %+v", got[0].Evidence[0])
	}
	cited := map[string]bool{}
	for _, ev := range got[0].Evidence {
		if ev.Fact != "" {
			cited[ev.Fact] = true
		}
	}
	for _, want := range []string{"domain/entity -> presentation/views", "presentation/views"} {
		if !cited[want] {
			t.Errorf("evidence must cite %q so the diff can attribute it; cited %v", want, cited)
		}
	}
}

// TestDetectViolations_ResolvesRepoPrefixedFiles covers append mode, where a fact's
// File is repo-prefixed ("server/index.js") while module facts keep their bare name.
// Deriving the module from the raw File yields the repo label, which no module is
// called — so the explainer used to report nothing at all on a multi-repo graph.
func TestDetectViolations_ResolvesRepoPrefixedFiles(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"domain/entity", "presentation/views", "adapter/rest", "application/svc"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m, Repo: "server", File: "server/" + m})
	}
	s.Add(facts.Fact{
		Kind: facts.KindDependency,
		Name: "domain/entity -> presentation/views",
		Repo: "server",
		File: "server/domain/entity/a.go",
		Relations: []facts.Relation{
			{Kind: facts.RelImports, Target: "presentation/views"},
		},
	})

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := countViolations(insights); got != 1 {
		t.Fatalf("repo-prefixed file must still resolve to its module, got %d violations", got)
	}
}

// TestDetectPatterns_ExcludesTestModules pins that test trees do not vote on the
// architecture. len(modules) is the coverage denominator and the signature-layer gate
// counts distinct layers, so a repo whose only `adapter` and `application` directories
// live under a test source set must not be reported as hexagonal.
func TestDetectPatterns_ExcludesTestModules(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{
		"src/main/kotlin/app",
		"src/test/kotlin/adapter",
		"src/test/kotlin/application",
	} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	for _, in := range insights {
		if strings.Contains(in.Title, "hexagonal") {
			t.Fatalf("hexagonal must not be claimed from test-only directories: %q", in.Title)
		}
	}
}

// TestDetectPatterns_ModuleRoleOutranksPath is the other direction, and the reason
// this filter uses common.IsTestModule rather than a path test. A build file that says
// a module is production wins over a path that looks like a test tree — a large Android
// app really does ship `app/src/main/java/…/ui/base/testing`.
func TestDetectPatterns_ModuleRoleOutranksPath(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"domain/entity", "application/svc", "port/iface"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m})
	}
	s.Add(facts.Fact{
		Kind:  facts.KindModule,
		Name:  "adapter/rest/testing",
		Props: map[string]any{facts.PropModuleRole: facts.ModuleRoleProduction},
	})

	e := New()
	mods := []facts.Fact{}
	for _, m := range s.ByKind(facts.KindModule) {
		if !common.IsTestModule(m) {
			mods = append(mods, m)
		}
	}
	pattern := firstSelected(e, e.detectPatterns(mods, nil))
	if pattern == nil {
		t.Fatal("no pattern detected")
	}
	if _, ok := pattern.Modules["adapter/rest/testing"]; !ok {
		t.Errorf("a module the build file calls production must stay classified: %v", pattern.Modules)
	}
}

// TestDetectPatterns_ConfidenceStaysBelowOne pins the reserved meaning of 1.0. A
// pattern match is a coverage ratio over directory names — a well-supported guess,
// never a proof — so even a repo where every module matches every layer must not
// present as a structural fact.
func TestDetectPatterns_ConfidenceStaysBelowOne(t *testing.T) {
	s := facts.NewStore()
	for _, m := range []string{"cmd/server", "internal/auth", "pkg/utils", "api/v1"} {
		s.Add(facts.Fact{Kind: facts.KindModule, Name: m, Props: map[string]any{"language": "go"}})
	}

	e := New()
	for _, p := range e.detectPatterns(s.ByKind(facts.KindModule), nil) {
		if p.Confidence >= 1.0 {
			t.Errorf("pattern %q reached confidence %v; 1.0 is reserved for structural facts",
				p.Name, p.Confidence)
		}
	}
}

// TestGoStdLayers_VisibilityIsNotLayering pins the one ordering the Go standard layout
// actually expresses, and the three it does not.
//
// `internal` and `pkg` are a visibility distinction the compiler enforces, not a
// dependency ordering: pkg/ over internal/ is how you publish an API over a private
// implementation, and internal/ importing pkg/ is how a published plugin interface gets
// implemented. Ranking them reported both as violations on essentially every Go
// repository with both directories — enola's own snapshot carried 15, all false.
//
// What remains is real: `cmd` holds entrypoints, so a library importing INTO cmd is a
// package reaching into a binary.
func TestGoStdLayers_VisibilityIsNotLayering(t *testing.T) {
	level := func(name string) int {
		t.Helper()
		for _, d := range goStdLayers {
			if d.Name == name {
				return d.Level
			}
		}
		t.Fatalf("no layer named %q", name)
		return 0
	}
	// A violation is reported when source.Level < target.Level (see detectViolations).
	violates := func(from, to string) bool { return level(from) < level(to) }

	for _, tc := range []struct{ from, to string }{
		{"pkg", "internal"},      // publishing an API over a private implementation
		{"internal", "pkg"},      // implementing a contract published in pkg
		{"internal", "api"},      // depending on a contract definition
		{"pkg", "api"},           //
		{"cmd", "internal"},      // an entrypoint wiring the implementation
		{"cmd", "pkg"},           //
		{"internal", "internal"}, //
	} {
		if violates(tc.from, tc.to) {
			t.Errorf("%s -> %s reported as a layer violation; it is idiomatic Go", tc.from, tc.to)
		}
	}

	// The one the layout does express.
	for _, from := range []string{"internal", "pkg", "api"} {
		if !violates(from, "cmd") {
			t.Errorf("%s -> cmd must be a violation: a library reaching into an entrypoint", from)
		}
	}
}

// An autoload root must be the repository's own top-level app/, not any
// directory called "app" anywhere in the tree.
//
// A Rails monolith that also ships a front-end has ember_app/app/routes and
// ember_app/app/components. Matching "app" at any depth pulled that layout into
// the Rails taxonomy, inflating its coverage until it displaced the pattern
// that was correctly winning — measured on a real monolith, where it replaced
// 185 genuine Ember layer violations with a different set.
func TestAutoloadedLayerOnlyClaimsTheTopLevelAppDirectory(t *testing.T) {
	if name, ok := autoloadedLayer("app/tools/replan_week", "app"); !ok || name != "tools" {
		t.Fatalf("a top-level app/ directory is an autoload root, got %q/%v", name, ok)
	}
	if name, ok := autoloadedLayer("app/models/coaching", "app"); !ok || name != "models" {
		t.Fatalf("nested files belong to their root's layer, got %q/%v", name, ok)
	}
	for _, foreign := range []string{"ember_app/app/routes", "vendor/app/components", "app"} {
		if name, ok := autoloadedLayer(foreign, "app"); ok {
			t.Fatalf("%q must not be claimed as a Rails autoload root, got %q", foreign, name)
		}
	}
}

// TestEmberUtilLayerDoesNotClaimLib guards the single largest source of false layer
// violations the corpus has produced.
//
// discourse is a Rails backend beside an Ember frontend in one tree. Both patterns are
// framework-gated, so they tie on specificity and ember-octane wins on confidence — and
// the Ember pattern then classifies the RUBY directories, because layer matching is by
// path segment and knows nothing about language. With `lib` in the util layer at level
// 0, every Ruby `lib/` and `plugins/*/lib/*` directory — where a large Rails app keeps
// most of its domain code — became the innermost layer, and each of the models and
// services it legitimately calls became a violation: 397 of 426 reported violations,
// all of them false. Removing `lib` took discourse to 27.
//
// It is also wrong on Ember's own terms: Octane puts utilities in `app/utils/`, while
// `lib/` holds in-repo addons, which are whole packages rather than a bottom layer.
func TestEmberUtilLayerDoesNotClaimLib(t *testing.T) {
	var util *layerDef
	for i := range emberLayers {
		if emberLayers[i].Name == "util" {
			util = &emberLayers[i]
		}
	}
	if util == nil {
		t.Fatal("ember util layer is gone; this guard needs updating")
	}
	for _, p := range util.Patterns {
		if p == "lib" {
			t.Error("ember util layer claims the bare segment `lib` again — this classified " +
				"Ruby lib/ directories as level 0 and manufactured 397 false violations on discourse")
		}
	}
	// A Ruby lib directory must not land in the util layer under the Ember pattern.
	if matchesLayer("plugins/chat/lib/chat", util.Patterns) {
		t.Error("a Ruby plugin lib directory still matches the ember util layer")
	}
	// The real Ember utility directory still does.
	if !matchesLayer("frontend/discourse/app/utils", util.Patterns) {
		t.Error("app/utils should still match the ember util layer")
	}
}

// --- P0 regression cases: the three false-positive classes the corpus exposed ---

// firstSelected returns the strongest reported pattern, or nil. Tests that
// predate per-cohort selection ask "which one wins", which is the first.
func firstSelected(e *LayerExplainer, patterns []*archPattern) *archPattern {
	if got := e.selectPatterns(patterns); len(got) > 0 {
		return got[0]
	}
	return nil
}

// mustExplain runs the explainer over a store and fails the test on error.
func mustExplain(t *testing.T, s *facts.Store) []facts.Insight {
	t.Helper()
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	return insights
}

// violationTitles returns the titles of the heuristic layer violations in a
// snapshot, which is what the three tests below assert over.
func violationTitles(t *testing.T, s *facts.Store) []string {
	t.Helper()
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	var titles []string
	for _, in := range insights {
		if strings.HasPrefix(in.Title, "Layer violation:") {
			titles = append(titles, in.Title)
		}
	}
	return titles
}

// TestDetectViolations_AndroidDomainMayImportData pins the layer ORDER of the
// Android taxonomy against the reference application it was measured on.
//
// nowinandroid's architecture guide puts "the data layer at the bottom", and its
// use cases import repositories. With domain ranked innermost, three of those
// imports were reported as violations on Google's own sample. Only the reverse —
// a repository reaching up into the UI — is a defect.
func TestDetectViolations_AndroidDomainMayImportData(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("kotlin",
		"core/data/repository", "core/domain", "feature/ui", "core/designsystem")...)
	s.Add(frameworkFact("android"))
	// The documented direction of flow: UI -> domain -> data.
	addDep(s, "core/domain/GetTopicsUseCase.kt", "core/data/repository")
	addDep(s, "feature/ui/TopicsScreen.kt", "core/domain")
	// The genuine smell: data reaching up into the UI.
	addDep(s, "core/data/repository/TopicsRepository.kt", "feature/ui")

	titles := violationTitles(t, s)
	if len(titles) != 1 {
		t.Fatalf("expected exactly 1 Android violation (data -> ui), got %d: %v", len(titles), titles)
	}
	if !strings.Contains(titles[0], "data -> ui") {
		t.Errorf("expected the data -> ui violation, got %q", titles[0])
	}
}

// TestDetectViolations_WiringLayerIsNeutral pins layerDef.Neutral: a Spring
// `config` package is referenced by, and references, every layer it wires, so no
// edge touching it may be verdicted. Giving it a level produced 61 of
// thingsboard's 75 findings and all 7 of dubbo's. The ordinary Spring smell in
// the same snapshot must still be reported.
func TestDetectViolations_WiringLayerIsNeutral(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("java",
		"app/controller", "app/service", "app/repository", "app/entity", "app/config")...)
	s.Add(frameworkFact("spring"))
	addDep(s, "app/service/UserService.java", "app/config")
	addDep(s, "app/repository/UserRepo.java", "app/config")
	addDep(s, "app/config/WebConfig.java", "app/controller")
	// A service reaching up into a controller is still a violation.
	addDep(s, "app/service/UserService.java", "app/controller")

	titles := violationTitles(t, s)
	if len(titles) != 1 {
		t.Fatalf("expected exactly 1 Spring violation (service -> controller), got %d: %v", len(titles), titles)
	}
	if !strings.Contains(titles[0], "service -> controller") {
		t.Errorf("expected the service -> controller violation, got %q", titles[0])
	}
}

// TestAutoloadRootSkipsAssetDirectories pins notAutoloaded. app/javascript holds
// a whole second application, not a Rails layer: chatwoot keeps a Vue app there,
// and claiming it made five entrypoints importing app/views into violations.
func TestAutoloadRootSkipsAssetDirectories(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("ruby",
		"app/models", "app/controllers", "app/views", "app/javascript/widget", "app/assets")...)
	s.Add(frameworkFact("rails"))
	addDep(s, "app/javascript/widget/router.js", "app/views")

	if titles := violationTitles(t, s); len(titles) != 0 {
		t.Fatalf("expected no violation out of a frontend directory, got %v", titles)
	}
	for _, name := range []string{"javascript", "assets"} {
		if _, ok := autoloadedLayer("app/"+name+"/x", "app"); ok {
			t.Errorf("app/%s must not be claimed as an autoloaded layer", name)
		}
	}
	if _, ok := autoloadedLayer("app/tools/replan", "app"); !ok {
		t.Error("app/tools is autoloaded and must still be claimed")
	}
}

// TestMinClassifiedShare_SuppressesThinClaims pins the coverage floor. A
// taxonomy recognising its own vocabulary across a corner of a repository built
// to a different plan is a wrong statement, not a tentative one: gitea has no
// internal/ or pkg/ and was named go-standard on directories under routers/api
// matching the word "api".
func TestMinClassifiedShare_SuppressesThinClaims(t *testing.T) {
	s := facts.NewStore()
	names := []string{"routers/api", "cmd/serve"}
	for i := 0; i < 30; i++ {
		names = append(names, fmt.Sprintf("models/thing%d", i))
	}
	s.Add(makeModulesLang("go", names...)...)

	for _, in := range mustExplain(t, s) {
		if strings.HasPrefix(in.Title, "Architecture pattern:") {
			t.Errorf("named an architecture from 2 of %d modules: %q", len(names), in.Title)
		}
	}

	// The same taxonomy on a repository that actually follows it.
	fat := facts.NewStore()
	fat.Add(makeModulesLang("go", "cmd/serve", "internal/app", "internal/store", "pkg/api", "docs")...)
	if findInsight(mustExplain(t, fat), "Architecture pattern: go-standard") == nil {
		t.Error("expected go-standard on a repository that follows the layout")
	}
}

// TestMinClassifiedShare_SuppressionDoesNotPromote pins WHERE the floor is
// applied. A modular CMS matched the framework-gated .NET taxonomy across 3% of
// itself and the language-agnostic hexagonal one across 26%; flooring at
// admission removed the first and handed the repository to the second, trading a
// wrong statement for a worse one. The floor belongs on the winner.
func TestMinClassifiedShare_SuppressionDoesNotPromote(t *testing.T) {
	s := facts.NewStore()
	var names []string
	// Thin, specific: two .NET clean-architecture project names.
	names = append(names, "src/Shop.Domain", "src/Shop.Infrastructure")
	// Fat, generic: enough ports-and-adapters vocabulary to satisfy hexagonal.
	for i := 0; i < 10; i++ {
		// Vocabulary the generic taxonomy owns and the .NET one does not, so the
		// two patterns stay separable: `infrastructure` is a layer in BOTH.
		names = append(names, fmt.Sprintf("modules/mod%d/interfaces", i))
		names = append(names, fmt.Sprintf("modules/mod%d/adapters", i))
	}
	for i := 0; i < 40; i++ {
		names = append(names, fmt.Sprintf("modules/mod%d/unclassified", i))
	}
	s.Add(makeModulesLang("csharp", names...)...)
	s.Add(frameworkFact("aspnetcore"))

	for _, in := range mustExplain(t, s) {
		if strings.HasPrefix(in.Title, "Architecture pattern:") {
			t.Errorf("a floored winner promoted a worse pattern: %q", in.Title)
		}
	}
}

// TestDescribePattern_StatesWhatItCannotGrade: two taxonomies deliberately
// collapse most of their directories to one tier, so on many repositories they
// can express no ordering at all. A statement that says nothing about direction
// reads as a clean bill of health; this one says which it is.
func TestDescribePattern_StatesWhatItCannotGrade(t *testing.T) {
	p := &archPattern{Name: "go-standard", Scanned: 10, Classified: 8, Graded: 8}
	if got := describePattern(p, conformance{Same: 40}, 10); !strings.Contains(got, "without grading anything") {
		t.Errorf("expected an ungradeable statement, got %q", got)
	}
	if got := describePattern(p, conformance{Inward: 9, Against: 1}, 10); !strings.Contains(got, "90% obey") {
		t.Errorf("expected the obedience share, got %q", got)
	}
	if got := describePattern(p, conformance{Inward: 9}, 10); !strings.Contains(got, "none run against") {
		t.Errorf("expected a clean statement, got %q", got)
	}
}

// TestDescribePattern_NamesItsCohort: a taxonomy is scored over the modules it
// could describe, so a Go SDK of 28 modules inside a Python repository of 1310
// produces a true statement that reads as a claim about the whole repository
// unless it says which part of it was measured.
func TestDescribePattern_NamesItsCohort(t *testing.T) {
	p := &archPattern{
		Name: "go-standard", Scanned: 28, Classified: 18, Graded: 18,
		Languages: map[string]bool{"go": true},
	}
	got := describePattern(p, conformance{}, 1310)
	if !strings.Contains(got, "18 of 28 go modules classified") {
		t.Errorf("expected the cohort's language named, got %q", got)
	}
	if !strings.Contains(got, "2% of this repository's 1310 modules") {
		t.Errorf("expected the cohort's share of the repository, got %q", got)
	}
	// A taxonomy measured over the whole repository has no cohort to distinguish.
	whole := &archPattern{Name: "rails-mvc", Scanned: 100, Classified: 90, Graded: 90,
		Languages: map[string]bool{"ruby": true}}
	if got := describePattern(whole, conformance{}, 100); strings.Contains(got, "of this repository's") {
		t.Errorf("a whole-repository cohort must not report a share, got %q", got)
	}
}

// TestHexagonal_CoreIsNotADomainLayer pins the removal of `core` from the
// hexagonal domain patterns. A platform that keeps its whole product under
// src/Core/ had 1049 of 1491 classified modules read as domain on that one
// segment, which made every Core/… module reaching Core/…/Api a violation.
func TestHexagonal_CoreIsNotADomainLayer(t *testing.T) {
	s := facts.NewStore()
	var names []string
	for i := 0; i < 12; i++ {
		names = append(names, fmt.Sprintf("src/Core/Content/thing%d", i))
	}
	names = append(names, "src/Core/Framework/Adapter", "src/Core/Framework/Api",
		"src/Core/Framework/Interfaces", "src/Core/Application")
	s.Add(makeModulesLang("csharp", names...)...)
	// A Core/… module reaching the platform's own Api directory.
	addDep(s, "src/Core/Content/thing0/x.cs", "src/Core/Framework/Api")

	for _, in := range mustExplain(t, s) {
		if strings.HasPrefix(in.Title, "Layer violation:") {
			t.Errorf("a container segment must not make its own subtree a domain layer: %q", in.Title)
		}
	}
}

// TestPHPLayered pins the language-gated PHP taxonomy. There is no single PHP
// framework the way there is a single Rails, so it is built only from the words
// that recur across unrelated PHP codebases, and its wiring tier is unordered for
// the reason the Spring config package is.
func TestPHPLayered(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("php",
		"lib/Controller", "lib/Http", "lib/Service", "lib/Db", "lib/Migration",
		"lib/Listener", "lib/Exception")...)
	// Idiomatic: delivery down to domain down to data.
	addDep(s, "lib/Controller/PageController.php", "lib/Service")
	addDep(s, "lib/Service/PageService.php", "lib/Db")
	// Wiring is referenced by and references everything, and is never a violation.
	addDep(s, "lib/Service/PageService.php", "lib/Listener")
	addDep(s, "lib/Listener/Hook.php", "lib/Controller")
	// The genuine smell: data reaching up into delivery.
	addDep(s, "lib/Db/PageMapper.php", "lib/Controller")

	insights := mustExplain(t, s)
	if findInsight(insights, "Architecture pattern: php-layered") == nil {
		t.Fatal("expected php-layered to be detected")
	}
	var titles []string
	for _, in := range insights {
		if strings.HasPrefix(in.Title, "Layer violation:") {
			titles = append(titles, in.Title)
		}
	}
	if len(titles) != 1 || !strings.Contains(titles[0], "data -> controller") {
		t.Fatalf("expected exactly the data -> controller violation, got %v", titles)
	}
}

// TestPrescribedTaxonomies covers the two taxonomies taken from a framework's
// documented directory structure rather than from what repositories share. Both
// are framework-gated, so neither can reach a repository that is not one of these
// applications, and neither carries a signature gate for the same reason nextjs
// does not.
func TestPrescribedTaxonomies(t *testing.T) {
	t.Run("nuxt", func(t *testing.T) {
		s := facts.NewStore()
		s.Add(makeModulesLang("typescript",
			"app/pages/home", "app/components/card", "app/composables/useAuth",
			"app/utils/format", "server/api", "app/plugins")...)
		s.Add(frameworkFact("nuxt"))
		addDep(s, "app/pages/home/index.vue", "app/components/card")
		addDep(s, "app/components/card/Card.vue", "app/composables/useAuth")
		// The Nitro backend is a different runtime, at no point in the front
		// end's order, so neither direction across it is a violation.
		addDep(s, "app/composables/useAuth/index.ts", "server/api")
		addDep(s, "server/api/handler.ts", "app/utils/format")
		// The genuine smell: a composable reaching up into a page.
		addDep(s, "app/composables/useAuth/index.ts", "app/pages/home")

		insights := mustExplain(t, s)
		if findInsight(insights, "Architecture pattern: nuxt") == nil {
			t.Fatal("expected nuxt to be detected")
		}
		var titles []string
		for _, in := range insights {
			if strings.HasPrefix(in.Title, "Layer violation:") {
				titles = append(titles, in.Title)
			}
		}
		if len(titles) != 1 || !strings.Contains(titles[0], "composables -> pages") {
			t.Fatalf("expected exactly the composables -> pages violation, got %v", titles)
		}
	})

	t.Run("sveltekit", func(t *testing.T) {
		s := facts.NewStore()
		s.Add(makeModulesLang("typescript",
			"src/routes/article", "src/routes/login", "src/lib/api", "src/lib/stores")...)
		s.Add(frameworkFact("sveltekit"))
		addDep(s, "src/routes/article/+page.svelte", "src/lib/api")

		in := findInsight(mustExplain(t, s), "Architecture pattern: sveltekit")
		if in == nil {
			t.Fatal("expected sveltekit to be detected")
		}
		if !strings.Contains(in.Description, "none run against") {
			t.Errorf("a route importing lib runs with the grain: %q", in.Description)
		}
	})
}

// TestClassifyModule_PrefersUnorderedLayers pins the wiring-first rule. Matching
// is position-blind, so a wiring directory nested inside an ordered one took the
// enclosing layer: nowinandroid's core/data/…/data/di classified as `data`, and a
// Spring package at …/service/config as `service`. Preferring the neutral layer is
// the fail-safe direction — misclassifying into one only silences a verdict, while
// misclassifying out of one invents one about a directory every layer references.
func TestClassifyModule_PrefersUnorderedLayers(t *testing.T) {
	android := patternDefs[indexOfPattern(t, "android-clean")]
	i, ok := classifyModule("core/data/src/main/kotlin/app/core/data/di", android)
	if !ok || android.layers[i].Name != "di" {
		t.Errorf("nested wiring classified as %q, want di", android.layers[i].Name)
	}
	if !android.layers[i].Neutral {
		t.Error("the di layer must be unordered")
	}

	spring := patternDefs[indexOfPattern(t, "spring-layered")]
	i, ok = classifyModule("src/main/java/app/service/config", spring)
	if !ok || spring.layers[i].Name != "config" {
		t.Errorf("nested config classified as %q, want config", spring.layers[i].Name)
	}

	// An ordered layer with no wiring segment is unaffected.
	i, ok = classifyModule("src/main/java/app/service/impl", spring)
	if !ok || spring.layers[i].Name != "service" {
		t.Errorf("ordinary module classified as %q, want service", spring.layers[i].Name)
	}
}

func indexOfPattern(t *testing.T, name string) int {
	t.Helper()
	for i := range patternDefs {
		if patternDefs[i].name == name {
			return i
		}
	}
	t.Fatalf("no taxonomy named %q", name)
	return 0
}

// --- metrics ---

// TestPatternMetrics_AgreeWithTheProse is the guard the whole Metrics field exists
// for. The description and the metrics are two renderings of the same measurement,
// and the failure mode is that one is updated and the other is not — which is
// exactly how the benchmark's regex over "N of M modules classified" broke when the
// cohort's language was added to that sentence. Anything the prose states, this
// asserts the metrics state identically.
func TestPatternMetrics_AgreeWithTheProse(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("ruby",
		"app/controllers", "app/models", "app/services", "app/views", "app/jobs")...)
	s.Add(frameworkFact("rails"))
	addDep(s, "app/controllers/x.rb", "app/services")
	addDep(s, "app/models/y.rb", "app/controllers") // against the order

	in := findInsight(mustExplain(t, s), "Architecture pattern: rails-mvc")
	if in == nil {
		t.Fatal("expected rails-mvc")
	}
	if in.Metrics == nil {
		t.Fatal("the pattern insight carries no metrics")
	}

	// Every number the sentence quotes must appear in the metrics as the same value.
	for _, tc := range []struct {
		key  string
		want int
	}{
		{MetricModulesClassified, 5},
		{MetricModulesScanned, 5},
		{MetricImportsInward, 1},
		{MetricImportsAgainst, 1},
	} {
		if got := in.MetricInt(tc.key); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.key, got, tc.want)
		}
	}
	if !strings.Contains(in.Description, "5 of 5 ruby modules classified") {
		t.Errorf("prose disagrees with modules_classified/scanned: %q", in.Description)
	}
	if !strings.Contains(in.Description, "1 run inward and 1 against") {
		t.Errorf("prose disagrees with the conformance counts: %q", in.Description)
	}
	if langs := in.MetricStrings(MetricCohortLanguages); len(langs) != 1 || langs[0] != "ruby" {
		t.Errorf("cohort_languages = %v, want [ruby]", langs)
	}
}

// TestPatternMetrics_LayerOrderAndExamples pins the part no reader could
// reconstruct: the order runs OUTERMOST FIRST, unordered layers are separated
// rather than ranked, and each layer carries a module from this repository.
func TestPatternMetrics_LayerOrderAndExamples(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("java",
		"app/controller", "app/service", "app/entity", "app/config")...)
	s.Add(frameworkFact("spring"))

	in := findInsight(mustExplain(t, s), "Architecture pattern: spring-layered")
	if in == nil {
		t.Fatal("expected spring-layered")
	}
	ordered := in.MetricStrings(MetricLayersOrdered)
	if len(ordered) != 3 || ordered[0] != "controller" || ordered[2] != "entity" {
		t.Errorf("layers_ordered = %v, want controller … entity", ordered)
	}
	if un := in.MetricStrings(MetricLayersUnordered); len(un) != 1 || un[0] != "config" {
		t.Errorf("layers_unordered = %v, want [config]", un)
	}
	ex := in.MetricStringMap(MetricLayerExamples)
	if ex["controller"] != "app/controller" || ex["entity"] != "app/entity" {
		t.Errorf("layer_examples = %v, want modules from this repository", ex)
	}
}

// TestPatternMetrics_DeclaredOrder: a repository that STATES its architecture gets
// the same metrics as one that had it guessed. This is the case the feature guide
// could not reach — a declared order lives in intent facts, so there is nothing to
// look up by pattern name.
func TestPatternMetrics_DeclaredOrder(t *testing.T) {
	s := facts.NewStore()
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "repo/cmd", Repo: "repo",
		Props: map[string]any{"language": "go"}})
	s.Add(facts.Fact{Kind: facts.KindModule, Name: "repo/internal/core", Repo: "repo",
		Props: map[string]any{"language": "go"}})
	for i, layer := range []struct {
		name  string
		paths []string
	}{{"entrypoint", []string{"cmd/**"}}, {"core", []string{"internal/**"}}} {
		s.Add(facts.Fact{Kind: facts.KindIntent, Name: "layer:" + layer.name, Repo: "repo",
			Props: map[string]any{
				"intent_kind": "layer", "layer_name": layer.name,
				"order": i, "paths": layer.paths, "intent_owner": "repo",
			}})
	}

	in := findInsight(mustExplain(t, s), "Architecture pattern: declared (repo)")
	if in == nil {
		t.Fatal("expected the declared pattern")
	}
	ordered := in.MetricStrings(MetricLayersOrdered)
	if len(ordered) != 2 || ordered[0] != "entrypoint" || ordered[1] != "core" {
		t.Fatalf("layers_ordered = %v, want [entrypoint core] — outermost first", ordered)
	}
	if ex := in.MetricStringMap(MetricLayerExamples); ex["entrypoint"] != "repo/cmd" {
		t.Errorf("layer_examples = %v, want the declared module", ex)
	}
	if got := in.MetricInt(MetricModulesClassified); got != 2 {
		t.Errorf("modules_classified = %d, want 2", got)
	}
}

// TestDetectViolations_AndroidDataMayImportDomain: an Android app built the
// clean-architecture way has its repositories implement interfaces the domain
// declares, so `data -> domain` is the intended direction. Graded against
// Android's own order, every such import was a violation; the imports between the
// two layers now choose the order, and only the real smell is left.
func TestDetectViolations_AndroidDataMayImportDomain(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("kotlin",
		"core/data/repository", "core/domain", "feature/ui", "core/designsystem")...)
	s.Add(frameworkFact("android"))
	addDep(s, "core/data/repository/TopicsRepositoryImpl.kt", "core/domain")
	addDep(s, "core/data/repository/NewsRepositoryImpl.kt", "core/domain")
	addDep(s, "feature/ui/TopicsScreen.kt", "core/domain")
	addDep(s, "core/data/repository/TopicsRepositoryImpl.kt", "feature/ui")

	titles := violationTitles(t, s)
	if len(titles) != 1 || !strings.Contains(titles[0], "data -> ui") {
		t.Fatalf("expected only the data -> ui violation, got %v", titles)
	}

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range insights {
		if in.Title != "Architecture pattern: android-clean" {
			continue
		}
		got, _ := in.Metrics[MetricLayerOrientation].(string)
		if !strings.HasPrefix(got, "domain innermost") {
			t.Errorf("%s = %q, want the clean orientation", MetricLayerOrientation, got)
		}
		if !strings.Contains(in.Description, got) {
			t.Errorf("the statement should name the orientation it graded with: %q", in.Description)
		}
		return
	}
	t.Fatal("no android-clean pattern insight")
}

// TestDetectViolations_AndroidMinorityDirectionIsReported: once the majority has
// chosen the clean order, the import that runs the other way is the finding.
func TestDetectViolations_AndroidMinorityDirectionIsReported(t *testing.T) {
	s := facts.NewStore()
	s.Add(makeModulesLang("kotlin", "core/data/repository", "core/domain", "feature/ui")...)
	s.Add(frameworkFact("android"))
	addDep(s, "core/data/repository/A.kt", "core/domain")
	addDep(s, "core/data/repository/B.kt", "core/domain")
	addDep(s, "core/domain/UseCase.kt", "core/data/repository")
	addDep(s, "feature/ui/Screen.kt", "core/domain")

	titles := violationTitles(t, s)
	if len(titles) != 1 || !strings.Contains(titles[0], "domain -> data") {
		t.Fatalf("expected only the domain -> data violation, got %v", titles)
	}
}
