package command

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

func writeLintRepo(t *testing.T, inline string, constraintsFiles map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if inline != "" {
		if err := os.WriteFile(filepath.Join(dir, intent.RepoFileName), []byte(inline), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if len(constraintsFiles) > 0 {
		cdir := filepath.Join(dir, filepath.FromSlash(intent.ConstraintsDirName))
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range constraintsFiles {
			if err := os.WriteFile(filepath.Join(cdir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func lintCapturing(t *testing.T, repoPath string) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = prev }()

	count := testRunner().lintRepoDeclaration(nil, repoPath, "", facts.NewStore())

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), count
}

// TestLintStoreKeepsPageCompiledIntent pins the store `constraints lint`
// resolves components against. The snapshot's DECLARATION facts are replaced by
// the working tree's, so an edit to a constraints file lints without
// regenerating. The facts compiled from knowledge PAGES have no declaration
// file to be re-read from, and a governed_by component resolves through exactly
// those: while they were dropped with the rest, lint reported such a component
// as matching nothing while the explainer resolved its members and reported
// breaches against them.
func TestLintStoreKeepsPageCompiledIntent(t *testing.T) {
	pageFact := func(kind, name string, props map[string]any) facts.Fact {
		props["intent_kind"] = kind
		return facts.Fact{Kind: facts.KindIntent, Name: name, File: "policy/p.md", Props: props}
	}
	measured := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "legacy.Row", File: "legacy/cardstore.go", Repo: "shop"},
		pageFact("page", "page: policy/p.md", map[string]any{"source": "policy/p.md", "status": "superseded"}),
		pageFact("anchor", "anchor: shop legacy/cardstore.go", map[string]any{"source": "policy/p.md", "path": "legacy/cardstore.go"}),
		pageFact("relation", "policy/p.md supersedes policy/q.md", map[string]any{"source": "policy/p.md", "rel": "supersedes", "to": "policy/q.md"}),
		// A stale declaration fact from the snapshot: replaced by the working
		// tree's, never carried forward.
		{Kind: facts.KindIntent, Name: "component: stale", File: intent.RepoFileName,
			Props: map[string]any{"intent_kind": "component", "component": "stale", "match": "gone/**"}},
	}
	declared := []facts.Fact{
		{Kind: facts.KindIntent, Name: "component: retired", File: intent.RepoFileName,
			Props: map[string]any{"intent_kind": "component", "component": "retired", "match": "legacy/**"}},
	}

	store := lintStore(measured, declared, "shop")

	kinds := map[string]int{}
	for _, f := range store.ByKind(facts.KindIntent) {
		kinds[f.PropString("intent_kind")]++
	}
	for _, want := range []string{"page", "anchor", "relation"} {
		if kinds[want] != 1 {
			t.Errorf("intent_kind %q survived %d times, want 1: a governed_by component resolves through it", want, kinds[want])
		}
	}
	if kinds["component"] != 1 {
		t.Errorf("component facts = %d, want only the working tree's one", kinds["component"])
	}
	for _, f := range store.ByKind(facts.KindIntent) {
		if f.Name == "component: stale" {
			t.Error("the snapshot's own declaration must be replaced by the working tree's, not merged with it")
		}
		if f.PropString("component") == "retired" && f.Repo != "shop" {
			t.Errorf("declared fact repo = %q, want the snapshot's label stamped on it", f.Repo)
		}
	}
	if len(store.ByKind(facts.KindSymbol)) != 1 {
		t.Error("measured facts must be kept as they are")
	}
}

func TestConstraintsLintListsPerFileCounts(t *testing.T) {
	repo := writeLintRepo(t, `
components:
  - name: domain
    match: ["app/domain/**"]
rules:
  - id: domain-pure
    forbid: domain
    to: adapters
    via: imports
    because: the domain must not know its delivery mechanisms
`, map[string]string{
		"adapters.yaml": "components:\n  - name: adapters\n    match: [\"app/adapters/**\"]\n",
		"billing.yaml":  "components:\n  - name: billing\n    match: [\"app/billing/**\"]\nrules:\n  - id: billing-empty\n    forbid_fact: billing\n    because: nothing may live here yet\n",
	})
	out, count := lintCapturing(t, repo)
	if count != 0 {
		t.Fatalf("a valid split declaration must lint clean, got %d problems:\n%s", count, out)
	}
	inlineLine := filepath.Join(repo, intent.RepoFileName) + ": 1 component(s), 1 rule(s)"
	adaptersLine := intent.ConstraintsDirName + "/adapters.yaml: 1 component(s), 0 rule(s)"
	billingLine := intent.ConstraintsDirName + "/billing.yaml: 1 component(s), 1 rule(s)"
	for _, line := range []string{inlineLine, adaptersLine, billingLine} {
		if !strings.Contains(out, line) {
			t.Fatalf("per-file counts must list %q, got:\n%s", line, out)
		}
	}
	if strings.Index(out, inlineLine) > strings.Index(out, adaptersLine) || strings.Index(out, adaptersLine) > strings.Index(out, billingLine) {
		t.Fatalf("counts must list the inline file first, then constraints files in sorted order:\n%s", out)
	}
}

func TestConstraintsLintCitesTheDeclaringFile(t *testing.T) {
	repo := writeLintRepo(t, "components:\n  - name: domain\n    match: [\"app/domain/**\"]\n", map[string]string{
		"extra.yaml": "components:\n  - name: domain\n    match: [\"elsewhere/**\"]\n",
	})
	out, count := lintCapturing(t, repo)
	if count != 1 {
		t.Fatalf("a cross-source duplicate is one problem, got %d:\n%s", count, out)
	}
	if !strings.Contains(out, intent.ConstraintsDirName+"/extra.yaml") || !strings.Contains(out, intent.RepoFileName) {
		t.Fatalf("the finding must name both declaring files:\n%s", out)
	}
}

func TestConstraintsLintCountsExemptionsPerFile(t *testing.T) {
	repo := writeLintRepo(t, "", map[string]string{
		"adapters.yaml": "components:\n  - name: adapters\n    match: [\"app/adapters/**\"]\n",
		"billing.yaml": "components:\n  - name: billing\n    match: [\"app/billing/**\"]\nrules:\n" +
			"  - id: billing-empty\n    forbid_fact: billing\n    because: nothing may live here yet\n" +
			"    exempt:\n" +
			"      - witness: \"app/billing/ledger is measured in billing\"\n" +
			"        owner: alice\n        because: the ledger retires with the Q4 rewrite\n        since: \"2026-08-10\"\n" +
			"      - witness: \"app/billing/export is measured in billing\"\n" +
			"        owner: bob\n        because: the export path is grandfathered\n        since: \"2026-07-01\"\n",
	})
	out, count := lintCapturing(t, repo)
	if count != 0 {
		t.Fatalf("signed exemptions must lint clean, got %d problems:\n%s", count, out)
	}
	adaptersLine := intent.ConstraintsDirName + "/adapters.yaml: 1 component(s), 0 rule(s), 0 exemption(s)"
	billingLine := intent.ConstraintsDirName + "/billing.yaml: 1 component(s), 1 rule(s), 2 exemption(s)"
	for _, line := range []string{adaptersLine, billingLine} {
		if !strings.Contains(out, line) {
			t.Fatalf("per-file counts must list %q, got:\n%s", line, out)
		}
	}
}

func TestConstraintsLintRejectsUnsignedExemptionWithItsFile(t *testing.T) {
	repo := writeLintRepo(t, "", map[string]string{
		"billing.yaml": "components:\n  - name: billing\n    match: [\"app/billing/**\"]\nrules:\n" +
			"  - id: billing-empty\n    forbid_fact: billing\n    because: nothing may live here yet\n" +
			"    exempt:\n" +
			"      - witness: \"app/billing/ledger is measured in billing\"\n" +
			"        owner: alice\n",
	})
	out, count := lintCapturing(t, repo)
	if count != 2 {
		t.Fatalf("an exemption missing because and since is two problems, got %d:\n%s", count, out)
	}
	for _, want := range []string{intent.ConstraintsDirName + "/billing.yaml", "exempt[0]", "missing because", "YYYY-MM-DD"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the problems must contain %q:\n%s", want, out)
		}
	}
}

func writeLintRecipes(t *testing.T, repo string, recipeFiles map[string]string) {
	t.Helper()
	rdir := filepath.Join(repo, filepath.FromSlash(intent.RecipesDirName))
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range recipeFiles {
		if err := os.WriteFile(filepath.Join(rdir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const lintEventDrivenRecipe = "recipe: event-driven\n" +
	"roles:\n  - name: events\n  - name: handlers\n" +
	"rules:\n" +
	"  - id: events-consumed\n    require_edge: events\n    to: handlers\n    via: calls\n    direction: inbound\n    because: an event nobody consumes is dead weight\n"

const lintOrdersInstantiation = "use_recipe:\n" +
	"  - recipe: event-driven\n    as: orders-events\n" +
	"    bind:\n      events: { match: [\"app/events/orders/**\"] }\n      handlers: { match: [\"app/handlers/orders/**\"] }\n"

func TestConstraintsLintListsRecipesAndInstantiations(t *testing.T) {
	repo := writeLintRepo(t, "", map[string]string{"orders.yaml": lintOrdersInstantiation})
	writeLintRecipes(t, repo, map[string]string{"event-driven.yaml": lintEventDrivenRecipe})
	out, count := lintCapturing(t, repo)
	if count != 0 {
		t.Fatalf("a valid instantiation must lint clean, got %d problems:\n%s", count, out)
	}
	recipeLine := intent.RecipesDirName + "/event-driven.yaml: recipe event-driven — roles events, handlers, 1 rule"
	instanceLine := "use_recipe orders-events (recipe event-driven): binds events, handlers, expands 1 rule"
	for _, line := range []string{recipeLine, instanceLine} {
		if !strings.Contains(out, line) {
			t.Fatalf("the listing must contain %q, got:\n%s", line, out)
		}
	}
}

func TestConstraintsLintWarnsOnDeadRoleWithoutFailing(t *testing.T) {
	repo := writeLintRepo(t, "", map[string]string{"orders.yaml": lintOrdersInstantiation})
	deadRole := strings.Replace(lintEventDrivenRecipe, "roles:\n", "roles:\n  - name: audit\n", 1)
	writeLintRecipes(t, repo, map[string]string{"event-driven.yaml": deadRole})
	out, count := lintCapturing(t, repo)
	if count != 0 {
		t.Fatalf("a dead role is a warning, never a problem, got %d:\n%s", count, out)
	}
	if !strings.Contains(out, "role \"audit\" is referenced by no rule (dead role)") {
		t.Fatalf("the dead-role warning must be printed:\n%s", out)
	}
}

func TestConstraintsLintCountsInstantiationProblemsWithTheirFile(t *testing.T) {
	missingBinding := "use_recipe:\n" +
		"  - recipe: event-driven\n    as: orders-events\n" +
		"    bind:\n      events: { match: [\"app/events/orders/**\"] }\n"
	repo := writeLintRepo(t, "", map[string]string{"orders.yaml": missingBinding})
	writeLintRecipes(t, repo, map[string]string{"event-driven.yaml": lintEventDrivenRecipe})
	out, count := lintCapturing(t, repo)
	if count != 1 {
		t.Fatalf("an unbound referenced role is one problem, got %d:\n%s", count, out)
	}
	if !strings.Contains(out, intent.ConstraintsDirName+"/orders.yaml: use_recipe[0]") ||
		!strings.Contains(out, "binds no paths to it") {
		t.Fatalf("the problem must cite the instantiating file:\n%s", out)
	}
}
