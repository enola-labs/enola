package orphans

import (
	"strings"
	"testing"
)

// A symbol only test code reaches is not dead (something calls it) and not used
// (the application never does). It is its own class, and each way test use can
// reach the index has to land on the test side.
func TestTestOnlyClassification(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"Orders::Cleanup","file":"app/orders/cleanup.rb","props":{"symbol_kind":"class"}}
{"kind":"symbol","name":"Orders::Cleanup.run","file":"app/orders/cleanup.rb","props":{"symbol_kind":"method"},"relations":[{"kind":"calls","target":"Orders::Archive.store"}]}
{"kind":"symbol","name":"Orders::Archive","file":"app/orders/archive.rb","props":{"symbol_kind":"class"}}
{"kind":"symbol","name":"Orders::Archive.store","file":"app/orders/archive.rb","props":{"symbol_kind":"method"}}
{"kind":"symbol","name":"Orders::Report","file":"app/orders/report.rb","props":{"symbol_kind":"class"}}
{"kind":"symbol","name":"Orders::Report.build","file":"app/orders/report.rb","props":{"symbol_kind":"method"}}
{"kind":"symbol","name":"orders.format_total","file":"orders/totals.py","props":{"symbol_kind":"function"}}
{"kind":"symbol","name":"orders.unused","file":"orders/totals.py","props":{"symbol_kind":"function"}}
{"kind":"symbol","name":"tests/test_totals.check","file":"tests/test_totals.py","props":{"symbol_kind":"function"},"relations":[{"kind":"calls","target":"orders.format_total"}]}
{"kind":"symbol","name":"Billing::Job.perform","file":"app/billing/job.rb","props":{"symbol_kind":"method"},"relations":[{"kind":"calls","target":"Orders::Report.build"}]}
{"kind":"test_ref","name":"spec/orders/cleanup_spec.rb","file":"spec/orders/cleanup_spec.rb","relations":[{"kind":"calls","target":"Orders::Cleanup.run"}]}
{"kind":"test_ref","name":"spec/orders/report_spec.rb","file":"spec/orders/report_spec.rb","relations":[{"kind":"calls","target":"Orders::Report.build"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))

	cases := []struct {
		name  string
		class string // "" means not reported
	}{
		{"Orders::Cleanup.run", classTestOnly}, // only a spec calls it
		{"Orders::Cleanup", classTestOnly},     // its only member's use is test use
		{"Orders::Archive.store", ""},          // called by Cleanup.run, a production symbol
		{"Orders::Report.build", ""},           // a spec and production both call it
		{"Orders::Report", ""},                 // its member is used in production
		{"orders.format_total", classTestOnly}, // called from a symbol declared in a test tree
		{"orders.unused", classIsolated},       // nothing calls it
		{"Billing::Job.perform", classUnreferenced},
	}
	for _, c := range cases {
		o, reported := got[c.name]
		switch {
		case c.class == "" && reported:
			t.Errorf("%s: reported as %s, want not reported", c.name, o.Class)
		case c.class != "" && !reported:
			t.Errorf("%s: not reported, want %s", c.name, c.class)
		case c.class != "" && o.Class != c.class:
			t.Errorf("%s: class %s, want %s", c.name, o.Class, c.class)
		}
	}
	if o := got["Orders::Cleanup.run"]; !strings.Contains(o.Note, "test code") {
		t.Errorf("a test-only finding must say so in its note, got %q", o.Note)
	}
}

func TestTestOnlyModes(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"orders.covered","file":"orders/a.py","props":{"symbol_kind":"function"}}
{"kind":"symbol","name":"orders.dead","file":"orders/a.py","props":{"symbol_kind":"function"}}
{"kind":"test_ref","name":"tests/test_a.py","relations":[{"kind":"calls","target":"orders.covered"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	cases := []struct {
		opts options
		want []string
	}{
		{options{Mode: "both", Visibility: "all"}, []string{"orders.covered", "orders.dead"}},
		{options{Mode: modeTestOnly, Visibility: "all"}, []string{"orders.covered"}},
		{options{Mode: classIsolated, Visibility: "all"}, []string{"orders.dead"}},
		{options{Mode: classUnreferenced, Visibility: "all"}, nil},
		{options{Mode: "both", Visibility: "all", deadOnly: true}, []string{"orders.dead"}},
	}
	for _, c := range cases {
		got := classify(syms, refs, c.opts)
		sortOrphans(got)
		var names []string
		for _, o := range got {
			names = append(names, o.Name)
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("mode=%s deadOnly=%v: got %v, want %v", c.opts.Mode, c.opts.deadOnly, names, c.want)
		}
	}

	sum := summarize(classify(syms, refs, options{Mode: "both", Visibility: "all"}), 2, 2, "both")
	if sum.TestOnly != 1 || sum.Isolated != 1 || sum.Unreferenced != 0 {
		t.Errorf("summary split: %+v", sum)
	}
}

// The dead-code explainer reports test-only symbols, but as a different finding:
// labelled, below the dead ones in confidence, and after them in the budget.
func TestExplainerLabelsTestOnly(t *testing.T) {
	insights := orphansToInsights([]Orphan{
		{Name: "orders.covered", Kind: "function", Class: classTestOnly, Confidence: confHigh, File: "orders/a.py"},
		{Name: "orders.dead", Kind: "function", Class: classIsolated, Confidence: confMedium, File: "orders/a.py"},
	})
	if len(insights) != 2 {
		t.Fatalf("got %d insights, want 2", len(insights))
	}
	if !strings.HasPrefix(insights[0].Title, "Dead code candidate") {
		t.Errorf("dead code must rank first, got %q", insights[0].Title)
	}
	testOnly := insights[1]
	if !strings.HasPrefix(testOnly.Title, "Test-only code candidate") {
		t.Errorf("test-only finding not labelled: %q", testOnly.Title)
	}
	if testOnly.Confidence >= insights[0].Confidence {
		t.Errorf("test-only confidence %.2f must sit below dead code's %.2f", testOnly.Confidence, insights[0].Confidence)
	}
}

func TestReferencedInProductionIgnoresTestsAndSelf(t *testing.T) {
	cases := []struct {
		name string
		add  func(r refIndex)
		prod bool
		any  bool
	}{
		{"test only", func(r refIndex) { r.addTest("x", "spec/x_spec.rb") }, false, true},
		{"self only", func(r refIndex) { r.add("x", "x") }, false, false},
		{"self and a test", func(r refIndex) { r.add("x", "x"); r.addTest("x", "spec/x_spec.rb") }, false, true},
		{"production", func(r refIndex) { r.addTest("x", "spec/x_spec.rb"); r.add("x", "Other.y") }, true, true},
	}
	for _, c := range cases {
		r := make(refIndex)
		c.add(r)
		if got := r["x"].referencedInProduction("x"); got != c.prod {
			t.Errorf("%s: referencedInProduction=%v, want %v", c.name, got, c.prod)
		}
		if got := r["x"].referenced("x"); got != c.any {
			t.Errorf("%s: referenced=%v, want %v", c.name, got, c.any)
		}
	}
}

// A symbol a framework invokes (a routed action, a registry contract) is an entry
// point however its references read, and so is a class entered through one of its
// own members: neither is dead or test-only because only specs name it.
func TestFrameworkRootsAreEntryPoints(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"Shop::OrdersController","file":"app/controllers/shop/orders_controller.rb","props":{"symbol_kind":"class"}}
{"kind":"symbol","name":"Shop::OrdersController#index","file":"app/controllers/shop/orders_controller.rb","props":{"symbol_kind":"method","root":"route"}}
{"kind":"symbol","name":"Orders::Purge","file":"app/orders/purge.rb","props":{"symbol_kind":"class"}}
{"kind":"symbol","name":"Orders::Purge.run","file":"app/orders/purge.rb","props":{"symbol_kind":"function","root":"registry"}}
{"kind":"symbol","name":"Orders::Unrouted","file":"app/orders/unrouted.rb","props":{"symbol_kind":"class"}}
{"kind":"test_ref","name":"spec/shop/orders_controller_spec.rb","file":"spec/shop/orders_controller_spec.rb","relations":[{"kind":"calls","target":"Shop::OrdersController"}]}
{"kind":"test_ref","name":"spec/orders/purge_spec.rb","file":"spec/orders/purge_spec.rb","relations":[{"kind":"calls","target":"Orders::Purge.run"}]}
{"kind":"test_ref","name":"spec/orders/unrouted_spec.rb","file":"spec/orders/unrouted_spec.rb","relations":[{"kind":"calls","target":"Orders::Unrouted"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	for _, name := range []string{"Shop::OrdersController", "Shop::OrdersController#index", "Orders::Purge", "Orders::Purge.run"} {
		if o, reported := got[name]; reported {
			t.Errorf("%s: reported as %s; a framework root is an entry point", name, o.Class)
		}
	}
	if o := got["Orders::Unrouted"]; o.Class != classTestOnly {
		t.Errorf("Orders::Unrouted: class %q, want %s (no root, only a spec)", o.Class, classTestOnly)
	}
	all := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, reported := all["Shop::OrdersController#index"]; !reported {
		t.Error("include_entrypoints must still list a rooted symbol")
	}
}

// A published library's exported API is written for the projects that install it,
// so tests being its only in-repo callers is the normal state, not a lead. The
// unexported code inside it, and the same API in an unpublished module, still are.
func TestTestOnlySkipsPublishedLibraryAPI(t *testing.T) {
	jsonl := `{"kind":"module","name":"src/Lib","file":"src/Lib/Lib.csproj","props":{"published":true}}
{"kind":"module","name":"src/Lib/Protocol","file":"src/Lib/Protocol"}
{"kind":"module","name":"src/App","file":"src/App"}
{"kind":"symbol","name":"src/Lib/Protocol.Result","file":"src/Lib/Protocol/Result.cs","props":{"symbol_kind":"class","exported":true},"relations":[{"kind":"declares","target":"src/Lib/Protocol"}]}
{"kind":"symbol","name":"src/Lib/Protocol.helper","file":"src/Lib/Protocol/Result.cs","props":{"symbol_kind":"function","exported":false},"relations":[{"kind":"declares","target":"src/Lib/Protocol"}]}
{"kind":"symbol","name":"src/App.Options","file":"src/App/Options.cs","props":{"symbol_kind":"class","exported":true},"relations":[{"kind":"declares","target":"src/App"}]}
{"kind":"test_ref","name":"tests/LibTests.cs","file":"tests/LibTests.cs","relations":[{"kind":"calls","target":"src/Lib/Protocol.Result"},{"kind":"calls","target":"src/Lib/Protocol.helper"},{"kind":"calls","target":"src/App.Options"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))

	if o, reported := got["src/Lib/Protocol.Result"]; reported {
		t.Errorf("exported API of a published module (found by walking up from a subfolder) reported as %s", o.Class)
	}
	if got["src/Lib/Protocol.helper"].Class != classTestOnly {
		t.Errorf("unexported code in a published module: want %s, got %+v", classTestOnly, got["src/Lib/Protocol.helper"])
	}
	if got["src/App.Options"].Class != classTestOnly {
		t.Errorf("exported code in an unpublished module: want %s, got %+v", classTestOnly, got["src/App.Options"])
	}
}

// A generator's templates are copied into applications and never run where they
// live, so they are not orphans; a templates/ directory elsewhere is ordinary code.
func TestGeneratorTemplatesAreNotOrphans(t *testing.T) {
	for p, want := range map[string]bool{
		"lib/generators/templates/controllers/sessions_controller.rb": true,
		"lib/generators/devise/templates/devise.rb":                   true,
		"app/views/templates/mailer.rb":                               false,
		"lib/generators/devise/devise_generator.rb":                   false,
	} {
		if got := isFrameworkLoadedPath(p); got != want {
			t.Errorf("isFrameworkLoadedPath(%q) = %v, want %v", p, got, want)
		}
	}
}
