package constraints

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// atlasUnion is a Rust workspace whose pic crate depends on nothing, declared
// so, beside a Rails application, the way an append-mode union holds them:
// repo-labelled facts, one synthetic service node per repository, and the
// Ruby job's queue a topic fact named for the queue.
func atlasUnion(picCalls ...facts.Relation) *facts.Store {
	store := facts.NewStore()
	intents := []facts.Fact{
		componentIntent("pic", "crates/pic/src/**"),
		componentIntent("entry", "src/main.rs"),
		ruleIntentProps("pic-calls-nothing-else", map[string]any{
			"allow": "pic", "only": "pic", "via": "calls", "mode": "strict",
			"because": "pic has no dependencies, so the compiler refuses any call out of it"}),
	}
	for _, f := range intents {
		f.Repo = "atlas"
		f.File = "atlas/" + f.File
		store.Add(f)
	}
	store.Add(
		facts.Fact{Kind: facts.KindSymbol, Name: "crates/pic/src.Kind.atlas", File: "atlas/crates/pic/src/lib.rs", Repo: "atlas",
			Props: map[string]any{"language": "rust", "receiver": "Kind"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "crates/pic/src.write", File: "atlas/crates/pic/src/lib.rs", Repo: "atlas",
			Props: map[string]any{"language": "rust"}, Relations: picCalls},
		facts.Fact{Kind: facts.KindSymbol, Name: "src.main", File: "atlas/src/main.rs", Repo: "atlas",
			Props: map[string]any{"language": "rust"}},
		facts.Fact{Kind: facts.KindStorage, Name: "default", File: "jobs/app/jobs/report_job.rb", Repo: "jobs",
			Props: map[string]any{"language": "ruby", "storage_kind": "topic"}},
		facts.Fact{Kind: facts.KindModule, Name: "app/jobs", File: "jobs/app/jobs", Repo: "jobs"},
		facts.Fact{Kind: facts.KindSymbol, Name: "ReportJob#perform", File: "jobs/app/jobs/report_job.rb", Repo: "jobs",
			Props: map[string]any{"language": "ruby"}},
		facts.Fact{Kind: facts.KindService, Name: "atlas", Repo: "atlas", Props: map[string]any{"synthetic": "crossrepo"}},
		facts.Fact{Kind: facts.KindService, Name: "jobs", Repo: "jobs", Props: map[string]any{"synthetic": "crossrepo"}},
	)
	return store
}

func picBreaches(t *testing.T, store *facts.Store) []facts.Insight {
	t.Helper()
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	return withTitlePrefix(insights, "Strict constraint pic-calls-nothing-else violated")
}

// A derived Default gives a Rust type a default() no extractor measures, so
// the call's bare name matches nothing in its repository and, in the union,
// a Ruby queue named default in another. Nothing measured is what it reaches.
func TestExplain_AllowOnlyCallMatchingAnotherRepositorysQueueIsNoLanding(t *testing.T) {
	store := atlasUnion(facts.Relation{Kind: facts.RelCalls, Target: "default"})
	if got := picBreaches(t, store); len(got) != 0 {
		t.Fatalf("breaches = %+v, want none: the call's name meets a queue in another repository by coincidence", got)
	}
}

// A method named like its own repository matches that repository's service
// node, and a service node is no call's landing.
func TestExplain_AllowOnlyCallMatchingAServiceNodeIsNoLanding(t *testing.T) {
	store := atlasUnion(
		facts.Relation{Kind: facts.RelCalls, Target: "atlas"},
		facts.Relation{Kind: facts.RelCalls, Target: "jobs"},
	)
	if got := picBreaches(t, store); len(got) != 0 {
		t.Fatalf("breaches = %+v, want none: a service node stands for a repository, not for anything a call reaches", got)
	}
}

// The law still binds: a call onto a symbol the caller's own repository
// measures outside pic, and onto a symbol another repository declares, are
// both breaches, and a call within pic is not.
func TestExplain_AllowOnlyStillJudgesWhatACallReaches(t *testing.T) {
	store := atlasUnion(
		facts.Relation{Kind: facts.RelCalls, Target: "src.main"},
		facts.Relation{Kind: facts.RelCalls, Target: "ReportJob#perform"},
		facts.Relation{Kind: facts.RelCalls, Target: "crates/pic/src.Kind.atlas"},
	)
	got := picBreaches(t, store)
	want := map[string]bool{
		"Strict constraint pic-calls-nothing-else violated: crates/pic/src.write -> src.main via calls":          true,
		"Strict constraint pic-calls-nothing-else violated: crates/pic/src.write -> ReportJob#perform via calls": true,
	}
	if len(got) != len(want) {
		t.Fatalf("breaches = %+v, want exactly %d", got, len(want))
	}
	for _, in := range got {
		if !want[in.Title] {
			t.Errorf("unexpected breach %q", in.Title)
		}
	}
}

// A call that meets a same-named queue in its own repository is its
// language's to decide, as it was: the queue is a member of the repository the
// call was written in, and the rule judges it.
func TestExplain_AllowOnlyCallMatchingItsOwnRepositorysFactStillLands(t *testing.T) {
	store := atlasUnion(facts.Relation{Kind: facts.RelCalls, Target: "default"})
	store.Add(facts.Fact{Kind: facts.KindStorage, Name: "default", File: "atlas/src/queue.rs", Repo: "atlas",
		Props: map[string]any{"language": "rust", "storage_kind": "topic"}})
	if got := picBreaches(t, store); len(got) != 1 {
		t.Fatalf("breaches = %+v, want the one onto the repository's own default", got)
	}
}

// Service-to-service edges are what the service nodes are measured for, and an
// allow-only rule over them still judges a cross-repository dependency.
func TestExplain_AllowOnlyServiceToServiceStillLands(t *testing.T) {
	store := crossRepoStore()
	store.Add(
		serviceComponentIntent("frontend", "frontend", ""),
		serviceComponentIntent("billing-api", "billing", "api/**"),
		ruleIntentProps("frontend-reaches-the-api-only", map[string]any{
			"allow": "frontend", "only": "billing-api", "via": "depends_on",
			"because": "billing is reached through its api"}),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	got := withTitlePrefix(insights, "Constraint frontend-reaches-the-api-only violated")
	if len(got) != 1 || got[0].Title != "Constraint frontend-reaches-the-api-only violated: frontend -> billing via depends_on" {
		t.Fatalf("breaches = %+v, want the service-to-service dependency", got)
	}
}
