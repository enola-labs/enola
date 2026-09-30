package constraints

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The shape finding 0010 was written about: lib/express.js requires
// ./application, and the extractor measures that as an imports edge onto the
// path lib/application — which names no member fact, because the file itself is
// measured as lib/application.js and file_ref is not a member kind. The rule
// yielded nothing on a dependency any reader of the file can see, and read
// exactly like the reverse rule, which is silent because the dependency is
// genuinely absent.
func TestExplain_FileGranularImportTargetGroundsInItsComponent(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("entry", "lib/express.js"),
		componentIntent("support", "lib/application.js"),
		ruleIntent("entry-avoids-support", "entry", "support", "imports", "the entry file must not reach the support files"),
		facts.Fact{Kind: facts.KindDependency, Name: "lib -> lib/application", File: "lib/express.js",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "lib/application"}}},
		facts.Fact{Kind: facts.KindFileRef, Name: "lib/application.js", File: "lib/application.js"},
		facts.Fact{Kind: facts.KindFileRef, Name: "lib/express.js", File: "lib/express.js"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var verdict *facts.Insight
	for i := range insights {
		if strings.HasPrefix(insights[i].Title, "Constraint entry-avoids-support violated") {
			verdict = &insights[i]
		}
	}
	if verdict == nil {
		t.Fatalf("the import is measured and the target names a measured file, so the rule must verdict: %+v", insights)
	}
	if verdict.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0: the membership is the component's own match glob", verdict.Confidence)
	}
}

// The reverse direction stays silent for the reason it always did — there is no
// such import — so the fallback cannot be answering "yes" to everything.
func TestExplain_FileGranularFallbackDoesNotInventTheReverseEdge(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("entry", "lib/express.js"),
		componentIntent("support", "lib/application.js"),
		ruleIntent("support-avoids-entry", "support", "entry", "imports", "the support files must not reach back"),
		facts.Fact{Kind: facts.KindDependency, Name: "lib -> lib/application", File: "lib/express.js",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "lib/application"}}},
		facts.Fact{Kind: facts.KindFileRef, Name: "lib/application.js", File: "lib/application.js"},
		facts.Fact{Kind: facts.KindFileRef, Name: "lib/express.js", File: "lib/express.js"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, insight := range insights {
		if strings.Contains(insight.Title, "support-avoids-entry violated") {
			t.Fatalf("no such import is measured, yet the rule verdicted: %+v", insight)
		}
	}
}

// A target that names neither a fact nor a measured file — a package outside the
// tree — is skipped, and the skip is counted. Silence about it would read as
// compliance, which is the failure the advisory exists to prevent.
func TestExplain_UngroundableImportTargetsAreCounted(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("entry", "lib/express.js"),
		componentIntent("support", "lib/application.js"),
		ruleIntent("entry-avoids-support", "entry", "support", "imports", "the entry file must not reach the support files"),
		facts.Fact{Kind: facts.KindDependency, Name: "lib -> serve-static", File: "lib/express.js",
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: "serve-static"}}},
		facts.Fact{Kind: facts.KindFileRef, Name: "lib/application.js", File: "lib/application.js"},
		facts.Fact{Kind: facts.KindFileRef, Name: "lib/express.js", File: "lib/express.js"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var advisory *facts.Insight
	for i := range insights {
		if strings.Contains(insights[i].Title, "reached no verdict on 1 imports target") {
			advisory = &insights[i]
		}
		if strings.Contains(insights[i].Title, "entry-avoids-support violated") {
			t.Fatalf("an unresolvable target must never be guessed into a breach: %+v", insights[i])
		}
	}
	if advisory == nil {
		t.Fatalf("the skipped target must be counted, not dropped: %+v", insights)
	}
	if !strings.Contains(advisory.Description, "serve-static") {
		t.Errorf("the advisory must name the sample it counted, got: %q", advisory.Description)
	}
	if advisory.Confidence >= 1.0 {
		t.Errorf("confidence = %v, want a skip advisory below the gate floor", advisory.Confidence)
	}
}

// TestGrounding_ResolvesPythonImportsByTheImportersLanguage: the Python
// extractor measures `import inventory.stock` as the path inventory/stock. It
// grounds onto the module file or the package's __init__.py, and only for a
// Python importer: a TypeScript "./stock" never means stock.py.
func TestGrounding_ResolvesPythonImportsByTheImportersLanguage(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindFileRef, Name: "inventory/stock.py", File: "inventory/stock.py"},
		facts.Fact{Kind: facts.KindFileRef, Name: "payments/__init__.py", File: "payments/__init__.py"},
	)
	g := newGrounding(store, nil)
	if got, ok := g.resolve("inventory/stock", "", "orders/service.py"); !ok || got != "inventory/stock.py" {
		t.Errorf("resolve(inventory/stock) from Python = %q/%v, want the module file", got, ok)
	}
	if got, ok := g.resolve("payments", "", "orders/service.py"); !ok || got != "payments/__init__.py" {
		t.Errorf("resolve(payments) from Python = %q/%v, want the package's __init__.py", got, ok)
	}
	if _, ok := g.resolve("inventory/stock", "", "web/app.ts"); ok {
		t.Error("a TypeScript import must not ground onto a Python file")
	}
}

// Directory imports resolve to their index file, and the resolution is scoped to
// the importing repository: a file measured in one repo can never ground a
// target written in another.
func TestGrounding_ResolvesIndexAndStaysInsideTheRepo(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		facts.Fact{Kind: facts.KindFileRef, Repo: "a", Name: "src/widgets/index.ts", File: "a/src/widgets/index.ts"},
		facts.Fact{Kind: facts.KindFileRef, Repo: "b", Name: "src/other.ts", File: "b/src/other.ts"},
	)
	g := newGrounding(store, nil)
	if got, ok := g.resolve("src/widgets", "a", "a/src/app.ts"); !ok || got != "a/src/widgets/index.ts" {
		t.Errorf("resolve(src/widgets, a) = %q/%v, want the index file", got, ok)
	}
	if _, ok := g.resolve("src/other", "a", "a/src/app.ts"); ok {
		t.Error("a file measured in repo b must not ground a target written in repo a")
	}
	if _, ok := g.resolve("src/missing", "a", "a/src/app.ts"); ok {
		t.Error("a target naming no measured file must not resolve")
	}
}

// markupBinding is the shape the Ruby extractor's Stimulus pass emits for
// `data-controller="dropdown"` in an ERB view: a dependency fact filed under
// the VIEW, carrying resolution_level=markup-declared, whose depends_on target
// is the controller FILE the naming convention resolved — a path, never a fact
// name, exactly like a file-granular import target.
func markupBinding(view, controllerFile string) facts.Fact {
	return facts.Fact{Kind: facts.KindDependency, Name: "stimulus-binding: " + view + " -> dropdown", File: view,
		Props:     map[string]any{"framework": "stimulus", "resolution_level": "markup-declared"},
		Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: controllerFile}}}
}

// The Stimulus half of finding 0007 measured the binding and finding 0010's
// grounding admitted only imports edges, so a rule over the markup-declared
// depends_on edge held vacuously: the target is a controller file path, which
// names no member fact. It must now ground on the component that file belongs
// to, exactly as an import target does.
func TestExplain_MarkupDeclaredBindingGroundsInItsComponent(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("views", "app/views/**"),
		componentIntent("js-controllers", "app/javascript/controllers/**"),
		ruleIntent("views-avoid-controllers", "views", "js-controllers", "depends_on", "a view must not bind a controller directly"),
		markupBinding("app/views/jobs/show.html.erb", "app/javascript/controllers/dropdown_controller.js"),
		facts.Fact{Kind: facts.KindFileRef, Name: "app/javascript/controllers/dropdown_controller.js", File: "app/javascript/controllers/dropdown_controller.js"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var verdict *facts.Insight
	for i := range insights {
		if strings.HasPrefix(insights[i].Title, "Constraint views-avoid-controllers violated") {
			verdict = &insights[i]
		}
	}
	if verdict == nil {
		t.Fatalf("the binding is measured and its target names a measured file, so the rule must verdict: %+v", insights)
	}
	if verdict.Confidence != 1.0 {
		t.Errorf("confidence = %v, want 1.0: the membership is the component's own match glob", verdict.Confidence)
	}
}

// A depends_on edge the extractors did NOT declare in markup keeps naming a
// symbol — an association, a Terraform address, a render target — so it must not
// be resolved as a path. The literal-declared render edge is the near neighbor
// that would break first: its target IS a file, and admitting it is a separate
// decision this predicate does not make.
func TestExplain_NonMarkupDependsOnTargetIsNotGrounded(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("views", "app/views/**"),
		componentIntent("partials", "app/views/shared/**"),
		ruleIntent("views-avoid-partials", "views", "partials", "depends_on", "a view must not render a shared partial"),
		facts.Fact{Kind: facts.KindDependency, Name: "render: app/views/jobs/show.html.erb -> shared/help", File: "app/views/jobs/show.html.erb",
			Props:     map[string]any{"framework": "rails", "resolution_level": "literal-declared"},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "app/views/shared/_help.html.erb"}}},
		facts.Fact{Kind: facts.KindFileRef, Name: "app/views/shared/_help.html.erb", File: "app/views/shared/_help.html.erb"},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, insight := range insights {
		if strings.HasPrefix(insight.Title, "Constraint views-avoid-partials violated") {
			t.Fatalf("a depends_on edge that was not declared in markup must not ground as a path: %+v", insight)
		}
	}
}

// The predicate itself, at the unit: what it admits and what it refuses. A
// markup-declared fact's OTHER edge kinds stay out too — the level says how the
// fact was resolved, never that every edge it carries is a path.
func TestPathTargetEdge_AdmitsImportsAndMarkupDependsOnOnly(t *testing.T) {
	markup := facts.Fact{Props: map[string]any{"resolution_level": "markup-declared"}}
	literal := facts.Fact{Props: map[string]any{"resolution_level": "literal-declared"}}
	plain := facts.Fact{}
	cases := []struct {
		name string
		rel  facts.Relation
		from facts.Fact
		want bool
	}{
		{"an imports edge is always a path target", facts.Relation{Kind: facts.RelImports}, plain, true},
		{"a markup-declared depends_on edge is a path target", facts.Relation{Kind: facts.RelDependsOn}, markup, true},
		{"a literal-declared depends_on edge is not", facts.Relation{Kind: facts.RelDependsOn}, literal, false},
		{"an unlevelled depends_on edge is not", facts.Relation{Kind: facts.RelDependsOn}, plain, false},
		{"a markup-declared calls edge is not", facts.Relation{Kind: facts.RelCalls}, markup, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathTargetEdge(tc.rel, tc.from); got != tc.want {
				t.Errorf("pathTargetEdge = %v, want %v", got, tc.want)
			}
		})
	}
}

// The skip advisory reached the markup edge with an importer's diagnosis: a
// binding whose target resolved to nothing is an IN-TREE application path the
// naming convention produced, so calling it a package outside the tree names the
// wrong thing, and neither remedy an import's residue takes can reach it —
// ignoring third-party packages does not apply, and no snapshot is wide enough
// to contain a path the tree already holds.
func TestExplain_MarkupSkipAdvisoryDiagnosesAnInTreePath(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("views", "app/views/**"),
		componentIntent("js-controllers", "app/javascript/controllers/**"),
		ruleIntent("views-avoid-controllers", "views", "js-controllers", "depends_on", "a view must not bind a controller directly"),
		markupBinding("app/views/dashboard/show.html.erb", "app/javascript/controllers/admin/menu_controller.js"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var advisory *facts.Insight
	for i := range insights {
		if strings.Contains(insights[i].Title, "reached no verdict on 1 depends_on target") {
			advisory = &insights[i]
		}
		if strings.Contains(insights[i].Title, "views-avoid-controllers violated") {
			t.Fatalf("a target naming no measured file must never be guessed into a breach: %+v", insights[i])
		}
	}
	if advisory == nil {
		t.Fatalf("the skipped binding must be counted, not dropped: %+v", insights)
	}
	if strings.Contains(advisory.Description, "package outside the tree") {
		t.Errorf("a markup-declared target is in-tree by construction, got: %q", advisory.Description)
	}
	if !strings.Contains(advisory.Description, "in-tree path the declaring markup's naming convention produced") {
		t.Errorf("the advisory must say what the target actually is, got: %q", advisory.Description)
	}
	for _, action := range advisory.Actions {
		if strings.Contains(action, "third-party packages") || strings.Contains(action, "Widen the snapshot") {
			t.Errorf("an import's remedy cannot reach an in-tree path, got action: %q", action)
		}
	}
	if len(advisory.Actions) != 2 {
		t.Fatalf("actions = %+v, want the two a markup-declared residue has", advisory.Actions)
	}
	if !strings.Contains(advisory.Actions[0], "Fix the binding") || !strings.Contains(advisory.Actions[1], "Extend extraction") {
		t.Errorf("actions = %+v, want the binding and the extraction levers", advisory.Actions)
	}
}

// The imports diagnosis is the one this advisory was written for and must not
// move: a third-party package IS the common residue there, and the snapshot IS
// the lever. Two situations, two diagnoses.
func TestGroundSkipDiagnosis_ImportsAndMarkupDiagnoseDifferently(t *testing.T) {
	importsCause, importsActions := groundSkipDiagnosis(facts.RelImports)
	if importsCause != "typically a package outside the tree" {
		t.Errorf("imports cause = %q, want the package-outside-the-tree diagnosis", importsCause)
	}
	if len(importsActions) != 2 || !strings.Contains(importsActions[1], "Widen the snapshot") {
		t.Errorf("imports actions = %+v, want the third-party and snapshot levers", importsActions)
	}
	markupCause, markupActions := groundSkipDiagnosis(facts.RelDependsOn)
	if markupCause == importsCause {
		t.Errorf("both edge kinds got one diagnosis: %q", markupCause)
	}
	if strings.Contains(markupCause, "outside the tree") {
		t.Errorf("markup cause = %q, want an in-tree diagnosis", markupCause)
	}
	for i, action := range markupActions {
		if action == importsActions[i] {
			t.Errorf("markup remedy %d is the import's: %q", i, action)
		}
	}
}

// A module reopened in two files is a member of both components by name. A
// producer that says which file a read's target is defined in keeps the edge
// off the reopening it never touched: `Foo::VERSION` read from the formatters
// is a dependency on the support file that defines VERSION, not on the cli
// file that reopens Foo.
func TestExplain_TargetFileKeepsAReadOffTheReopening(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("formatters", "lib/formatters/**"),
		componentIntent("cli", "lib/cli.rb"),
		componentIntent("support", "lib/version.rb"),
		ruleIntent("formatters-avoid-cli", "formatters", "cli", "depends_on", "printing must not reach the command line"),
		facts.Fact{Kind: facts.KindSymbol, Name: "Foo", File: "lib/cli.rb"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Foo", File: "lib/version.rb"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Foo::VERSION", File: "lib/version.rb"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Foo::Formatters::Sarif", File: "lib/formatters/sarif.rb"},
		facts.Fact{Kind: facts.KindDependency, Name: "rubydex-ref: Foo::Formatters::Sarif -> Foo::VERSION", File: "lib/formatters/sarif.rb",
			Props:     map[string]any{"resolution_level": "resolved", facts.PropTargetFile: "lib/version.rb"},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "Foo::VERSION"}}},
		facts.Fact{Kind: facts.KindDependency, Name: "rubydex-ref: Foo::Formatters::Sarif -> Foo", File: "lib/formatters/sarif.rb",
			Props:     map[string]any{"resolution_level": "resolved", facts.PropTargetFile: "lib/version.rb"},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "Foo"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range insights {
		if strings.HasPrefix(in.Title, "Constraint formatters-avoid-cli violated") {
			t.Fatalf("the reads land in lib/version.rb, which is not the cli: %s", in.Title)
		}
	}
}

// The same read without a carried file resolves by name, as every other
// producer's edges do, and the reopening still answers: the preference is the
// producer's statement, never a rule the consumer invents from name shapes.
func TestExplain_WithoutATargetFileTheNameStillResolvesToEveryReopening(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		componentIntent("formatters", "lib/formatters/**"),
		componentIntent("cli", "lib/cli.rb"),
		ruleIntent("formatters-avoid-cli", "formatters", "cli", "depends_on", "printing must not reach the command line"),
		facts.Fact{Kind: facts.KindSymbol, Name: "Foo", File: "lib/cli.rb"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Foo", File: "lib/version.rb"},
		facts.Fact{Kind: facts.KindSymbol, Name: "Foo::Formatters::Sarif", File: "lib/formatters/sarif.rb"},
		facts.Fact{Kind: facts.KindDependency, Name: "ref: Foo::Formatters::Sarif -> Foo", File: "lib/formatters/sarif.rb",
			Props:     map[string]any{"resolution_level": "resolved"},
			Relations: []facts.Relation{{Kind: facts.RelDependsOn, Target: "Foo"}}},
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range insights {
		if strings.HasPrefix(in.Title, "Constraint formatters-avoid-cli violated") {
			return
		}
	}
	t.Fatal("a name-only edge onto a reopened module resolves by name, as before")
}
