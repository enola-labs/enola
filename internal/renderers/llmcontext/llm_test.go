package llmcontext

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/enola-labs/enola/internal/explainers/layers"
	"github.com/enola-labs/enola/internal/facts"
)

func makeSnapshot(ff []facts.Fact, insights []facts.Insight) *facts.Snapshot {
	return &facts.Snapshot{
		Meta: facts.SnapshotMeta{
			GeneratedAt:  "2024-01-01T00:00:00Z",
			Duration:     "1s",
			FactCount:    len(ff),
			InsightCount: len(insights),
		},
		Facts:    ff,
		Insights: insights,
	}
}

func TestTokenBudgetEnforcement(t *testing.T) {
	// Create a snapshot with enough facts to generate long content
	var ff []facts.Fact
	for i := 0; i < 50; i++ {
		ff = append(ff, facts.Fact{
			Kind: facts.KindModule,
			Name: strings.Repeat("module_", 10) + string(rune('A'+i%26)),
			Props: map[string]any{
				"language": "go",
			},
		})
	}

	snapshot := makeSnapshot(ff, nil)

	// Small token budget (100 tokens = 400 chars) — enough for truncation logic
	// to work (maxChars-100 must be positive; see BUG note below)
	r := New(100)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}

	content := string(artifacts[0].Content)
	hasTruncation := strings.Contains(content, "[Truncated in:") || strings.Contains(content, "[Omitted:")
	if !hasTruncation {
		t.Error("expected truncation or omission marker in output")
	}
	// Content should be within budget (400 chars + truncation/omission message)
	maxExpected := 100*4 + 80
	if len(content) > maxExpected {
		t.Errorf("content length %d exceeds expected truncated size %d", len(content), maxExpected)
	}
}

func TestRender_EmptySnapshot(t *testing.T) {
	snapshot := makeSnapshot(nil, nil)
	r := New(4000)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}

	content := string(artifacts[0].Content)
	if !strings.Contains(content, "# Architecture Snapshot") {
		t.Error("expected Architecture Snapshot header")
	}
	if !strings.Contains(content, "_No modules detected._") {
		t.Error("expected 'No modules detected' fallback")
	}
	if !strings.Contains(content, "_No entry points detected._") {
		t.Error("expected 'No entry points detected' fallback")
	}
}

func TestCrossRepo_RendersDependencyTable(t *testing.T) {
	ff := []facts.Fact{
		{Kind: facts.KindService, Name: "svc-alpha", Repo: "svc-alpha",
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "svc-beta"}}},
		{Kind: facts.KindService, Name: "svc-beta", Repo: "svc-beta"},
		{Kind: facts.KindDependency, Name: "svc-alpha -> svc-beta", Repo: "svc-alpha",
			Props: map[string]any{
				"type": "cross_repo", "via": []string{"http"},
				"endpoint_count": 1, "endpoints": []string{"GET /api/items/{id}"},
			}},
		{Kind: facts.KindDependency, Name: "svc-alpha -> lib-core", Repo: "svc-alpha",
			Props: map[string]any{
				"type": "cross_repo", "via": []string{"import"},
				"import_count": 1, "import_samples": []string{"lib-core/money/converter"},
			}},
	}
	content := string(mustRender(t, makeSnapshot(ff, nil)))

	if !strings.Contains(content, "## Cross-Repo Dependencies") {
		t.Fatalf("missing Cross-Repo Dependencies section:\n%s", content)
	}
	for _, want := range []string{
		"`svc-alpha` | `svc-beta` | http",
		"GET /api/items/{id}",
		"`svc-alpha` | `lib-core` | import",
		"lib-core/money/converter",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("cross-repo table missing %q in:\n%s", want, content)
		}
	}
}

func TestCrossRepo_OmittedWhenNoEdges(t *testing.T) {
	content := string(mustRender(t, makeSnapshot([]facts.Fact{{Kind: facts.KindModule, Name: "m"}}, nil)))
	if strings.Contains(content, "Cross-Repo Dependencies") {
		t.Errorf("cross-repo section should be omitted for single-repo snapshots:\n%s", content)
	}
}

func mustRender(t *testing.T, snapshot *facts.Snapshot) []byte {
	t.Helper()
	artifacts, err := New(16000).Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	return artifacts[0].Content
}

func TestRiskZones_IncludesCyclesAndViolations(t *testing.T) {
	insights := []facts.Insight{
		{Title: "Architecture pattern: hexagonal", Confidence: 0.8, Description: "Detected hexagonal"},
		{Title: "Cyclic dependency detected (3 modules)", Confidence: 1.0, Description: "A -> B -> C -> A"},
		{Title: "Layer violation: domain -> presentation", Confidence: 0.8, Description: "Domain imports presentation"},
	}

	snapshot := makeSnapshot(nil, insights)
	r := New(4000)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	content := string(artifacts[0].Content)

	// Cycle and violation should appear in Risk Zones
	if !strings.Contains(content, "Cyclic dependency") {
		t.Error("expected Cyclic dependency in Risk Zones")
	}
	if !strings.Contains(content, "Layer violation") {
		t.Error("expected Layer violation in Risk Zones")
	}

	// Architecture pattern should NOT appear in Risk Zones section
	// It appears in the Architecture Pattern section instead
	riskIdx := strings.Index(content, "## Risk Zones")
	if riskIdx >= 0 {
		riskSection := content[riskIdx:]
		nextSection := strings.Index(riskSection[1:], "## ")
		if nextSection > 0 {
			riskSection = riskSection[:nextSection+1]
		}
		if strings.Contains(riskSection, "Architecture pattern") {
			t.Error("Architecture pattern insight should NOT appear in Risk Zones")
		}
	}
}

// threeWayTieSnapshot returns the shape a tie takes in a real repository: a, b
// and c all import lib-core, so lib-core scores 3 and the three importers tie at
// 1 with nothing but their names to separate them.
func threeWayTieSnapshot() *facts.Snapshot {
	ff := []facts.Fact{
		{Kind: facts.KindModule, Name: "lib-core"},
		{Kind: facts.KindModule, Name: "a"},
		{Kind: facts.KindModule, Name: "b"},
		{Kind: facts.KindModule, Name: "c"},
	}
	for _, src := range []string{"a", "b", "c"} {
		ff = append(ff, facts.Fact{
			Kind: facts.KindDependency,
			File: src + "/file.go",
			Relations: []facts.Relation{
				{Kind: facts.RelImports, Target: "lib-core"},
			},
		})
	}
	return makeSnapshot(ff, nil)
}

func TestCriticalModules_FanInFanOut(t *testing.T) {
	snapshot := threeWayTieSnapshot()
	r := New(4000)
	artifacts, err := r.Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	content := string(artifacts[0].Content)
	if !strings.Contains(content, "`lib-core`") {
		t.Error("expected lib-core module in Critical Modules table")
	}
	// lib-core has fanIn=3, fanOut=0, score=3 → "low" criticality
	if !strings.Contains(content, "| 3 | 0 |") {
		t.Error("expected lib-core to have fanIn=3, fanOut=0")
	}
}

// One snapshot rendered twice must produce one document. The rankings are built
// out of maps, so before the name tie-breaks a tie ordered by Go's randomized map
// iteration and the same snapshot rendered differently on nearly every pass. The
// receipt's output_hashes cannot assert this: llm_context.md's footer carries the
// wall clock, so its hash never repeats across two runs regardless.
func TestRender_RepeatedRendersAreIdentical(t *testing.T) {
	snapshot := threeWayTieSnapshot()
	first := mustRender(t, snapshot)
	for i := 2; i <= 64; i++ {
		if got := mustRender(t, snapshot); !bytes.Equal(first, got) {
			t.Fatalf("render %d differs from the first:\nfirst:\n%s\nrender %d:\n%s", i, first, i, got)
		}
	}
}

// renderWithCoverage renders a minimal snapshot whose only extraction-quality
// signal is the given cross-repo coverage summary. A nil summary is the
// single-repo shape: coverageSummary returns nil when there are no service nodes.
func renderWithCoverage(t *testing.T, cov *facts.CoverageSummary) string {
	t.Helper()
	snapshot := makeSnapshot(nil, nil)
	snapshot.Meta.Coverage = cov
	return string(mustRender(t, snapshot))
}

// lineWith returns the first rendered line containing substr, or "" if none does.
func lineWith(content, substr string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// A connected service resolves at least one outbound edge, so it is not a coverage
// gap — yet any call site it could not resolve still lands in UnresolvedEdges. That
// is the normal healthy multi-repo shape, and it must not hide the count.
func TestExtractionQuality_UnresolvedEdgesWithoutGaps(t *testing.T) {
	content := renderWithCoverage(t, &facts.CoverageSummary{
		ServicesTotal: 4, CoverageGaps: 0, UnresolvedEdges: 23, ExternalEdges: 2,
	})

	unresolved := lineWith(content, "Unresolved outbound edges")
	if !strings.Contains(unresolved, "**23**") {
		t.Errorf("unresolved-edge count not rendered; got line %q in:\n%s", unresolved, content)
	}
	if !strings.Contains(unresolved, "⚠️") {
		t.Errorf("unresolved edges are an internal blind spot and must be flagged; got %q", unresolved)
	}

	external := lineWith(content, "external hosts")
	if !strings.Contains(external, "**2**") {
		t.Errorf("external-edge count not rendered; got line %q in:\n%s", external, content)
	}
	// External call sites are expected, not a blind spot: flagging them would invite
	// exactly the misreading this section exists to prevent.
	if strings.Contains(external, "⚠️") {
		t.Errorf("external edges must not be flagged as a warning; got %q", external)
	}

	if strings.Contains(content, "coverage gaps") {
		t.Errorf("no gaps in this snapshot, yet a gaps line rendered:\n%s", content)
	}
	if !strings.Contains(content, "These are extraction limits") {
		t.Errorf("unresolved edges are an extraction limit; footer missing in:\n%s", content)
	}
}

func TestExtractionQuality_GapsStillRender(t *testing.T) {
	content := renderWithCoverage(t, &facts.CoverageSummary{
		ServicesTotal: 2, CoverageGaps: 1, UnresolvedEdges: 348,
	})

	if gaps := lineWith(content, "Cross-repo coverage gaps"); !strings.Contains(gaps, "**1**") {
		t.Errorf("gap count not rendered; got line %q in:\n%s", gaps, content)
	}
	if unresolved := lineWith(content, "Unresolved outbound edges"); !strings.Contains(unresolved, "**348**") {
		t.Errorf("unresolved count not rendered alongside gaps; got line %q in:\n%s", unresolved, content)
	}
	if strings.Contains(content, "external hosts") {
		t.Errorf("no external edges in this snapshot, yet a line rendered:\n%s", content)
	}
	if !strings.Contains(content, "These are extraction limits") {
		t.Errorf("footer missing in:\n%s", content)
	}
}

// Clean cross-repo coverage stays quiet: the section header still renders (Coverage
// is non-nil), but nothing warns.
func TestExtractionQuality_CleanCoverageQuiet(t *testing.T) {
	content := renderWithCoverage(t, &facts.CoverageSummary{ServicesTotal: 4})

	if !strings.Contains(content, "## Extraction Quality") {
		t.Fatalf("section should render whenever Coverage is non-nil:\n%s", content)
	}
	for _, unwanted := range []string{
		"coverage gaps", "Unresolved outbound edges", "external hosts", "These are extraction limits",
	} {
		if strings.Contains(content, unwanted) {
			t.Errorf("clean coverage must not render %q in:\n%s", unwanted, content)
		}
	}
}

// A single-repo snapshot has no service nodes, so Coverage is nil and there is no
// receipt to report on at all.
func TestExtractionQuality_SingleRepoNoCoverage(t *testing.T) {
	content := renderWithCoverage(t, nil)
	if strings.Contains(content, "## Extraction Quality") {
		t.Errorf("section should be absent with no receipt and no coverage:\n%s", content)
	}
}

// GAP-LK-04: a real single-repo snapshot (files parsed, but Coverage nil because
// there are no service nodes) renders the section and must state that the cross-repo
// linker did not run — otherwise renderCrossRepo's empty section reads identically
// to a fully-resolved cluster's.
func TestExtractionQuality_SingleRepoNotesCrossRepoNotRun(t *testing.T) {
	snapshot := makeSnapshot(nil, nil)
	snapshot.Meta.FilesSeen = 1920
	snapshot.Meta.FilesParsed = 1595
	snapshot.Meta.Coverage = nil // single-repo: no service nodes
	content := string(mustRender(t, snapshot))

	if !strings.Contains(content, "## Extraction Quality") {
		t.Fatalf("section should render when files were seen:\n%s", content)
	}
	if !strings.Contains(content, "Cross-repo analysis: not run") {
		t.Errorf("single-repo snapshot must state the cross-repo linker did not run:\n%s", content)
	}
}

// The multi-repo shape (Coverage non-nil) must NOT carry the single-repo note.
func TestExtractionQuality_MultiRepoOmitsCrossRepoNotRun(t *testing.T) {
	content := renderWithCoverage(t, &facts.CoverageSummary{ServicesTotal: 4})
	if strings.Contains(content, "Cross-repo analysis: not run") {
		t.Errorf("multi-repo snapshot must not claim cross-repo analysis did not run:\n%s", content)
	}
}

// bigRepoMapSnapshot returns a snapshot whose Repository Map alone overruns any
// small token budget — the shape of a real multi-repo cluster.
func bigRepoMapSnapshot(modules int, cov *facts.CoverageSummary) *facts.Snapshot {
	var ff []facts.Fact
	for i := 0; i < modules; i++ {
		ff = append(ff, facts.Fact{
			Kind: facts.KindModule,
			// One AREA per module, because the repository map now groups by the
			// first path segment: a fixture whose modules all share one segment
			// summarises to a single row and stops exercising truncation at all.
			Name:  fmt.Sprintf("service_%03d/%s", i, strings.Repeat("segment_", 4)),
			Props: map[string]any{"language": "go"},
		})
	}
	snapshot := makeSnapshot(ff, nil)
	snapshot.Meta.FilesSeen = 5552
	snapshot.Meta.FilesParsed = 2870
	snapshot.Meta.Coverage = cov
	return snapshot
}

// The Extraction Quality preface is the signal an agent uses to calibrate how far to
// trust the graph, and it is worth more than the tail of a module table. An earlier
// oversized section must not starve it — which is exactly what happened on a 4-repo
// snapshot, where the whole section vanished behind "[Truncated in: Repository Map]".
func TestRender_ExtractionQualitySurvivesTruncatedRepoMap(t *testing.T) {
	const maxTokens = 1000
	snapshot := bigRepoMapSnapshot(400, &facts.CoverageSummary{
		ServicesTotal: 4, CoverageGaps: 0, UnresolvedEdges: 23, ExternalEdges: 2,
	})

	artifacts, err := New(maxTokens).Render(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	content := string(artifacts[0].Content)

	if !strings.Contains(content, "[Truncated in: Repository Map]") {
		t.Fatalf("test is not exercising truncation; Repository Map fit the budget:\n%s", content)
	}
	if !strings.Contains(content, "## Extraction Quality") {
		t.Errorf("Extraction Quality starved by an earlier oversized section:\n%s", content)
	}
	if !strings.Contains(content, "**23**") {
		t.Errorf("unresolved-edge count lost to truncation:\n%s", content)
	}
	// The reservation must come out of the budget, not be added on top of it.
	if limit := maxTokens*4 + 100; len(content) > limit {
		t.Errorf("content length %d exceeds budget %d", len(content), limit)
	}
}

// The budget is a byte count, but the content is UTF-8: a warning glyph or a
// non-ASCII identifier must never be cut in half into invalid UTF-8.
func TestCutAt_NeverSplitsRune(t *testing.T) {
	s := "map: `ünïcode/módule` ⚠️ tail"
	for n := 0; n <= len(s); n++ {
		got := cutAt(s, n)
		if !utf8.ValidString(got) {
			t.Errorf("cutAt(%q, %d) = %q, which is not valid UTF-8", s, n, got)
		}
		if len(got) > n {
			t.Errorf("cutAt(%q, %d) returned %d bytes, over the limit", s, n, len(got))
		}
	}
}

// The cut point moves with the budget, so sweep it: some budget lands mid-rune in a
// non-ASCII module name, and the artifact must stay valid UTF-8 at every one.
func TestRender_TruncatedOutputIsValidUTF8(t *testing.T) {
	snapshot := bigRepoMapSnapshot(400, nil)
	for i := range snapshot.Facts {
		snapshot.Facts[i].Name = fmt.Sprintf("sérvice/%03d/%s", i, strings.Repeat("ø", 20))
	}

	for tokens := 200; tokens <= 400; tokens++ {
		artifacts, err := New(tokens).Render(context.Background(), snapshot)
		if err != nil {
			t.Fatalf("Render(%d): %v", tokens, err)
		}
		if !utf8.Valid(artifacts[0].Content) {
			t.Fatalf("truncation at maxTokens=%d produced invalid UTF-8", tokens)
		}
	}
}

func TestFileDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"src/foo/bar.go", "src/foo"},
		{"file.go", "."},
		{"a/b/c/d.go", "a/b/c"},
	}
	for _, tt := range tests {
		got := fileDir(tt.input)
		if got != tt.want {
			t.Errorf("fileDir(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestFeatureGuideIsDerived: the guide is a rendering of the recognised layer
// order, not authored prose. It used to be a switch over three taxonomy names with
// a generic fallback for the other eleven, so most repositories were told to
// "identify the appropriate module/package" while the snapshot held their exact
// layer order.
func TestFeatureGuideIsDerived(t *testing.T) {
	snap := makeSnapshot(nil, []facts.Insight{{
		Title: "Architecture pattern: rails-mvc", Confidence: 0.8, Informational: true,
		Metrics: map[string]any{
			layers.MetricLayersOrdered: []string{"controller", "view", "job", "model", "service", "presenter"},
			layers.MetricLayerLevels: map[string]any{
				"controller": 2, "view": 2, "job": 1, "model": 1, "service": 1, "presenter": 1,
			},
			layers.MetricLayerExamples: map[string]string{
				"controller": "app/controllers", "job": "app/jobs",
				"model": "app/models", "service": "app/services",
			},
		},
	}})

	got := New(16000).renderFeatureGuide(snap)

	if !strings.Contains(got, "laid out as **rails-mvc**") {
		t.Fatalf("expected the recognised taxonomy to be named:\n%s", got)
	}
	// Peers share a tier and must be listed together, not numbered as steps: the
	// Rails taxonomy deliberately puts its domain directories at one level.
	if !strings.Contains(got, "**job, model, service**") {
		t.Errorf("expected the domain peers grouped on one tier:\n%s", got)
	}
	// A layer the repository does not have says nothing about the repository.
	for _, absent := range []string{"presenter", "view"} {
		if strings.Contains(got, absent) {
			t.Errorf("layer %q has no module here and must not be listed:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, "app/controllers") {
		t.Errorf("expected a real module as the example:\n%s", got)
	}

	// Nothing recognised: no section at all, rather than advice true of every
	// codebase ever written.
	if out := New(16000).renderFeatureGuide(makeSnapshot(nil, nil)); out != "" {
		t.Errorf("expected no section when no order was recognised, got:\n%s", out)
	}
}

// TestFeatureGuide_ServesADeclaredOrder is the capability the metrics field was
// added for. A declared layer order lives in the repository's own intent facts, so
// there is no built-in taxonomy to look up by name — the guide read the order out
// of patternDefs and therefore had nothing to say about a repository that states
// its own architecture, enola's own tree included.
func TestFeatureGuide_ServesADeclaredOrder(t *testing.T) {
	snap := makeSnapshot(nil, []facts.Insight{{
		Title: "Architecture pattern: declared (enola)", Confidence: 1.0, Informational: true,
		Metrics: map[string]any{
			layers.MetricLayersOrdered:   []string{"entrypoint", "surface", "core"},
			layers.MetricLayerLevels:     map[string]any{"entrypoint": 2, "surface": 1, "core": 0},
			layers.MetricLayersUnordered: []string{"wiring"},
			layers.MetricLayerExamples: map[string]string{
				"entrypoint": "cmd/enola", "surface": "pkg/cli", "core": "internal/facts",
				"wiring": "internal/bootstrap",
			},
		},
	}})

	got := New(16000).renderFeatureGuide(snap)
	if !strings.Contains(got, "laid out as **declared (enola)**") {
		t.Fatalf("a declared order must produce a guide:\n%s", got)
	}
	for i, want := range []string{"**entrypoint** — e.g. `cmd/enola`", "**surface** — e.g. `pkg/cli`", "**core** — e.g. `internal/facts`"} {
		if !strings.Contains(got, want) {
			t.Errorf("step %d missing (%q):\n%s", i+1, want, got)
		}
	}
	if !strings.Contains(got, "Outside that order: wiring") {
		t.Errorf("unordered layers must be named as exempt:\n%s", got)
	}
}
