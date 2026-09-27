package diff

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// mod builds a module fact with the given import relations.
func mod(name, file string, imports ...string) facts.Fact {
	f := facts.Fact{Kind: facts.KindModule, Name: name, File: file, Repo: "r"}
	for _, t := range imports {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelImports, Target: t})
	}
	return f
}

func sym(name, file string, line int) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Line: line, Repo: "r",
		Props: map[string]any{"symbol_kind": facts.SymbolFunc}}
}

func cycleInsight(modules ...string) facts.Insight {
	in := facts.Insight{Source: "cycles", Title: "Dependency cycle: " + strings.Join(modules, " → "), Confidence: 1.0}
	for _, m := range modules {
		in.Evidence = append(in.Evidence, facts.Evidence{Fact: m})
	}
	return in
}

func snap(fs []facts.Fact, ins []facts.Insight) *facts.Snapshot {
	return &facts.Snapshot{Facts: fs, Insights: ins}
}

func TestCompute_AddedRemovedEdges(t *testing.T) {
	base := snap([]facts.Fact{mod("a", "a/a.go", "b"), mod("b", "b/b.go")}, nil)
	cur := snap([]facts.Fact{mod("a", "a/a.go", "b", "c"), mod("b", "b/b.go"), mod("c", "c/c.go")}, nil)

	d := Compute(base, cur)

	if len(d.FactsAdded) != 1 || d.FactsAdded[0].Name != "c" {
		t.Fatalf("expected module c added, got %+v", d.FactsAdded)
	}
	if len(d.FactsRemoved) != 0 {
		t.Fatalf("expected no removals, got %+v", d.FactsRemoved)
	}
	if len(d.EdgesAdded) != 1 || d.EdgesAdded[0].Source != "a" || d.EdgesAdded[0].Target != "c" {
		t.Fatalf("expected edge a->c added, got %+v", d.EdgesAdded)
	}
}

func TestCompute_RemovedSymbol(t *testing.T) {
	base := snap([]facts.Fact{sym("Foo", "x.go", 10), sym("Bar", "x.go", 20)}, nil)
	cur := snap([]facts.Fact{sym("Foo", "x.go", 10)}, nil)

	d := Compute(base, cur)
	if len(d.FactsRemoved) != 1 || d.FactsRemoved[0].Name != "Bar" {
		t.Fatalf("expected Bar removed, got %+v", d.FactsRemoved)
	}
}

func TestCompute_LineShiftIsNotAChange(t *testing.T) {
	// A symbol that only moved lines (props identical) must NOT register as changed —
	// otherwise any edit floods the diff with noise.
	base := snap([]facts.Fact{sym("Foo", "x.go", 10)}, nil)
	cur := snap([]facts.Fact{sym("Foo", "x.go", 42)}, nil)

	d := Compute(base, cur)
	if !d.Empty() {
		t.Fatalf("expected empty diff for line-only shift, got %+v", d)
	}
}

func TestCompute_PropsChangeIsAChange(t *testing.T) {
	a := sym("Foo", "x.go", 10)
	b := sym("Foo", "x.go", 10)
	b.Props = map[string]any{"symbol_kind": facts.SymbolFunc, "signature": "func Foo(x int)"}

	d := Compute(snap([]facts.Fact{a}, nil), snap([]facts.Fact{b}, nil))
	if len(d.FactsChanged) != 1 {
		t.Fatalf("expected 1 changed fact, got %+v", d.FactsChanged)
	}
}

func TestCompute_NewFinding(t *testing.T) {
	// A new cycle comes with the structural edges that created it, so its members
	// are in the change's touched set and it is a real regression.
	base := snap([]facts.Fact{mod("a", "a.go"), mod("b", "b.go")}, nil)
	cur := snap([]facts.Fact{mod("a", "a.go", "b"), mod("b", "b.go", "a")}, []facts.Insight{cycleInsight("a", "b")})

	d := Compute(base, cur)
	if len(d.FindingsNew) != 1 {
		t.Fatalf("expected 1 new finding, got %+v", d.FindingsNew)
	}
	if len(d.FindingsResolved) != 0 {
		t.Fatalf("expected 0 resolved, got %+v", d.FindingsResolved)
	}
}

// TestCompute_NewLayerViolationIsAttributed pins the property that makes
// `--fail-on=layers` mean anything: a layer violation must cite an entity the change
// actually touched, so that it lands in FindingsNew and reaches the gate.
//
// touchedNames holds fact NAMES plus both endpoints of added/removed edges. An
// importing source file is never a fact name, so evidence citing only the file cannot
// be attributed under any change — it lands in FindingsNewIncidental, which pkg/check
// never grades. The explainer therefore also cites the dependency fact and the raw
// import target, which ARE the two edge endpoints.
func TestCompute_NewLayerViolationIsAttributed(t *testing.T) {
	dep := func(name, file, target string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: name, File: file, Repo: "r",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}}}
	}
	violation := facts.Insight{
		Source: "layers", Title: "Layer violation: domain -> adapter", Confidence: 0.8,
		Evidence: []facts.Evidence{
			{File: "domain/order.go", Detail: "import of adapter"},
			{Fact: "domain -> adapter", Detail: "import edge"},
			{Fact: "adapter", Detail: "import edge target"},
		},
	}

	base := snap([]facts.Fact{mod("domain", "domain/x.go")}, nil)
	cur := snap([]facts.Fact{
		mod("domain", "domain/x.go"),
		dep("domain -> adapter", "domain/order.go", "adapter"),
	}, []facts.Insight{violation})

	d := Compute(base, cur)
	if len(d.FindingsNew) != 1 {
		t.Fatalf("a newly introduced layer violation must be a real regression, got new=%+v incidental=%+v",
			d.FindingsNew, d.FindingsNewIncidental)
	}
	if len(d.FindingsNewIncidental) != 0 {
		t.Fatalf("expected no incidental findings, got %+v", d.FindingsNewIncidental)
	}
}

// TestCompute_FileOnlyEvidenceCannotBeAttributed is the negative half of the test
// above, and explains why the extra evidence entries exist at all. Kept so that
// reverting the explainer to file-only evidence fails here with a reason attached.
func TestCompute_FileOnlyEvidenceCannotBeAttributed(t *testing.T) {
	fileOnly := facts.Insight{
		Source: "layers", Title: "Layer violation: domain -> adapter", Confidence: 0.8,
		Evidence: []facts.Evidence{{File: "domain/order.go", Detail: "import of adapter"}},
	}
	base := snap([]facts.Fact{mod("domain", "domain/x.go")}, nil)
	cur := snap([]facts.Fact{mod("domain", "domain/x.go"), mod("adapter", "adapter/y.go")},
		[]facts.Insight{fileOnly})

	d := Compute(base, cur)
	if len(d.FindingsNew) != 0 || len(d.FindingsNewIncidental) != 1 {
		t.Fatalf("evidence citing only a source file is unattributable and must be incidental, got new=%+v incidental=%+v",
			d.FindingsNew, d.FindingsNewIncidental)
	}
}

// TestCompute_PreexistingFindingIsSilent is the false-signal guard: a finding
// present in BOTH snapshots (e.g. an API-first route unused before and after, or
// a pre-existing cycle) must never surface. The ratchet reports movement, not state.
func TestCompute_PreexistingFindingIsSilent(t *testing.T) {
	preexisting := cycleInsight("a", "b")
	base := snap(nil, []facts.Insight{preexisting})
	cur := snap(nil, []facts.Insight{preexisting})

	d := Compute(base, cur)
	if !d.Empty() {
		t.Fatalf("pre-existing finding leaked into diff: %+v", d)
	}
	if len(d.FindingsNew) != 0 {
		t.Fatalf("pre-existing finding reported as new: %+v", d.FindingsNew)
	}
}

// TestCompute_FindingStableAcrossVolatileTitle verifies a finding about the same
// entities stays IDENTIFIED even when its title/detail metrics change, so a god-class
// whose fan-in ticked up does not churn as resolve+new.
//
// It used to additionally assert d.Empty() — that the diff say nothing at all. That was
// an over-correction, and it is the bug new/53 reports: it conflated "this is the same
// finding" (true, and what the number-normalized key is for) with "this finding is
// unchanged" (false — it moved). Held to its literal wording, it certified silence for
// the dead-code rollup swinging 5621 -> 3662, a 1,959-symbol change, in the very tool
// the improvement loop uses to check for collateral damage.
//
// The identity guarantee is unchanged and still asserted. What is new: the drift is
// reported, in a bucket of its own, instead of vanishing.
func TestCompute_FindingStableAcrossVolatileTitle(t *testing.T) {
	before := facts.Insight{Source: "god-class", Title: "God class Foo (fan-in 11)", Confidence: 0.6,
		Evidence: []facts.Evidence{{Symbol: "Foo", Detail: "fan-in: 11"}}}
	after := facts.Insight{Source: "god-class", Title: "God class Foo (fan-in 13)", Confidence: 0.7,
		Evidence: []facts.Evidence{{Symbol: "Foo", Detail: "fan-in: 13"}}}

	d := Compute(snap(nil, []facts.Insight{before}), snap(nil, []facts.Insight{after}))

	// The point of the number-normalized key: no churn.
	if n := len(d.FindingsNew) + len(d.FindingsNewIncidental); n != 0 {
		t.Errorf("finding churned as new (%d) despite identity-stable key", n)
	}
	if n := len(d.FindingsResolved) + len(d.FindingsResolvedIncidental); n != 0 {
		t.Errorf("finding churned as resolved (%d) despite identity-stable key", n)
	}
	// And the honest part: it did change, and the diff says so.
	if len(d.FindingsChanged) != 1 {
		t.Fatalf("FindingsChanged = %d, want 1 — the fan-in moved 11 -> 13", len(d.FindingsChanged))
	}
}

func TestCompute_ResolvedFinding(t *testing.T) {
	// Breaking the cycle removes the b→a edge, so the cycle members are touched and
	// the cleared finding is a real improvement.
	base := snap([]facts.Fact{mod("a", "a.go", "b"), mod("b", "b.go", "a")}, []facts.Insight{cycleInsight("a", "b")})
	cur := snap([]facts.Fact{mod("a", "a.go", "b"), mod("b", "b.go")}, nil)

	d := Compute(base, cur)
	if len(d.FindingsResolved) != 1 {
		t.Fatalf("expected 1 resolved finding, got %+v", d.FindingsResolved)
	}
}

// TestCompute_RankWindowFindingIsIncidental is the regression guard for the false
// positives found dogfooding: a finding about an UNCHANGED subject — e.g. a module
// that rose into a top-N list only because a worse one was removed — must be
// classified incidental, not a real regression. The genuinely-removed subject's
// finding still resolves as a real improvement.
func TestCompute_RankWindowFindingIsIncidental(t *testing.T) {
	surf := func(module, sym string) facts.Insight {
		return facts.Insight{Source: "exported-surface", Confidence: 0.6,
			Title:    "Large public surface: " + module + " exports 67 of 67 symbols (100%)",
			Evidence: []facts.Evidence{{Symbol: sym, Detail: "exported"}}}
	}
	base := snap(
		[]facts.Fact{sym("app/golf_rule.Svc", "app/golf_rule/svc.go", 1), sym("db/messaging.Repo", "db/messaging/repo.go", 1)},
		[]facts.Insight{surf("app/golf_rule", "app/golf_rule.Svc")}, // only golf_rule reported (messaging below the line)
	)
	cur := snap(
		[]facts.Fact{sym("db/messaging.Repo", "db/messaging/repo.go", 1)}, // golf_rule removed; messaging unchanged
		[]facts.Insight{surf("db/messaging", "db/messaging.Repo")},        // messaging rose into the window
	)

	d := Compute(base, cur)
	if len(d.FindingsNew) != 0 {
		t.Fatalf("unchanged module rising into top-N must be incidental, got real: %+v", d.FindingsNew)
	}
	if len(d.FindingsNewIncidental) != 1 {
		t.Fatalf("expected 1 incidental new finding (messaging), got %+v", d.FindingsNewIncidental)
	}
	if len(d.FindingsResolved) != 1 {
		t.Fatalf("expected golf_rule (actually removed) as 1 real improvement, got %+v", d.FindingsResolved)
	}
}

// TestCompute_SummaryFindingDoesNotChurn guards the layers/summary case: a finding
// whose evidence enumerates the whole codebase keeps its identity when modules
// change, so it never churns resolve+introduce.
func TestCompute_SummaryFindingDoesNotChurn(t *testing.T) {
	layers := func(mods ...string) facts.Insight {
		in := facts.Insight{Source: "layers", Confidence: 0.89,
			Title: fmt.Sprintf("Architecture pattern: go-standard (%d modules)", len(mods))}
		for _, m := range mods {
			in.Evidence = append(in.Evidence, facts.Evidence{Fact: m})
		}
		return in
	}
	base := snap(nil, []facts.Insight{layers("a", "b", "c", "d")})
	cur := snap(nil, []facts.Insight{layers("a", "b", "c")})

	d := Compute(base, cur)

	// Identity holds: a whole-codebase summary finding must not resolve+reintroduce
	// every time one module leaves its evidence list.
	if n := len(d.FindingsNew) + len(d.FindingsNewIncidental) +
		len(d.FindingsResolved) + len(d.FindingsResolvedIncidental); n != 0 {
		t.Errorf("summary finding churned (%d entries) on a module-count change", n)
	}
	// But it did shrink from 4 modules to 3, and that is worth saying out loud.
	if len(d.FindingsChanged) != 1 {
		t.Fatalf("FindingsChanged = %d, want 1", len(d.FindingsChanged))
	}
}

// TestCompute_Deterministic guards the brand promise: identical inputs render
// byte-identically regardless of input ordering or map iteration.
func TestCompute_Deterministic(t *testing.T) {
	mkBase := func() *facts.Snapshot {
		return snap([]facts.Fact{mod("a", "a.go", "b"), mod("b", "b.go")}, []facts.Insight{cycleInsight("x", "y")})
	}
	mkCur := func() *facts.Snapshot {
		return snap(
			[]facts.Fact{mod("b", "b.go", "a"), mod("a", "a.go", "b"), mod("c", "c.go")},
			[]facts.Insight{cycleInsight("a", "b"), cycleInsight("x", "y")},
		)
	}
	first := Compute(mkBase(), mkCur()).RenderSummary()
	second := Compute(mkBase(), mkCur()).RenderSummary()
	if first != second {
		t.Fatalf("non-deterministic render:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestFocused_NarrowsToArea(t *testing.T) {
	cur := snap([]facts.Fact{sym("PaymentService", "pay/svc.go", 1), sym("CartService", "cart/svc.go", 1)}, nil)
	d := Compute(snap(nil, nil), cur).Focused("payment")
	if len(d.FactsAdded) != 1 || d.FactsAdded[0].Name != "PaymentService" {
		t.Fatalf("focus did not narrow correctly: %+v", d.FactsAdded)
	}
}

// store fact helper for storage table-reference facts that share (kind,file,name).
func storageRef(name, file, operation, storageKind string, line int) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: name, File: file, Line: line, Repo: "r",
		Props: map[string]any{"operation": operation, "storage_kind": storageKind, "language": "go"}}
}

// TestCompute_NoChurnFromCollidingStorageRefs is the regression guard for the
// identity-collision bug found dogfooding on a real backend: the same table is
// referenced by several SQL operations in one file, so several storage facts
// share (kind, repo, file, name). They must be keyed apart by their operation —
// otherwise the on-disk baseline (one iteration order) and in-memory current
// (another) pick different colliding representatives and the diff falsely reports
// "changed" with zero real change. Here the two snapshots hold the SAME facts in
// REVERSED order; the diff must be empty.
func TestCompute_NoChurnFromCollidingStorageRefs(t *testing.T) {
	insert := storageRef("analytics_events", "db/analytics.go", "INSERT", "table_reference", 38)
	selectF := storageRef("analytics_events", "db/analytics.go", "SELECT", "table_reference", 127)

	base := snap([]facts.Fact{insert, selectF}, nil)
	cur := snap([]facts.Fact{selectF, insert}, nil) // reversed iteration order

	d := Compute(base, cur)
	if !d.Empty() {
		t.Fatalf("colliding storage refs churned the diff: %+v", d)
	}
}

// TestCompute_NoChurnFromCollidingRouteMethods is the same guard for routes: one
// path served under multiple methods shares (kind, repo, file, name).
func TestCompute_NoChurnFromCollidingRouteMethods(t *testing.T) {
	get := facts.Fact{Kind: facts.KindRoute, Name: "/users/{id}", File: "routes.go", Line: 10, Repo: "r",
		Props: map[string]any{"method": "GET"}}
	del := facts.Fact{Kind: facts.KindRoute, Name: "/users/{id}", File: "routes.go", Line: 11, Repo: "r",
		Props: map[string]any{"method": "DELETE"}}

	d := Compute(snap([]facts.Fact{get, del}, nil), snap([]facts.Fact{del, get}, nil))
	if !d.Empty() {
		t.Fatalf("colliding route methods churned the diff: %+v", d)
	}
}

// classSym builds a symbol fact for a class whose only distinguishing prop is
// its signature — the shape of a Swift type declared twice under mutually
// exclusive #if/#else branches. symbol_kind is "class" on both, so no prop
// discriminates them and only positional pairing can tell them apart.
func classSym(name, file, signature string, line int) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Line: line, Repo: "r",
		Props: map[string]any{"symbol_kind": facts.SymbolClass, "signature": signature}}
}

// depFact builds a dependency fact. Real dependency names embed the import
// target ("pkg -> react"), so two imports of the same target in one file are
// identical in name, props and relations — they differ only by line.
func depFact(from, target, file string, line int) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: from + " -> " + target, File: file, Line: line, Repo: "r",
		Props:     map[string]any{"source": "external"},
		Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}}}
}

// TestCompute_NoChurnFromCollidingSymbols is the regression guard for the
// phantom diff observed on my-golf-journal-ios: ArchitectureFitnessGateTests is
// declared twice in one file (#if/#else), so two symbol facts share
// (kind, repo, file, name) and differ only in line and signature. The on-disk
// baseline and the in-memory current iterate them in different orders, so
// last-write-wins picks a different representative on each side and the diff
// reports "changed" between byte-identical fact sets.
func TestCompute_NoChurnFromCollidingSymbols(t *testing.T) {
	macOS := classSym("Tests.ArchitectureFitnessGateTests", "Tests/A.swift", "func testFull() throws", 5)
	iOS := classSym("Tests.ArchitectureFitnessGateTests", "Tests/A.swift", "func testSkipped() throws", 101)

	// Same facts, different iteration order — as disk and memory produce them.
	d := Compute(snap([]facts.Fact{macOS, iOS}, nil), snap([]facts.Fact{iOS, macOS}, nil))
	if !d.Empty() {
		t.Fatalf("colliding symbols churned the diff: %+v", d)
	}
}

// TestCompute_CollidingSymbolRemovalIsDetected pins the silent-drop half of the
// bug: with last-write-wins, one of two colliding facts never reaches the map,
// so deleting it produces no diff entry at all.
func TestCompute_CollidingSymbolRemovalIsDetected(t *testing.T) {
	macOS := classSym("Tests.Gate", "Tests/A.swift", "func testFull() throws", 5)
	iOS := classSym("Tests.Gate", "Tests/A.swift", "func testSkipped() throws", 101)

	d := Compute(snap([]facts.Fact{macOS, iOS}, nil), snap([]facts.Fact{macOS}, nil))
	if len(d.FactsRemoved) != 1 {
		t.Fatalf("expected exactly 1 removed fact, got %d: %+v", len(d.FactsRemoved), d.FactsRemoved)
	}
	if got := d.FactsRemoved[0].Line; got != 101 {
		t.Fatalf("expected the line-101 declaration removed, got line %d", got)
	}
	if len(d.FactsAdded) != 0 || len(d.FactsChanged) != 0 {
		t.Fatalf("removal must not churn: added=%+v changed=%+v", d.FactsAdded, d.FactsChanged)
	}
}

// TestCompute_CollidingSymbolPropsChangeIsAChange asserts a real edit to one of
// two colliding facts still surfaces — the fix must not silence the diff.
func TestCompute_CollidingSymbolPropsChangeIsAChange(t *testing.T) {
	a := classSym("Tests.Gate", "Tests/A.swift", "func testFull() throws", 5)
	b := classSym("Tests.Gate", "Tests/A.swift", "func testSkipped() throws", 101)
	bEdited := classSym("Tests.Gate", "Tests/A.swift", "func testSkipped() async throws", 101)

	d := Compute(snap([]facts.Fact{a, b}, nil), snap([]facts.Fact{a, bEdited}, nil))
	if len(d.FactsChanged) != 1 {
		t.Fatalf("expected exactly 1 changed fact, got %d: %+v", len(d.FactsChanged), d.FactsChanged)
	}
	if len(d.FactsAdded) != 0 || len(d.FactsRemoved) != 0 {
		t.Fatalf("props change must not become add+remove: added=%+v removed=%+v", d.FactsAdded, d.FactsRemoved)
	}
	if got := d.FactsChanged[0].After.Line; got != 101 {
		t.Fatalf("wrong member paired: expected line 101, got %d", got)
	}
}

// TestCompute_NoChurnFromCollidingDependencies covers the kind the original bug
// report named. A dependency fact's name already embeds its import target, so
// two imports of the same target in one file are identical in name, props AND
// relations — discriminating on Relations[0].Target cannot separate them.
func TestCompute_NoChurnFromCollidingDependencies(t *testing.T) {
	first := depFact("src/app/hub", "react", "src/app/hub/page.tsx", 3)
	second := depFact("src/app/hub", "react", "src/app/hub/page.tsx", 5)

	d := Compute(snap([]facts.Fact{first, second}, nil), snap([]facts.Fact{second, first}, nil))
	if !d.Empty() {
		t.Fatalf("colliding dependencies churned the diff: %+v", d)
	}

	// And dropping one must be visible.
	d2 := Compute(snap([]facts.Fact{first, second}, nil), snap([]facts.Fact{first}, nil))
	if len(d2.FactsRemoved) != 1 {
		t.Fatalf("expected 1 removed dependency, got %d: %+v", len(d2.FactsRemoved), d2.FactsRemoved)
	}
}

// TestCompute_CollidingFactsAreDeterministic guards the output ordering: once
// colliding facts both survive, factKey alone no longer totally orders them.
func TestCompute_CollidingFactsAreDeterministic(t *testing.T) {
	base := snap(nil, nil)
	cur := snap([]facts.Fact{
		classSym("Tests.Gate", "Tests/A.swift", "func b() throws", 101),
		classSym("Tests.Gate", "Tests/A.swift", "func a() throws", 5),
	}, nil)

	want := Compute(base, cur).RenderCompact()
	for i := 0; i < 50; i++ {
		if got := Compute(base, cur).RenderCompact(); got != want {
			t.Fatalf("nondeterministic output on run %d:\n--- want ---\n%s\n--- got ---\n%s", i, want, got)
		}
	}
}

func TestRenderSummary_EmptyIsExplicit(t *testing.T) {
	d := Compute(snap(nil, nil), snap(nil, nil))
	out := d.RenderSummary()
	if !strings.Contains(out, "No architectural changes detected") {
		t.Fatalf("empty diff should state so explicitly, got:\n%s", out)
	}
}

// TestRenderSummary_NoPreexistingSection ensures the report never lists existing
// state — only regressions, improvements, and structural deltas.
func TestRenderSummary_OnlyDeltas(t *testing.T) {
	base := snap([]facts.Fact{mod("a", "a.go")}, []facts.Insight{cycleInsight("p", "q")})
	cur := snap([]facts.Fact{mod("a", "a.go"), mod("b", "b.go", "a")}, []facts.Insight{cycleInsight("p", "q"), cycleInsight("a", "b")})
	out := Compute(base, cur).RenderSummary()

	if !strings.Contains(out, "Regressions introduced (1)") {
		t.Fatalf("expected exactly 1 new regression in summary, got:\n%s", out)
	}
	// The pre-existing p→q cycle must not appear anywhere.
	if strings.Contains(out, "p → q") {
		t.Fatalf("pre-existing cycle leaked into report:\n%s", out)
	}
}

// --- new/53: the diff was blind to two whole classes of finding change ---

// TestCompute_FindingContentChangeIsReported is the reported bug. findingKey is
// Source + normalizeTitle(Title), and normalizeTitle strips EVERY number — so the
// dead-code rollup, whose title embeds a live count, kept the same key when its count
// moved by 1,959 symbols. The finding was in neither map's difference, so it landed in
// no bucket, Empty() was true, and diff_snapshot printed:
//
//	"No architectural changes detected. The change did not add, remove, or ALTER
//	 any facts, edges, or findings."
//
// while insights.json's sha256 had changed. This is the loop's own collateral-damage
// instrument, so a silent diff reads as "nothing broke".
//
// The key must NOT change — it is what stops a god-class fan-in ticking 11→13 from
// churning as a spurious resolve+introduce pair (see the test below). The fix is a
// bucket for "same identity, different content".
func TestCompute_FindingContentChangeIsReported(t *testing.T) {
	before := facts.Insight{Source: "dead-code", Confidence: 0.7,
		Title: "Additional dead-code candidates: 5621 more (see find_orphans)"}
	after := facts.Insight{Source: "dead-code", Confidence: 0.7,
		Title: "Additional dead-code candidates: 3662 more (see find_orphans)"}

	d := Compute(snap(nil, []facts.Insight{before}), snap(nil, []facts.Insight{after}))

	if d.Empty() {
		t.Fatal("a finding whose content changed was reported as no change at all")
	}
	if len(d.FindingsNew) != 0 || len(d.FindingsResolved) != 0 ||
		len(d.FindingsNewIncidental) != 0 || len(d.FindingsResolvedIncidental) != 0 {
		t.Errorf("identity must stay stable — the finding must not churn as resolve+new; got new=%d resolved=%d",
			len(d.FindingsNew)+len(d.FindingsNewIncidental),
			len(d.FindingsResolved)+len(d.FindingsResolvedIncidental))
	}
	if len(d.FindingsChanged) != 1 {
		t.Fatalf("FindingsChanged = %d, want 1", len(d.FindingsChanged))
	}
	if !strings.Contains(d.FindingsChanged[0].Before.Title, "5621") ||
		!strings.Contains(d.FindingsChanged[0].After.Title, "3662") {
		t.Errorf("change does not carry before/after: %+v", d.FindingsChanged[0])
	}
}

// TestCompute_CollidingFindingsAreAllVisible is the SECOND bug, which the report did
// not name: Compute keyed findings into a map[string]facts.Insight — last writer wins.
// On a large Android app, 78 layer violations share the identical title
// "Layer violation: di -> ui" (they are distinguished only by their evidence), so 77 of
// them were dropped from the diff map entirely and could appear or vanish in total
// silence.
//
// This is exactly fixed/10's factKey collision (which hid 172 facts) on the findings
// side, which never got the groupByKey treatment the facts side has. Evidence cannot go
// into the key — TestCompute_SummaryFindingDoesNotChurn forbids it — so the fix is to
// group and pair positionally, as groupByKey already does for facts.
func TestCompute_CollidingFindingsAreAllVisible(t *testing.T) {
	violation := func(mod string) facts.Insight {
		return facts.Insight{Source: "layers", Title: "Layer violation: di -> ui", Confidence: 0.8,
			Evidence: []facts.Evidence{{Fact: mod}}}
	}
	base := []facts.Insight{violation("a"), violation("b"), violation("c")}
	cur := []facts.Insight{violation("a"), violation("b")} // one violation fixed

	d := Compute(snap(nil, base), snap(nil, cur))

	if d.Empty() {
		t.Fatal("one of three identically-titled findings was removed and the diff saw nothing")
	}
	got := len(d.FindingsResolved) + len(d.FindingsResolvedIncidental)
	if got != 1 {
		t.Errorf("resolved = %d, want 1 — colliding findings are being dropped from the map", got)
	}
}

// TestCompute_IdenticalFindingsStayEmpty is the control for both fixes above: a
// snapshot pair with identical findings — including a colliding group — must still be
// Empty(). If this fails, the altered bucket is manufacturing churn.
func TestCompute_IdenticalFindingsStayEmpty(t *testing.T) {
	violation := func(mod string) facts.Insight {
		return facts.Insight{Source: "layers", Title: "Layer violation: di -> ui",
			Evidence: []facts.Evidence{{Fact: mod}}}
	}
	ins := []facts.Insight{
		violation("a"), violation("b"), violation("c"),
		{Source: "god-class", Title: "High fan-in symbol: Foo (11 dependents)", Confidence: 0.6},
	}
	// Same content, different slice order — the pairing must be order-stable.
	shuffled := []facts.Insight{ins[3], ins[2], ins[0], ins[1]}

	if d := Compute(snap(nil, ins), snap(nil, shuffled)); !d.Empty() {
		t.Fatalf("identical finding sets reported a change: %+v", d)
	}
}

// TestCompareMeta_StaleBaselineWarns is the second hazard new/53 reports, and it is
// real: a `pinned` baseline persists on disk indefinitely (that is advertised as a
// feature), and BaselineGeneratedAt is PRINTED but never COMPARED. An 11-day-old
// baseline — same enola version, same extractors, so Comparable:true and zero warnings
// — produced a confident 24-regression / 215-improvement report, dominated by repo
// drift, for a change that touched zero facts.
//
// The delta looks authoritative and the only signal is a timestamp on line 3 that the
// reader has to diff in their head.
// Two builds reporting the SAME enola version can still extract differently, because
// version.Version is the constant "dev" until a release sets it — which makes every locally
// built binary indistinguishable from every other by version alone.
//
// Nothing else in the receipt moves either: same config, same globs, same extractor set. So
// before ExtractorVersion was compared, a change to an EXTRACTOR was graded as a change to
// the CODE, comparably and with no caveat. That is not hypothetical: enola's own fix for a
// fabricated-fact bug removed 21 facts, and the gate reported it as a confident deletion.
func TestCompareMeta_SameVersionDifferentExtractionIsNotComparable(t *testing.T) {
	meta := func(extractorVersion string) facts.SnapshotMeta {
		return facts.SnapshotMeta{
			RepoPath: "/repo", EnolaVersion: "dev", ExtractorVersion: extractorVersion,
			GeneratedAt: "2026-08-03T10:00:00Z", Extractors: []string{"go"},
		}
	}

	c := compareMeta(meta("v146"), meta("v147"))
	if c.Comparable {
		t.Error("a build whose extractors changed must not be graded against one whose did not")
	}
	if !c.HasKind(WarnVersionMismatch) {
		t.Errorf("want a version-mismatch warning, got kinds %v", c.Kinds)
	}

	// The ordinary case stays quiet, or the warning is worthless.
	if same := compareMeta(meta("v147"), meta("v147")); !same.Comparable {
		t.Errorf("two identical builds must compare cleanly, got %v", same.Warnings)
	}

	// And a snapshot taken before the field existed must not be treated as a mismatch:
	// unknown is not the same as different, and inventing a caveat from absent data would
	// decline every diff against an older baseline.
	if old := compareMeta(meta(""), meta("v147")); !old.Comparable {
		t.Errorf("an older baseline with no recorded extractor version must not be rejected: %v", old.Warnings)
	}
}

func TestCompareMeta_StaleBaselineWarns(t *testing.T) {
	meta := func(ts string) facts.SnapshotMeta {
		return facts.SnapshotMeta{
			RepoPath: "/repo", EnolaVersion: "dev", GeneratedAt: ts,
			Extractors: []string{"go"},
		}
	}
	stale := compareMeta(meta("2026-07-02T10:00:00Z"), meta("2026-07-13T10:00:00Z"))
	var found bool
	for _, w := range stale.Warnings {
		if strings.Contains(w, "day") {
			found = true
		}
	}
	if !found {
		t.Errorf("an 11-day-old baseline produced no staleness warning: %v", stale.Warnings)
	}

	// Same-day baselines are the normal case and must stay quiet.
	fresh := compareMeta(meta("2026-07-13T09:00:00Z"), meta("2026-07-13T10:00:00Z"))
	for _, w := range fresh.Warnings {
		if strings.Contains(w, "day") {
			t.Errorf("a one-hour-old baseline warned about staleness: %q", w)
		}
	}
}

// TestCompareMeta_BaselineNewerThanCurrentWarns covers the inverted pair, which used to
// be silent: baselineAgeDays returns ok=false for a non-positive gap, sharing that
// signal with "no timestamp recorded", so a baseline NEWER than the snapshot being
// compared produced zero warnings. That is the sharpest form of a stale current
// snapshot — the provenance line renders backwards ("Baseline <newer> → current
// <older>") and nothing says so.
func TestCompareMeta_BaselineNewerThanCurrentWarns(t *testing.T) {
	meta := func(ts string) facts.SnapshotMeta {
		return facts.SnapshotMeta{
			RepoPath: "/repo", EnolaVersion: "dev", GeneratedAt: ts,
			Extractors: []string{"go"},
		}
	}
	// Baseline is 13 minutes NEWER than "current".
	inverted := compareMeta(meta("2026-07-30T06:30:00Z"), meta("2026-07-30T06:17:24Z"))
	var found bool
	for _, w := range inverted.Warnings {
		if strings.Contains(w, "newer") {
			found = true
		}
	}
	if !found {
		t.Errorf("a baseline newer than the current snapshot produced no warning: %v", inverted.Warnings)
	}
	if inverted.Comparable {
		t.Error("an inverted baseline/current pair must not be reported as comparable")
	}

	// A missing timestamp is a DIFFERENT condition and keeps its own softer note; it
	// must not start claiming the pair is inverted.
	missing := compareMeta(meta(""), meta("2026-07-30T06:17:24Z"))
	for _, w := range missing.Warnings {
		if strings.Contains(w, "newer") {
			t.Errorf("a missing baseline timestamp was reported as an inverted pair: %q", w)
		}
	}
}

// TestCompareMeta_RelocatedBaselineIsComparable is the portable-baseline guarantee at the
// comparability boundary: a baseline pinned on a CI runner and restored on a workstation
// must GRADE, not decline.
//
// Comparing absolute RepoPath meant a downloaded baseline artifact always tripped
// "different repositories" — which pkg/check treats as blocking — so the CI story could
// not work at all, however correct the delta underneath it was.
func TestCompareMeta_RelocatedBaselineIsComparable(t *testing.T) {
	at := func(path, remote string) facts.SnapshotMeta {
		return facts.SnapshotMeta{
			RepoPath: path, EnolaVersion: "dev", GeneratedAt: "2026-07-30T06:17:24Z",
			Extractors: []string{"go"},
			Git:        &facts.GitInfo{Remote: remote},
		}
	}

	ci := at("/home/runner/work/app/app", "https://github.com/org/app.git")
	local := at("/Users/dev/src/app", "git@github.com:org/app.git")

	got := compareMeta(ci, local)
	if got.HasKind(WarnDifferentRepo) {
		t.Errorf("a relocated baseline of the same repo was reported as a different repository: %v", got.Warnings)
	}
	if !got.Comparable {
		t.Errorf("expected comparable, got warnings: %v", got.Warnings)
	}

	// The guard must still fire for a genuinely different repository, or it would be
	// worth nothing: the gate would happily diff two unrelated graphs.
	other := at("/home/runner/work/app/app", "https://github.com/org/unrelated.git")
	mismatch := compareMeta(other, local)
	if !mismatch.HasKind(WarnDifferentRepo) {
		t.Errorf("genuinely different repositories were not flagged: %v", mismatch.Warnings)
	}
	// And the message must name the signal AND both paths, so the remedy is obvious.
	joined := strings.Join(mismatch.Warnings, " ")
	for _, want := range []string{"remote", "github.com/org/unrelated", "github.com/org/app", "/Users/dev/src/app"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mismatch warning omitted %q: %v", want, mismatch.Warnings)
		}
	}
}

// TestCompareMeta_SameSecondIsNotInverted — GeneratedAt is RFC3339, i.e. second
// resolution, so a baseline pinned and then diffed within the same second produces a
// ZERO gap. Zero is simultaneous, not inverted.
//
// It mattered as soon as a caller acted on the verdict instead of only printing it: the
// `enola check` gate consumes WarnInvertedPair as a usage error, so treating a zero gap
// as inverted made a no-op check on an untouched repository report "the current snapshot
// does not contain your change" and exit non-zero.
func TestCompareMeta_SameSecondIsNotInverted(t *testing.T) {
	meta := func(ts string) facts.SnapshotMeta {
		return facts.SnapshotMeta{
			RepoPath: "/repo", EnolaVersion: "dev", GeneratedAt: ts,
			Extractors: []string{"go"},
		}
	}
	const ts = "2026-07-30T06:17:24Z"
	same := compareMeta(meta(ts), meta(ts))

	if same.HasKind(WarnInvertedPair) {
		t.Errorf("identical timestamps were classified as an inverted pair: %v", same.Warnings)
	}
	if !same.Comparable {
		t.Errorf("identical timestamps must be comparable, got warnings: %v", same.Warnings)
	}
}

// A caveat that fires on most rows is one readers learn to skip.
//
// Comparability spans a spectrum: at one end "these are different repositories", at the
// other "these two snapshots are four days apart". Treating the whole spectrum as
// "incomparable" was fine for a pinned baseline and wrong for a TIMELINE, where revisions
// months apart are the normal shape — sampling a repository by release tag marked 57 of 80
// revisions suspect purely on elapsed time, drowning the three that meant a rebuild.
func TestComparability_InvalidatesDeltaSeparatesRebuildsFromElapsedTime(t *testing.T) {
	for kind, want := range map[WarningKind]bool{
		WarnDifferentRepo:   true,
		WarnVersionMismatch: true,
		WarnExtractorSet:    true,
		WarnIgnoreGlobs:     true,
		WarnInvertedPair:    true,
		WarnUnclassified:    true,
		// Advisory: the fact delta stands, it just also contains the repo's own drift or
		// a finding misattribution.
		WarnStaleBaseline: false,
		WarnPreReceipt:    false,
		WarnExplainerSet:  false,
		WarnOtherSession:  false,
	} {
		c := Comparability{Kinds: []WarningKind{kind}}
		if got := c.InvalidatesDelta(); got != want {
			t.Errorf("%s: InvalidatesDelta() = %v, want %v", kind, got, want)
		}
	}

	if (Comparability{Comparable: true}).InvalidatesDelta() {
		t.Error("a clean comparison invalidates nothing")
	}
	// A mixture: one blocking kind is enough.
	mixed := Comparability{Kinds: []WarningKind{WarnStaleBaseline, WarnExtractorSet}}
	if !mixed.InvalidatesDelta() {
		t.Error("a real mismatch alongside an advisory one must still invalidate")
	}
}

// A worktree, a second clone, a CI job that checks the base out beside the head: the
// same repository under two directory names. SameRepo waves those through on the shared
// remote — correctly, they ARE one repository — but every fact key embeds the repo
// label, so if the labels differ nothing matches and the delta is the whole graph added
// and removed. Findings survive it (keyed by title), which is what let a fabricated
// delta carry a plausible verdict on top of it.
func TestCompareMeta_SameRepoDifferentLabelIsBlocking(t *testing.T) {
	side := func(label, path string) facts.SnapshotMeta {
		return facts.SnapshotMeta{
			RepoPath:  path,
			RepoLabel: label,
			Git:       &facts.GitInfo{Remote: "github.com/acme/demo"},
		}
	}

	c := compareMeta(side("base", "/tmp/wt/base"), side("demo", "/work/demo"))
	if !c.HasKind(WarnRepoLabel) {
		t.Fatalf("a label mismatch must be reported, got kinds %v", c.Kinds)
	}
	if !c.InvalidatesDelta() {
		t.Error("a delta in which no fact can match must not be graded")
	}
	if c.HasKind(WarnDifferentRepo) {
		t.Error("it is the SAME repository — reporting a different one sends the reader after the wrong remedy")
	}

	// Matching labels: nothing to say, whatever the paths are.
	if same := compareMeta(side("demo", "/tmp/wt/base"), side("demo", "/work/demo")); same.HasKind(WarnRepoLabel) {
		t.Errorf("identical labels must not warn: %v", same.Warnings)
	}

	// A baseline predating the recorded label is read under the rule that tagged it —
	// the checkout directory name — rather than under today's.
	old := facts.SnapshotMeta{RepoPath: "/tmp/wt/demo", Git: &facts.GitInfo{Remote: "github.com/acme/demo"}}
	if c := compareMeta(old, side("demo", "/work/demo")); c.HasKind(WarnRepoLabel) {
		t.Errorf("a pre-label baseline whose directory matched must still compare: %v", c.Warnings)
	}
}

// With the changed files known, a newly declared rule's breach is new only
// when its witness sits in one of them, whatever facts happened to differ
// between the snapshots; and each bucketed finding says why.
func TestCompute_ChangedFilesDecideWhatANewRuleTouched(t *testing.T) {
	component := componentFact("errors", map[string]string{"superclass": "StandardError"})
	form := map[string]any{"require_name": "errors", "pattern": "*Error"}
	standing := constraintInsight("Constraint errors-are-recognisable violated: Failed does not match *Error", "Failed")
	standing.Evidence[0].File = "lib/failed.rb"
	made := constraintInsight("Constraint errors-are-recognisable violated: Broken does not match *Error", "Broken")
	made.Evidence[0].File = "lib/broken.rb"
	base := snap([]facts.Fact{component, errorClass("Failed", "StandardError")}, nil)
	cur := snap([]facts.Fact{component, ruleFact("errors-are-recognisable", form),
		errorClass("Failed", "StandardError"), errorClass("Broken", "StandardError")},
		[]facts.Insight{standing, made})
	d := ComputeChanged(base, cur, []string{"lib/broken.rb", "enola/constraints/errors.yaml"})
	if len(d.FindingsDeclared) != 1 || d.FindingsDeclared[0].Title != standing.Title {
		t.Fatalf("FindingsDeclared = %+v, want the breach in a file the change left alone", d.FindingsDeclared)
	}
	if len(d.FindingsNew) != 1 || d.FindingsNew[0].Title != made.Title {
		t.Fatalf("FindingsNew = %+v, want the breach in a changed file", d.FindingsNew)
	}
	last := d.FindingsNew[0].Evidence[len(d.FindingsNew[0].Evidence)-1]
	if last.Fact != "classified: new" || last.Detail != "the rule is newly declared and the witness was touched by the changed file lib/broken.rb" {
		t.Fatalf("the bucketing reason must be on the finding, got %+v", last)
	}
	d = ComputeChanged(base, cur, []string{"enola/constraints/errors.yaml"})
	if len(d.FindingsDeclared) != 2 || len(d.FindingsNew) != 0 {
		t.Fatalf("with only the declaration changed every breach is declared, got declared=%d new=%d", len(d.FindingsDeclared), len(d.FindingsNew))
	}
}

// TestMetricsDoNotDisturbTheDelta pins the constraint that shaped Insight.Metrics.
//
// The diff compares Title, Confidence, Description and Evidence by explicit
// enumeration. Metrics is deliberately outside that set: a baseline taken before
// the field existed must not report every finding as altered on the first run
// after an upgrade, and a coverage share drifting by one module is data for a
// tool rather than a structural change a gate should grade. The numbers stay in
// the description, which IS compared, so nothing the gate could see was lost.
func TestMetricsDoNotDisturbTheDelta(t *testing.T) {
	base := facts.Insight{
		Source: "layers", Title: "Architecture pattern: rails-mvc", Confidence: 0.8,
		Description: "Recognised rails-mvc from directory names: 5 of 5 ruby modules classified.",
	}
	withMetrics := base
	withMetrics.Metrics = map[string]any{"modules_scanned": 5, "layers_ordered": []string{"controller"}}

	if insightChanged(base, withMetrics) {
		t.Error("adding metrics must not read as an altered finding")
	}

	// And a metric moving on its own is still not a change...
	moved := withMetrics
	moved.Metrics = map[string]any{"modules_scanned": 6, "layers_ordered": []string{"controller", "model"}}
	if insightChanged(withMetrics, moved) {
		t.Error("a metric moving must not read as an altered finding on its own")
	}

	// ...while the same movement stated in the prose is, which is what keeps the
	// gate's sensitivity where it was.
	reworded := moved
	reworded.Description = "Recognised rails-mvc from directory names: 6 of 6 ruby modules classified."
	if !insightChanged(moved, reworded) {
		t.Error("a number changing in the description must still read as altered")
	}
}
