package queryloops

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/explainers/common"
	"github.com/enola-labs/enola/internal/facts"
)

func store(fs ...facts.Fact) *facts.Store {
	s := facts.NewStore()
	for _, f := range fs {
		s.Add(f)
	}
	return s
}

func model(name string) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: name, Repo: "app",
		Props: map[string]any{"storage_kind": "model", "language": "ruby"}}
}

func symbol(name string, callsInLoop []string, depth int) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "app", File: "app/x.rb",
		Props: map[string]any{"language": "ruby", "loop_depth": depth,
			"calls_in_loop": callsInLoop}}
}

func titles(t *testing.T, s *facts.Store) string {
	got, err := New().Explain(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, i := range got {
		b.WriteString(i.Title + "\n")
		for _, e := range i.Evidence {
			b.WriteString("  " + e.Detail + "\n")
		}
	}
	return b.String()
}

func TestClassLevelQueryInALoopIsReported(t *testing.T) {
	out := titles(t, store(model("AccessLevel"),
		symbol("Helpers#create_defaults", []string{"AccessLevel.find_or_create_by"}, 1)))
	if !strings.Contains(out, "AccessLevel.find_or_create_by") {
		t.Fatalf("the query-per-iteration was not reported:\n%s", out)
	}
}

// The whole reason this shape is detectable and the association-read shape is
// not: the receiver IS the type. `channel.trigger` matched an association name
// on an unrelated model while `channel` was a PusherChannel — seven such false
// positives, and every one of the seven candidates was one.
func TestAnUnknownReceiverIsNotAModel(t *testing.T) {
	out := titles(t, store(model("AccessLevel"),
		symbol("Requisition#send_pusher_event!", []string{"channel.trigger"}, 1)))
	if out != "" {
		t.Fatalf("a non-model receiver was reported:\n%s", out)
	}
}

func TestANonQueryMethodOnAModelIsNotReported(t *testing.T) {
	out := titles(t, store(model("AccessLevel"),
		symbol("X#y", []string{"AccessLevel.human_name"}, 1)))
	if out != "" {
		t.Fatalf("a non-query class method was reported:\n%s", out)
	}
}

// Ruby's loop_depth already excludes constant-bounded iterators — `6.times`
// walks its block at the same depth — so a call recorded in calls_in_loop is
// already inside a data-sized loop. A symbol with no loop signal has none.
func TestNoLoopSignalMeansNoFinding(t *testing.T) {
	out := titles(t, store(model("AccessLevel"),
		facts.Fact{Kind: facts.KindSymbol, Name: "X#y", Repo: "app", File: "app/x.rb",
			Props: map[string]any{"language": "ruby"}}))
	if out != "" {
		t.Fatalf("a symbol with no loop reported a finding:\n%s", out)
	}
}

func TestNestedLoopsRankAbove(t *testing.T) {
	out := titles(t, store(model("User"), model("AccessLevel"),
		symbol("A#shallow", []string{"AccessLevel.find_by"}, 1),
		symbol("B#nested", []string{"User.find"}, 2)))
	if strings.Index(out, "B#nested") > strings.Index(out, "A#shallow") {
		t.Fatalf("a depth-2 query should rank above a depth-1 one:\n%s", out)
	}
}

func assoc(model, name, target string) facts.Fact {
	return facts.Fact{Kind: facts.KindAssociation, Name: model + "#" + name, Repo: "app",
		Props: map[string]any{"model": model, "association": name, "target": target}}
}

func bound(name string, bindings, calls []string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: "app", File: "app/x.rb",
		Props: map[string]any{"language": "ruby", "loop_depth": 1,
			"block_bindings": bindings, "calls_in_loop": calls}}
}

// The shape everyone means by N+1, and the one that measured to zero true
// findings before block bindings existed: an association read on the element of
// a collection. `form_questions.each { |q| q.form_answers }` is a query per
// iteration only because `q` is a FormQuestion — and the binding is what says so.
func TestAnAssociationReadOnABoundElementIsReported(t *testing.T) {
	out := titles(t, store(
		assoc("Company", "form_questions", "FormQuestion"),
		assoc("FormQuestion", "form_answers", "FormAnswer"),
		bound("Report#build", []string{"q=form_questions"}, []string{"q.form_answers"})))
	if !strings.Contains(out, "q.form_answers") {
		t.Fatalf("the typed association read was not reported:\n%s", out)
	}
}

// Without the binding the receiver is untyped, and `client.post` matching an
// association named `post` on an unrelated model is exactly how the first
// version produced seven candidates and zero true findings.
func TestAnUnboundReceiverIsNotReported(t *testing.T) {
	out := titles(t, store(
		assoc("Blog", "post", "Post"),
		bound("Poster#send", []string{"x=widgets"}, []string{"client.post"})))
	if out != "" {
		t.Fatalf("an untyped receiver was reported:\n%s", out)
	}
}

// A method that is not an association on the bound element's type is a plain
// call, not a query.
func TestANonAssociationMethodOnABoundElementIsNotReported(t *testing.T) {
	out := titles(t, store(
		assoc("Company", "users", "User"),
		bound("R#run", []string{"u=users"}, []string{"u.full_name"})))
	if out != "" {
		t.Fatalf("a non-association read was reported:\n%s", out)
	}
}

// A relation the method preloaded is read from the cache per element, so the
// association read is not a query. TtGraphql::Queries::CannedResponsesQuery#resolve
// carried `includes(:questions, ...)` and was reported anyway; the reviewer
// rejected it, and this is that verdict as a rule.
func TestAPreloadedAssociationReadIsNotReported(t *testing.T) {
	f := bound("CannedResponsesQuery#resolve", []string{"r=canned_responses"}, []string{"r.questions", "r.answers"})
	f.Props["preloads"] = []string{"questions"}
	out := titles(t, store(
		assoc("Company", "canned_responses", "CannedResponse"),
		assoc("CannedResponse", "questions", "Question"),
		assoc("CannedResponse", "answers", "Answer"),
		f))
	if strings.Contains(out, "r.questions") {
		t.Fatalf("a preloaded association read was reported:\n%s", out)
	}
	if !strings.Contains(out, "r.answers") {
		t.Fatalf("the association that was NOT preloaded must still be reported:\n%s", out)
	}
}

// A local typed by `new` was never saved: its association reads build in
// memory. BlockLayoutsController#mock_company built a Company.new and read its
// associations in a loop; a rejected finding, now a rule. A persistence call on
// it is still a round trip and stays reported.
func TestAssociationReadsOnAnUnpersistedLocalAreNotReported(t *testing.T) {
	f := facts.Fact{Kind: facts.KindSymbol, Name: "BlockLayoutsController#mock_company", Repo: "app", File: "app/x.rb",
		Props: map[string]any{"language": "ruby", "loop_depth": 1,
			"local_types":        []string{"company=Company"},
			"unpersisted_locals": []string{"company"},
			"calls_in_loop":      []string{"company.pages", "company.save"}}}
	out := titles(t, store(model("Company"), assoc("Company", "pages", "Page"), f))
	if strings.Contains(out, "company.pages") {
		t.Fatalf("an association read on an unpersisted local was reported:\n%s", out)
	}
	if !strings.Contains(out, "company.save") {
		t.Fatalf("a write on the unpersisted local is still a round trip and must be reported:\n%s", out)
	}
}

func scopeFact(model, name string, preloads ...string) facts.Fact {
	props := map[string]any{"language": "ruby", "scope": true, "model": model}
	if len(preloads) > 0 {
		props["preloads"] = preloads
	}
	return facts.Fact{Kind: facts.KindSymbol, Name: "scope:" + name, Repo: "app", File: "app/models/x.rb", Props: props}
}

// The preload sits on the scope the chain passes through, one hop from the
// loop: `Current.company.users.allowed_to_login.preload(:authorizations)` was
// the sibling session's enterprise_calendars fix, and a scope carrying the
// includes is the recent_activity shape once the caller is followed.
func TestAPreloadStatedByAScopeOnTheChainSilencesTheRead(t *testing.T) {
	out := titles(t, store(
		model("Action"), model("Candidate"),
		assoc("Candidate", "actions", "Action"),
		assoc("Action", "user", "User"),
		assoc("Action", "candidate", "Candidate"),
		assoc("Action", "comments", "Comment"),
		scopeFact("Action", "activity_stream_for_candidate", "user", "candidate"),
		bound("Presenter#recent", []string{"action=candidate.actions.activity_stream_for_candidate.where"}, []string{"action.user", "action.comments"})))
	if strings.Contains(out, "action.user") {
		t.Fatalf("a read the scope preloaded was reported:\n%s", out)
	}
	if !strings.Contains(out, "action.comments") {
		t.Fatalf("a read the scope did not preload must still be reported:\n%s", out)
	}
}

// A class-level relation walks back to the constant: `Action.recent.each`
// types the element as Action, and the scope's preloads join.
func TestAClassLevelRelationTypesItsElementsAndJoinsScopePreloads(t *testing.T) {
	out := titles(t, store(
		model("Action"),
		assoc("Action", "user", "User"),
		assoc("Action", "comments", "Comment"),
		scopeFact("Action", "recent", "user"),
		bound("Report#run", []string{"action=Action.recent.limit"}, []string{"action.user", "action.comments"})))
	if strings.Contains(out, "action.user") || !strings.Contains(out, "action.comments") {
		t.Fatalf("class-level relation resolution wrong:\n%s", out)
	}
}

// `def users; company.users.preload(:authorizations); end` then `users.each`:
// the preload lives on a same-class method the chain starts from.
func TestAPreloadStatedByASameClassMethodSilencesTheRead(t *testing.T) {
	helper := facts.Fact{Kind: facts.KindSymbol, Name: "Exporter#users", Repo: "app", File: "app/x.rb",
		Props: map[string]any{"language": "ruby", "preloads": []string{"authorizations"}}}
	out := titles(t, store(
		model("User"),
		assoc("Company", "users", "User"),
		assoc("User", "authorizations", "Authorization"),
		assoc("User", "roles", "Role"),
		helper,
		bound("Exporter#call", []string{"user=users"}, []string{"user.authorizations", "user.roles"})))
	if strings.Contains(out, "user.authorizations") || !strings.Contains(out, "user.roles") {
		t.Fatalf("same-class method preload join wrong:\n%s", out)
	}
}

// A collection that is nothing but a method parameter is typed by its name
// alone. The finding stays, at half the confidence, and says why: whether the
// caller preloaded it is not visible from here.
func TestACollectionThatIsAParameterIsReportedWeakly(t *testing.T) {
	f := bound("Presenter#recent_activity", []string{"action=actions"}, []string{"action.user"})
	f.Props["params"] = []string{"actions"}
	got, err := New().Explain(context.Background(), store(
		model("Action"),
		assoc("Candidate", "actions", "Action"),
		assoc("Action", "user", "User"),
		f))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want one finding, got %d", len(got))
	}
	if got[0].Confidence != 0.5 {
		t.Fatalf("confidence = %v, want 0.5", got[0].Confidence)
	}
	var saysWhy bool
	for _, e := range got[0].Evidence {
		saysWhy = saysWhy || strings.Contains(e.Detail, "parameter")
	}
	if !saysWhy {
		t.Fatalf("the weak finding must say why in its evidence: %+v", got[0].Evidence)
	}
	strong := titles(t, store(model("Action"), assoc("Candidate", "actions", "Action"), assoc("Action", "user", "User"),
		bound("Presenter#recent_activity", []string{"action=candidate.actions"}, []string{"action.user"})))
	if !strings.Contains(strong, "action.user") {
		t.Fatalf("a receiver-typed collection must still be reported at full strength:\n%s", strong)
	}
}

// A method that hands its reads to BatchLoader.for is batched by construction.
func TestReadsInsideABatchLoaderMethodAreNotReported(t *testing.T) {
	f := bound("Loader#locations", []string{"promotion=Promotion.where"}, []string{"promotion.locations"})
	f.Props["batch_loader"] = true
	out := titles(t, store(model("Promotion"), assoc("Promotion", "locations", "Location"), f))
	if out != "" {
		t.Fatalf("a batch-loaded read was reported:\n%s", out)
	}
}

func placed(fact facts.Fact, file string) facts.Fact {
	fact.File = file
	return fact
}

// Where the loop runs is read from the file's place in the Rails layout and
// said in the evidence; a one-off task is informational at half confidence,
// a spec is no finding at all, and everything else keeps its grade.
func TestAFindingSaysWhereItRunsAndGradesOneOffTasksInformational(t *testing.T) {
	mk := func(name, file string) facts.Fact {
		return placed(bound(name, []string{"q=form_questions"}, []string{"q.form_answers"}), file)
	}
	got, err := New().Explain(context.Background(), store(
		assoc("Company", "form_questions", "FormQuestion"),
		assoc("FormQuestion", "form_answers", "FormAnswer"),
		mk("Api::ReportsController#show", "shop/app/controllers/api/reports_controller.rb"),
		mk("ReportJob#perform", "app/jobs/report_job.rb"),
		mk("Maintenance::BackfillTask#process", "app/tasks/maintenance/backfill_task.rb"),
		mk("Development::Seeder#run", "app/services/development/seeder.rb"),
		mk("ReportService#call", "app/services/report_service.rb"),
		mk("ReportSpec#example", "spec/services/report_service_spec.rb")))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]facts.Insight{}
	for _, in := range got {
		byName[in.Evidence[0].Symbol] = in
	}
	if _, ok := byName["ReportSpec#example"]; ok {
		t.Fatalf("a loop in a spec is not a finding")
	}
	want := map[string][3]any{
		"Api::ReportsController#show":       {"request", 0.8, false},
		"ReportJob#perform":                 {"job", 0.8, false},
		"ReportService#call":                {"shared", 0.8, false},
		"Maintenance::BackfillTask#process": {"task", 0.5, true},
		"Development::Seeder#run":           {"task", 0.5, true},
	}
	for name, w := range want {
		in, ok := byName[name]
		if !ok {
			t.Fatalf("%s was not reported", name)
		}
		if in.Confidence != w[1].(float64) || in.Informational != w[2].(bool) {
			t.Fatalf("%s: confidence %v informational %v, want %v %v", name, in.Confidence, in.Informational, w[1], w[2])
		}
		if !strings.Contains(in.Evidence[1].Detail, "surface: "+w[0].(string)) {
			t.Fatalf("%s: evidence %q does not name the surface %s", name, in.Evidence[1].Detail, w[0])
		}
	}
}

// A bare model constant at the base types the element (`Company.find_each do
// |company| company.users`), a constant that is not a model types nothing.
func TestAClassLevelIterationOnABareConstantTypesItsElement(t *testing.T) {
	out := titles(t, store(
		model("Company"),
		assoc("Company", "users", "User"),
		bound("Backfill#run", []string{"company=Company", "c=STOP_CHARS"}, []string{"company.users", "c.users"})))
	if !strings.Contains(out, "company.users") {
		t.Fatalf("the class-level iteration was not reported:\n%s", out)
	}
	if strings.Contains(out, "c.users") {
		t.Fatalf("a constant that is not a model typed an element:\n%s", out)
	}
}

// All returns every finding, and says which ones the explainer names itself. Past
// the cap a finding is counted in the rollup and listed nowhere else, which is
// what a consumer of All is for.
func TestAllReturnsFindingsPastTheCap(t *testing.T) {
	fs := []facts.Fact{model("Account")}
	const n = common.MaxIndividualInsights + 7
	for i := 0; i < n; i++ {
		fs = append(fs, symbol(fmt.Sprintf("Worker%03d#run", i), []string{"Account.find_by"}, 1))
	}
	s := store(fs...)
	all := All(s)
	if len(all) != n {
		t.Fatalf("All returned %d findings, want %d", len(all), n)
	}
	listed := 0
	for _, f := range all {
		if f.Listed {
			listed++
		}
		if f.Call != "Account.find_by" || f.Confidence != 0.8 {
			t.Errorf("%s: call %q confidence %.1f, want Account.find_by at 0.8", f.Symbol, f.Call, f.Confidence)
		}
	}
	if listed != common.MaxIndividualInsights {
		t.Errorf("%d findings marked listed, want the cap of %d", listed, common.MaxIndividualInsights)
	}
	// The listed ones are exactly the ones Explain names.
	if got := titles(t, s); !strings.Contains(got, fmt.Sprintf("Additional per-iteration queries: %d more", n-common.MaxIndividualInsights)) {
		t.Errorf("Explain does not roll up the rest:\n%s", got[len(got)-200:])
	}
}
