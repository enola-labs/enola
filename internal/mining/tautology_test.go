package mining

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func symbolPropFact(name, file string, props map[string]any) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, File: file, Props: props}
}

func samePropStore() []facts.Fact {
	var ff []facts.Fact
	for i := 0; i < 12; i++ {
		ff = append(ff, storageFact(fmt.Sprintf("boiler_%02d", i), []string{"id", "created_at"}, nil))
	}
	for i := 0; i < 3; i++ {
		ff = append(ff, storageFact(fmt.Sprintf("bare_%02d", i), []string{"uuid"}, nil))
	}
	return ff
}

func TestMineSuppressesSamePropImplications(t *testing.T) {
	report := Mine(storeOf(samePropStore()), DefaultConfig())
	for _, c := range report.Candidates {
		if strings.Contains(c.Statement, "whose columns contains") {
			t.Errorf("a same-prop implication survived: %s", c.Statement)
		}
	}
	if got := suppressedFor(report, FamilyPropImplication).Tautological; got != 2 {
		t.Errorf("tautological count = %d, want 2 (id->created_at and created_at->id)", got)
	}
}

func TestMineSuppressesEchoedValueAcrossProps(t *testing.T) {
	var ff []facts.Fact
	for i := 0; i < 12; i++ {
		ff = append(ff, symbolPropFact(fmt.Sprintf("Gql%02d", i), fmt.Sprintf("src/gql%02d.rb", i),
			map[string]any{"framework": "graphql", "type": "graphql"}))
	}
	for i := 0; i < 3; i++ {
		ff = append(ff, symbolPropFact(fmt.Sprintf("Rest%02d", i), fmt.Sprintf("src/rest%02d.rb", i),
			map[string]any{"framework": "rest", "type": "soap"}))
	}
	report := Mine(storeOf(ff), DefaultConfig())
	for _, c := range report.Candidates {
		if strings.Contains(c.Statement, "graphql also have") {
			t.Errorf("an echoed-value implication survived: %s", c.Statement)
		}
	}
	if got := suppressedFor(report, FamilyPropImplication).Tautological; got != 2 {
		t.Errorf("tautological count = %d, want 2 (both echoed directions)", got)
	}
}

func TestMineKeepsInformativeImplicationsUnderSuppression(t *testing.T) {
	report := Mine(storeOf(companyFKStore()), DefaultConfig())
	findCandidate(t, report, FamilyPropImplication, "columns contains company_id also have fk_constraints containing company_id->companies")
}

func TestMineSuppressesReversedDuplicateImplications(t *testing.T) {
	var ff []facts.Fact
	for i := 0; i < 10; i++ {
		ff = append(ff, symbolPropFact(fmt.Sprintf("Both%02d", i), fmt.Sprintf("src/both%02d.rb", i),
			map[string]any{"p": "aaa", "q": "bbb"}))
	}
	for i := 0; i < 5; i++ {
		ff = append(ff, symbolPropFact(fmt.Sprintf("Neither%02d", i), fmt.Sprintf("src/neither%02d.rb", i),
			map[string]any{"p": "zzz", "q": "yyy"}))
	}
	report := Mine(storeOf(ff), DefaultConfig())
	var directions []Candidate
	for _, c := range report.Candidates {
		if strings.Contains(c.Statement, "aaa") && strings.Contains(c.Statement, "bbb") {
			directions = append(directions, c)
		}
	}
	if len(directions) != 1 {
		t.Fatalf("want exactly one direction of the pair, got %d:\n%s", len(directions), statements(report))
	}
	if !strings.Contains(directions[0].Statement, "whose p contains aaa") {
		t.Errorf("the kept direction should be the identity-first one, got: %s", directions[0].Statement)
	}
	if got := suppressedFor(report, FamilyPropImplication).Tautological; got != 1 {
		t.Errorf("tautological count = %d, want 1 (the reversed duplicate)", got)
	}
}

func pathMirroredNamingStore() []facts.Fact {
	var ff []facts.Fact
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("app/assets/f%02d.js", i)
		ff = append(ff, symbolPropFact(name, name, nil))
	}
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("app/javascript/g%02d.js", i)
		ff = append(ff, symbolPropFact(name, name, nil))
	}
	for i := 0; i < 10; i++ {
		ff = append(ff, symbolPropFact(fmt.Sprintf("Lib::A%02d", i), fmt.Sprintf("lib/x/a%02d.rb", i), nil))
	}
	return ff
}

func TestMineSuppressesConstructionSatisfiedNaming(t *testing.T) {
	report := Mine(storeOf(pathMirroredNamingStore()), DefaultConfig())
	for _, c := range report.Candidates {
		if c.Family != FamilyNaming {
			continue
		}
		if strings.Contains(c.Statement, "are named app/") {
			t.Errorf("a path-mirroring naming candidate survived: %s", c.Statement)
		}
	}
	findCandidate(t, report, FamilyNaming, "under lib/ are named Lib::*")
	if got := suppressedFor(report, FamilyNaming).Tautological; got < 3 {
		t.Errorf("tautological naming count = %d, want at least 3 (app/, app/assets/, app/javascript/)", got)
	}
}

func TestMineSuppressesSingleMemberNamingClusters(t *testing.T) {
	ff := []facts.Fact{
		symbolPropFact("Solo::Thing", "solo/dir/thing.rb", nil),
		symbolPropFact("Other::X", "other/dir/x.rb", nil),
	}
	cfg := DefaultConfig()
	cfg.MinSupport = 1
	report := Mine(storeOf(ff), cfg)
	for _, c := range report.Candidates {
		if c.Family == FamilyNaming {
			t.Errorf("a single-member naming cluster survived: %s", c.Statement)
		}
	}
	if got := suppressedFor(report, FamilyNaming).Tautological; got < 2 {
		t.Errorf("tautological naming count = %d, want at least 2", got)
	}
}

func TestMineIncludeTautologiesPrintsThem(t *testing.T) {
	cfg := DefaultConfig()
	cfg.IncludeTautologies = true
	report := Mine(storeOf(samePropStore()), cfg)
	findCandidate(t, report, FamilyPropImplication, "whose columns contains id also have columns containing created_at")
	for _, sc := range report.Suppressed {
		if sc.Tautological != 0 {
			t.Errorf("%s counted %d tautological under --include-tautologies, want 0", sc.Family, sc.Tautological)
		}
	}
}

func TestWriteTextCountsTautologicalSuppression(t *testing.T) {
	report := Mine(storeOf(samePropStore()), DefaultConfig())
	var text bytes.Buffer
	report.WriteText(&text, 0)
	if !strings.Contains(text.String(), "2 tautological candidate(s) suppressed") {
		t.Errorf("report text does not carry the tautology counter:\n%s", text.String())
	}
	if !strings.Contains(text.String(), "--include-tautologies") {
		t.Errorf("report text does not name the escape flag:\n%s", text.String())
	}
}

// A comment is prose. Twelve symbols whose comments share their words must not
// be read as twelve symbols obeying a rule.
func TestMineDoesNotMineTheDocProp(t *testing.T) {
	var ff []facts.Fact
	for i := 0; i < 12; i++ {
		ff = append(ff, symbolPropFact(fmt.Sprintf("Handler%02d", i), fmt.Sprintf("src/h%02d.rb", i),
			map[string]any{"framework": "rails", facts.PropDoc: "Handles the request and returns it."}))
	}
	report := Mine(storeOf(ff), DefaultConfig())
	for _, c := range report.Candidates {
		if strings.Contains(c.Statement, "doc") || strings.Contains(c.Statement, "Handles") {
			t.Errorf("a rule was mined out of a comment: %s", c.Statement)
		}
	}
}
