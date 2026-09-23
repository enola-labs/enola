package perf

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func findFinding(fs []Finding, symbol, kind string) (Finding, bool) {
	for _, f := range fs {
		if f.Symbol == symbol && f.Kind == kind {
			return f, true
		}
	}
	return Finding{}, false
}

func TestBigOForDepth(t *testing.T) {
	cases := map[int]string{0: "O(1)", 1: "O(n)", 2: "O(n²)", 3: "O(n³)", 4: "O(n^4)"}
	for d, want := range cases {
		if got := bigOForDepth(d); got != want {
			t.Errorf("bigOForDepth(%d) = %q, want %q", d, got, want)
		}
	}
}

func TestIsExpensiveCall(t *testing.T) {
	storage := map[string]bool{"app/data.users": true}
	expensive := []string{
		// storage fact + Go idioms
		"app/data.users", "db.Query", "repo.FindByID", "client.Request", "http.Get",
		// Python SQLAlchemy / DBAPI (resolve to <type>.method)
		"sqlalchemy.orm.Session.execute", "models.DagRun.scalars", "conn.Connection.fetchall",
		"orm.Session.commit", "models.Dag.bulk_create",
		// Python HTTP (module-qualified)
		"requests.get", "httpx.post",
		// Ruby / ActiveRecord (resolve as Model.method / receiver.method)
		"User.where", "Account.find_by", "posts.pluck", "User.find_each", "record.update",
		// Swift (Core Data / network / file)
		"context.fetch", "URLSession.shared.dataTask", "repo.save", "store.load(for",
		// Kotlin (Room / Retrofit / coroutine)
		"dao.insert", "api.enqueue", "userDao.getAll", "call.await",
		// TypeScript (Prisma / TypeORM / network)
		"prisma.user.findMany", "repo.findOne", "client.fetch(",
		// Java (JPA / Spring Data / JDBC / RestTemplate)
		"userRepo.findAll", "repo.findById", "jdbcTemplate.queryForObject", "restTemplate.getForObject",
	}
	for _, c := range expensive {
		if !isExpensiveCall(c, storage, nil) {
			t.Errorf("isExpensiveCall(%q) = false, want true", c)
		}
	}
	cheap := []string{
		"strings.ToLower", "math.Max", "pkg.helper", "module.compute_total", "utils.format_name",
		// keywords must match the METHOD segment, not the receiver / path / argument:
		"searchQuery.toLowerCase",                           // "Query" is the receiver, not the method
		"src/components/GolfShop/RentalRequests.parseFloat", // "Request" is in the path
		"new Date(ticket.updated_at).toLocaleDateString",    // ".update" is in the argument
		"queryClient.invalidateQueries",                     // not a DB query method
		// Whole-word matching: a keyword must not fire on a within-token infix —
		// these are common C/C++ names that previously false-matched via substring.
		"stream.fflush",   // "flush" is an infix of "fflush" (C stdlib), not a DB flush
		"obj.preload",     // "load" is an infix of "preload"
		"row.updatedAt",   // "update" is a prefix of the getter "updatedAt"
		"node.insertedAt", // "insert" is a prefix of "insertedAt"
	}
	for _, c := range cheap {
		if isExpensiveCall(c, storage, nil) {
			t.Errorf("isExpensiveCall(%q) = true, want false", c)
		}
	}

	// Ruby association reads: flagged only when the method name is a known association.
	assoc := map[string]bool{"posts": true, "account": true}
	for _, c := range []string{"posts", "u.posts", "account", "u.account"} {
		if !isExpensiveCall(c, storage, assoc) {
			t.Errorf("isExpensiveCall(%q, assoc) = false, want true (association read)", c)
		}
	}
	for _, c := range []string{"name", "u.name", "title"} {
		if isExpensiveCall(c, storage, assoc) {
			t.Errorf("isExpensiveCall(%q, assoc) = true, want false (not an association)", c)
		}
	}
}

func TestAnalyze_NestedLoopSeverity(t *testing.T) {
	funcs := []funcInfo{
		{Name: "p.Quad", File: "p/q.go", Line: 3, LoopDepth: 2, LoopCount: 2, Cyclomatic: 3},
		{Name: "p.Cubic", File: "p/c.go", Line: 9, LoopDepth: 3, LoopCount: 3, Cyclomatic: 4},
	}
	got := analyze(funcs, nil, nil, nil)

	q, ok := findFinding(got, "p.Quad", "nested-loop")
	if !ok {
		t.Fatalf("no nested-loop finding for p.Quad; got %+v", got)
	}
	if q.BigO != "O(n²)" || q.Severity != "medium" {
		t.Errorf("p.Quad: got BigO=%q sev=%q, want O(n²)/medium", q.BigO, q.Severity)
	}
	c, ok := findFinding(got, "p.Cubic", "nested-loop")
	if !ok {
		t.Fatalf("no nested-loop finding for p.Cubic")
	}
	if c.BigO != "O(n³)" || c.Severity != "high" {
		t.Errorf("p.Cubic: got BigO=%q sev=%q, want O(n³)/high", c.BigO, c.Severity)
	}
}

func TestAnalyze_CompoundedAcrossCallGraph(t *testing.T) {
	// f loops once and, inside that loop, calls g which itself loops once.
	// Effective worst-case nesting compounds to O(n²) across the call boundary.
	funcs := []funcInfo{
		{Name: "p.f", File: "p/f.go", Line: 1, LoopDepth: 1, LoopCount: 1, Calls: []string{"p.g"}, CallsInLoop: []string{"p.g"}},
		{Name: "p.g", File: "p/g.go", Line: 1, LoopDepth: 1, LoopCount: 1},
	}
	got := analyze(funcs, nil, nil, nil)

	c, ok := findFinding(got, "p.f", "compounded")
	if !ok {
		t.Fatalf("no compounded finding for p.f; got %+v", got)
	}
	if c.BigO != "O(n²)" {
		t.Errorf("p.f compounded BigO = %q, want O(n²)", c.BigO)
	}
	// p.f loops only once locally, so it must NOT produce a nested-loop finding.
	if _, ok := findFinding(got, "p.f", "nested-loop"); ok {
		t.Errorf("p.f should not have a nested-loop finding (loop_depth=1)")
	}
	// p.g loops once and calls nothing in a loop → no compounded finding.
	if _, ok := findFinding(got, "p.g", "compounded"); ok {
		t.Errorf("p.g should not have a compounded finding")
	}
}

func TestAnalyze_DirectRecursion(t *testing.T) {
	// Flagged via the extractor's recursive_self prop.
	funcs := []funcInfo{{Name: "p.Fib", File: "p/f.go", Line: 1, Recursive: true, Calls: []string{"p.Fib"}}}
	got := analyze(funcs, nil, nil, nil)
	r, ok := findFinding(got, "p.Fib", "recursion")
	if !ok {
		t.Fatalf("no recursion finding for p.Fib; got %+v", got)
	}
	if r.Severity != "medium" {
		t.Errorf("p.Fib recursion severity = %q, want medium (no loop)", r.Severity)
	}
}

func TestAnalyze_RecursionWithLoopIsMediumNotExponential(t *testing.T) {
	// Recursion combined with a loop is the common tree/divide-and-conquer shape
	// (each level consumes a disjoint subset), NOT a combinatorial blow-up — so it
	// is medium and must not claim "exponential".
	funcs := []funcInfo{{Name: "p.Walk", File: "p/w.go", Line: 1, LoopDepth: 1, Recursive: true, Calls: []string{"p.Walk"}}}
	got := analyze(funcs, nil, nil, nil)
	r, ok := findFinding(got, "p.Walk", "recursion")
	if !ok {
		t.Fatalf("no recursion finding for p.Walk")
	}
	if r.Severity != "medium" {
		t.Errorf("recursion+loop severity = %q, want medium", r.Severity)
	}
	if r.BigO != "O(?) — recursive (with loop)" {
		t.Errorf("recursion+loop big_o = %q, want %q", r.BigO, "O(?) — recursive (with loop)")
	}
	if strings.Contains(r.BigO, "exponential") || strings.Contains(r.Why, "exponential") {
		t.Errorf("recursion+loop must not claim exponential: big_o=%q why=%q", r.BigO, r.Why)
	}
}

func TestAnalyze_OverloadDelegationNotRecursion(t *testing.T) {
	// Two overloads share the name "p.M.f"; one delegates to the other (resolves to
	// the shared name) and also loops. That must NOT be reported as recursion.
	funcs := []funcInfo{
		{Name: "p.M.f", File: "p/m.go", Line: 1, Recursive: true, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"p.M.f"}, CallsInLoop: []string{"p.M.g"}},
		{Name: "p.M.f", File: "p/m.go", Line: 8},
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "p.M.f", "recursion"); ok {
		t.Errorf("overloaded p.M.f must not be reported as recursion")
	}
}

func TestAnalyze_GenuineRecursionStillFlagged(t *testing.T) {
	// A single (non-overloaded) self-recursive function is still flagged.
	funcs := []funcInfo{{Name: "p.fib", File: "p/f.go", Line: 1, Recursive: true, Calls: []string{"p.fib"}}}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "p.fib", "recursion"); !ok {
		t.Errorf("genuine direct recursion should still be flagged")
	}
}

func TestAnalyze_NoCompoundedOnLoopFreeOverload(t *testing.T) {
	// Two overloads named "p.M.toOffers": one is loop-free, the other loops and
	// calls a looping helper. The loop-free overload must NOT get a compounded finding.
	funcs := []funcInfo{
		{Name: "p.M.toOffers", File: "p/m.go", Line: 1}, // loop-free
		{Name: "p.M.toOffers", File: "p/m.go", Line: 9, LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"p.M.helper"}},
		{Name: "p.M.helper", File: "p/m.go", Line: 20, LoopDepth: 1, LoopCount: 1},
	}
	got := analyze(funcs, nil, nil, nil)
	// The loop-free overload (Line 1) must have no compounded finding; assert that
	// NO compounded finding cites loop_depth=0 evidence and that a loop-free fact
	// can't be compounded by checking the only compounded finding (if any) is high-depth.
	for _, f := range got {
		if f.Kind == "compounded" && f.Line == 1 {
			t.Errorf("loop-free overload (line 1) must not get a compounded finding: %+v", f)
		}
	}
}

func TestAnalyze_MutualRecursionNotFlagged(t *testing.T) {
	// A→B→A with no self-call. Mutual-recursion (SCC) detection was removed because
	// in real call graphs it flags event-driven closure/delegate callback cycles as
	// recursion (the dominant Swift false-positive source). This A→B→A pattern is a
	// deliberate, accepted false negative — it must NOT produce a recursion finding.
	funcs := []funcInfo{
		{Name: "p.A", File: "p/a.go", Line: 1, Calls: []string{"p.B"}},
		{Name: "p.B", File: "p/b.go", Line: 1, Calls: []string{"p.A"}},
	}
	got := analyze(funcs, nil, nil, nil)
	if _, ok := findFinding(got, "p.A", "recursion"); ok {
		t.Errorf("mutual recursion (SCC) must no longer be flagged for p.A")
	}
	if _, ok := findFinding(got, "p.B", "recursion"); ok {
		t.Errorf("mutual recursion (SCC) must no longer be flagged for p.B")
	}
}

func TestAnalyze_ExpensiveCallInLoop_N1(t *testing.T) {
	storage := map[string]bool{"app/data.LoadUser": true}
	funcs := []funcInfo{
		// confirmed I/O (storage fact) → high, regardless of export status
		{Name: "app.Handler", File: "app/h.go", Line: 5, Exported: true, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"app/data.LoadUser"}, CallsInLoop: []string{"app/data.LoadUser"}},
		// heuristic-only match (generic verb name, no storage/prefix) → medium
		{Name: "app.worker", File: "app/w.go", Line: 9, Exported: false, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"repo.FindByID"}, CallsInLoop: []string{"repo.FindByID"}},
	}
	got := analyze(funcs, storage, nil, nil)

	h, ok := findFinding(got, "app.Handler", "call-in-loop")
	if !ok {
		t.Fatalf("no call-in-loop finding for app.Handler; got %+v", got)
	}
	if h.Severity != "high" {
		t.Errorf("confirmed-I/O N+1 severity = %q, want high", h.Severity)
	}
	w, ok := findFinding(got, "app.worker", "call-in-loop")
	if !ok {
		t.Fatalf("no call-in-loop finding for app.worker")
	}
	if w.Severity != "medium" {
		t.Errorf("heuristic-only N+1 severity = %q, want medium", w.Severity)
	}
}

func TestAnalyze_CallInLoopAggregatesCallees(t *testing.T) {
	// Multiple expensive callees in one function collapse to a SINGLE call-in-loop
	// finding that enumerates each in its evidence, rather than one near-duplicate
	// row per callee (same symbol/line/Big-O).
	funcs := []funcInfo{{
		Name: "app.Handler", File: "app/h.go", Line: 5, Exported: true, LoopDepth: 1, LoopCount: 1,
		CallsInLoop: []string{"db.Query", "db.Exec"},
	}}
	got := analyze(funcs, nil, nil, nil)

	var n int
	var f Finding
	for _, x := range got {
		if x.Symbol == "app.Handler" && x.Kind == "call-in-loop" {
			n++
			f = x
		}
	}
	if n != 1 {
		t.Fatalf("call-in-loop findings for app.Handler = %d, want 1 (aggregated)", n)
	}
	for _, want := range []string{"call_in_loop=db.Query", "call_in_loop=db.Exec"} {
		found := false
		for _, e := range f.Evidence {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("aggregated evidence %v missing %q", f.Evidence, want)
		}
	}
}

func TestIsExpensiveTSCall(t *testing.T) {
	storage := map[string]bool{"app/data.users": true}
	byName := map[string]funcInfo{
		"api.loadFeed": {Name: "api.loadFeed", PerformsIO: true},
	}
	// Short-name performs_io index (built by analyze from every performs_io func): a
	// wrapper the extractor tagged performs_io whose method segment is "updateChannels".
	ioMethods := map[string]bool{"updateChannels": true}
	expensive := []string{
		"app/data.users",       // storage fact
		"api.loadFeed",         // resolved callee flagged performs_io (exact byName)
		"svc.updateChannels",   // performs_io wrapper matched by short-name index
		"fetch",                // global fetch
		"prisma.user.findMany", // distinctive ORM method
		"repo.findUnique",      // ORM method
		"client.queryRaw",      // Prisma raw query
		"axios.get",            // HTTP receiver token
		"http.request",         // HTTP receiver token
		"prisma.$queryRaw",     // prisma. prefix
	}
	for _, c := range expensive {
		if !isExpensiveTSCall(c, storage, byName, ioMethods) {
			t.Errorf("isExpensiveTSCall(%q) = false, want true", c)
		}
	}
	cheap := []string{
		// The camelCase-verb collisions this fix targets: pure in-memory helpers
		// whose names merely contain a DB verb at a hump boundary.
		"getFetchAllUpdate", "updateState", "findIndex", "getOtherListUpdates",
		"scopeList.reduce", "collection.push", "items.flatMap",
		// A locally-defined helper resolved to a non-IO fact.
		"utils.formatName",
	}
	for _, c := range cheap {
		if isExpensiveTSCall(c, storage, byName, ioMethods) {
			t.Errorf("isExpensiveTSCall(%q) = true, want false", c)
		}
	}
}

func TestAnalyze_TSCallInLoop_LocalHelperNotExpensive(t *testing.T) {
	// A frontend reducer that calls a pure in-memory helper (getFetchAllUpdate)
	// inside a reduce must NOT produce a call-in-loop finding, even though the
	// helper name contains the DB verbs "Fetch"/"Update" at camelCase humps and
	// the reducer is exported (which would otherwise escalate to high).
	funcs := []funcInfo{
		{Name: "search/state.Reducer", File: "client/search/state/reducer.jsx", Line: 26,
			Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"getFetchAllUpdate"}},
		{Name: "getFetchAllUpdate", File: "client/search/state/utils.jsx", Line: 3},
	}
	got := analyze(funcs, nil, nil, nil)
	if f, ok := findFinding(got, "search/state.Reducer", "call-in-loop"); ok {
		t.Errorf("unexpected call-in-loop finding on in-memory helper: %+v", f)
	}
}

func TestAnalyze_TSCallInLoop_RealN1Flagged(t *testing.T) {
	// A genuine per-iteration network/DB call in TS/JS is still flagged.
	funcs := []funcInfo{
		{Name: "feed.load", File: "client/feed/api.ts", Line: 10,
			Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"prisma.user.findMany", "fetch"}},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "feed.load", "call-in-loop")
	if !ok {
		t.Fatalf("expected a call-in-loop finding for a real TS N+1; got %+v", got)
	}
	if f.Severity != "high" {
		t.Errorf("exported TS N+1 severity = %q, want high", f.Severity)
	}
}

func TestAnalyze_TSCallInLoop_PerformsIOWrapperFlagged(t *testing.T) {
	// A loop calling a wrapper the extractor transitively tagged performs_io is a real
	// per-iteration network call — flagged even though the wrapper's name matches no
	// keyword. The in-loop callee is the receiver-qualified metric string; the match
	// comes through the short-name ioMethods index built from the wrapper's own fact.
	funcs := []funcInfo{
		{Name: "notif.Subcategory", File: "client/notif/index.tsx", Line: 29,
			Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"svc.updateChannels"}},
		// The wrapper fact carries performs_io (propagated by the TS extractor).
		{Name: "client/notif/api.updateChannels", File: "client/notif/api.ts", Line: 4,
			Exported: true, PerformsIO: true},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "notif.Subcategory", "call-in-loop")
	if !ok {
		t.Fatalf("expected a call-in-loop finding for a performs_io wrapper N+1; got %+v", got)
	}
	if f.Severity != "high" {
		t.Errorf("exported performs_io N+1 severity = %q, want high", f.Severity)
	}
}

func TestAnalyze_PyDottedLocalHelperNotN1(t *testing.T) {
	// The Python extractor emits an in-loop call to an *unresolved* local function in
	// dotted import-path form ("app.helpers.merge_config"), while byName is keyed by the
	// slash-form canonical name ("app/helpers.merge_config"). The two never match, so the
	// resolution-based downgrade could not see that the callee is a pure in-memory helper,
	// and the "merge" keyword produced a false-positive call-in-loop N+1. A dotted callee
	// that resolves to a known non-I/O local must NOT be flagged.
	funcs := []funcInfo{
		{Name: "app/svc.Processor.run", File: "app/svc.py", Line: 10,
			LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"app.helpers.merge_config"}},
		// The helper's canonical name is slash-form; its dotted form is the callee above.
		{Name: "app/helpers.merge_config", File: "app/helpers.py", Line: 3},
	}
	got := analyze(funcs, nil, nil, nil)
	if f, ok := findFinding(got, "app/svc.Processor.run", "call-in-loop"); ok {
		t.Errorf("unexpected call-in-loop finding on a dotted-form in-memory helper: %+v", f)
	}
}

func TestAnalyze_PyDottedPerformsIOStillN1(t *testing.T) {
	// Guard against over-suppression: a dotted-form in-loop call that resolves to a local
	// the extractor tagged performs_io is a genuine per-iteration I/O call and must stay
	// flagged. The dotted-form resolution must downgrade only NON-I/O locals.
	funcs := []funcInfo{
		{Name: "app/svc.Processor.run", File: "app/svc.py", Line: 10,
			LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"app.daos.user.UserDAO.save"}},
		{Name: "app/daos/user.UserDAO.save", File: "app/daos/user.py", Line: 3,
			PerformsIO: true},
	}
	got := analyze(funcs, nil, nil, nil)
	if _, ok := findFinding(got, "app/svc.Processor.run", "call-in-loop"); !ok {
		t.Fatalf("expected a call-in-loop finding for a dotted performs_io N+1; got %+v", got)
	}
}

func TestAnalyze_AssociationReadInLoop(t *testing.T) {
	// A no-arg association read inside a loop is an N+1 only when the method name
	// is a known association.
	funcs := []funcInfo{{
		Name: "Report#run", File: "app/report.rb", Line: 3, LoopDepth: 1, LoopCount: 1,
		CallsInLoop: []string{"posts"},
	}}
	assoc := map[string]bool{"posts": true}

	got := analyze(funcs, nil, nil, assoc)
	f, ok := findFinding(got, "Report#run", "call-in-loop")
	if !ok {
		t.Fatalf("expected a call-in-loop finding for an association read; got %+v", got)
	}
	if !strings.Contains(f.Why, "association") {
		t.Errorf("association finding Why should mention association; got %q", f.Why)
	}

	// Without the association set, the bare method name is not flagged.
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "Report#run", "call-in-loop"); ok {
		t.Errorf("posts should not be flagged when it is not a known association")
	}
}

func TestAnalyze_RubyInMemoryNotN1(t *testing.T) {
	// In-memory Hash/Array/dirty-tracking ops (merge/fetch/insert/will_save_change_*)
	// collide with the cross-language DB/IO keywords but are not N+1 for a Ruby caller.
	// cache.fetch and insert_all are real I/O and must still be flagged.
	funcs := []funcInfo{
		{Name: "P#render", File: "app/p.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"hash.fetch", "params.merge", "will_save_change_to_name?", "results.insert"}},
		{Name: "Q#run", File: "app/q.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"cache.fetch", "Model.insert_all"}},
	}
	got := analyze(funcs, nil, nil, nil)
	if _, ok := findFinding(got, "P#render", "call-in-loop"); ok {
		t.Errorf("in-memory Hash/Array/dirty-tracking ops must not be flagged as N+1")
	}
	if _, ok := findFinding(got, "Q#run", "call-in-loop"); !ok {
		t.Errorf("cache.fetch / insert_all are real I/O and must still be flagged")
	}
}

func TestAnalyze_ConstantReceiverNotAssociationRead(t *testing.T) {
	// A class-method call on a constant receiver (`SystemEventService.trigger`) is not
	// an instance association read, even when its method name matches an association;
	// a bare name and a lowercase-variable instance read still are.
	assoc := map[string]bool{"trigger": true}
	funcs := []funcInfo{
		{Name: "ClassCall#perform", File: "app/c.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"SystemEventService.trigger"}},
		{Name: "InstanceCall#perform", File: "app/i.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"job.trigger"}},
		{Name: "BareCall#perform", File: "app/b.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"trigger"}},
	}
	got := analyze(funcs, nil, nil, assoc)
	if _, ok := findFinding(got, "ClassCall#perform", "call-in-loop"); ok {
		t.Errorf("Const.method must not be flagged as an association-read N+1")
	}
	if _, ok := findFinding(got, "InstanceCall#perform", "call-in-loop"); !ok {
		t.Errorf("instance read job.trigger should still be flagged as an association N+1")
	}
	if _, ok := findFinding(got, "BareCall#perform", "call-in-loop"); !ok {
		t.Errorf("bare association read trigger should still be flagged")
	}
}

func TestAnalyze_BoundedFanoutDoesNotCompound(t *testing.T) {
	// Compounding through a bounded background-job fan-out (perform_later over a
	// handler registry) must not produce O(n²).
	funcs := []funcInfo{
		{Name: "Caller#perform", File: "app/c.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"SystemEventService.trigger"}},
		{Name: "SystemEventService.trigger", File: "app/s.rb", LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"inline", "klass.perform_now", "klass.perform_later"}},
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "Caller#perform", "compounded"); ok {
		t.Errorf("compounding through a bounded job fan-out must not produce O(n²)")
	}

	// Control: a genuinely-looping callee (real per-iteration I/O) still compounds.
	funcs2 := []funcInfo{
		{Name: "Caller2#perform", File: "app/c.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"Looper#each_thing"}},
		{Name: "Looper#each_thing", File: "app/l.rb", LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"record.save"}},
	}
	if _, ok := findFinding(analyze(funcs2, nil, nil, nil), "Caller2#perform", "compounded"); !ok {
		t.Errorf("compounding through a genuinely-looping function should still be flagged")
	}
}

func TestSortFindings_SeverityFirst(t *testing.T) {
	got := analyze([]funcInfo{
		{Name: "p.Med", LoopDepth: 2},  // nested-loop medium
		{Name: "p.High", LoopDepth: 3}, // nested-loop high
	}, nil, nil, nil)
	if len(got) < 2 {
		t.Fatalf("expected >=2 findings, got %d", len(got))
	}
	if got[0].Severity != "high" {
		t.Errorf("first finding severity = %q, want high (ranked first)", got[0].Severity)
	}
}

func TestComputeEffectiveDepths_CycleTerminates(t *testing.T) {
	// A and B call each other in loops; the DFS must not loop forever.
	funcs := map[string]funcInfo{
		"A": {Name: "A", LoopDepth: 1, CallsInLoop: []string{"B"}},
		"B": {Name: "B", LoopDepth: 1, CallsInLoop: []string{"A"}},
	}
	eff := computeEffectiveDepths(funcs)
	if eff["A"] < 1 || eff["B"] < 1 {
		t.Errorf("effective depths should be >= local loop depth; got %v", eff)
	}
}

func TestAnalyze_SwiftInMemoryCallInLoopNotFlagged(t *testing.T) {
	// Swift callees whose names merely share the generic verbs (updateCenter,
	// updateValue) are in-memory UI/state ops, not I/O — no call-in-loop finding.
	funcs := []funcInfo{
		{Name: "A.refresh", File: "a.swift", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"badge.updateCenter", "self.updateValue", "stack.insertArrangedSubview", "$0.refresh"}},
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "A.refresh", "call-in-loop"); ok {
		t.Errorf("Swift in-memory verb callees must not produce a call-in-loop finding")
	}
}

func TestAnalyze_SwiftIOPrimitiveCallInLoop(t *testing.T) {
	// A direct high-confidence I/O primitive in a Swift loop IS flagged.
	funcs := []funcInfo{
		{Name: "A.load", File: "a.swift", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"URLSession.shared.dataTask"}},
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "A.load", "call-in-loop"); !ok {
		t.Errorf("Swift URLSession.dataTask in a loop should be a call-in-loop finding")
	}
}

func TestAnalyze_SwiftPerformsIOCallInLoop(t *testing.T) {
	// A Swift in-loop call to a resolved method the extractor flagged performs_io is
	// an N+1; an in-memory resolved callee (performs_io=false) is not. (The transitive
	// I/O closure itself lives in the extractor; the analyzer just trusts the prop.)
	ioFuncs := []funcInfo{
		{Name: "A.foo", File: "a.swift", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"A.bar"}, Calls: []string{"A.bar"}},
		{Name: "A.bar", File: "a.swift", PerformsIO: true},
	}
	if _, ok := findFinding(analyze(ioFuncs, nil, nil, nil), "A.foo", "call-in-loop"); !ok {
		t.Errorf("in-loop call to a performs_io method should be flagged")
	}

	memFuncs := []funcInfo{
		{Name: "A.foo", File: "a.swift", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"A.bar"}, Calls: []string{"A.bar"}},
		{Name: "A.bar", File: "a.swift"}, // performs_io=false
	}
	if _, ok := findFinding(analyze(memFuncs, nil, nil, nil), "A.foo", "call-in-loop"); ok {
		t.Errorf("in-loop call to a non-I/O resolved method must not be flagged")
	}
}

func TestAnalyze_NonSwiftUpdateInLoopStillFlagged(t *testing.T) {
	// The generic (non-Swift) path is unchanged: a Ruby ActiveRecord update in a loop
	// is still an N+1.
	funcs := []funcInfo{
		{Name: "app.sync", File: "app/sync.rb", Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"record.update"}},
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "app.sync", "call-in-loop"); !ok {
		t.Errorf("Ruby record.update in a loop should still be flagged (generic path unchanged)")
	}
}

func TestAnalyze_RustCallInLoop_PerformsIOWrapperFlagged(t *testing.T) {
	// A Rust for-loop over a variable calls a helper the extractor transitively
	// tagged performs_io (it does a DB/HTTP round-trip). This is a real N+1 and
	// must be flagged high, even though the helper's name matches no keyword —
	// the .rs branch trusts the extractor's performs_io signal, not verbs. The
	// in-loop callee sits in the scaling subset (data-dependent loop).
	funcs := []funcInfo{
		{Name: "svc/feed.load_all", File: "crates/feed/src/lib.rs", Line: 12,
			Exported: true, LoopDepth: 1, LoopCount: 1, ScalingLoopDepth: 1, HasScalingDepth: true,
			CallsInLoop: []string{"crates/feed/src/db.load_row"}, CallsInScalingLoop: []string{"crates/feed/src/db.load_row"}, HasScalingLoopCalls: true},
		{Name: "crates/feed/src/db.load_row", File: "crates/feed/src/db.rs", Line: 4, PerformsIO: true},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "svc/feed.load_all", "call-in-loop")
	if !ok {
		t.Fatalf("expected a call-in-loop finding for a Rust performs_io wrapper N+1; got %+v", got)
	}
	if f.Severity != "high" {
		t.Errorf("exported Rust performs_io N+1 severity = %q, want high", f.Severity)
	}
}

func TestAnalyze_RustCallInLoop_InMemoryHelperNotFlagged(t *testing.T) {
	// A Rust loop calling a pure in-memory helper (no performs_io, no storage,
	// no DB/HTTP method) must NOT produce a call-in-loop finding — the .rs branch
	// fires only on a confirmed-I/O callee, so ordinary per-element computation
	// (the common case) is never a false N+1.
	funcs := []funcInfo{
		{Name: "svc/feed.total", File: "crates/feed/src/lib.rs", Line: 20,
			Exported: true, LoopDepth: 1, LoopCount: 1, ScalingLoopDepth: 1, HasScalingDepth: true,
			CallsInLoop: []string{"crates/feed/src/math.combine"}, CallsInScalingLoop: []string{"crates/feed/src/math.combine"}, HasScalingLoopCalls: true},
		{Name: "crates/feed/src/math.combine", File: "crates/feed/src/math.rs", Line: 3}, // performs_io=false
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "svc/feed.total", "call-in-loop"); ok {
		t.Errorf("Rust in-loop call to a non-I/O resolved helper must not be flagged")
	}
}

func TestAnalyze_PhpCallInLoop_PerformsIOWrapperFlagged(t *testing.T) {
	// A WordPress foreach calls a helper the extractor tagged performs_io (it does a
	// $wpdb->get_results). This is a real N+1 and must be flagged high, even though
	// the helper's name matches no expensive keyword — the .php branch trusts the
	// extractor's performs_io signal.
	funcs := []funcInfo{
		{Name: "wp_render_all", File: "src/wp-includes/blocks/list.php", Line: 12,
			Exported: true, LoopDepth: 1, LoopCount: 1, ScalingLoopDepth: 1, HasScalingDepth: true,
			CallsInLoop: []string{"wp_stress_load_row"}, CallsInScalingLoop: []string{"wp_stress_load_row"}, HasScalingLoopCalls: true},
		{Name: "wp_stress_load_row", File: "src/wp-includes/blocks/db.php", Line: 4, Exported: true, PerformsIO: true},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "wp_render_all", "call-in-loop")
	if !ok {
		t.Fatalf("expected a call-in-loop finding for a PHP performs_io wrapper N+1; got %+v", got)
	}
	if f.Severity != "high" {
		t.Errorf("exported PHP performs_io N+1 severity = %q, want high", f.Severity)
	}
}

func TestAnalyze_PhpCallInLoop_InMemoryGetHelperNotFlagged(t *testing.T) {
	// WordPress is full of exported in-memory helpers named with DB verbs
	// (wp_get_object_terms style). A resolved non-I/O local must NOT be flagged just
	// because its name contains "get" — the .php branch downgrades resolved non-I/O locals.
	funcs := []funcInfo{
		{Name: "wp_build_list", File: "src/wp-includes/blocks/list.php", Line: 20,
			Exported: true, LoopDepth: 1, LoopCount: 1, ScalingLoopDepth: 1, HasScalingDepth: true,
			CallsInLoop: []string{"wp_get_cached_label"}, CallsInScalingLoop: []string{"wp_get_cached_label"}, HasScalingLoopCalls: true},
		{Name: "wp_get_cached_label", File: "src/wp-includes/blocks/util.php", Line: 3, Exported: true}, // performs_io=false
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "wp_build_list", "call-in-loop"); ok {
		t.Errorf("PHP in-loop call to a resolved non-I/O helper must not be flagged despite the get* name")
	}
}

func TestAnalyze_CppCallInLoop_PerformsIOWrapperFlagged(t *testing.T) {
	// A C++ for-loop calls a helper the extractor tagged performs_io (it does a
	// fopen/fread). A real N+1 — flagged high even though the helper's name matches
	// no keyword; the .cpp branch trusts the extractor's performs_io signal.
	funcs := []funcInfo{
		{Name: "Post.PViewFactory::buildAll", File: "Post/PViewFactory.cpp", Line: 30,
			Exported: true, LoopDepth: 1, LoopCount: 1, ScalingLoopDepth: 1, HasScalingDepth: true,
			CallsInLoop: []string{"Post.enola_load_row"}, CallsInScalingLoop: []string{"Post.enola_load_row"}, HasScalingLoopCalls: true},
		{Name: "Post.enola_load_row", File: "Post/PViewFactory.cpp", Line: 8, Exported: true, PerformsIO: true},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "Post.PViewFactory::buildAll", "call-in-loop")
	if !ok {
		t.Fatalf("expected a call-in-loop finding for a C++ performs_io wrapper N+1; got %+v", got)
	}
	if f.Severity != "high" {
		t.Errorf("exported C++ performs_io N+1 severity = %q, want high", f.Severity)
	}
}

func TestAnalyze_CppCallInLoop_InMemoryHelperNotFlagged(t *testing.T) {
	// A C++ loop calling a pure in-memory helper (no performs_io) must NOT fire — the
	// .cpp branch fires only on a confirmed-I/O callee, so ordinary per-element compute
	// (the overwhelmingly common case in a numeric codebase) is never a false N+1.
	funcs := []funcInfo{
		{Name: "Numeric.integrate", File: "Numeric/gauss.cpp", Line: 20,
			Exported: true, LoopDepth: 1, LoopCount: 1, ScalingLoopDepth: 1, HasScalingDepth: true,
			CallsInLoop: []string{"Numeric.evalBasis"}, CallsInScalingLoop: []string{"Numeric.evalBasis"}, HasScalingLoopCalls: true},
		{Name: "Numeric.evalBasis", File: "Numeric/gauss.cpp", Line: 3, Exported: true}, // performs_io=false
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "Numeric.integrate", "call-in-loop"); ok {
		t.Errorf("C++ in-loop call to a non-I/O resolved helper must not be flagged")
	}
}

func TestIsTestPath(t *testing.T) {
	tests := []string{
		"Tests/Feed/BaseFeedTest.swift",
		"internal/app/handler_test.go", "spec/models/user_spec.rb",
		"src/__tests__/app.test.ts", "app/Mocks/MockRepo.swift", "tests/test_thing.py",
	}
	for _, p := range tests {
		if !isTestPath(p) {
			t.Errorf("isTestPath(%q) = false, want true", p)
		}
	}
	production := []string{
		"Sources/Core/POIDataModel.swift", "internal/app/handler.go",
		"app/models/user.rb", "src/app/App.tsx", "Sources/Testable/Contest.swift",
		// Deliberate change of behaviour: a Swift file is classified by its TARGET,
		// not its name. `Sources/Foo/BarTests.swift` used to be treated as a test on
		// the strength of the `Tests.swift` basename suffix — but neither SPM nor
		// Xcode will compile a test from a `Sources/` target, so on a real repo that
		// shape is always production. On nan/nebenan-iOS the suffix rule uniquely
		// matched 9 files and every one was a production A/B-test feature, e.g.
		// `Settings.ABUserTests.swift` below; the 393 genuine test files it also
		// matched were already claimed by the `Tests/` segment. It had no true
		// positives to contribute, so it is gone. (fixed/28 defect class.)
		"Sources/Foo/BarTests.swift",
		"Sources/NebenanFoundation/Components/Settings/Settings.ABUserTests.swift",
	}
	for _, p := range production {
		if isTestPath(p) {
			t.Errorf("isTestPath(%q) = true, want false", p)
		}
	}
}

// --- Kotlin/Java (JVM) precision: cross-language keyword bleed, severity, depth ---

func TestIsExpensiveJvmCall(t *testing.T) {
	storage := map[string]bool{"app/data.RoomDb.loadUser": true}
	byName := map[string]funcInfo{}

	// Genuine Room / Retrofit / JPA / JDBC calls — must stay expensive.
	expensive := []string{
		"app/data.RoomDb.loadUser",        // storage fact
		"db.postCategoryDao().insert",     // Room insert (method + db. prefix)
		"dao.upsert", "api.awaitResponse", // Room upsert / Retrofit coroutine adapter
		"userRepo.findById", "userRepo.findAll", "repo.getAll",
		"jdbcTemplate.queryForObject", "restTemplate.getForObject", "webClient.retrieve",
		"cursor.query",
	}
	// Extractor-flagged I/O methods (Retrofit/Room) matched by short name — these are
	// NOT in the keyword list and would otherwise be missed.
	ioMethods := map[string]bool{"fetchFurly": true, "getNeighboursRx": true}
	for _, c := range expensive {
		if !isExpensiveJvmCall(c, storage, byName, ioMethods) {
			t.Errorf("isExpensiveJvmCall(%q) = false, want true", c)
		}
	}
	// A per-iteration call to a Retrofit/Room method is expensive via its I/O identity,
	// even though its name is in no keyword list.
	for _, c := range []string{"service.fetchFurly", "api.getNeighboursRx"} {
		if !isExpensiveJvmCall(c, storage, byName, ioMethods) {
			t.Errorf("isExpensiveJvmCall(%q) = false, want true (performs_io leaf)", c)
		}
	}

	// The cross-language bleed that produced the Kotlin false positives — the generic
	// verbs are in-memory here and must NOT be expensive.
	cheap := []string{
		"_basicState.update",                     // StateFlow.update, not a DB update
		"adapter.updateItem",                     // "update" prefix on an in-memory op
		"model.updateWith",                       // ditto
		"avatarView.loadAvatar",                  // "load" prefix, image draw
		"viewModel.loadFeed",                     // "load" prefix
		"m.awaitPointerEvent",                    // Compose await, not I/O
		"lastReplyDeferred.await",                // coroutine Deferred.await, dropped as too broad
		"translator.translate(line).await",       // ditto
		"request.getQuery",                       // "Query" is CamelCase-humped, but case-sensitive lowercase list won't fire
		"presenter.save", "repo.find", "x.fetch", // bare verbs excluded
	}
	for _, c := range cheap {
		if isExpensiveJvmCall(c, storage, byName, nil) {
			t.Errorf("isExpensiveJvmCall(%q) = true, want false", c)
		}
	}
}

func TestAnalyze_JvmPerformsIOAccessorNameNoCollision(t *testing.T) {
	// A Retrofit endpoint named `get` (e.g. Furly `@GET fun get(@Url …)`) puts "get" in
	// the performs_io set. A per-iteration list/map `.get(...)` must NOT be flagged as an
	// N+1 through that name collision.
	funcs := []funcInfo{
		{Name: "ui.View.draw", File: "ui/V.kt", Line: 1, Exported: true,
			LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"participants.get"}},
		{Name: "api.Furly.get", File: "api/F.kt", Line: 9, PerformsIO: true},
	}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "ui.View.draw", "call-in-loop"); ok {
		t.Errorf("a collection .get() must not be an N+1 via a Retrofit method named get")
	}
}

func TestAnalyze_JvmPerformsIOLeafIsHighN1(t *testing.T) {
	// A public Kotlin method looping over a call to a Retrofit endpoint (flagged
	// performs_io by the extractor) is a genuine N+1 — expensive AND high, even though
	// the callee name matches no keyword.
	funcs := []funcInfo{
		{Name: "biz.UseCase.getEmbeddedUrls", File: "biz/U.kt", Line: 31, Exported: true,
			LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"service.fetchFurly"}},
		{Name: "api.Service.fetchFurly", File: "api/S.kt", Line: 9, PerformsIO: true},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "biz.UseCase.getEmbeddedUrls", "call-in-loop")
	if !ok {
		t.Fatalf("no call-in-loop for a per-iteration Retrofit call; got %+v", got)
	}
	if f.Severity != "high" {
		t.Errorf("performs_io N+1 severity = %q, want high", f.Severity)
	}
}

func TestAnalyze_JvmKeywordBleedCleared(t *testing.T) {
	// A public Kotlin method looping over image previews and calling _basicState.update
	// must NOT produce a call-in-loop finding (update is in-memory StateFlow work).
	funcs := []funcInfo{{
		Name: "ui.VM.onRetryImageLoad", File: "ui/VM.kt", Line: 5, Exported: true,
		LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"_basicState.update", "image.isError"},
	}}
	if _, ok := findFinding(analyze(funcs, nil, nil, nil), "ui.VM.onRetryImageLoad", "call-in-loop"); ok {
		t.Errorf("Kotlin _basicState.update in a loop must not be an N+1 finding")
	}
}

func TestAnalyze_JvmPublicNotHot(t *testing.T) {
	storage := map[string]bool{"data.Db.loadUser": true}
	funcs := []funcInfo{
		// public Kotlin method, keyword-only expensive call → medium (public != hot on JVM)
		{Name: "ui.A.f", File: "ui/A.kt", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"dao.insert"}},
		// public Kotlin method, storage-backed call → high
		{Name: "ui.B.g", File: "ui/B.kt", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"data.Db.loadUser"}},
	}
	got := analyze(funcs, storage, nil, nil)
	a, ok := findFinding(got, "ui.A.f", "call-in-loop")
	if !ok {
		t.Fatalf("no call-in-loop for ui.A.f")
	}
	if a.Severity != "medium" {
		t.Errorf("keyword-only JVM N+1 severity = %q, want medium", a.Severity)
	}
	b, ok := findFinding(got, "ui.B.g", "call-in-loop")
	if !ok {
		t.Fatalf("no call-in-loop for ui.B.g")
	}
	if b.Severity != "high" {
		t.Errorf("storage-backed JVM N+1 severity = %q, want high", b.Severity)
	}
}

func TestAnalyze_RecursiveNotCompoundedToN3(t *testing.T) {
	// A directly-recursive tree walk (loop + self-call) must be reported as recursion
	// only — NOT compounded to O(n³). Regression for the effectiveDepthOf self-cycle
	// double-count (hideKeyboardOnTouch / BaseNeighbourListAdapter.updateItem).
	funcs := []funcInfo{{
		Name: "ui.hideKeyboardOnTouch", File: "ui/K.kt", Line: 38, Exported: true,
		LoopDepth: 1, LoopCount: 1, Recursive: true,
		Calls: []string{"ui.hideKeyboardOnTouch"}, CallsInLoop: []string{"ui.hideKeyboardOnTouch"},
	}}
	got := analyze(funcs, nil, nil, nil)
	if _, ok := findFinding(got, "ui.hideKeyboardOnTouch", "recursion"); !ok {
		t.Errorf("recursive tree walk should still be flagged as recursion")
	}
	if c, ok := findFinding(got, "ui.hideKeyboardOnTouch", "compounded"); ok {
		t.Errorf("recursive function must not get a compounded finding: %+v", c)
	}
}

func TestEffectiveDepthOf_SelfCycleNoDoubleCount(t *testing.T) {
	f := funcInfo{Name: "p.walk", LoopDepth: 1, CallsInLoop: []string{"p.walk"}}
	eff := computeEffectiveDepths(map[string]funcInfo{"p.walk": f})
	if got := effectiveDepthOf(f, eff, map[string]funcInfo{f.Name: f}); got != 1 {
		t.Errorf("effectiveDepthOf(self-recursive, loop_depth=1) = %d, want 1 (no double-count)", got)
	}
}

func TestAnalyze_ComposeEventLoopSuppressed(t *testing.T) {
	// A Compose while(true){ awaitPointerEvent() } gesture loop iterates on input, not a
	// data size — no nested-loop / call-in-loop / compounded finding.
	funcs := []funcInfo{{
		Name: "ui.interceptGestures", File: "ui/R.kt", Line: 350, Exported: true,
		LoopDepth: 2, LoopCount: 2,
		CallsInLoop: []string{"m.awaitPointerEvent", "m.pressFunc"},
	}}
	got := analyze(funcs, nil, nil, nil)
	for _, kind := range []string{"nested-loop", "call-in-loop", "compounded"} {
		if f, ok := findFinding(got, "ui.interceptGestures", kind); ok {
			t.Errorf("Compose event loop must not produce a %s finding: %+v", kind, f)
		}
	}
}

// --- False-positive reduction: scaling depth, cold paths, Python precision, confidence ---

func TestAnalyze_ScalingDepthDeflatesBigO(t *testing.T) {
	// A function with 3 lexical loops but only 1 that scales with input (the other two
	// iterate literal/constant ranges) must read as O(n), not O(n³).
	funcs := []funcInfo{
		{Name: "p.merge", File: "p/m.ts", Line: 1, LoopDepth: 3, ScalingLoopDepth: 1, HasScalingDepth: true, LoopCount: 3, Cyclomatic: 4},
	}
	got := analyze(funcs, nil, nil, nil)
	if f, ok := findFinding(got, "p.merge", "nested-loop"); ok {
		t.Errorf("scaling depth 1 must not emit a nested-loop finding, got %+v", f)
	}
}

func TestAnalyze_FullyBoundedLoopsNoFinding(t *testing.T) {
	// All loops bounded (scaling 0) → no nested-loop finding at all.
	funcs := []funcInfo{
		{Name: "p.headers", File: "p/h.ts", Line: 1, LoopDepth: 3, ScalingLoopDepth: 0, HasScalingDepth: true, LoopCount: 3},
	}
	got := analyze(funcs, nil, nil, nil)
	if f, ok := findFinding(got, "p.headers", "nested-loop"); ok {
		t.Errorf("fully-bounded loops must not emit a finding, got %+v", f)
	}
}

func TestAnalyze_ScalingDepthKeepsGenuineNesting(t *testing.T) {
	// No scaling signal (older extractor) falls back to loop_depth: behavior unchanged.
	funcs := []funcInfo{
		{Name: "p.Cubic", File: "p/c.go", Line: 1, LoopDepth: 3, LoopCount: 3},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "p.Cubic", "nested-loop")
	if !ok || f.BigO != "O(n³)" || f.Severity != "high" {
		t.Fatalf("fallback to loop_depth failed: ok=%v %+v", ok, f)
	}
}

func TestAnalyze_ColdPathDownrankedToLow(t *testing.T) {
	// A deeply-nested loop in an Alembic migration is real but one-shot: downranked to low.
	funcs := []funcInfo{
		{Name: "m.upgrade", File: "airflow/migrations/versions/0101_x.py", Line: 1, LoopDepth: 3, ScalingLoopDepth: 3, HasScalingDepth: true, LoopCount: 3},
		{Name: "s.hot", File: "airflow/jobs/scheduler.py", Line: 1, LoopDepth: 3, ScalingLoopDepth: 3, HasScalingDepth: true, LoopCount: 3},
	}
	got := analyze(funcs, nil, nil, nil)
	m, ok := findFinding(got, "m.upgrade", "nested-loop")
	if !ok || m.Severity != "low" {
		t.Errorf("migration finding severity = %q (ok=%v), want low (cold path)", m.Severity, ok)
	}
	h, ok := findFinding(got, "s.hot", "nested-loop")
	if !ok || h.Severity != "high" {
		t.Errorf("hot-path finding severity = %q (ok=%v), want high", h.Severity, ok)
	}
}

func TestAnalyze_PythonPureHelperNotExpensive(t *testing.T) {
	// merge_dicts: the callee resolves to a known non-I/O local whose name ("merge")
	// collides with SQLAlchemy Session.merge — it must NOT be flagged as an N+1.
	funcs := []funcInfo{
		{Name: "u.merge_dicts", File: "airflow/utils/helpers.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"u.merge_dicts"}, CallsInLoop: []string{"u.merge_dicts"}},
	}
	got := analyze(funcs, nil, nil, nil)
	if f, ok := findFinding(got, "u.merge_dicts", "call-in-loop"); ok {
		t.Errorf("pure recursive dict merge must not be an N+1: %+v", f)
	}
}

func TestAnalyze_PythonExportedNotAutoHigh(t *testing.T) {
	// An in-loop call that is only a name-heuristic match (not a confirmed I/O fact) stays
	// medium, never high — exported is not a hotness signal.
	funcs := []funcInfo{
		{Name: "s.is_safe_url", File: "airflow/api/security.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"x.where"}, CallsInLoop: []string{"x.where"}},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "s.is_safe_url", "call-in-loop")
	if !ok || f.Severity != "medium" {
		t.Errorf("exported Python non-IO call-in-loop severity = %q (ok=%v), want medium", f.Severity, ok)
	}
}

func TestAnalyze_PythonRealDBCallInLoopStaysHigh(t *testing.T) {
	// A genuine DB round-trip (session.execute) inside a loop must remain high.
	funcs := []funcInfo{
		{Name: "d.delete_dag", File: "airflow/api/delete_dag.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"session.execute"}, CallsInLoop: []string{"session.execute"}},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "d.delete_dag", "call-in-loop")
	if !ok || f.Severity != "high" {
		t.Errorf("Python DB call-in-loop severity = %q (ok=%v), want high", f.Severity, ok)
	}
}

func TestAnalyze_PythonIOWrapperInLoopStaysExpensive(t *testing.T) {
	// A local helper that the extractor flagged performs_io, called in a loop, is a real
	// per-iteration I/O even though its name is generic.
	funcs := []funcInfo{
		{Name: "svc.run", File: "svc/run.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"svc.load_row"}, CallsInLoop: []string{"svc.load_row"}},
		{Name: "svc.load_row", File: "svc/run.py", Line: 9, PerformsIO: true},
	}
	got := analyze(funcs, nil, nil, nil)
	f, ok := findFinding(got, "svc.run", "call-in-loop")
	if !ok || f.Severity != "high" {
		t.Errorf("Python performs_io wrapper in loop = %q (ok=%v), want high", f.Severity, ok)
	}
}

func TestConfidenceFor_ByKindAndDecay(t *testing.T) {
	// Base confidence is set by kind reliability.
	if c := confidenceFor("call-in-loop", true, 1, false); c != 0.9 {
		t.Errorf("confirmed call-in-loop = %v, want 0.9", c)
	}
	if c := confidenceFor("call-in-loop", false, 1, false); c != 0.6 {
		t.Errorf("heuristic call-in-loop = %v, want 0.6", c)
	}
	if c := confidenceFor("nested-loop", false, 2, false); c != 0.8 {
		t.Errorf("nested-loop = %v, want 0.8", c)
	}
	if c := confidenceFor("compounded", false, 2, false); c != 0.55 {
		t.Errorf("compounded = %v, want 0.55 (least reliable kind)", c)
	}
	if c := confidenceFor("recursion", false, 0, false); c != 0.5 {
		t.Errorf("recursion = %v, want 0.5", c)
	}
	// Depth decay and bounded-discount caps still apply on top of the base.
	if c := confidenceFor("call-in-loop", true, 4, false); c > 0.5 {
		t.Errorf("depth-4 confidence = %v, want <= 0.5", c)
	}
	if c := confidenceFor("call-in-loop", true, 6, false); c > 0.35 {
		t.Errorf("depth-6 confidence = %v, want <= 0.35", c)
	}
	if c := confidenceFor("call-in-loop", true, 2, true); c > 0.6 {
		t.Errorf("bounded-discounted confidence = %v, want <= 0.6", c)
	}
}

func TestRiskRanking_WorstFirst(t *testing.T) {
	// Within the high tier, the deeper/more-confident finding must sort first.
	storage := map[string]bool{"s.exec": true}
	funcs := []funcInfo{
		{Name: "app.shallow", File: "app/a.py", Line: 1, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"s.exec"}}, // O(n) confirmed N+1
		{Name: "app.deep", File: "app/z.py", Line: 1, LoopDepth: 2, LoopCount: 2,
			CallsInLoop: []string{"s.exec"}}, // O(n²) confirmed N+1 — higher risk
	}
	got := analyze(funcs, storage, nil, nil)
	// app.deep (later alphabetically) must come before app.shallow purely on risk.
	var iDeep, iShallow = -1, -1
	for i, f := range got {
		if f.Symbol == "app.deep" && f.Kind == "call-in-loop" {
			iDeep = i
		}
		if f.Symbol == "app.shallow" && f.Kind == "call-in-loop" {
			iShallow = i
		}
	}
	if iDeep < 0 || iShallow < 0 || iDeep >= iShallow {
		t.Errorf("risk order wrong: deep=%d shallow=%d (deep must lead)", iDeep, iShallow)
	}
	if got[iDeep].RiskScore <= got[iShallow].RiskScore {
		t.Errorf("deep risk %.2f must exceed shallow %.2f", got[iDeep].RiskScore, got[iShallow].RiskScore)
	}
}

func TestRiskRanking_RouteHotPathBonus(t *testing.T) {
	// Two equal-complexity confirmed N+1s; the route handler outranks the internal helper.
	storage := map[string]bool{"s.exec": true}
	routes := map[string]bool{"app.handler": true}
	funcs := []funcInfo{
		{Name: "app.internal", File: "app/a.py", Line: 1, LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"s.exec"}},
		{Name: "app.handler", File: "app/z.py", Line: 1, LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"s.exec"}},
	}
	got := analyze(funcs, storage, routes, nil)
	h, _ := findFinding(got, "app.handler", "call-in-loop")
	in, _ := findFinding(got, "app.internal", "call-in-loop")
	if h.RiskScore <= in.RiskScore {
		t.Errorf("route handler risk %.2f must exceed internal %.2f (hot-path bonus)", h.RiskScore, in.RiskScore)
	}
}

func TestRiskRanking_CompoundedDeemphasised(t *testing.T) {
	// An O(n²) confirmed call-in-loop N+1 outranks an O(n²) compounded estimate.
	storage := map[string]bool{"s.exec": true}
	funcs := []funcInfo{
		{Name: "p.n1", File: "p/a.py", Line: 1, LoopDepth: 2, LoopCount: 2, CallsInLoop: []string{"s.exec"}},
		// compounded O(n²): loops once, calls a looping function.
		{Name: "p.comp", File: "p/b.py", Line: 1, LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"p.g"}},
		{Name: "p.g", File: "p/g.py", Line: 1, LoopDepth: 1, LoopCount: 1},
	}
	got := analyze(funcs, storage, nil, nil)
	n1, _ := findFinding(got, "p.n1", "call-in-loop")
	comp, ok := findFinding(got, "p.comp", "compounded")
	if !ok {
		t.Fatalf("no compounded finding for p.comp; got %+v", got)
	}
	if comp.Confidence >= n1.Confidence {
		t.Errorf("compounded confidence %.2f must be below confirmed N+1 %.2f", comp.Confidence, n1.Confidence)
	}
	if comp.RiskScore >= n1.RiskScore {
		t.Errorf("compounded risk %.2f must be below equal-complexity N+1 %.2f", comp.RiskScore, n1.RiskScore)
	}
}

func TestIsColdPath(t *testing.T) {
	cold := []string{
		"airflow/migrations/versions/0101_x.py", "dev/breeze/commands/x.py",
		"scripts/ci/y.py", "airflow/cli/commands/dag_command.py", "pkg/__main__.py",
	}
	for _, p := range cold {
		if !isColdPath(p) {
			t.Errorf("isColdPath(%q) = false, want true", p)
		}
	}
	hot := []string{"airflow/jobs/scheduler_job_runner.py", "airflow/models/dagrun.py"}
	for _, p := range hot {
		if isColdPath(p) {
			t.Errorf("isColdPath(%q) = true, want false", p)
		}
	}
}

func TestAnalyze_PythonUrllibParseNotIO(t *testing.T) {
	// urllib.parse.* is pure in-memory URL string work, not network I/O — a loop over
	// a small tuple calling it (airflow's is_safe_url) must NOT be flagged as an N+1.
	funcs := []funcInfo{
		{Name: "s.is_safe_url", File: "airflow/api/security.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			Calls:       []string{"urllib.parse.urlparse", "urllib.parse.urljoin", "urllib.parse.unquote"},
			CallsInLoop: []string{"urllib.parse.urlparse", "urllib.parse.urljoin", "urllib.parse.unquote"}},
	}
	if f, ok := findFinding(analyze(funcs, nil, nil, nil), "s.is_safe_url", "call-in-loop"); ok {
		t.Errorf("urllib.parse loop must not be an N+1: %+v", f)
	}
}

func TestAnalyze_PythonUrllibRequestIsIO(t *testing.T) {
	// urllib.request.urlopen IS network I/O and must stay a high N+1 in a route handler.
	funcs := []funcInfo{
		{Name: "s.fetch_all", File: "airflow/api/x.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			Calls: []string{"urllib.request.urlopen"}, CallsInLoop: []string{"urllib.request.urlopen"}},
	}
	f, ok := findFinding(analyze(funcs, nil, nil, nil), "s.fetch_all", "call-in-loop")
	if !ok || f.Severity != "high" {
		t.Errorf("urllib.request.urlopen loop = %q (ok=%v), want high", f.Severity, ok)
	}
}

func TestAnalyze_BoundedLoopCallNotN1(t *testing.T) {
	// A confirmed DB call that appears in calls_in_loop but NOT in calls_in_scaling_loop
	// (it only runs inside a bounded loop) is not an N+1.
	funcs := []funcInfo{
		{Name: "app.boundedSetup", File: "app/s.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"session.execute"}, CallsInScalingLoop: []string{}, HasScalingLoopCalls: true},
		{Name: "app.realN1", File: "app/r.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"session.execute"}, CallsInScalingLoop: []string{"session.execute"}, HasScalingLoopCalls: true},
	}
	got := analyze(funcs, nil, nil, nil)
	if f, ok := findFinding(got, "app.boundedSetup", "call-in-loop"); ok {
		t.Errorf("call only in a bounded loop must not be an N+1: %+v", f)
	}
	if f, ok := findFinding(got, "app.realN1", "call-in-loop"); !ok || f.Severity != "high" {
		t.Errorf("call in a scaling loop = %q (ok=%v), want high", f.Severity, ok)
	}
}

func TestAnalyze_ScalingLoopCallsFallback(t *testing.T) {
	// An extractor that did not emit calls_in_scaling_loop falls back to calls_in_loop.
	funcs := []funcInfo{
		{Name: "app.f", File: "app/f.py", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"session.execute"}}, // HasScalingLoopCalls == false
	}
	if f, ok := findFinding(analyze(funcs, nil, nil, nil), "app.f", "call-in-loop"); !ok || f.Severity != "high" {
		t.Errorf("fallback to calls_in_loop failed: sev=%q ok=%v", f.Severity, ok)
	}
}

func TestAnalyze_DeepCompoundedRelabeledAndCapped(t *testing.T) {
	// f (depth 1) → g (depth 1) → h (depth 1) → i (depth 1): effective depth 4. A deep
	// cross-call-graph estimate is relabeled O(n³+) and capped at medium.
	funcs := []funcInfo{
		{Name: "p.f", File: "p/f.go", Line: 1, LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"p.g"}},
		{Name: "p.g", File: "p/g.go", Line: 1, LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"p.h"}},
		{Name: "p.h", File: "p/h.go", Line: 1, LoopDepth: 1, LoopCount: 1, CallsInLoop: []string{"p.i"}},
		{Name: "p.i", File: "p/i.go", Line: 1, LoopDepth: 1, LoopCount: 1},
	}
	got := analyze(funcs, nil, nil, nil)
	c, ok := findFinding(got, "p.f", "compounded")
	if !ok {
		t.Fatalf("no compounded finding for p.f; got %+v", got)
	}
	if c.BigO != "O(n³+)" {
		t.Errorf("deep compounded BigO = %q, want O(n³+)", c.BigO)
	}
	if c.Severity != "medium" {
		t.Errorf("deep compounded severity = %q, want medium (capped)", c.Severity)
	}
}

func TestAnalyze_HeuristicN1IsMedium_ConfirmedIsHigh(t *testing.T) {
	// A JVM curated-method match (Room insert) is probable, not fact-confirmed → medium.
	// A storage-backed call → high.
	storage := map[string]bool{"data.Db.loadUser": true}
	funcs := []funcInfo{
		{Name: "ui.A.f", File: "ui/A.kt", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"dao.insert"}},
		{Name: "ui.B.g", File: "ui/B.kt", Line: 1, Exported: true, LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"data.Db.loadUser"}},
	}
	got := analyze(funcs, storage, nil, nil)
	if a, ok := findFinding(got, "ui.A.f", "call-in-loop"); !ok || a.Severity != "medium" {
		t.Errorf("curated-method N+1 = %q (ok=%v), want medium", a.Severity, ok)
	}
	if b, ok := findFinding(got, "ui.B.g", "call-in-loop"); !ok || b.Severity != "high" {
		t.Errorf("storage-backed N+1 = %q (ok=%v), want high", b.Severity, ok)
	}
}

// TestBoundedFanout_RubyInMemoryCallKeepsDiscount pins the Ruby guard that analyze()
// applies to in-loop calls but isBoundedFanout used to omit: `opts.merge` is a Hash#merge
// (in-memory), not a DB round-trip, so a mailer fan-out that dispatches AND merges a Hash
// is still a bounded fan-out. Without the guard the generic keyword list reads "merge" as
// I/O, suppresses the discount, and compounds the CALLER to O(n²) — a false finding.
func TestBoundedFanout_RubyInMemoryCallKeepsDiscount(t *testing.T) {
	funcs := []funcInfo{
		{
			Name: "Notifier#fanout", File: "app/services/notifier.rb", Line: 20, Exported: true,
			LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"deliver_later", "opts.merge"},
		},
		{
			Name: "Caller#run", File: "app/services/caller.rb", Line: 5, Exported: true,
			LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"Notifier#fanout"},
		},
	}
	got := analyze(funcs, nil, nil, nil)
	if c, ok := findFinding(got, "Caller#run", "compounded"); ok {
		t.Errorf("caller of a bounded Ruby fan-out must not be compounded: %+v", c)
	}
}

// TestEffectiveDepthOf_BoundedFanoutIsZero pins the contract effectiveDepthOf's own comment
// states — it "must repeat" computeEffectiveDepths's special-case logic because it recomputes
// per-fact, not per-name. The isEventLoop mirror was there; the isBoundedFanout mirror was not,
// so the two paths disagreed on the same function.
func TestEffectiveDepthOf_BoundedFanoutIsZero(t *testing.T) {
	fanout := funcInfo{
		Name: "Mailer#notify", File: "app/mailers/mailer.rb", Line: 11, Exported: true,
		LoopDepth: 2, LoopCount: 2,
		CallsInLoop: []string{"deliver_later"},
	}
	funcs := []funcInfo{fanout}
	byName := map[string]funcInfo{fanout.Name: fanout}
	markNonScaling(funcs, byName, nil, nil)

	eff := computeEffectiveDepths(byName)
	if eff[fanout.Name] != 0 {
		t.Fatalf("computeEffectiveDepths(bounded fan-out) = %d, want 0", eff[fanout.Name])
	}
	if got := effectiveDepthOf(funcs[0], eff, byName); got != 0 {
		t.Errorf("effectiveDepthOf(bounded fan-out) = %d, want 0 (must mirror computeEffectiveDepths)", got)
	}
}

// TestBoundedFanout_JvmExecutorSubmitIsDispatch pins the JVM half of the bounded fan-out
// discount. A loop that only hands each item to an executor does no scaling work in-process,
// so it must not compound its CALLER to O(n²). Regression for thingsboard's
// DefaultDataUpdateService.upgradeRuleNodes, which was reported high-severity O(n³) because
// processRuleNodePack's `executorService.submit` loop was read as a scaling loop.
func TestBoundedFanout_JvmExecutorSubmitIsDispatch(t *testing.T) {
	funcs := []funcInfo{
		{
			Name: "svc.processPack", File: "src/main/java/svc/Updater.java", Line: 88, Exported: true,
			LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"node.getId", "executorService.submit", "log.warn"},
		},
		{
			Name: "svc.upgradeAll", File: "src/main/java/svc/Updater.java", Line: 59, Exported: true,
			LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"svc.processPack"},
		},
	}
	got := analyze(funcs, nil, nil, nil)
	if c, ok := findFinding(got, "svc.upgradeAll", "compounded"); ok {
		t.Errorf("caller of a JVM executor fan-out must not be compounded: %+v", c)
	}
}

// TestBoundedFanout_JdbcExecuteIsNotDispatch pins a deliberate exclusion. `execute` is NOT a
// dispatch verb: Executor.execute collides with JDBC Statement.execute, and the dispatch check
// runs BEFORE the expensive check — so admitting it would let a real per-iteration query be read
// as a job dispatch and silence a true N+1. Adding "execute" for symmetry must fail here.
func TestBoundedFanout_JdbcExecuteIsNotDispatch(t *testing.T) {
	funcs := []funcInfo{{
		Name: "dao.loadAllRows", File: "src/main/java/dao/Dao.java", Line: 20, Exported: true,
		LoopDepth: 1, LoopCount: 1,
		CallsInLoop: []string{"stmt.execute", "executorService.submit"},
	}}
	f := funcs[0]
	byName := map[string]funcInfo{f.Name: f}
	markNonScaling(funcs, byName, nil, nil)
	if funcs[0].BoundedFanout {
		t.Error("a loop containing a JDBC stmt.execute is per-iteration I/O, not a bounded fan-out")
	}
}

// TestBoundedFanout_SubmitOutsideJvmIsNotDispatch pins the language gate: `submit` is an ordinary
// in-memory verb outside the JVM (a form submit, a queue helper), so it must not silently discount
// a scaling loop in Ruby/TS/Swift.
func TestBoundedFanout_SubmitOutsideJvmIsNotDispatch(t *testing.T) {
	funcs := []funcInfo{{
		Name: "Form#submit_all", File: "app/services/form.rb", Line: 7, Exported: true,
		LoopDepth: 1, LoopCount: 1,
		CallsInLoop: []string{"form.submit"},
	}}
	f := funcs[0]
	byName := map[string]funcInfo{f.Name: f}
	markNonScaling(funcs, byName, nil, nil)
	if funcs[0].BoundedFanout {
		t.Error("`submit` outside a JVM file must not be read as a job dispatch")
	}
}

// TestIsTestPath_SharedWithOrphans pins perf onto the shared facts.IsTestPath.
// perf's own copy knew nothing of the Gradle/KMP test source sets, so functions
// under androidTest/ were analyzed as production code; and its Swift/Kotlin
// basename suffixes silently skipped production A/B-test features. Both halves are
// pinned here so the two analyzers cannot drift apart again.
func TestIsTestPath_SharedWithOrphans(t *testing.T) {
	testPaths := []string{
		// Segments perf's old switch was missing entirely (this is bug new/51).
		"app/src/androidTest/java/de/nebenan/app/ui/EspressoMocks.kt",
		"shared/src/commonTest/kotlin/FooTest.kt",
		"shared/src/jvmTest/kotlin/BarTest.kt",
		"internal/testutil/helpers.go",
		"src/dashboard/fixtures/mockNativeFilters.ts",
		"tests_common/pytest_plugins.py",
		// Already covered, must stay covered.
		"Tests/Testability/Sources/Assert.swift",
		"Mocks/APIStub.swift",
		"src/components/__tests__/Button.tsx",
		"internal/engine/cache_test.go",
		"superset/conftest.py",
	}
	for _, p := range testPaths {
		if !isTestPath(p) {
			t.Errorf("isTestPath(%q) = false, want true", p)
		}
	}

	// fixed/28 class: production code named like a test. perf's old Swift/Kotlin
	// basename suffixes claimed every one of these, so their performance findings
	// were being silently dropped.
	prodPaths := []string{
		"Sources/NebenanFoundation/Components/KindnessReminderABTest/KindnessReminderABTest.swift",
		"Sources/NebenanFoundation/Components/Tracking/Tracking+ShortenNameABTest.swift",
		"Sources/NebenanFoundation/Components/Settings/Settings.ABUserTests.swift",
		"api/src/main/java/de/nebenan/app/api/model/ABTest.kt",
		"app/jobs/test_job.rb",
		"superset/cli/test_db.py",
	}
	for _, p := range prodPaths {
		if isTestPath(p) {
			t.Errorf("isTestPath(%q) = true, want false — production code must not be skipped", p)
		}
	}
}

// --- new/56: the summary must be computed from the set the caller is shown ---

// perfFindingsFixture mimics a mixed repo: Kotlin symbols whose NAME embeds the
// path, and Ruby symbols whose name does not (the case package= never handled).
func perfFindingsFixture() []Finding {
	return []Finding{
		{Symbol: "app/src/main/java/de/nebenan/app/ui.Feed.load", Package: "app/src/main/java/de/nebenan/app/ui",
			File: "app/src/main/java/de/nebenan/app/ui/Feed.kt", Severity: "high", BigO: "O(n²)", Kind: "call-in-loop"},
		{Symbol: "app/src/androidTest/java/de/nebenan/app.FeedTest.check", Package: "app/src/androidTest/java/de/nebenan/app",
			File: "app/src/androidTest/java/de/nebenan/app/FeedTest.kt", Severity: "medium", BigO: "O(n)", Kind: "call-in-loop"},
		// Ruby: the symbol name carries no path at all.
		{Symbol: "ABResult.refresh_all", Package: "app/models",
			File: "app/models/ab_result.rb", Severity: "medium", BigO: "O(n)", Kind: "call-in-loop"},
		{Symbol: "E2eTest::CleanUp#call", Package: "app/interactors/e2e_test",
			File: "app/interactors/e2e_test/clean_up.rb", Severity: "low", BigO: "O(n)", Kind: "nested-loop"},
	}
}

// TestSummarize_HonorsFilter is the core of new/56. The headline count must equal
// the number of rows the caller is actually shown. Before the fix, the summary was
// built from the unfiltered slice eleven lines before filterFindings ran, so
// package="androidTest" printed the repo-wide total (61) above an empty table.
func TestSummarize_HonorsFilter(t *testing.T) {
	all := perfFindingsFixture()

	tests := []struct {
		name      string
		in        args
		wantCount int
		wantHigh  int
		wantMed   int
		wantLow   int
	}{
		{"unfiltered", args{}, 4, 1, 2, 1},
		// The reported repro.
		{"package=androidTest", args{Package: "androidTest"}, 1, 0, 1, 0},
		// The Ruby silent zero: package= must match the PACKAGE, not the symbol name.
		{"package=app/models (ruby)", args{Package: "app/models"}, 1, 0, 1, 0},
		// The second, independent instance — the severity counts must be filtered too.
		{"min_severity=high", args{MinSeverity: "high"}, 1, 1, 0, 0},
		{"symbol= name match", args{Symbol: "ABResult"}, 1, 0, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered := filterFindings(all, tt.in)
			sum := summarize(all, filtered)

			if sum.TotalFindings != tt.wantCount {
				t.Errorf("TotalFindings = %d, want %d (must equal the rows shown, %d)",
					sum.TotalFindings, tt.wantCount, len(filtered))
			}
			if sum.TotalFindings != len(filtered) {
				t.Errorf("headline (%d) disagrees with the finding list (%d) — this is new/56",
					sum.TotalFindings, len(filtered))
			}
			if sum.BySeverity["high"] != tt.wantHigh || sum.BySeverity["medium"] != tt.wantMed || sum.BySeverity["low"] != tt.wantLow {
				t.Errorf("BySeverity = high %d / medium %d / low %d, want high %d / medium %d / low %d",
					sum.BySeverity["high"], sum.BySeverity["medium"], sum.BySeverity["low"],
					tt.wantHigh, tt.wantMed, tt.wantLow)
			}
		})
	}
}

// TestFilterFindings_PackageMatchesPackageNotJustName pins the second defect. The
// Finding carried no Package field, so package= was implemented as a substring match
// on the symbol NAME — identical to symbol=. That works by accident where the name
// embeds the path (Kotlin/Swift/Go) and returns a silent, permanent zero on Ruby,
// whose names carry none. On nan/nebenan, 64 symbols under app/models/ are flaggable
// and package="app/models" matched none of them.
func TestFilterFindings_PackageMatchesPackageNotJustName(t *testing.T) {
	all := perfFindingsFixture()

	got := filterFindings(all, args{Package: "app/models"})
	if len(got) != 1 || got[0].Symbol != "ABResult.refresh_all" {
		t.Fatalf("package=\"app/models\" returned %d findings, want the Ruby model finding; got %+v", len(got), got)
	}

	// The documented contract is "package OR name", so a name fragment must keep
	// working — narrowing to package-only would break a documented call.
	if got := filterFindings(all, args{Package: "ABResult"}); len(got) != 1 {
		t.Errorf("package= must still match a symbol name (documented contract); got %d", len(got))
	}

	// And it must not over-match: a package that matches nothing stays empty.
	if got := filterFindings(all, args{Package: "app/controllers"}); len(got) != 0 {
		t.Errorf("package=\"app/controllers\" matched %d findings, want 0", len(got))
	}
}

// TestRenderPerfSummary_NamesTheFilter — a bare "0 findings." cannot be told apart
// from a filter that silently matched nothing, which is precisely how the package=
// defect stayed invisible. The headline states the filter and the population.
func TestRenderPerfSummary_NamesTheFilter(t *testing.T) {
	all := perfFindingsFixture()
	in := args{Package: "androidTest"}
	filtered := filterFindings(all, in)

	out := renderPerfSummary(summarize(all, filtered), filtered, in)
	for _, want := range []string{`under package="androidTest"`, "(of 4 repo-wide)"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary does not name the filter: want %q in\n%s", want, out)
		}
	}

	unfiltered := renderPerfSummary(summarize(all, all), all, args{})
	if strings.Contains(unfiltered, "repo-wide") {
		t.Errorf("unfiltered summary should not qualify its count:\n%s", unfiltered)
	}
}

// TestRenderPerfSummary_HeadlineIsPreLimit is a regression I introduced while fixing
// new/56 and caught by running the tool, not by a test.
//
// new/56 moved the summary behind the filter — correctly — but renderPerfSummary
// built its Scoped from the slice AFTER the display limit had truncated it, so the
// headline counted the limit. On python/superset the default view read:
//
//	Performance: 100 findings.
//	Across 764 function(s) (high 145 / medium 499 / low 267).      ← sums to 911
//
// The headline is the LIMIT; the tier counts are the real filtered total. Exactly the
// defect new/56 was about — a headline disagreeing with the numbers beside it —
// reintroduced by its own fix, because every test called summarize() directly and none
// exercised a result set larger than the limit.
//
// The count must always be the filtered, PRE-limit total. The display list is capped
// separately (renderPerfSummary shows its own topN).
func TestRenderPerfSummary_HeadlineIsPreLimit(t *testing.T) {
	all := make([]Finding, 0, 250)
	for i := 0; i < 250; i++ {
		all = append(all, Finding{
			Symbol: fmt.Sprintf("pkg/app.Fn%d", i), Package: "pkg/app", File: "pkg/app/f.go",
			Severity: "high", BigO: "O(n)", Kind: "call-in-loop",
		})
	}

	sum := summarize(all, all)
	if sum.TotalFindings != 250 {
		t.Fatalf("TotalFindings = %d, want 250", sum.TotalFindings)
	}

	out := renderPerfSummary(sum, all, args{})
	if !strings.Contains(out, "250 findings") {
		t.Errorf("headline must report the filtered total (250), not the display limit; got:\n%s",
			strings.SplitN(out, "\n", 2)[0])
	}
	// And it must still agree with the severity line below it.
	if !strings.Contains(out, "high 250") {
		t.Errorf("severity line disagrees with the headline:\n%s", out)
	}
}

// TestIsColdPath_CommandsIsNotColdByItself is new/47. isColdPath downranks a finding
// to `low` and caps confidence at 0.4. Its CLI rule was:
//
//	strings.Contains(file, "/cli/commands/") || strings.Contains(file, "/commands/")
//
// The second half matched ANY path containing /commands/. CQRS, command buses and
// event sourcing all use commands/ as a first-class production directory, so a real
// N+1 in a command handler was pushed below every high and medium finding — invisible
// in the default view.
//
// On python/superset all 101 flaggable symbols under superset/commands/ were
// downranked, including an O(n³+) call-in-loop in BaseStreamingCSVExportCommand.
// _execute_query_and_stream — a streaming query executor, about as hot-path as code
// gets — and none of them was cold by any other rule.
//
// The existing TestIsColdPath could never have caught this: both of its /commands/
// paths matched an earlier rule first ("dev/breeze/..." via the `dev` segment,
// "airflow/cli/commands/..." via /cli/commands/), so the bare pattern had ZERO
// independent coverage.
func TestIsColdPath_CommandsIsNotColdByItself(t *testing.T) {
	production := []string{
		"superset/commands/dashboard/importers/v1/utils.py",
		"superset/commands/streaming_export/base.py",
		"api/commands/payment/handler.py",
		"internal/services/commands/sync/processor.go",
		"src/domain/commands/user/create_handler.ts",
	}
	for _, p := range production {
		if isColdPath(p) {
			t.Errorf("isColdPath(%q) = true — a production command handler must not be downranked", p)
		}
	}

	// The specific half is correct and stays: a CLI command really is one-shot.
	cold := []string{
		"airflow/cli/commands/dag_command.py",
		"superset/cli/commands/foo.py",
	}
	for _, p := range cold {
		if !isColdPath(p) {
			t.Errorf("isColdPath(%q) = false, want true (/cli/commands/ is still cold)", p)
		}
	}

	// The other cold rules are untouched.
	for _, p := range []string{
		"migrations/versions/abc.py", "dev/breeze/x.py", "scripts/gen.py",
		"app/__main__.py", "setup.py", "benchmarks/run.py",
	} {
		if !isColdPath(p) {
			t.Errorf("isColdPath(%q) = false, want true", p)
		}
	}
}

// TestIsIgnoredPath_DoublestarGlobs is new/48. isIgnoredPath matched with path.Match,
// which has NO `**` — `*` cannot cross `/`. So the doc's own advertised examples
// silently matched nothing: an operator setting ENOLA_PERF_EXCLUDE to hide a generated
// tree got zero exclusions and no error. It now uses pathglob.Match, the **-capable
// matcher the OSS engine has had since the ignore globs were directory-scoped.
func TestIsIgnoredPath_DoublestarGlobs(t *testing.T) {
	t.Setenv("ENOLA_PERF_EXCLUDE", "**/proto/**,src/legacy/**,**/*.pb.go,*.thrift.go")

	ignored := []string{
		"a/b/c/proto/messages.py",    // **/proto/** — a dir at any depth
		"proto/x.go",                 // ditto, at the root
		"src/legacy/deep/nested.ts",  // src/legacy/** — recursive, not one level
		"gen/api/v1/service.pb.go",   // **/*.pb.go — a basename glob at any depth
		"any/where/schema.thrift.go", // a bare basename glob (the basename fallback)
	}
	for _, p := range ignored {
		if !isIgnoredPath(p) {
			t.Errorf("isIgnoredPath(%q) = false, want true — the documented pattern does not work", p)
		}
	}

	for _, p := range []string{"superset/commands/handler.py", "src/app/main.ts", "internal/proto_helper.go"} {
		if isIgnoredPath(p) {
			t.Errorf("isIgnoredPath(%q) = true, want false — over-matched", p)
		}
	}

	// Unset → no-op, and no pattern may accidentally match everything. t.Setenv above
	// already registered the restore, so this only has to clear it for the rest of
	// this test — but a failure to clear would make the assertion below vacuous.
	if err := os.Unsetenv("ENOLA_PERF_EXCLUDE"); err != nil {
		t.Fatalf("clearing ENOLA_PERF_EXCLUDE: %v", err)
	}
	if isIgnoredPath("superset/commands/handler.py") {
		t.Error("with no globs set, nothing should be ignored")
	}
}

// TestRiskScore_RouteHandlerEscalation is the CONSUMER half of new/18, and the half
// that makes the whole thing observable.
//
// collect() keyed routeHandlers by the route's raw `handler` prop, which goextractor
// renders from the REGISTRATION site — the receiver VARIABLE chain
// ("h.weatherHandler.GetDailyWeatherRange"). analyze() and riskScore() then look it up
// by the canonical symbol name. Disjoint key spaces: on fairwayhub/golf, 1397 handler
// props intersect 13482 symbol names exactly TWICE (both Python). So no Go or Ruby route
// handler has EVER been escalated, and the comment promising a handler is "always hot"
// described behaviour the code did not have.
//
// The engine now resolves the route to its handler structurally (handled_by, v111), so
// read the resolved EDGE rather than re-deriving it from a name that cannot match.
func TestRiskScore_RouteHandlerEscalation(t *testing.T) {
	handler := Finding{
		Symbol: "internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange",
		File:   "internal/adapters/http/weather/handler_v2.go",
		Kind:   "call-in-loop", BigO: "O(n)", Severity: "high", Confidence: 0.9,
	}
	plain := Finding{
		Symbol: "internal/app/weather.Service.GetDailyWeatherRange",
		File:   "internal/app/weather/service.go",
		Kind:   "call-in-loop", BigO: "O(n)", Severity: "high", Confidence: 0.9,
	}

	// The resolved set, as bindHTTPHandlers now provides it: canonical symbol names.
	routeHandlers := map[string]bool{
		"internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange": true,
	}

	hot := riskScore(handler, routeHandlers)
	cold := riskScore(plain, routeHandlers)

	if hot <= cold {
		t.Errorf("a route handler must outrank the same finding off the request path: handler=%.2f plain=%.2f", hot, cold)
	}
	// The escalation is a 1.3x lift, and it has never once applied on the corpus.
	if want := cold * 1.3; hot < want-0.001 || hot > want+0.001 {
		t.Errorf("riskScore = %.3f, want %.3f (1.3x the non-handler score)", hot, want)
	}

	// And the receiver-VARIABLE spelling from the registration site must NOT be what we
	// key on — that is the bug. A set keyed that way escalates nothing.
	byRawProp := map[string]bool{"h.weatherHandler.GetDailyWeatherRange": true}
	if riskScore(handler, byRawProp) != cold {
		t.Error("keying on the raw registration-site handler prop escalated something — it cannot match a symbol name")
	}
}

// TestCollectRouteHandlers_FromResolvedEdge pins the actual defect site. riskScore and
// analyze were always correct GIVEN a correctly-keyed set; this is what handed them a set
// keyed by the registration-site expression, which can never match a symbol name.
func TestCollectRouteHandlers_FromResolvedEdge(t *testing.T) {
	routes := []facts.Fact{
		// A Go route as the engine now leaves it: the raw prop is the registration-site
		// expression, and the resolved symbol is on the handled_by edge.
		{
			Kind: facts.KindRoute, Name: "/api/weather/daily-range",
			Props: map[string]any{
				"language": "go", "method": "GET",
				"handler": "h.weatherHandler.GetDailyWeatherRange",
			},
			Relations: []facts.Relation{
				{Kind: facts.RelHandledBy, Target: "internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange"},
			},
		},
		// A Flask route, whose prop already IS the canonical name — the fallback keeps it.
		{
			Kind: facts.KindRoute, Name: "/health",
			Props: map[string]any{"language": "python", "handler": "mediapipe/app.health"},
		},
	}

	got := collectRouteHandlers(routes)

	if !got["internal/adapters/http/weather.HandlerV2.GetDailyWeatherRange"] {
		t.Error("the resolved handled_by target is not in routeHandlers — the escalation still cannot fire")
	}
	if !got["mediapipe/app.health"] {
		t.Error("the Python fallback regressed: a prop that already is a canonical name must still match")
	}
}

// TestIsExpensiveDartCall is the guard on the fourth ecosystem-specific handler.
//
// Dart earns one the way Swift, the JVM and TypeScript did, and measurably: the shared
// expensiveMethods list carries `where` for Ruby, where `Model.where(...)` is a lazy
// ActiveRecord query — but in Dart `.where()` is `Iterable.where`, the standard
// in-memory filter and the direct equivalent of JavaScript's `.filter()`. Measured on
// AppFlowy's Flutter half, the generic gate produced 58 call-in-loop findings dominated
// by `children.where` inside a loop, which is list processing and not I/O.
func TestIsExpensiveDartCall(t *testing.T) {
	storage := map[string]bool{"mobile/lib/db.assets": true}
	byName := map[string]funcInfo{
		"repo.loadPage": {Name: "repo.loadPage", PerformsIO: true},
	}
	// Short-name performs_io index, from the extractor's import-gated io_direct plus
	// its transitive closure.
	ioMethods := map[string]bool{"syncRemote": true, "call": true, "build": true}

	expensive := []string{
		"mobile/lib/db.assets",       // storage fact
		"repo.loadPage",              // resolved callee flagged performs_io
		"service.syncRemote",         // performs_io wrapper via the short-name index
		"_db.select",                 // drift receiver — immich's real N+1 shape
		"_db.insertOnConflictUpdate", // drift round-trip method
		"db.rawQuery",                // sqflite
		"dio.get",                    // HTTP receiver token
		"http.post",                  // HTTP receiver token
		"file.readAsString",          // dart:io
		"rootBundle.loadString",      // asset I/O
	}
	for _, c := range expensive {
		if !isExpensiveDartCall(c, storage, byName, ioMethods) {
			t.Errorf("isExpensiveDartCall(%q) = false, want true", c)
		}
	}

	cheap := []string{
		// The collision this handler exists for: Dart's in-memory collection API.
		"children.where", "items.map", "list.insert", "map.update",
		"nodes.firstWhere", "values.fold", "set.add",
		// Dispatch primitives. `callback.call(x)` is how a function object is invoked,
		// so a short-name performs_io match on `call` would make every callback
		// invocation in the codebase look like per-iteration I/O — 12 of AppFlowy's 28
		// remaining findings were driven by nothing else. `build` is a widget
		// constructing its subtree.
		"callback.call", "widget.build",
		// A locally-defined helper with no I/O evidence.
		"utils.formatDuration",
	}
	for _, c := range cheap {
		if isExpensiveDartCall(c, storage, byName, ioMethods) {
			t.Errorf("isExpensiveDartCall(%q) = true, want false", c)
		}
	}
}

// The cross-call form of the hierarchical rule. `for _, x := range xs { g(x) }`,
// where g loops over what it was handed, visits each element of each x once across
// the whole nest — so the callee finishes the caller's walk instead of running one
// per element, and its depth must not compound.
func TestAnalyze_ContinuedWalkDoesNotCompound(t *testing.T) {
	continued := []funcInfo{
		{Name: "p.Outer", File: "p/o.go", LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"p.inner"}, CallsInScalingLoop: []string{"p.inner"},
			HasScalingLoopCalls: true, CallsOnLoopElement: []string{"p.inner"}},
		{Name: "p.inner", File: "p/i.go", LoopDepth: 1, LoopCount: 1, LoopsOverParam: true},
	}
	if f, ok := findFinding(analyze(continued, nil, nil, nil), "p.Outer", "compounded"); ok {
		t.Errorf("a continued walk must not compound; got %s", f.BigO)
	}

	// Both halves are required, and each control removes one of them.
	//
	// The callee loops over a parameter, but this caller passes it something else:
	// nothing says the two loops walk the same data.
	notPassed := []funcInfo{
		{Name: "p.Outer", File: "p/o.go", LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"p.inner"}, CallsInScalingLoop: []string{"p.inner"},
			HasScalingLoopCalls: true},
		{Name: "p.inner", File: "p/i.go", LoopDepth: 1, LoopCount: 1, LoopsOverParam: true},
	}
	if _, ok := findFinding(analyze(notPassed, nil, nil, nil), "p.Outer", "compounded"); !ok {
		t.Errorf("a callee that is not handed the element must still compound")
	}

	// The element is passed, but the callee walks its own state, so the work really
	// is one traversal per element.
	ownState := []funcInfo{
		{Name: "p.Outer", File: "p/o.go", LoopDepth: 1, LoopCount: 1,
			CallsInLoop: []string{"p.inner"}, CallsInScalingLoop: []string{"p.inner"},
			HasScalingLoopCalls: true, CallsOnLoopElement: []string{"p.inner"}},
		{Name: "p.inner", File: "p/i.go", LoopDepth: 1, LoopCount: 1},
	}
	if _, ok := findFinding(analyze(ownState, nil, nil, nil), "p.Outer", "compounded"); !ok {
		t.Errorf("a callee looping over its own state must still compound")
	}
}
