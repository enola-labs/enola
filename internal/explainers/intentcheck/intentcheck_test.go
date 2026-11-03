package intentcheck

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func consumesFact(repo, target, via string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Repo: repo, Name: "consumes " + target + " via " + via,
		Props: map[string]any{"intent_kind": "consumes", "target": target, "via": via, "source": "enola-intent.yaml"}}
}

func edgeFact(consumer, provider string, vias ...string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Repo: consumer, Name: consumer + " -> " + provider,
		Props: map[string]any{"type": "cross_repo", "via": vias}}
}

func repoMarker(repo string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Repo: repo, Name: repo + "/src"}
}

func explain(t *testing.T, ff ...facts.Fact) []facts.Insight {
	t.Helper()
	store := facts.NewStore()
	store.Add(ff...)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	return insights
}

func titles(insights []facts.Insight) string {
	var out []string
	for _, i := range insights {
		out = append(out, i.Title)
	}
	return strings.Join(out, " | ")
}

func TestIntentCheck_DeclaredAndMeasuredAgreeIsSilent(t *testing.T) {
	got := explain(t,
		repoMarker("app"), repoMarker("api"),
		consumesFact("app", "api", "http-client"),
		edgeFact("app", "api", "http-client"),
	)
	if len(got) != 0 {
		t.Fatalf("agreement produced findings: %s", titles(got))
	}
}

func TestIntentCheck_UnexpectedSeamIsProofClass(t *testing.T) {
	got := explain(t,
		repoMarker("app"), repoMarker("api"), repoMarker("other"),
		consumesFact("app", "api", "http-client"),
		edgeFact("app", "api", "http-client"),
		edgeFact("app", "other", "http-client"),
	)
	if len(got) != 1 || !strings.Contains(got[0].Title, "Unexpected seam: app -> other") {
		t.Fatalf("findings = %s", titles(got))
	}
	if got[0].Confidence != 1.0 {
		t.Fatalf("unexpected seam is set difference — confidence %v, want 1.0", got[0].Confidence)
	}
}

func TestIntentCheck_MisViaNamedSeparately(t *testing.T) {
	got := explain(t,
		repoMarker("app"), repoMarker("api"),
		consumesFact("app", "api", "graphql"),
		edgeFact("app", "api", "http-client"),
	)
	if len(got) != 1 || !strings.Contains(got[0].Title, "mis-via") || got[0].Confidence != 1.0 {
		t.Fatalf("findings = %s (conf %v)", titles(got), got[0].Confidence)
	}
}

func TestIntentCheck_MissingSeamCappedAndSkippedWithoutCounterparty(t *testing.T) {
	got := explain(t,
		repoMarker("app"), repoMarker("api"),
		consumesFact("app", "api", "kafka"),
		consumesFact("app", "ghost", "http-client"),
	)
	if len(got) != 1 || !strings.Contains(got[0].Title, "Missing intended seam: app -> api") {
		t.Fatalf("findings = %s — the ghost target is absent from the graph and must be skipped, not failed", titles(got))
	}
	if got[0].Confidence != missingSeamConfidence {
		t.Fatalf("missing seam confidence %v, want capped %v", got[0].Confidence, missingSeamConfidence)
	}
}

func TestIntentCheck_UndeclaredRepoIsUnasked(t *testing.T) {
	got := explain(t,
		repoMarker("app"), repoMarker("api"),
		edgeFact("app", "api", "http-client"),
	)
	if len(got) != 0 {
		t.Fatalf("a repo with no declarations was verdicted: %s", titles(got))
	}
}

func TestIntentCheck_OverrideNoticeSurfaced(t *testing.T) {
	f := consumesFact("app", "api", "http-client")
	f.Props["overridden"] = true
	f.Props["source"] = "cluster-config"
	got := explain(t, repoMarker("app"), repoMarker("api"), f, edgeFact("app", "api", "http-client"))
	if len(got) != 1 || !strings.Contains(got[0].Title, "Intent override") {
		t.Fatalf("findings = %s", titles(got))
	}
}

func pageFact(repo, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Repo: repo, Name: "page: " + file, File: file,
		Props: map[string]any{"intent_kind": "page", "page_type": "decision", "source": file}}
}

func relationFact(repo, from, rel, to string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Repo: repo, Name: from + " " + rel + " " + to, File: from,
		Props: map[string]any{"intent_kind": "relation", "rel": rel, "to": to, "source": from}}
}

// TestIntentCheck_DanglingRelationCapped pins the knowledge-graph verdict: a
// relation edge to a page absent from the compiled set surfaces at capped
// confidence (the target may be deleted or merely not opted in), and an edge
// whose target compiles is silence.
func TestIntentCheck_DanglingRelationCapped(t *testing.T) {
	insights := explain(t,
		pageFact("wiki", "wiki/a/adrs/one.md"),
		pageFact("wiki", "wiki/a/adrs/two.md"),
		relationFact("wiki", "wiki/a/adrs/one.md", "depends-on", "wiki/a/adrs/two.md"),
		relationFact("wiki", "wiki/a/adrs/two.md", "supersedes", "wiki/a/adrs/gone.md"),
	)
	if len(insights) != 1 || !strings.Contains(insights[0].Title, "Dangling knowledge relation") {
		t.Fatalf("exactly the broken edge must surface, got %s", titles(insights))
	}
	if insights[0].Confidence != danglingRelationConfidence {
		t.Fatalf("a dangling relation is an estimate, got confidence %v", insights[0].Confidence)
	}
}

// TestIntentCheck_PageOnlyGraphStillVerdicts pins the guard fix: a store
// carrying only page/relation intent (no consumes declarations) must not
// early-return before the relation verdicts run.
func TestIntentCheck_PageOnlyGraphStillVerdicts(t *testing.T) {
	insights := explain(t,
		pageFact("wiki", "wiki/a/prds/spec.md"),
		relationFact("wiki", "wiki/a/prds/spec.md", "part-of", "wiki/a/epics/missing.md"),
	)
	if len(insights) != 1 {
		t.Fatalf("page-only intent must still verdict relations, got %s", titles(insights))
	}
}

// TestIntentCheck_RelationJoinsAcrossLabelPrefix pins the union-store shape:
// a multi-repo snapshot prefixes fact files with the repo label, while a
// relation's `to` stays repo-relative — the join must trim the label or every
// edge in a cluster graph dangles.
func TestIntentCheck_RelationJoinsAcrossLabelPrefix(t *testing.T) {
	target := pageFact("wiki", "wiki/wiki/a/adrs/two.md")
	target.RepoPrefixed = true
	rel := relationFact("wiki", "wiki/wiki/a/adrs/one.md", "depends-on", "wiki/a/adrs/two.md")
	rel.RepoPrefixed = true
	insights := explain(t, target, rel)
	if len(insights) != 0 {
		t.Fatalf("a label-prefixed page must satisfy its repo-relative relation, got %s", titles(insights))
	}
}

func fileFact(repo, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Repo: repo, File: file, Name: file + ":fn"}
}

func anchorFact(page, owner, path string) facts.Fact {
	return facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: page,
		Name: "anchor: " + owner + " " + path,
		Props: map[string]any{"intent_kind": "anchor", "intent_owner": owner,
			"path": path, "source": page}}
}

// TestIntentCheck_AnchorJoinsOrDangles pins the page-to-code join: an anchor
// whose path a measured fact touches (exactly, under a directory prefix, or
// behind a repo-label file prefix) is silence; a path nothing touches is a
// capped dangling finding; a repo absent from the graph is unasked.
func TestIntentCheck_AnchorJoinsOrDangles(t *testing.T) {
	got := explain(t,
		fileFact("backend", "app/services/formatter.rb"),
		fileFact("backend", "backend/app/jobs/x_job.rb"),
		anchorFact("wiki/adrs/fmt.md", "backend", "app/services/formatter.rb"),
		anchorFact("wiki/adrs/jobs.md", "backend", "app/jobs"),
		anchorFact("wiki/adrs/gone.md", "backend", "app/services/gone.rb"),
		anchorFact("wiki/adrs/mob.md", "mobile", "src/App.tsx"),
	)
	// Two: the one path this graph could prove untouched, and the notice that
	// the page anchoring a repo this graph never loaded was not checked at all.
	if len(got) != 2 {
		t.Fatalf("want the dangling anchor and the unasked notice, got: %s", titles(got))
	}
	var dangling, unasked facts.Insight
	for _, i := range got {
		switch {
		case strings.HasPrefix(i.Title, "Dangling code anchor"):
			dangling = i
		case strings.HasPrefix(i.Title, "Anchors not checked"):
			unasked = i
		}
	}
	if !strings.Contains(dangling.Title, "gone.rb") || dangling.Confidence != danglingAnchorConfidence {
		t.Fatalf("dangling anchor must name the path at capped confidence, got %+v", dangling)
	}
	if !strings.Contains(unasked.Title, "wiki/adrs/mob.md") || !strings.Contains(unasked.Title, "mobile") {
		t.Fatalf("the notice must name the page and the repo it anchors, got %+v", unasked)
	}
	if unasked.Confidence != absentAnchorRepoConfidence {
		t.Fatalf("the notice rates a question nobody asked, got %v", unasked.Confidence)
	}
}

// TestIntentCheck_UnaskedAnchorsAreReportedNotSilent pins the fix for a skip
// that said nothing. An anchor into a repository this snapshot does not carry
// stays unasked — accusing its citations of going stale would verdict facts
// nobody measured — but the skip is now reported, because a snapshot of one
// repository takes its label from that repository's DIRECTORY name, so a clone
// or a checkout under another name switched the stale-citation check off with
// no output at all while governed_by and require_governed kept resolving the
// same anchors.
func TestIntentCheck_UnaskedAnchorsAreReportedNotSilent(t *testing.T) {
	got := explain(t,
		fileFact("checkout", "app/models/order.rb"),
		anchorFact("policy/pci.md", "storefront", "app/models/order.rb"),
		anchorFact("policy/pci.md", "storefront", "app/models/card.rb"),
		anchorFact("policy/gdpr.md", "storefront", "app/models/person.rb"),
	)
	if len(got) != 2 {
		t.Fatalf("want one notice per page, not per anchor, got: %s", titles(got))
	}
	if !strings.Contains(got[0].Title, "policy/gdpr.md") || !strings.Contains(got[1].Title, "policy/pci.md") {
		t.Fatalf("notices must be ordered by page, got: %s", titles(got))
	}
	if !strings.Contains(got[1].Description, "2 anchor(s)") {
		t.Fatalf("the notice must count the anchors it covers, got: %q", got[1].Description)
	}
	if !strings.Contains(got[1].Description, "carries checkout") {
		t.Fatalf("the notice must name the labels this snapshot does carry, got: %q", got[1].Description)
	}
	for _, i := range got {
		if i.Confidence != absentAnchorRepoConfidence {
			t.Errorf("%q rates %v, want the unasked advisory confidence", i.Title, i.Confidence)
		}
	}

	// The counterparty is loaded: the anchors are checked as usual and the
	// notice does not fire.
	got = explain(t,
		fileFact("storefront", "app/models/order.rb"),
		anchorFact("policy/pci.md", "storefront", "app/models/order.rb"),
	)
	if len(got) != 0 {
		t.Fatalf("a resolved anchor in a loaded repo is silence, got: %s", titles(got))
	}
}

// TestIntentCheck_ScopeNamesNeverVerdict pins the boundary: a page's scope
// and affects speak the wiki's own repo vocabulary, whose mapping to cluster
// labels lives on the wiki's side — names the graph never measured must NOT
// fire (a 60-repo regression showed them firing on correct pages, e.g. a
// page scoped "ledger" over a cluster labeled "ledger-service").
func TestIntentCheck_ScopeNamesNeverVerdict(t *testing.T) {
	page := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/x.md",
		Name: "page: wiki/adrs/x.md",
		Props: map[string]any{"intent_kind": "page", "page_type": "decision",
			"scope":   []string{"backend", "ledger"},
			"affects": []any{"mobile"},
			"source":  "wiki/adrs/x.md"}}
	got := explain(t, repoMarker("backend"), page)
	if len(got) != 0 {
		t.Fatalf("scope/affects names must not verdict against cluster labels, got: %s", titles(got))
	}
}

// TestIntentCheck_UnparseableAnchorUnasked pins the unasked-vs-dangling
// split for file kinds: an anchor to a file whose extension the repo's
// graph never measures (a README, a manifest) is unasked, never dangling —
// no extractor could have proven it either way. The same regression run
// surfaced ~260 standing findings for exactly this shape.
func TestIntentCheck_UnparseableAnchorUnasked(t *testing.T) {
	got := explain(t,
		fileFact("backend", "app/services/formatter.rb"),
		anchorFact("wiki/adrs/readme.md", "backend", "README.md"),
		anchorFact("wiki/adrs/manifest.md", "backend", "config/manifest.json"),
	)
	if len(got) != 0 {
		t.Fatalf("anchors to unmeasured file kinds must be unasked, got: %s", titles(got))
	}
}

// TestIntentCheck_ExtensionlessKindIsTheBasename pins the completion of the
// unasked rule for the manifests it was stated for: an extensionless file's
// kind is its exact basename, so a repo measuring extensionless scripts has
// not thereby measured its Gemfile, Dockerfile, or version dotfiles — those
// anchors are unasked. An extensionless kind the repo DOES measure still
// dangles when the anchored path goes untouched.
func TestIntentCheck_ExtensionlessKindIsTheBasename(t *testing.T) {
	got := explain(t,
		fileFact("backend", "bin/setup"),
		fileFact("backend", "Rakefile"),
		anchorFact("wiki/adrs/deps.md", "backend", "Gemfile"),
		anchorFact("wiki/adrs/deploy.md", "backend", "Dockerfile"),
		anchorFact("wiki/adrs/ruby.md", "backend", ".ruby-version"),
	)
	if len(got) != 0 {
		t.Fatalf("extensionless kinds the graph never measures must be unasked, got: %s", titles(got))
	}

	got = explain(t,
		fileFact("backend", "Rakefile"),
		anchorFact("wiki/adrs/build.md", "backend", "tools/Rakefile"),
	)
	if len(got) != 1 || !strings.Contains(got[0].Title, "tools/Rakefile") {
		t.Fatalf("a measured extensionless kind at an untouched path must dangle, got: %s", titles(got))
	}
}

// TestIntentCheck_AnchorJoinsBothFileForms pins the normalization: measured
// files join in the label-prefixed AND repo-relative forms, so a genuine
// path that happens to start with the repo's own name (repo "app", file
// app/models/x.rb) is not mis-trimmed into a false dangling anchor.
func TestIntentCheck_AnchorJoinsBothFileForms(t *testing.T) {
	got := explain(t,
		fileFact("app", "app/models/x.rb"),
		anchorFact("wiki/adrs/x.md", "app", "app/models/x.rb"),
	)
	if len(got) != 0 {
		t.Fatalf("a path starting with the repo's own name must join, got: %s", titles(got))
	}
}

// TestIntentCheck_SupersededIntentRetires pins status-aware verdicting: a
// page marked superseded (by status token or by an outgoing superseded-by
// relation) stops verdicting as current intent — its missing seams, claims,
// anchors and scope are silence — while a measured edge that only its
// declaration covers surfaces as the precise superseded-intent finding, not
// as an unexpected seam.
func TestIntentCheck_SupersededIntentRetires(t *testing.T) {
	oldPage := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/old.md",
		Name: "page: wiki/adrs/old.md",
		Props: map[string]any{"intent_kind": "page", "page_type": "decision",
			"status": "superseded", "scope": []string{"ghost"}, "source": "wiki/adrs/old.md"}}
	oldSeam := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/old.md",
		Name: "app consumes legacy via http-client",
		Props: map[string]any{"intent_kind": "consumes", "intent_owner": "app",
			"target": "legacy", "via": "http-client", "source": "wiki/adrs/old.md"}}
	oldAnchor := anchorFact("wiki/adrs/old.md", "app", "src/gone.rb")
	oldClaim := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/old.md",
		Name: "claim: app symbol  = 99",
		Props: map[string]any{"intent_kind": "claim", "metric": "fact-count",
			"intent_owner": "app", "fact_kind": facts.KindSymbol, "value": 99, "source": "wiki/adrs/old.md"}}

	got := explain(t,
		repoMarker("app"), repoMarker("legacy"),
		fileFact("app", "src/app.rb"),
		oldPage, oldSeam, oldAnchor, oldClaim,
		edgeFact("app", "legacy", "http-client"),
	)
	if len(got) != 1 {
		t.Fatalf("want only the superseded-intent finding, got: %s", titles(got))
	}
	if !strings.Contains(got[0].Title, "Superseded intent still measured: app -> legacy via http-client") ||
		got[0].Confidence != supersededIntentConfidence {
		t.Fatalf("measured edge covered only by superseded intent must surface capped, got %+v", got[0])
	}

	// Remove the measured edge: the superseded declaration is history, so its
	// missing seam, dangling anchor, failed claim, and unknown scope all stay
	// silent.
	silent := explain(t,
		repoMarker("app"), repoMarker("legacy"),
		fileFact("app", "src/app.rb"),
		oldPage, oldSeam, oldAnchor, oldClaim,
	)
	if len(silent) != 0 {
		t.Fatalf("retired intent must not verdict as current, got: %s", titles(silent))
	}
}

// TestIntentCheck_SupersededByRelationRetires pins the second retirement
// signal: an outgoing superseded-by relation retires a page even when its
// status token is outside the vocabulary enola interprets.
func TestIntentCheck_SupersededByRelationRetires(t *testing.T) {
	page := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/old.md",
		Name:  "page: wiki/adrs/old.md",
		Props: map[string]any{"intent_kind": "page", "page_type": "decision", "status": "shipped"}}
	rel := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/old.md",
		Name: "wiki/adrs/old.md superseded-by wiki/adrs/new.md",
		Props: map[string]any{"intent_kind": "relation", "rel": "superseded-by",
			"to": "wiki/adrs/new.md", "source": "wiki/adrs/old.md"}}
	newPage := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/new.md",
		Name:  "page: wiki/adrs/new.md",
		Props: map[string]any{"intent_kind": "page", "page_type": "decision"}}
	staleAnchor := anchorFact("wiki/adrs/old.md", "app", "src/gone.rb")

	got := explain(t, repoMarker("app"), fileFact("app", "src/app.rb"), page, rel, newPage, staleAnchor)
	if len(got) != 0 {
		t.Fatalf("a superseded-by relation must retire the page's anchors, got: %s", titles(got))
	}
}

// TestIntentCheck_ClaimAboutAbsentRepoUnasked pins the counterparty rule
// for claims: a fact-count claim owned by, or a seam claim touching, a repo
// absent from the graph is unasked — "measures 0 because the repo is not
// loaded" must never present as a proof-class failed claim. A
// partial-cluster snapshot fails every out-of-graph claim otherwise.
func TestIntentCheck_ClaimAboutAbsentRepoUnasked(t *testing.T) {
	v := 42
	countClaim := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/x.md",
		Name: "claim: ghost route /api = 42",
		Props: map[string]any{"intent_kind": "claim", "metric": "fact-count",
			"intent_owner": "ghost", "fact_kind": "route", "value": v, "source": "wiki/adrs/x.md"}}
	seamClaim := facts.Fact{Kind: facts.KindIntent, Repo: "wiki", File: "wiki/adrs/x.md",
		Name: "claim: seam app -> ghost via graphql",
		Props: map[string]any{"intent_kind": "claim", "metric": "seam",
			"intent_owner": "app", "provider": "ghost", "via": "graphql", "source": "wiki/adrs/x.md"}}
	got := explain(t, repoMarker("app"), countClaim, seamClaim)
	if len(got) != 0 {
		t.Fatalf("claims about absent repos must be unasked, got: %s", titles(got))
	}
}
