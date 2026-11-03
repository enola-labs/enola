package surface

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// addModuleSymbols adds `total` symbols under module mod, the first `exported`
// of which are marked exported=true.
func addModuleSymbols(s *facts.Store, mod string, total, exported int) {
	for i := 0; i < total; i++ {
		s.Add(facts.Fact{
			Kind:  facts.KindSymbol,
			Name:  fmt.Sprintf("%s.Sym%d", mod, i),
			File:  mod + "/file.go",
			Props: map[string]any{"exported": i < exported},
		})
	}
}

func TestExplain_LargePublicSurface(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "pkg/leaky", 20, 19) // 95% exported, above floor and ratio
	addModuleSymbols(s, "pkg/tight", 20, 4)  // well encapsulated

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d: %+v", len(insights), insights)
	}
	if !strings.Contains(insights[0].Title, "pkg/leaky") {
		t.Errorf("title %q should name the over-exposed module", insights[0].Title)
	}
	if len(insights[0].Evidence) > maxEvidence {
		t.Errorf("evidence not capped: got %d", len(insights[0].Evidence))
	}
}

// TestExplain_CollapsesConditionalDuplicates asserts a type declared in both
// branches of a #if/#else block (two same-name conditional facts) is counted once
// toward the module's public surface, not twice. GAP-SW-10.
func TestExplain_CollapsesConditionalDuplicates(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "pkg/app", 19, 19) // 19 distinct exported symbols
	// One type declared once per #if/#else branch → two same-name conditional facts.
	for _, line := range []int{5, 20} {
		s.Add(facts.Fact{
			Kind:  facts.KindSymbol,
			Name:  "pkg/app.Gate",
			File:  "pkg/app/gate.swift",
			Line:  line,
			Props: map[string]any{"exported": true, "conditional": true, "language": "swift"},
		})
	}

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d: %+v", len(insights), insights)
	}
	// 19 + 1 canonical Gate = 20, NOT 21 (the second branch fact is collapsed).
	if !strings.Contains(insights[0].Title, "exports 20 of 20 symbols") {
		t.Errorf("conditional duplicate must be counted once; title = %q", insights[0].Title)
	}
}

// TestExplain_RubySkipped: Ruby symbols are public-by-default, so the exported
// ratio is uninformative and Ruby modules must never be flagged; other languages in
// the same store still are.
func TestExplain_RubySkipped(t *testing.T) {
	s := facts.NewStore()
	// A large, fully-"exported" Ruby namespace — must be skipped.
	for i := 0; i < 30; i++ {
		s.Add(facts.Fact{
			Kind: facts.KindSymbol, Name: fmt.Sprintf("Core::V3.Sym%d", i),
			File:  "app/controllers/core/v3/file.rb",
			Props: map[string]any{"exported": true, "language": "ruby"},
		})
	}
	// A non-Ruby over-exposed module — must still be reported.
	addModuleSymbols(s, "pkg/leaky", 20, 19)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	for _, in := range insights {
		if strings.Contains(in.Title, "Core::V3") {
			t.Errorf("Ruby module should be skipped by exported-surface: %q", in.Title)
		}
	}
	if len(insights) != 1 || !strings.Contains(insights[0].Title, "pkg/leaky") {
		t.Errorf("non-Ruby module should still be flagged; got %v", func() []string {
			out := make([]string, len(insights))
			for i, in := range insights {
				out[i] = in.Title
			}
			return out
		}())
	}
}

func TestExplain_BelowRatio(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "pkg/mid", 20, 15) // 75% exported, below minExportedRatio

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights below the ratio gate, got %d", len(insights))
	}
}

func TestExplain_SmallModuleExempt(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "pkg/tiny", 10, 10) // all exported but below minSymbols

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights for a small module, got %d", len(insights))
	}
}

func TestExplain_MockAndGeneratedExcluded(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "internal/mocks", 30, 30)
	addModuleSymbols(s, "pkg/db/generated", 30, 30)
	addModuleSymbols(s, "app/testing/support", 30, 30)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("mock/generated/testing modules should be excluded, got %d insights", len(insights))
	}
}

func TestExplain_CappedAndRankedBySurface(t *testing.T) {
	s := facts.NewStore()
	// More qualifying modules than the cap; larger surfaces must come first.
	for i := 0; i < maxInsights+5; i++ {
		total := 20 + i // strictly increasing surface size
		addModuleSymbols(s, fmt.Sprintf("pkg/m%02d", i), total, total)
	}

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != maxInsights {
		t.Fatalf("expected output capped at %d, got %d", maxInsights, len(insights))
	}
	// The largest module (m24, total 44) should be reported first.
	if !strings.Contains(insights[0].Title, "pkg/m24") {
		t.Errorf("largest surface should rank first, got %q", insights[0].Title)
	}
}

// TestExplain_MinSymbolsBoundary: a module with exactly minSymbols (all exported)
// is reported; one symbol fewer is exempt.
func TestExplain_MinSymbolsBoundary(t *testing.T) {
	at := facts.NewStore()
	addModuleSymbols(at, "pkg/edge", minSymbols, minSymbols)
	got, err := New().Explain(context.Background(), at)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("module with exactly minSymbols (%d) should be reported, got %d", minSymbols, len(got))
	}

	below := facts.NewStore()
	addModuleSymbols(below, "pkg/edge", minSymbols-1, minSymbols-1)
	got, err = New().Explain(context.Background(), below)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("module with minSymbols-1 (%d) should be exempt, got %d", minSymbols-1, len(got))
	}
}

// TestExplain_RatioBoundary: a module at exactly minExportedRatio is reported
// (the gate is >=), just below is not.
func TestExplain_RatioBoundary(t *testing.T) {
	at := facts.NewStore()
	addModuleSymbols(at, "pkg/edge", 20, 17) // 17/20 = 0.85 == minExportedRatio
	got, err := New().Explain(context.Background(), at)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("ratio == minExportedRatio (0.85) should be reported, got %d", len(got))
	}

	below := facts.NewStore()
	addModuleSymbols(below, "pkg/edge", 20, 16) // 0.80 < 0.85
	got, err = New().Explain(context.Background(), below)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ratio below minExportedRatio should not be reported, got %d", len(got))
	}
}

// TestExplain_MixedVisibilityRatioOnKnownOnly: symbols without an "exported" prop
// are excluded from the tally, so the ratio is computed over known-visibility
// symbols only.
func TestExplain_MixedVisibilityRatioOnKnownOnly(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "pkg/mixed", 20, 19) // 20 known, 19 exported -> 95%
	// Add symbols with unknown visibility; these must not change the ratio.
	for i := 0; i < 10; i++ {
		s.Add(facts.Fact{Kind: facts.KindSymbol, Name: fmt.Sprintf("pkg/mixed.Unknown%d", i), File: "pkg/mixed/u.go"})
	}
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	// Title reflects 19 of 20 (known only), not 19 of 30.
	if !strings.Contains(insights[0].Title, "19 of 20") {
		t.Errorf("ratio should be over known-visibility symbols only, got %q", insights[0].Title)
	}
}

// TestExplain_TitleFormatWithDigitName locks the title contract pkg/explain
// parses, for a module whose name contains digits (ties to the explain BUG-1 fix).
func TestExplain_TitleFormatWithDigitName(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "pkg/oauth2", 44, 40) // 40/44 = 90.9% -> "91%"
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("expected 1 insight, got %d", len(insights))
	}
	want := "Large public surface: pkg/oauth2 exports 40 of 44 symbols (91%)"
	if insights[0].Title != want {
		t.Errorf("title = %q, want %q", insights[0].Title, want)
	}
}

func TestExplain_MissingVisibilityIgnored(t *testing.T) {
	s := facts.NewStore()
	for i := 0; i < 20; i++ {
		s.Add(facts.Fact{Kind: facts.KindSymbol, Name: fmt.Sprintf("pkg/x.S%d", i), File: "pkg/x/f.go"})
	}

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		t.Errorf("expected 0 insights when visibility is unknown, got %d", len(insights))
	}
}

// TestExplain_GradleAndJestTestModulesExcluded pins the surface half of new/55.
// excludedSegments lowercases the module path, so `Tests/` already matched its
// `tests` entry — but `androidTest` lowercases to `androidtest`, which is not in the
// set, and neither are `__tests__` or the Kotlin-Multiplatform trees. On
// a large Android app that produced a real finding: "Large public surface:
// app/src/androidTest/java/de/example/app/ui/feature/compose". An instrumented-test
// source set is not a hand-maintained public API.
func TestExplain_GradleAndJestTestModulesExcluded(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "app/src/androidTest/java/de/example/app/ui/feature/compose", 30, 30)
	addModuleSymbols(s, "src/components/__tests__", 30, 30)
	addModuleSymbols(s, "shared/src/commonTest/kotlin", 30, 30)
	addModuleSymbols(s, "Tests/Testability/Sources", 30, 30)
	addModuleSymbols(s, "app/Mocks", 30, 30)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 0 {
		for _, in := range insights {
			t.Errorf("test-source module reported as a public surface: %q", in.Title)
		}
	}
}

// TestExplain_ProductionModuleNamedLikeATestKept is the fixed/28 guard, at the
// module level. A production package whose name merely CONTAINS a test-ish token —
// `contest`, `latest`, an A/B-test feature — must still be analyzed. Suppressing a
// real module is silent; reporting one is not.
func TestExplain_ProductionModuleNamedLikeATestKept(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "app/features/contest", 30, 30)

	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(insights) != 1 {
		t.Errorf("a production module named 'contest' must not be excluded, got %d insights", len(insights))
	}
}

// A markdown document is not a module with a public API. Its headings are
// exported by construction — there is no private one — so the ratio measures the
// format rather than the code, and the estate's most confident nonsense came out of
// reading one: a changelog "exports 1,405 of 1,405 symbols (100%)".
func TestSurface_MarkdownDocumentsAreNotAPublicSurface(t *testing.T) {
	store := facts.NewStore()
	store.Add(facts.Fact{Kind: facts.KindModule, Name: "CHANGELOG", File: "CHANGELOG.md", Repo: "app"})
	for i := range 40 {
		store.Add(facts.Fact{
			Kind: facts.KindSymbol,
			Name: fmt.Sprintf("CHANGELOG#release-%d", i),
			File: "CHANGELOG.md",
			Repo: "app",
			Props: map[string]any{
				"language": "markdown", "symbol_kind": "section", "exported": true,
			},
		})
	}
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range insights {
		if strings.Contains(i.Title, "CHANGELOG") {
			t.Errorf("reported a markdown document as a public surface: %q", i.Title)
		}
	}
}

// A member is a part of a declaration, not surface of its module, so many of them do not
// bring a near-fully-exported module under the ratio.
func TestExplain_MembersDoNotDiluteTheRatio(t *testing.T) {
	s := facts.NewStore()
	addModuleSymbols(s, "pkg/leaky", 20, 19)
	for i := 0; i < 20; i++ {
		s.Add(facts.Fact{
			Kind:  facts.KindSymbol,
			Name:  fmt.Sprintf("pkg/leaky.Outer.nested%d", i),
			File:  "pkg/leaky/a.py",
			Props: map[string]any{"exported": false, facts.PropMemberOf: "pkg/leaky.Outer"},
		})
	}
	insights, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(insights) != 1 {
		t.Errorf("expected the module reported, got %+v", insights)
	}
}
