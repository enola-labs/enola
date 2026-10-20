package mining

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func storageFact(table string, columns, fks []string) facts.Fact {
	props := map[string]any{
		"storage_kind": "table",
		"table":        table,
		"columns":      strings.Join(columns, " "),
	}
	if len(fks) > 0 {
		props["fk_constraints"] = strings.Join(fks, " ")
	}
	return facts.Fact{Kind: facts.KindStorage, Name: table, File: "db/structure.sql", Props: props}
}

func companyFKStore() []facts.Fact {
	var ff []facts.Fact
	for i := 0; i < 20; i++ {
		table := fmt.Sprintf("scoped_%02d", i)
		ff = append(ff, storageFact(table,
			[]string{"id", "company_id", "created_at"},
			[]string{"company_id->companies"}))
	}
	ff = append(ff, storageFact("interest_applications", []string{"id", "company_id", "created_at"}, nil))
	ff = append(ff, storageFact("user_statuses", []string{"id", "company_id", "created_at"}, nil))
	for i := 0; i < 8; i++ {
		table := fmt.Sprintf("global_%02d", i)
		ff = append(ff, storageFact(table, []string{"id", "created_at"}, nil))
	}
	return ff
}

func storeOf(ff []facts.Fact) *facts.Store {
	store := facts.NewStore()
	store.Add(ff...)
	return store
}

func findCandidate(t *testing.T, report *Report, family, statementPart string) Candidate {
	t.Helper()
	for _, c := range report.Candidates {
		if c.Family == family && strings.Contains(c.Statement, statementPart) {
			return c
		}
	}
	t.Fatalf("no %s candidate whose statement contains %q; candidates:\n%s", family, statementPart, statements(report))
	return Candidate{}
}

func statements(report *Report) string {
	var lines []string
	for _, c := range report.Candidates {
		lines = append(lines, c.Family+": "+c.Statement)
	}
	return strings.Join(lines, "\n")
}

func TestMineRederivesCompanyFKWithNamedExceptions(t *testing.T) {
	report := Mine(storeOf(companyFKStore()), DefaultConfig())
	c := findCandidate(t, report, FamilyPropImplication, "columns contains company_id also have fk_constraints containing company_id->companies")
	if c.Numerator != 20 || c.Denominator != 22 {
		t.Errorf("regularity = %d/%d, want 20/22", c.Numerator, c.Denominator)
	}
	if len(c.Exceptions) != 2 {
		t.Fatalf("exceptions = %v, want exactly interest_applications and user_statuses", c.Exceptions)
	}
	if c.Exceptions[0].Name != "interest_applications" || c.Exceptions[1].Name != "user_statuses" {
		t.Errorf("exceptions = %v, want interest_applications then user_statuses", c.Exceptions)
	}
	if c.Exceptions[0].File != "db/structure.sql" {
		t.Errorf("exception file = %q, want db/structure.sql", c.Exceptions[0].File)
	}
	if !strings.Contains(c.Rule.Because, "interest_applications") || !strings.Contains(c.Rule.Because, "user_statuses") {
		t.Errorf("because does not name the exceptions: %q", c.Rule.Because)
	}
	if c.Rule.WhenPropContains == nil || c.Rule.WhenPropContains.Value != "company_id" {
		t.Errorf("when clause = %+v, want columns contains company_id", c.Rule.WhenPropContains)
	}
	if c.Rule.MustPropContain == nil || c.Rule.MustPropContain.Value != "company_id->companies" {
		t.Errorf("must clause = %+v, want fk_constraints contains company_id->companies", c.Rule.MustPropContain)
	}
}

func TestMineEmitsUnconditionalRegularity(t *testing.T) {
	report := Mine(storeOf(companyFKStore()), DefaultConfig())
	c := findCandidate(t, report, FamilyPropImplication, "have columns containing id")
	if c.Numerator != 30 || c.Denominator != 30 {
		t.Errorf("regularity = %d/%d, want 30/30", c.Numerator, c.Denominator)
	}
	if len(c.Exceptions) != 0 {
		t.Errorf("exceptions = %v, want none", c.Exceptions)
	}
	if c.Rule.WhenPropContains != nil {
		t.Errorf("unconditional rule carries a when clause: %+v", c.Rule.WhenPropContains)
	}
}

func TestMineSupportFloorSuppressesAndCounts(t *testing.T) {
	var ff []facts.Fact
	for i := 0; i < 5; i++ {
		ff = append(ff, storageFact(fmt.Sprintf("t%d", i), []string{"id", "company_id"}, []string{"company_id->companies"}))
	}
	report := Mine(storeOf(ff), DefaultConfig())
	if len(report.Candidates) != 0 {
		t.Errorf("candidates = %v, want none below the support floor", statements(report))
	}
	suppressed := suppressedFor(report, FamilyPropImplication)
	if suppressed.BelowSupportFloor == 0 {
		t.Error("support-floor suppression was silent — the count must be reported")
	}
}

func TestMineExceptionCeilingSuppressesAndCounts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxExceptions = 1
	report := Mine(storeOf(companyFKStore()), cfg)
	for _, c := range report.Candidates {
		if strings.Contains(c.Statement, "columns contains company_id also have fk_constraints") {
			t.Errorf("candidate with 2 exceptions survived a ceiling of 1: %s", c.Statement)
		}
	}
	suppressed := suppressedFor(report, FamilyPropImplication)
	if suppressed.OverExceptionCeiling == 0 {
		t.Error("exception-ceiling suppression was silent — the count must be reported")
	}
}

func suppressedFor(report *Report, family string) SuppressedCount {
	for _, sc := range report.Suppressed {
		if sc.Family == family {
			return sc
		}
	}
	return SuppressedCount{}
}

func TestMineEmptyStoreProducesEmptyReport(t *testing.T) {
	report := Mine(facts.NewStore(), DefaultConfig())
	if len(report.Candidates) != 0 {
		t.Errorf("candidates = %v, want none", statements(report))
	}
	var text bytes.Buffer
	report.WriteText(&text, 0)
	if !strings.Contains(text.String(), "Mined 0 candidate constraints") {
		t.Errorf("empty report text does not say so:\n%s", text.String())
	}
}

func TestRatioZeroDenominator(t *testing.T) {
	if got := ratio(0, 0); got != 0 {
		t.Errorf("ratio(0,0) = %v, want 0", got)
	}
}

func TestMineIsDeterministicAcrossInsertionOrders(t *testing.T) {
	base := companyFKStore()
	base = append(base, namingWorld()...)
	base = append(base, edgeWorld()...)
	base = append(base, methodWorld()...)

	renderAll := func(ff []facts.Fact) (string, string) {
		report := Mine(storeOf(ff), DefaultConfig())
		var text, jsonl bytes.Buffer
		report.WriteText(&text, 0)
		if err := report.WriteJSONL(&jsonl); err != nil {
			t.Fatal(err)
		}
		return text.String(), jsonl.String()
	}

	wantText, wantJSONL := renderAll(base)
	for seed := int64(1); seed <= 3; seed++ {
		shuffled := append([]facts.Fact{}, base...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		gotText, gotJSONL := renderAll(shuffled)
		if gotText != wantText {
			t.Fatalf("seed %d: text report differs across insertion orders", seed)
		}
		if gotJSONL != wantJSONL {
			t.Fatalf("seed %d: JSONL artifact differs across insertion orders", seed)
		}
	}
}

func TestMineRanksByConfidenceTimesSupport(t *testing.T) {
	report := Mine(storeOf(companyFKStore()), DefaultConfig())
	for i := 1; i < len(report.Candidates); i++ {
		if report.Candidates[i].Score() > report.Candidates[i-1].Score() {
			t.Fatalf("rank %d (%.3f) outranks rank %d (%.3f)", i+1, report.Candidates[i].Score(), i, report.Candidates[i-1].Score())
		}
	}
}

// Two files declare `app/verbs.run`, and a dependency fact carries the file its
// target resolved into. Mining read the name's first file and attributed the
// edge to it.
func TestCollectEdgesReadsACarriedTargetFile(t *testing.T) {
	sym := func(name, file string) facts.Fact {
		return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file}
	}
	call := func(name, targetFile string) facts.Fact {
		return facts.Fact{Kind: facts.KindDependency, Name: name,
			File: "app/core/drive.rs", Props: map[string]any{facts.PropTargetFile: targetFile},
			Relations: []facts.Relation{{Kind: facts.RelCalls, Target: "app/verbs.run"}}}
	}
	s := newScope([]facts.Fact{
		sym("app/core.drive", "app/core/drive.rs"),
		sym("app/verbs.run", "app/aaa/run.rs"),
		sym("app/verbs.run", "app/verbs/validate.rs"),
		call("hit", "app/verbs/validate.rs"),
		call("miss", "app/zzz/missing.rs"),
	}, "", Config{}, nil)
	var got []string
	for _, e := range s.collectEdges() {
		got = append(got, e.source+" -> "+e.tgtCluster)
	}
	want := []string{"hit -> app/verbs"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("edges %v, want %v: the carried file places the target, and a file that does not declare it places nothing", got, want)
	}
}
