package orphans

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/mcputil"
)

// TestClassify_ExcludesTestSupportPackages guards against mis-reporting test
// infrastructure (mocks, testutils) as dead: _test.go files are excluded from the
// snapshot, so a mock's only callers (tests) are invisible. Such code must be
// excluded as a cleanup candidate by default, like _test.go symbols.
func TestClassify_ExcludesTestSupportPackages(t *testing.T) {
	syms := []symInput{
		{Name: "internal/mocks/services.NewMockBlogHandlerServices", Kind: facts.SymbolFunc,
			File: "internal/mocks/services/blog_mock.go", Package: "internal/mocks/services", Exported: true},
		{Name: "internal/testutils.NewHTTPTestHelper", Kind: facts.SymbolFunc,
			File: "internal/testutils/http.go", Package: "internal/testutils", Exported: true},
		{Name: "internal/domain/user.deadHelper", Kind: facts.SymbolFunc,
			File: "internal/domain/user/service.go", Package: "internal/domain/user", Exported: false},
	}
	got := classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"})

	names := orphansByName(got)
	if _, ok := names["internal/mocks/services.NewMockBlogHandlerServices"]; ok {
		t.Error("mock factory in internal/mocks should be excluded as a cleanup candidate")
	}
	if _, ok := names["internal/testutils.NewHTTPTestHelper"]; ok {
		t.Error("test helper in internal/testutils should be excluded as a cleanup candidate")
	}
	if _, ok := names["internal/domain/user.deadHelper"]; !ok {
		t.Error("genuine production orphan should still be reported")
	}

	// With include_tests=true, test-support code is brought back in.
	withTests := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all", IncludeTests: true}))
	if _, ok := withTests["internal/mocks/services.NewMockBlogHandlerServices"]; !ok {
		t.Error("include_tests=true should include test-support packages")
	}
}

// TestClassify_ExcludesAndroidTestSourceSets checks that test-support helpers in
// Android/KMP instrumented-test source sets (src/androidTest, commonTest, …) are
// not reported: their only callers are test files, which are excluded from the
// snapshot, so they would otherwise surface as high-confidence dead code.
func TestClassify_ExcludesAndroidTestSourceSets(t *testing.T) {
	syms := []symInput{
		{Name: "app/src/androidTest/java/com/example/app/ui.runOnUiThreadAndWait", Kind: facts.SymbolFunc,
			File: "app/src/androidTest/java/com/example/app/ui/TestUtil.kt", Package: "app/src/androidTest/java/com/example/app/ui", Exported: true},
		{Name: "shared/src/commonTest/kotlin/fixtures.buildUser", Kind: facts.SymbolFunc,
			File: "shared/src/commonTest/kotlin/fixtures/Users.kt", Package: "shared/src/commonTest/kotlin/fixtures", Exported: true},
		{Name: "app/src/main/java/com/example/app/ui/common.deadExtension", Kind: facts.SymbolFunc,
			File: "app/src/main/java/com/example/app/ui/common/StringExt.kt", Package: "app/src/main/java/com/example/app/ui/common", Exported: true},
	}
	names := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"}))
	for _, n := range []string{
		"app/src/androidTest/java/com/example/app/ui.runOnUiThreadAndWait",
		"shared/src/commonTest/kotlin/fixtures.buildUser",
	} {
		if _, ok := names[n]; ok {
			t.Errorf("%s is in a test source set and should be excluded from cleanup candidates", n)
		}
	}
	if _, ok := names["app/src/main/java/com/example/app/ui/common.deadExtension"]; !ok {
		t.Error("a genuine production (src/main) top-level function should still be reported")
	}
}

// TestOrphansToInsights_PrioritizesActionable guards the explainer-prioritization
// fix: when the capped individual-insight budget is full, the safest, highest-
// signal candidates (high-confidence + unexported) must be surfaced individually
// rather than buried in the rollup behind alphabetically-earlier exported symbols.
func TestOrphansToInsights_PrioritizesActionable(t *testing.T) {
	var orphans []Orphan
	for i := 0; i < maxIndividualInsights; i++ { // fill the budget with exported high-conf funcs
		orphans = append(orphans, Orphan{
			Name: fmt.Sprintf("internal/aaa.Fn%02d", i), Kind: facts.SymbolFunc,
			Class: classIsolated, Confidence: confHigh, Package: "internal/aaa", Exported: true,
		})
	}
	// The safest cleanup — unexported high-conf — but in an alphabetically-last package.
	orphans = append(orphans, Orphan{
		Name: "internal/zzz.deadHelper", Kind: facts.SymbolFunc,
		Class: classIsolated, Confidence: confHigh, Package: "internal/zzz", Exported: false,
	})

	insights := orphansToInsights(orphans)

	var sawActionable, sawRollup bool
	for _, in := range insights {
		if strings.Contains(in.Title, "deadHelper") {
			sawActionable = true
		}
		if strings.Contains(in.Title, "Additional dead-code candidates") {
			sawRollup = true
		}
	}
	if !sawActionable {
		t.Fatal("unexported high-confidence orphan was buried in the rollup instead of surfaced individually")
	}
	if !sawRollup {
		t.Fatal("expected a rollup insight for the candidate pushed out of the capped budget")
	}
}

// refsFrom builds a reference index the same way collect() does: each (source →
// targets) pair contributes both the full target and its last segment.
func refsFrom(pairs map[string][]string) map[string]map[string]struct{} {
	refs := make(map[string]map[string]struct{})
	for source, targets := range pairs {
		for _, t := range targets {
			addRef(refs, t, source)
			if seg := lastSeg(t); seg != t {
				addRef(refs, seg, source)
			}
		}
	}
	return refs
}

func orphansByName(os []Orphan) map[string]Orphan {
	m := make(map[string]Orphan, len(os))
	for _, o := range os {
		m[o.Name] = o
	}
	return m
}

func TestLastSeg(t *testing.T) {
	cases := map[string]string{
		"json.NewDecoder":              "NewDecoder",
		"handleError":                  "handleError",
		"HandlerV2.Create":             "Create",
		"HTTPRequestsTotal.WithLabels": "WithLabels",
		"PostsController#markdown_id":  "markdown_id", // Ruby instance method
		"Discourse::Utils.run":         "run",
	}
	for in, want := range cases {
		if got := lastSeg(in); got != want {
			t.Errorf("lastSeg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCandidateNames(t *testing.T) {
	cases := []struct {
		name, kind string
		want       []string
	}{
		{"internal/a/b/pkg.HandlerV2.Create", facts.SymbolMethod, []string{"Create", "HandlerV2.Create"}},
		{"internal/a/b/pkg.NewHandlerV2", facts.SymbolFunc, []string{"NewHandlerV2"}},
		{"internal/a/b/pkg.HandlerV2", "struct", []string{"HandlerV2"}},
		{".SecurityHeaders", facts.SymbolFunc, []string{"SecurityHeaders"}}, // root package
		// Ruby instance method "Class#method" must yield the bare method name.
		{"PostsController#markdown_id", facts.SymbolMethod, []string{"markdown_id", "PostsController.markdown_id"}},
		{"Ns::Cls#do_thing", facts.SymbolMethod, []string{"do_thing", "Ns::Cls.do_thing"}},
		// Ruby nested constant: match both the qualified name and the bare last segment.
		{"AdPlugin::HouseAdSerializer", facts.SymbolClass, []string{"AdPlugin::HouseAdSerializer", "HouseAdSerializer"}},
		// Top-level reopened class: leading "::" stripped, referenced as bare "Post".
		{"::Post", facts.SymbolClass, []string{"Post"}},
		{"plugins/x.::Assigner", facts.SymbolClass, []string{"Assigner"}},
	}
	for _, c := range cases {
		got := candidateNames(c.name, c.kind)
		if len(got) != len(c.want) {
			t.Errorf("candidateNames(%q,%q) = %v, want %v", c.name, c.kind, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("candidateNames(%q,%q) = %v, want %v", c.name, c.kind, got, c.want)
				break
			}
		}
	}
}

// TestSwiftCoordinatorDispatchContract locks the cross-module contract with the
// Swift extractor's dispatch fix. The extractor now emits member-call edges for
// coordinator routing (self?.method() and delegate?.method()) either resolved to a
// qualified dir.Type.method target (unique) or left as a bare short name
// (ambiguous). Both forms must clear the callee's false-positive, and Swift methods
// (receiver-bearing → SymbolMethod) must classify at LOW confidence, not the
// misleading HIGH "safest to remove first" tier that SymbolFunc used to yield.
func TestSwiftCoordinatorDispatchContract(t *testing.T) {
	syms := []symInput{
		{Name: "Feed.FeedSearchCoordinator.start", Kind: facts.SymbolMethod, Package: "Feed", UsageOut: true},
		// Called via a resolved qualified edge (self?.createContentButtonTapped()).
		{Name: "Feed.FeedSearchCoordinator.createContentButtonTapped", Kind: facts.SymbolMethod, Package: "Feed"},
		// Called via an ambiguous bare short-name edge (coordinator?.showRecoverPassword()).
		{Name: "Onb.PasswordCoordinator.showRecoverPassword", Kind: facts.SymbolMethod, Package: "Onb"},
		// Genuinely dead coordinator method — must still be flagged.
		{Name: "Feed.FeedSearchCoordinator.trulyDead", Kind: facts.SymbolMethod, Package: "Feed"},
	}
	refs := refsFrom(map[string][]string{
		"Feed.FeedSearchCoordinator.start": {
			"Feed.FeedSearchCoordinator.createContentButtonTapped", // resolved edge
			"showRecoverPassword", // ambiguous bare edge
		},
	})

	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))

	if _, ok := got["Feed.FeedSearchCoordinator.createContentButtonTapped"]; ok {
		t.Error("createContentButtonTapped is referenced by a resolved edge; should not be an orphan")
	}
	if _, ok := got["Onb.PasswordCoordinator.showRecoverPassword"]; ok {
		t.Error("showRecoverPassword is referenced by a bare short-name edge; should not be an orphan")
	}
	dead, ok := got["Feed.FeedSearchCoordinator.trulyDead"]
	if !ok {
		t.Fatal("trulyDead has no incoming edges; should still be flagged as an orphan")
	}
	if dead.Confidence != confLow {
		t.Errorf("Swift method confidence = %q, want %q (LOW/'verify each', not HIGH)", dead.Confidence, confLow)
	}
}

// A React component rendered via JSX or wired into a route table is referenced only
// through the extractor's file-scope reference edges (KindFileRef, matched by short
// name). Such a component must NOT be flagged; a genuinely-unrendered one still is,
// but at the downgraded verify-first confidence for dynamically-reached TS/JS code.
func TestClassify_ReactComponentViaFileRef(t *testing.T) {
	syms := []symInput{
		// Rendered as <UserCard/> — the file-ref pass records a "UserCard" reference.
		{Name: "client/components/user_card.UserCard", Kind: facts.SymbolFunc, Package: "client/components/user_card", Language: "typescript", WebComponent: "component", Exported: true},
		// Routed via `{ component: EventCalendar }` in a top-level route table.
		{Name: "client/containers/event_calendar.EventCalendar", Kind: facts.SymbolFunc, Package: "client/containers/event_calendar", Language: "typescript", WebComponent: "component", Exported: true},
		// Never rendered or routed anywhere → genuinely dead.
		{Name: "client/components/legacy_banner.LegacyBanner", Kind: facts.SymbolFunc, Package: "client/components/legacy_banner", Language: "typescript", WebComponent: "component", Exported: true},
	}
	// refSources as the KindFileRef fold would build them: short names of the
	// referenced components, sourced from the file that renders/routes them.
	refs := refsFrom(map[string][]string{
		"client/App.tsx":                 {"UserCard"},
		"client/modules/routes/feed.jsx": {"EventCalendar"},
	})

	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))

	if _, ok := got["client/components/user_card.UserCard"]; ok {
		t.Error("UserCard is rendered via JSX (file-ref short-name match); should not be an orphan")
	}
	if _, ok := got["client/containers/event_calendar.EventCalendar"]; ok {
		t.Error("EventCalendar is wired into a route table; should not be an orphan")
	}
	dead, ok := got["client/components/legacy_banner.LegacyBanner"]
	if !ok {
		t.Fatal("LegacyBanner is never rendered; should still be flagged as an orphan")
	}
	if dead.Confidence != confLow {
		t.Errorf("dead React component confidence = %q, want %q (verify-first, not HIGH)", dead.Confidence, confLow)
	}
}

// TestClassifyClasses checks the three outcomes: referenced (not an orphan),
// isolated (no in/out), and unreferenced (no in, has out).
func TestClassifyClasses(t *testing.T) {
	syms := []symInput{
		// Calls helper, and is itself called by mainFlow → referenced, not orphan.
		{Name: "pkg.usedFn", Kind: facts.SymbolFunc, Package: "pkg", UsageOut: true},
		// No incoming, no outgoing → isolated.
		{Name: "pkg.LonelyType", Kind: facts.SymbolStruct, Package: "pkg"},
		// No incoming, but calls helper → unreferenced.
		{Name: "pkg.deadButCalls", Kind: facts.SymbolFunc, Package: "pkg", UsageOut: true},
		// The helper that deadButCalls/usedFn invoke → referenced, not orphan.
		{Name: "pkg.helper", Kind: facts.SymbolFunc, Package: "pkg"},
	}
	refs := refsFrom(map[string][]string{
		"pkg.usedFn":        {"helper"},
		"pkg.deadButCalls":  {"helper"},
		"pkg.someRealEntry": {"usedFn"}, // marks usedFn as referenced
	})

	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := got["pkg.usedFn"]; ok {
		t.Error("usedFn is referenced; should not be an orphan")
	}
	if _, ok := got["pkg.helper"]; ok {
		t.Error("helper is referenced; should not be an orphan")
	}
	if o := got["pkg.LonelyType"]; o.Class != classIsolated {
		t.Errorf("LonelyType class = %q, want isolated", o.Class)
	}
	if o := got["pkg.deadButCalls"]; o.Class != classUnreferenced {
		t.Errorf("deadButCalls class = %q, want unreferenced", o.Class)
	}
}

// TestShortNameMatching checks that references via bare and Recv.Method forms
// both mark a symbol as used.
func TestShortNameMatching(t *testing.T) {
	syms := []symInput{
		{Name: "a/b/c.handleError", Kind: facts.SymbolFunc, Package: "a/b/c"},
		{Name: "a/b/c.HandlerV2.Create", Kind: facts.SymbolMethod, Package: "a/b/c"},
		{Name: "a/b/c.HandlerV2.Update", Kind: facts.SymbolMethod, Package: "a/b/c"},
	}
	refs := refsFrom(map[string][]string{
		"a/b/c.caller": {"handleError", "HandlerV2.Create"},
	})
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := got["a/b/c.handleError"]; ok {
		t.Error("handleError referenced by bare name; should not be orphan")
	}
	if _, ok := got["a/b/c.HandlerV2.Create"]; ok {
		t.Error("HandlerV2.Create referenced by Recv.Method; should not be orphan")
	}
	if _, ok := got["a/b/c.HandlerV2.Update"]; !ok {
		t.Error("HandlerV2.Update is unreferenced; should be an orphan")
	}
}

// TestSelfReferenceNotCounted verifies recursion does not mark a symbol as used.
func TestSelfReferenceNotCounted(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.recurse", Kind: facts.SymbolFunc, Package: "pkg", UsageOut: true},
	}
	refs := refsFrom(map[string][]string{
		"pkg.recurse": {"recurse"}, // calls itself only
	})
	got := classify(syms, refs, options{Mode: "both", Visibility: "all"})
	if len(got) != 1 || got[0].Name != "pkg.recurse" {
		t.Errorf("recursive-only function should still be an orphan, got %+v", got)
	}
}

// TestExclusions checks default exclusion of tests/entrypoints and the override
// flags.
func TestDataMembersExcludedByDefault(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.field", Kind: facts.SymbolVariable, Package: "pkg", File: "pkg/x.c"},
		{Name: "pkg.MAX", Kind: facts.SymbolConstant, Package: "pkg", File: "pkg/x.c"},
		{Name: "pkg.deadFn", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/x.c"},
	}
	refs := refsFrom(nil)

	// Default view: data members are dropped, the function orphan remains.
	def := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := def["pkg.field"]; ok {
		t.Error("variable data member should be excluded by default")
	}
	if _, ok := def["pkg.MAX"]; ok {
		t.Error("constant data member should be excluded by default")
	}
	if _, ok := def["pkg.deadFn"]; !ok {
		t.Error("unreferenced function should still be reported (no regression)")
	}

	// Explicit kind= opt-in still returns the data member.
	byKind := orphansByName(classify(syms, refs, options{
		Mode: "both", Visibility: "all", Kind: facts.SymbolVariable,
	}))
	if _, ok := byKind["pkg.field"]; !ok {
		t.Error("variable should be reported when explicitly requested via kind=variable")
	}
}

// TestFrameworkLoadedPathsExcluded checks that Rails migrations, the script/ and
// vendor/ trees are dropped by default (loaded/invoked by the framework, never
// referenced by name) but stay reachable via an explicit package= scope.
func TestFrameworkLoadedPathsExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "db/migrate.AddBookmarkNameIndex", Kind: facts.SymbolClass, Package: "db/migrate", File: "db/migrate/20200713071305_add_bookmark_name_index.rb"},
		{Name: "db/post_migrate.BackfillStuff", Kind: facts.SymbolClass, Package: "db/post_migrate", File: "db/post_migrate/20240101_backfill_stuff.rb"},
		{Name: "script/import_scripts.Smf2", Kind: facts.SymbolClass, Package: "script/import_scripts", File: "script/import_scripts/smf2.rb"},
		{Name: "vendor/holidays.Defs", Kind: facts.SymbolClass, Package: "vendor/holidays", File: "vendor/holidays/defs.rb"},
		{Name: "app/services.PostCreator", Kind: facts.SymbolClass, Package: "app/services", File: "app/services/post_creator.rb"},
	}
	refs := refsFrom(nil)

	def := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	for _, n := range []string{
		"db/migrate.AddBookmarkNameIndex", "db/post_migrate.BackfillStuff",
		"script/import_scripts.Smf2", "vendor/holidays.Defs",
	} {
		if _, ok := def[n]; ok {
			t.Errorf("%s is framework-loaded/non-app and should be excluded by default", n)
		}
	}
	if _, ok := def["app/services.PostCreator"]; !ok {
		t.Error("genuine app orphan should still be reported (no regression)")
	}

	// An explicit package= scope opts back in to inspect that tree.
	scoped := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all", Package: "db/migrate"}))
	if _, ok := scoped["db/migrate.AddBookmarkNameIndex"]; !ok {
		t.Error("migration should be reported when explicitly scoped via package=db/migrate")
	}
}

// TestKotlinFrameworkEntryPointsExcluded checks that Kotlin `override` callbacks
// (Android lifecycle, interface implementations) and Dagger/Hilt @Provides/@Binds
// methods — dispatched by the runtime/DI container, never referenced by name — are
// treated as entry points and excluded by default, while a plain unreferenced
// method is still reported.
func TestKotlinFrameworkEntryPointsExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "app/ui.MyView.onCreate", Kind: facts.SymbolMethod, Package: "app/ui", File: "app/ui/MyView.kt", Override: true},
		{Name: "app/di.MyModule.provideThing", Kind: facts.SymbolMethod, Package: "app/di", File: "app/di/MyModule.kt", DIProvider: true},
		{Name: "app/ui.MyActivity", Kind: facts.SymbolClass, Package: "app/ui", File: "app/ui/MyActivity.kt", AndroidComponent: "activity"},
		{Name: "app/ui.MyScreen", Kind: facts.SymbolFunc, Package: "app/ui", File: "app/ui/MyScreen.kt", AndroidComponent: "composable"},
		// A Repository is DI-created but an ordinary app class — a dead one is real
		// dead code, so it must NOT be swept up by the framework exclusion.
		{Name: "app/data.OrphanRepository", Kind: facts.SymbolClass, Package: "app/data", File: "app/data/OrphanRepository.kt", AndroidComponent: "repository"},
		{Name: "app/ui.MyView.plainDead", Kind: facts.SymbolMethod, Package: "app/ui", File: "app/ui/MyView.kt"},
	}
	refs := refsFrom(nil)

	def := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	for _, n := range []string{
		"app/ui.MyView.onCreate", "app/di.MyModule.provideThing",
		"app/ui.MyActivity", "app/ui.MyScreen",
	} {
		if _, ok := def[n]; ok {
			t.Errorf("%s is a framework/DI entry point and should be excluded by default", n)
		}
	}
	for _, n := range []string{"app/ui.MyView.plainDead", "app/data.OrphanRepository"} {
		if _, ok := def[n]; !ok {
			t.Errorf("%s is ordinary app code and should still be reported (no over-suppression)", n)
		}
	}

	// include_entrypoints opts back in to inspect framework/DI entry points.
	in := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, ok := in["app/ui.MyView.onCreate"]; !ok {
		t.Error("override callback should be reported when IncludeEntrypoints is set")
	}
}

// TestIOSFrameworkEntryPointsExcluded pins the ios_component rescue to the one
// role the SwiftUI runtime instantiates: the @main App. Every other role is
// constructed by app code — a View by its parent's body, a ViewModel by
// @StateObject, a ViewController by a coordinator — and those edges ARE tracked,
// so an unreferenced one is real dead code and must still be reported. Verified on
// my-golf-journal-ios and nebenan-iOS: 0 of 25 viewcontroller symbols are orphans,
// and each of the 6 uiview orphans is genuinely unused.
func TestIOSFrameworkEntryPointsExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "App.MyGolfJournalApp", Kind: facts.SymbolStruct, Package: "App", File: "App/MyGolfJournalApp.swift", IOSComponent: "swiftui_app"},
		// Constructed by app code, so a dead one is real dead code — these must NOT
		// be swept up by the entry-point exclusion (cf. OrphanRepository above).
		{Name: "UI.StrengthChips", Kind: facts.SymbolStruct, Package: "UI", File: "UI/StrengthChips.swift", IOSComponent: "swiftui_view"},
		{Name: "UI.AddEntryEditorState", Kind: facts.SymbolClass, Package: "UI", File: "UI/AddEntryView.swift", IOSComponent: "viewmodel"},
		{Name: "UI.FeedViewController", Kind: facts.SymbolClass, Package: "UI", File: "UI/FeedViewController.swift", IOSComponent: "viewcontroller"},
		{Name: "UI.ChristmasBannerCell", Kind: facts.SymbolClass, Package: "UI", File: "UI/ChristmasBannerCell.swift", IOSComponent: "uiview"},
	}
	refs := refsFrom(nil)

	def := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := def["App.MyGolfJournalApp"]; ok {
		t.Error("the @main SwiftUI App is instantiated by the runtime and should be excluded by default")
	}
	for _, n := range []string{
		"UI.StrengthChips", "UI.AddEntryEditorState",
		"UI.FeedViewController", "UI.ChristmasBannerCell",
	} {
		if _, ok := def[n]; !ok {
			t.Errorf("%s is constructed by app code and should still be reported (no over-suppression)", n)
		}
	}

	// include_entrypoints opts back in to inspect the App entry point.
	in := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, ok := in["App.MyGolfJournalApp"]; !ok {
		t.Error("the SwiftUI App should be reported when IncludeEntrypoints is set")
	}
}

// TestCollectReadsIOSComponent covers the prop plumbing classify() cannot: collect()
// must lift props["ios_component"] onto symInput, or the rescue never fires on a
// real snapshot.
func TestCollectReadsIOSComponent(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"App.MyGolfJournalApp","file":"App/MyGolfJournalApp.swift","props":{"symbol_kind":"struct","ios_component":"swiftui_app"},"relations":[{"kind":"declares","target":"App"}]}
{"kind":"symbol","name":"UI.StrengthChips","file":"UI/StrengthChips.swift","props":{"symbol_kind":"struct","ios_component":"swiftui_view"},"relations":[{"kind":"declares","target":"UI"}]}
`
	syms, refSources := collectFromJSONL(t, jsonl)

	byName := map[string]symInput{}
	for _, s := range syms {
		byName[s.Name] = s
	}
	if got := byName["App.MyGolfJournalApp"].IOSComponent; got != "swiftui_app" {
		t.Errorf("collect() ios_component = %q, want %q", got, "swiftui_app")
	}
	if got := byName["UI.StrengthChips"].IOSComponent; got != "swiftui_view" {
		t.Errorf("collect() ios_component = %q, want %q", got, "swiftui_view")
	}

	// End to end through the real collector: the App is rescued, the View is not.
	got := orphansByName(classify(syms, refSources, options{Mode: "both", Visibility: "all"}))
	if _, ok := got["App.MyGolfJournalApp"]; ok {
		t.Error("swiftui_app orphan survived collect()+classify(); the rescue is not wired")
	}
	if _, ok := got["UI.StrengthChips"]; !ok {
		t.Error("swiftui_view must remain an orphan; app code constructs views")
	}
}

// TestJVMFrameworkEntryPointsExcluded pins the JVM framework-instantiation rescue:
// a Spring stereotype class (@Component/@Service/@Configuration/@Controller/
// @Repository — `component` prop) and a Dubbo @Activate SPI extension
// (`dubbo_activate` prop) are instantiated by the container/ExtensionLoader by name,
// never referenced in code, so an unreferenced one is not dead code. An ordinary app
// class carrying neither prop must still be reported (no over-suppression). Measured
// on java/dubbo (find_orphans): 41 Spring + 110 @Activate classes were false orphans;
// java/thingsboard: 624 Spring.
func TestJVMFrameworkEntryPointsExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "svc.OrderService", Kind: facts.SymbolClass, Package: "svc", File: "svc/OrderService.java", SpringComponent: "service"},
		{Name: "cfg.AppConfig", Kind: facts.SymbolClass, Package: "cfg", File: "cfg/AppConfig.java", SpringComponent: "configuration"},
		{Name: "rpc.ProtocolFilterWrapper", Kind: facts.SymbolClass, Package: "rpc", File: "rpc/ProtocolFilterWrapper.java", DubboActivate: true},
		{Name: "rules.TbSendEmailNode", Kind: facts.SymbolClass, Package: "rules", File: "rules/TbSendEmailNode.java", ScannedPlugin: true},
		// An ordinary POJO carrying no framework prop — a dead one is real dead code,
		// so it must NOT be swept up by the framework exclusion (cf. OrphanRepository).
		{Name: "util.DeadHelper", Kind: facts.SymbolClass, Package: "util", File: "util/DeadHelper.java"},
	}
	refs := refsFrom(nil)

	def := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	for _, n := range []string{"svc.OrderService", "cfg.AppConfig", "rpc.ProtocolFilterWrapper", "rules.TbSendEmailNode"} {
		if _, ok := def[n]; ok {
			t.Errorf("%s is a framework-instantiated entry point and should be excluded by default", n)
		}
	}
	if _, ok := def["util.DeadHelper"]; !ok {
		t.Error("util.DeadHelper is ordinary app code and should still be reported (no over-suppression)")
	}

	// include_entrypoints opts back in to inspect framework entry points.
	in := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, ok := in["svc.OrderService"]; !ok {
		t.Error("a Spring component should be reported when IncludeEntrypoints is set")
	}
}

// TestCollectReadsJVMFrameworkProps covers the prop plumbing classify() cannot:
// collect() must lift props["component"] (string) and props["dubbo_activate"] (bool)
// onto symInput, or the rescue never fires on a real snapshot.
func TestCollectReadsJVMFrameworkProps(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"svc.OrderService","file":"svc/OrderService.java","props":{"symbol_kind":"class","component":"service"},"relations":[{"kind":"declares","target":"svc"}]}
{"kind":"symbol","name":"rpc.ProtocolFilterWrapper","file":"rpc/ProtocolFilterWrapper.java","props":{"symbol_kind":"class","dubbo_activate":true},"relations":[{"kind":"declares","target":"rpc"}]}
{"kind":"symbol","name":"util.DeadHelper","file":"util/DeadHelper.java","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"util"}]}
`
	syms, refSources := collectFromJSONL(t, jsonl)

	byName := map[string]symInput{}
	for _, s := range syms {
		byName[s.Name] = s
	}
	if got := byName["svc.OrderService"].SpringComponent; got != "service" {
		t.Errorf("collect() component = %q, want %q", got, "service")
	}
	if !byName["rpc.ProtocolFilterWrapper"].DubboActivate {
		t.Error("collect() dubbo_activate = false, want true")
	}

	// End to end through the real collector: the framework classes are rescued, the POJO is not.
	got := orphansByName(classify(syms, refSources, options{Mode: "both", Visibility: "all"}))
	for _, n := range []string{"svc.OrderService", "rpc.ProtocolFilterWrapper"} {
		if _, ok := got[n]; ok {
			t.Errorf("%s survived collect()+classify(); the JVM framework rescue is not wired", n)
		}
	}
	if _, ok := got["util.DeadHelper"]; !ok {
		t.Error("util.DeadHelper must remain an orphan; a plain POJO carries no framework prop")
	}
}

// TestScopePseudoSymbolExcluded checks that Rails `scope :name` pseudo-symbols
// (emitted as receiver-less scope:<name> functions, invoked only by dynamic
// dispatch) are never reported.
func TestScopePseudoSymbolExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "scope:active", Kind: facts.SymbolFunc, File: "app/models/user.rb"},
		{Name: "app/models.User", Kind: facts.SymbolClass, Package: "app/models", File: "app/models/user.rb", UsageOut: true},
	}
	got := orphansByName(classify(syms, refsFrom(nil), options{Mode: "both", Visibility: "all"}))
	if _, ok := got["scope:active"]; ok {
		t.Error("scope:active pseudo-symbol should be excluded (dynamic dispatch, never referenced by name)")
	}
}

// TestConstReceiverFolding checks that a class referenced only as the constant
// receiver of a call (Ruby `PostCreator.create`) is rescued from a false orphan.
func TestConstReceiverFolding(t *testing.T) {
	// caller.run calls PostCreator.create and Chat::Message.find — folding the
	// receiver in should mark both classes used.
	caller := symInput{Name: "app/jobs.Runner", Kind: facts.SymbolClass, Package: "app/jobs",
		File: "app/jobs/runner.rb", UsageOut: true}
	postCreator := symInput{Name: "PostCreator", Kind: facts.SymbolClass, Package: "app/services",
		File: "app/services/post_creator.rb"}
	chatMessage := symInput{Name: "Chat::Message", Kind: facts.SymbolClass, Package: "plugins/chat",
		File: "plugins/chat/app/models/chat/message.rb"}

	refs := make(map[string]map[string]struct{})
	for _, target := range []string{"PostCreator.create", "Chat::Message.find"} {
		addRef(refs, target, caller.Name)
		if seg := lastSeg(target); seg != target {
			addRef(refs, seg, caller.Name)
		}
		for _, recv := range constReceiverRefs(target) {
			addRef(refs, recv, caller.Name)
		}
	}

	got := orphansByName(classify([]symInput{caller, postCreator, chatMessage}, refs,
		options{Mode: "both", Visibility: "all"}))
	if _, ok := got["PostCreator"]; ok {
		t.Error("PostCreator is referenced via PostCreator.create; should not be an orphan")
	}
	if _, ok := got["Chat::Message"]; ok {
		t.Error("Chat::Message is referenced via Chat::Message.find; should not be an orphan")
	}
}

// TestConstReceiverRefs unit-checks the receiver extraction.
func TestConstReceiverRefs(t *testing.T) {
	cases := map[string][]string{
		"PostCreator.create": {"PostCreator"},
		"Chat::Message.find": {"Chat::Message", "Message"},
		"json.NewDecoder":    nil, // lowercase package qualifier, not a class
		"user.posts":         nil, // lowercase variable receiver
		"bareCall":           nil, // no receiver
	}
	for target, want := range cases {
		got := constReceiverRefs(target)
		if len(got) != len(want) {
			t.Errorf("constReceiverRefs(%q) = %v, want %v", target, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("constReceiverRefs(%q) = %v, want %v", target, got, want)
				break
			}
		}
	}
}

func TestExclusions(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.main", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/main.go"},
		{Name: "pkg.init", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/x.go"},
		{Name: "pkg.TestThing", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/x_test.go"},
		{Name: "pkg.helperInTest", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/x_test.go"},
		{Name: "pkg.realOrphan", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/x.go"},
	}
	refs := refsFrom(nil)

	def := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	for _, excluded := range []string{"pkg.main", "pkg.init", "pkg.TestThing", "pkg.helperInTest"} {
		if _, ok := def[excluded]; ok {
			t.Errorf("%s should be excluded by default", excluded)
		}
	}
	if _, ok := def["pkg.realOrphan"]; !ok {
		t.Error("realOrphan should be reported")
	}

	all := orphansByName(classify(syms, refs, options{
		Mode: "both", Visibility: "all", IncludeTests: true, IncludeEntrypoints: true,
	}))
	for _, name := range []string{"pkg.main", "pkg.init", "pkg.TestThing", "pkg.helperInTest", "pkg.realOrphan"} {
		if _, ok := all[name]; !ok {
			t.Errorf("%s should be included when flags are set", name)
		}
	}
}

// TestPythonExclusions checks Python test-code segregation and that dunders are
// always treated as used (implicitly invoked by the runtime).
func TestPythonExclusions(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.test_login", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/tests/test_login.py"},
		{Name: "pkg.fixture_db", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/conftest.py"},
		{Name: "pkg.helper", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/foo_test.py"},
		{Name: "pkg.MyClass.__init__", Kind: facts.SymbolMethod, Package: "pkg", File: "pkg/m.py"},
		{Name: "pkg.realOrphan", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/m.py"},
	}
	refs := refsFrom(nil)

	def := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	for _, excluded := range []string{"pkg.test_login", "pkg.fixture_db", "pkg.helper", "pkg.MyClass.__init__"} {
		if _, ok := def[excluded]; ok {
			t.Errorf("%s should be excluded by default", excluded)
		}
	}
	if _, ok := def["pkg.realOrphan"]; !ok {
		t.Error("realOrphan should be reported")
	}

	// IncludeTests re-includes test code — but dunders stay excluded (always used).
	all := orphansByName(classify(syms, refs, options{
		Mode: "both", Visibility: "all", IncludeTests: true,
	}))
	for _, name := range []string{"pkg.test_login", "pkg.fixture_db", "pkg.helper"} {
		if _, ok := all[name]; !ok {
			t.Errorf("%s should be included with IncludeTests", name)
		}
	}
	if _, ok := all["pkg.MyClass.__init__"]; ok {
		t.Error("dunder __init__ should never be reported, even with IncludeTests")
	}
}

// TestReexportRescue verifies that a symbol named in an __init__.py re-export
// (recorded via the dependency fact's reexports prop) is treated as referenced.
func TestReexportRescue(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"pkg/sub.PublicThing","file":"pkg/sub/x.py","props":{"symbol_kind":"class","exported":true},"relations":[{"kind":"declares","target":"pkg/sub"}]}
{"kind":"symbol","name":"pkg/sub.PrivateThing","file":"pkg/sub/x.py","props":{"symbol_kind":"class","exported":true},"relations":[{"kind":"declares","target":"pkg/sub"}]}
{"kind":"dependency","name":"pkg/__init__ -> .sub","file":"pkg/__init__.py","props":{"reexports":["PublicThing"]},"relations":[{"kind":"imports","target":"pkg/sub"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	out := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := out["pkg/sub.PublicThing"]; ok {
		t.Error("PublicThing is re-exported in __init__.py and should not be an orphan")
	}
	if _, ok := out["pkg/sub.PrivateThing"]; !ok {
		t.Error("PrivateThing is not re-exported and should still be reported")
	}
}

// TestRubyInstanceMethodMatching verifies a Ruby instance method ("Class#method")
// is matched by a "receiver.method" reference (the '#' separator must not block
// the short-name match), while an unreferenced one is still reported.
func TestRubyInstanceMethodMatching(t *testing.T) {
	syms := []symInput{
		{Name: "PostsController#image_sizes", Kind: facts.SymbolMethod, Package: "app/controllers", File: "app/controllers/posts_controller.rb"},
		{Name: "PostsController#unused_helper", Kind: facts.SymbolMethod, Package: "app/controllers", File: "app/controllers/posts_controller.rb"},
	}
	// A call edge target "post.image_sizes" contributes the short name "image_sizes".
	refs := refsFrom(map[string][]string{"SomeOther#caller": {"post.image_sizes"}})

	out := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := out["PostsController#image_sizes"]; ok {
		t.Error("image_sizes is referenced via post.image_sizes; should not be an orphan")
	}
	if _, ok := out["PostsController#unused_helper"]; !ok {
		t.Error("unused_helper has no reference; should be reported")
	}
}

// TestRubySpecExclusionAndMixinFold verifies RSpec specs are excluded by default
// and that a module included via a Ruby mixin dependency fact is not an orphan.
func TestRubySpecExclusionAndMixinFold(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"Trashable","file":"app/models/concerns/trashable.rb","props":{"symbol_kind":"interface","exported":true},"relations":[{"kind":"declares","target":"app/models/concerns"}]}
{"kind":"symbol","name":"UnusedModule","file":"app/models/concerns/unused.rb","props":{"symbol_kind":"interface","exported":true},"relations":[{"kind":"declares","target":"app/models/concerns"}]}
{"kind":"symbol","name":"PostSpecHelper#build","file":"spec/models/post_spec.rb","props":{"symbol_kind":"method","exported":true},"relations":[{"kind":"declares","target":"spec/models"}]}
{"kind":"dependency","name":"Post -> Trashable","file":"app/models/post.rb","props":{"mixin_kind":"include"},"relations":[{"kind":"implements","target":"Trashable"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	out := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, ok := out["Trashable"]; ok {
		t.Error("Trashable is included via a mixin; should not be an orphan")
	}
	if _, ok := out["UnusedModule"]; !ok {
		t.Error("UnusedModule is never included; should be reported")
	}
	if _, ok := out["PostSpecHelper#build"]; ok {
		t.Error("symbols under spec/ should be excluded by default")
	}
}

// TestVisibilityFilter checks the exported/unexported filters and the exported
// annotation.
func TestVisibilityFilter(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.PublicOrphan", Kind: facts.SymbolFunc, Package: "pkg", Exported: true},
		{Name: "pkg.privateOrphan", Kind: facts.SymbolFunc, Package: "pkg", Exported: false},
	}
	refs := refsFrom(nil)

	exp := classify(syms, refs, options{Mode: "both", Visibility: "exported"})
	if len(exp) != 1 || exp[0].Name != "pkg.PublicOrphan" {
		t.Errorf("visibility=exported: got %+v", exp)
	}
	if exp[0].Note == "" {
		t.Error("exported orphan should carry an annotation note")
	}
	unexp := classify(syms, refs, options{Mode: "both", Visibility: "unexported"})
	if len(unexp) != 1 || unexp[0].Name != "pkg.privateOrphan" {
		t.Errorf("visibility=unexported: got %+v", unexp)
	}
}

// TestModeFilter checks that mode narrows to a single class.
func TestModeFilter(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.isolatedOne", Kind: facts.SymbolStruct, Package: "pkg"},
		{Name: "pkg.unrefOne", Kind: facts.SymbolFunc, Package: "pkg", UsageOut: true},
	}
	refs := refsFrom(nil)

	iso := classify(syms, refs, options{Mode: classIsolated, Visibility: "all"})
	if len(iso) != 1 || iso[0].Name != "pkg.isolatedOne" {
		t.Errorf("mode=isolated: got %+v", iso)
	}
	unref := classify(syms, refs, options{Mode: classUnreferenced, Visibility: "all"})
	if len(unref) != 1 || unref[0].Name != "pkg.unrefOne" {
		t.Errorf("mode=unreferenced: got %+v", unref)
	}
	both := classify(syms, refs, options{Mode: "both", Visibility: "all"})
	if len(both) != 2 {
		t.Errorf("mode=both: got %d want 2", len(both))
	}
}

// TestConfidenceTier checks functions are high; struct/class/interface are medium
// (instantiate/inject/implement edges ARE tracked); methods and other types are
// low (method dispatch and field/param/return type usage are not edge-tracked).
func TestConfidenceTier(t *testing.T) {
	cases := map[string]string{
		facts.SymbolFunc:      confHigh,
		facts.SymbolStruct:    confMedium,
		facts.SymbolClass:     confMedium,
		facts.SymbolInterface: confMedium,
		facts.SymbolMethod:    confLow,
		"type":                confLow,
		"constant":            confLow,
		"variable":            confLow,
	}
	for kind, want := range cases {
		if got := confidenceFor(symInput{Name: "pkg.Ident", Kind: kind}); got != want {
			t.Errorf("confidenceFor(%q) = %q, want %q", kind, got, want)
		}
	}
	// Operator overloads are always LOW regardless of kind — their usage is not
	// edge-tracked, so they must never be flagged high-confidence removable.
	for _, op := range []string{"Foundation.+", "Foundation.<-", "Mod.^", "Foo#+"} {
		if got := confidenceFor(symInput{Name: op, Kind: facts.SymbolFunc}); got != confLow {
			t.Errorf("confidenceFor(func, %q) = %q, want %q", op, got, confLow)
		}
	}
}

// TS/JS usage (JSX, dynamic import, string dispatch) is only partially edge-tracked,
// so a TS/JS function is a verify-first MEDIUM lead — never the "safe to delete" HIGH
// a Go/Rust call-graph orphan earns — and React components/hooks drop a further tier.
func TestConfidenceTier_TypeScript(t *testing.T) {
	cases := []struct {
		name string
		sym  symInput
		want string
	}{
		{"ts function", symInput{Name: "src.helper", Kind: facts.SymbolFunc, Language: "typescript"}, confMedium},
		{"js function", symInput{Name: "src.helper", Kind: facts.SymbolFunc, Language: "javascript"}, confMedium},
		{"react component", symInput{Name: "src.Card", Kind: facts.SymbolFunc, Language: "typescript", WebComponent: "component"}, confLow},
		{"react hook", symInput{Name: "src.useThing", Kind: facts.SymbolFunc, Language: "typescript", WebComponent: "hook"}, confLow},
		{"ts class", symInput{Name: "src.Store", Kind: facts.SymbolClass, Language: "typescript"}, confLow},
		// A Go function keeps its HIGH tier — the downgrade is language-scoped.
		{"go function", symInput{Name: "pkg.Do", Kind: facts.SymbolFunc, Language: "go"}, confHigh},
	}
	for _, tc := range cases {
		if got := confidenceFor(tc.sym); got != tc.want {
			t.Errorf("%s: confidenceFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A Next.js route handler is a framework entry point (dispatched per request, never
// referenced by name), so it is excluded by default like main/init; a React component
// is NOT excluded (a dead one is real dead code, just lower confidence).
func TestExclude_RouteHandlerEntryPoint(t *testing.T) {
	handler := symInput{Name: "src.GET", Kind: facts.SymbolFunc, Language: "typescript", WebComponent: "route_handler"}
	if !isExcluded(handler, options{}) {
		t.Error("route_handler should be excluded by default (framework entry point)")
	}
	if isExcluded(handler, options{IncludeEntrypoints: true}) {
		t.Error("route_handler should be included with IncludeEntrypoints")
	}
	component := symInput{Name: "src.Card", Kind: facts.SymbolFunc, Language: "typescript", WebComponent: "component"}
	if isExcluded(component, options{}) {
		t.Error("a React component must NOT be excluded — a dead one is real dead code")
	}
}

// TS/JS CLI-task trees are dispatched by dynamic-string require and have no resolvable
// caller, so they are excluded by default — but only for TS/JS, and only when the
// caller has not scoped the query to that package.
func TestExclude_TSDynamicEntryTree(t *testing.T) {
	task := symInput{Name: "tasks.run", Kind: facts.SymbolFunc, File: "tasks/i18n_check.js", Language: "typescript"}
	if !isExcluded(task, options{}) {
		t.Error("tasks/ TS file should be excluded by default (dynamic-string require entry)")
	}
	if isExcluded(task, options{Package: "tasks"}) {
		t.Error("tasks/ should be inspectable when scoped with package=")
	}
	// A Go internal/tasks package is unaffected by the TS-gated exclusion.
	goTask := symInput{Name: "tasks.Run", Kind: facts.SymbolFunc, File: "internal/tasks/run.go", Language: "go"}
	if isExcluded(goTask, options{}) {
		t.Error("a Go tasks/ package must not be excluded by the TS-gated rule")
	}
}

// TestSummarize checks per-kind aggregation.
func TestSummarize(t *testing.T) {
	orphans := []Orphan{
		{Name: "pkg.A", Kind: facts.SymbolFunc, Class: classUnreferenced, Confidence: confHigh},
		{Name: "pkg.B", Kind: facts.SymbolFunc, Class: classIsolated, Confidence: confHigh, Exported: true},
		{Name: "pkg.C", Kind: facts.SymbolStruct, Class: classIsolated, Confidence: confMedium},
	}
	// 40 candidates were considered, out of 100 symbols in the repo. The two numbers
	// are distinct on purpose: total_symbols is the denominator of total_orphans,
	// repo_wide_symbols is context. Passing one literal and asserting it came back —
	// which is what this test used to do — could not have caught the scoping bug.
	s := summarize(orphans, 40, 100, "both")
	if s.TotalSymbols != 40 || s.RepoWideSymbols != 100 || s.TotalOrphans != 3 {
		t.Errorf("totals: candidates=%d repoWide=%d orphans=%d", s.TotalSymbols, s.RepoWideSymbols, s.TotalOrphans)
	}
	if s.Isolated != 2 || s.Unreferenced != 1 {
		t.Errorf("class totals: isolated=%d unreferenced=%d", s.Isolated, s.Unreferenced)
	}
	byKind := make(map[string]kindCount)
	for _, kc := range s.ByKind {
		byKind[kc.Kind] = kc
	}
	if fn := byKind[facts.SymbolFunc]; fn.Total != 2 || fn.Isolated != 1 || fn.Unreferenced != 1 || fn.Exported != 1 || fn.HighConfidence != 2 {
		t.Errorf("function kindCount = %+v", fn)
	}
	if st := byKind[facts.SymbolStruct]; st.Total != 1 || st.Isolated != 1 || st.HighConfidence != 0 || st.MediumConfidence != 1 {
		t.Errorf("struct kindCount = %+v", st)
	}
}

// TestIsGeneratedPath checks the conservative build/generated-output segment match.
func TestIsGeneratedPath(t *testing.T) {
	gen := []string{
		"data/build/kspCaches/devDebug/x.kt",
		"data/build/generated/ksp/x.kt",
		"Pods/Alamofire/Source/x.swift",
		"src/node_modules/foo/bar.js",
		"app/__pycache__/mod.pyc",
	}
	for _, p := range gen {
		if !mcputil.IsGeneratedPath(p) {
			t.Errorf("mcputil.IsGeneratedPath(%q) = false, want true", p)
		}
	}
	src := []string{
		"internal/adapters/http/auth_routes.go",
		"src/components/golf-course/rules.tsx",
		"buildconfig/config.go", // "buildconfig" != "build" segment
		"MyGolfJournal/AppShell/AppDelegate.swift",
	}
	for _, p := range src {
		if mcputil.IsGeneratedPath(p) {
			t.Errorf("mcputil.IsGeneratedPath(%q) = true, want false", p)
		}
	}
}

// collectFromJSONL builds an engine from inline JSONL facts and runs the real
// collect(), so dedup and generated-path filtering are exercised end-to-end
// without naming the un-importable facts.* types.
func collectFromJSONL(t *testing.T, jsonl string) ([]symInput, map[string]map[string]struct{}) {
	t.Helper()
	store := facts.NewStore()
	if err := store.ReadJSONL(strings.NewReader(jsonl)); err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	return collect(store)
}

// TestCollectDedupesByName verifies duplicate symbol facts for one name collapse
// into a single record with UsageOut OR-ed across copies — so the name is reported
// once and never classified as both isolated and unreferenced.
func TestCollectDedupesByName(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"pkg.Dup","file":"pkg/a.go","props":{"symbol_kind":"struct"},"relations":[{"kind":"declares","target":"pkg"}]}
{"kind":"symbol","name":"pkg.Dup","file":"pkg/a.go","props":{"symbol_kind":"struct"},"relations":[{"kind":"declares","target":"pkg"},{"kind":"calls","target":"pkg.Other"}]}
`
	syms, _ := collectFromJSONL(t, jsonl)
	n := 0
	for _, s := range syms {
		if s.Name == "pkg.Dup" {
			n++
			if !s.UsageOut {
				t.Error("UsageOut should be OR-ed to true across duplicate facts")
			}
		}
	}
	if n != 1 {
		t.Fatalf("expected pkg.Dup once after dedupe, got %d", n)
	}
}

// TestCollectSkipsGeneratedPaths verifies symbols under build/generated output
// directories are excluded from the classifier inputs.
func TestCollectSkipsGeneratedPaths(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"app.Real","file":"app/real.go","props":{"symbol_kind":"struct"},"relations":[{"kind":"declares","target":"app"}]}
{"kind":"symbol","name":"gen.Backup","file":"data/build/kspCaches/devDebug/backups/Gen.kt","props":{"symbol_kind":"class"},"relations":[{"kind":"declares","target":"data/build/kspCaches"}]}
`
	syms, _ := collectFromJSONL(t, jsonl)
	for _, s := range syms {
		if mcputil.IsGeneratedPath(s.File) {
			t.Errorf("generated symbol leaked into collect: %q (%s)", s.Name, s.File)
		}
	}
	if len(syms) != 1 || syms[0].Name != "app.Real" {
		t.Fatalf("expected only app.Real, got %+v", syms)
	}
}

// TestCollectFoldsRouteHandler verifies the route-handler rescue in collect():
// an HTTP handler method is wired to a route via the route fact's props["handler"]
// (handlers are registered as method values, never called), so without folding the
// route's handler prop in as a reference the method would be a false-positive
// orphan. A same-package method with no handler and no callers stays an orphan, so
// the rescue is specific, not a blanket suppression.
func TestCollectFoldsRouteHandler(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"internal/app.UserHandler.GetByCode","file":"internal/app/user.go","props":{"symbol_kind":"method"},"relations":[{"kind":"declares","target":"internal/app"}]}
{"kind":"symbol","name":"internal/app.UserHandler.Unused","file":"internal/app/user.go","props":{"symbol_kind":"method"},"relations":[{"kind":"declares","target":"internal/app"}]}
{"kind":"route","name":"GET /users/:code","props":{"handler":"UserHandler.GetByCode"}}
`
	syms, refSources := collectFromJSONL(t, jsonl)
	orphans := classify(syms, refSources, options{Mode: "both", Visibility: "all"})

	got := map[string]bool{}
	for _, o := range orphans {
		got[o.Name] = true
	}
	if got["internal/app.UserHandler.GetByCode"] {
		t.Error("route handler GetByCode reported as orphan; props[\"handler\"] rescue not applied")
	}
	if !got["internal/app.UserHandler.Unused"] {
		t.Error("Unused (no route, no callers) should be an orphan; rescue must be specific to wired handlers")
	}
}

// TestSelfReceiverRefs checks the self-qualified dispatch fold: self/self.class/
// cls/this receivers reduce to the bare method name; other receivers do not.
func TestSelfReceiverRefs(t *testing.T) {
	cases := map[string][]string{
		"self.class.perform_when_readonly?": {"perform_when_readonly?"},
		"self.foo":                          {"foo"},
		"cls.bar":                           {"bar"},
		"this.baz":                          {"baz"},
		"self::Thing":                       {"Thing"},
		"Foo.bar":                           nil, // constant receiver, not self
		"user.posts":                        nil, // lowercase variable receiver
		"bareCall":                          nil, // no receiver
	}
	for target, want := range cases {
		got := selfReceiverRefs(target)
		if len(got) != len(want) {
			t.Errorf("selfReceiverRefs(%q) = %v, want %v", target, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("selfReceiverRefs(%q) = %v, want %v", target, got, want)
				break
			}
		}
	}
}

// TestTestRefFold verifies a production method referenced only by a KindTestRef
// fact (a spec) is not reported as an orphan — the spec-only false-positive class.
func TestTestRefFold(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"Badge.trust_level_badge_ids","file":"app/models/badge.rb","props":{"symbol_kind":"method"},"relations":[{"kind":"declares","target":"app/models"}]}
{"kind":"symbol","name":"Badge.orphaned_helper","file":"app/models/badge.rb","props":{"symbol_kind":"method"},"relations":[{"kind":"declares","target":"app/models"}]}
{"kind":"test_ref","name":"spec/services/badge_granter_spec.rb","file":"spec/services/badge_granter_spec.rb","relations":[{"kind":"calls","target":"Badge.trust_level_badge_ids"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, isOrphan := got["Badge.trust_level_badge_ids"]; isOrphan {
		t.Error("Badge.trust_level_badge_ids is referenced by a spec and must not be an orphan")
	}
	// A method with no reference at all is still reported — the fold is additive,
	// not a blanket suppression.
	if _, isOrphan := got["Badge.orphaned_helper"]; !isOrphan {
		t.Error("Badge.orphaned_helper has no reference and should still be an orphan")
	}
}

// TestSetterNameNormalization verifies a setter symbol "Class.foo=" is matched by a
// reference to the bare "foo" (how assignment call sites `x.foo = v` are recorded).
func TestSetterNameNormalization(t *testing.T) {
	cands := candidateNames("Jobs::X.last_notified_id=", facts.SymbolMethod)
	var hasBare, hasRecv bool
	for _, c := range cands {
		if c == "last_notified_id" {
			hasBare = true
		}
		if c == "Jobs::X.last_notified_id" {
			hasRecv = true
		}
	}
	if !hasBare || !hasRecv {
		t.Fatalf("candidateNames setter forms missing: %v", cands)
	}

	jsonl := `{"kind":"symbol","name":"Jobs::X.last_notified_id=","file":"app/jobs/x.rb","props":{"symbol_kind":"method"},"relations":[{"kind":"declares","target":"app/jobs"}]}
{"kind":"file_ref","name":"app/jobs/x.rb","file":"app/jobs/x.rb","relations":[{"kind":"calls","target":"X.last_notified_id"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, isOrphan := got["Jobs::X.last_notified_id="]; isOrphan {
		t.Error("setter referenced via assignment (bare form) must not be an orphan")
	}
}

// TestRubyImplicitHooksExcluded verifies Ruby lifecycle hooks are dropped for .rb
// symbols but the same name in another language is not.
func TestRubyImplicitHooksExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "Jobs::Onceoff.inherited", Kind: facts.SymbolMethod, File: "app/jobs/onceoff.rb"},
		{Name: "pkg.inherited", Kind: facts.SymbolFunc, File: "pkg/thing.go"},
	}
	got := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"}))
	if _, isOrphan := got["Jobs::Onceoff.inherited"]; isOrphan {
		t.Error("Ruby lifecycle hook `inherited` must be excluded for .rb symbols")
	}
	if _, isOrphan := got["pkg.inherited"]; !isOrphan {
		t.Error("`inherited` in a non-.rb file must NOT be excluded by the Ruby-hook rule")
	}
}

// TestFileRefFold verifies a production method referenced only by a KindFileRef
// fact (top-level/fixture call) is not reported as an orphan.
func TestFileRefFold(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"Badge.like_badge_counts","file":"app/models/badge.rb","props":{"symbol_kind":"method"},"relations":[{"kind":"declares","target":"app/models"}]}
{"kind":"symbol","name":"Badge.dead_one","file":"app/models/badge.rb","props":{"symbol_kind":"method"},"relations":[{"kind":"declares","target":"app/models"}]}
{"kind":"file_ref","name":"db/fixtures/006_badges.rb","file":"db/fixtures/006_badges.rb","relations":[{"kind":"calls","target":"Badge.like_badge_counts"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, isOrphan := got["Badge.like_badge_counts"]; isOrphan {
		t.Error("method called from a fixture (KindFileRef) must not be an orphan")
	}
	if _, isOrphan := got["Badge.dead_one"]; !isOrphan {
		t.Error("unreferenced method should still be an orphan (fold is additive)")
	}
}

// TestModuleRefFold verifies a C/C++ production symbol referenced only through a
// KindModule usage edge is not reported as an orphan. The C/C++ extractor hangs
// file-scope registration-macro references (module_init(foo), EXPORT_SYMBOL(foo),
// DEVICE_ATTR(.., show, store)) off the directory module fact, because they have no
// enclosing symbol; collect()'s KindModule pass folds them into the reference index.
// That pass exists SOLELY for C/C++ — ten cacheVersion bumps (v8–v17) of macro
// machinery feed it — and had no test coverage (GAP-CP-02): removing the fold makes
// serial_probe an orphan and fails this test.
func TestModuleRefFold(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"drivers.serial_probe","file":"drivers/serial.c","props":{"symbol_kind":"function","language":"c","exported":true},"relations":[{"kind":"declares","target":"drivers"}]}
{"kind":"symbol","name":"drivers.unused_helper","file":"drivers/serial.c","props":{"symbol_kind":"function","language":"c","exported":true},"relations":[{"kind":"declares","target":"drivers"}]}
{"kind":"module","name":"drivers","file":"drivers","relations":[{"kind":"calls","target":"drivers.serial_probe"}]}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, isOrphan := got["drivers.serial_probe"]; isOrphan {
		t.Error("serial_probe is registered via a KindModule usage edge (module_init/EXPORT_SYMBOL) and must not be an orphan")
	}
	// A symbol the module fact does not reference is still reported — the fold is
	// additive, not a blanket suppression.
	if _, isOrphan := got["drivers.unused_helper"]; !isOrphan {
		t.Error("drivers.unused_helper has no reference and should still be an orphan")
	}
}

// TestDynamicSendPrefixFold verifies a function/method whose bare name matches a
// dynamic-dispatch prefix (from an interpolated symbol) is not reported as an
// orphan, while a non-matching one still is.
func TestDynamicSendPrefixFold(t *testing.T) {
	jsonl := `{"kind":"symbol","name":"IncomingLinksReport.report_top_referrers","file":"app/models/incoming_links_report.rb","props":{"symbol_kind":"function"},"relations":[{"kind":"declares","target":"app/models"}]}
{"kind":"symbol","name":"IncomingLinksReport.genuinely_dead","file":"app/models/incoming_links_report.rb","props":{"symbol_kind":"function"},"relations":[{"kind":"declares","target":"app/models"}]}
{"kind":"file_ref","name":"app/models/incoming_links_report.rb","file":"app/models/incoming_links_report.rb","props":{"dynamic_send_prefixes":["report_"]}}
`
	syms, refs := collectFromJSONL(t, jsonl)
	got := orphansByName(classify(syms, refs, options{Mode: "both", Visibility: "all"}))
	if _, isOrphan := got["IncomingLinksReport.report_top_referrers"]; isOrphan {
		t.Error("method matching a dynamic-dispatch prefix must not be an orphan")
	}
	if _, isOrphan := got["IncomingLinksReport.genuinely_dead"]; !isOrphan {
		t.Error("a non-matching method should still be an orphan (prefix fold is additive)")
	}
}

// TestRubyFrameworkHooksExcluded verifies reflection-invoked framework convention
// methods are excluded for .rb symbols, but not for other languages, and ordinary
// methods are still reported.
func TestRubyFrameworkHooksExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "Mutations::BaseMutation.authorizes_object?", Kind: facts.SymbolFunc, File: "app/graphql/mutations/base_mutation.rb"},
		{Name: "ApplicationCable::Channel.action_methods", Kind: facts.SymbolFunc, File: "app/channels/application_cable/channel.rb"},
		{Name: "GitlabSchema.unauthorized_field", Kind: facts.SymbolFunc, File: "app/graphql/gitlab_schema.rb"},
		{Name: "pkg.authorizes_object?", Kind: facts.SymbolFunc, File: "pkg/thing.go"}, // non-.rb
		{Name: "Foo.ordinary_method", Kind: facts.SymbolFunc, File: "app/models/foo.rb"},
	}
	got := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"}))
	for _, excluded := range []string{
		"Mutations::BaseMutation.authorizes_object?",
		"ApplicationCable::Channel.action_methods",
		"GitlabSchema.unauthorized_field",
	} {
		if _, isOrphan := got[excluded]; isOrphan {
			t.Errorf("framework hook %q must be excluded for .rb symbols", excluded)
		}
	}
	if _, isOrphan := got["pkg.authorizes_object?"]; !isOrphan {
		t.Error("authorizes_object? in a non-.rb file must NOT be excluded by the Ruby framework-hook rule")
	}
	if _, isOrphan := got["Foo.ordinary_method"]; !isOrphan {
		t.Error("an ordinary .rb method should still be reported")
	}
}

// TestRailsStiHooksExcluded verifies ActiveRecord STI reflection hooks are excluded
// for .rb symbols (added to rubyFrameworkHooks).
func TestRailsStiHooksExcluded(t *testing.T) {
	syms := []symInput{
		{Name: "Event.find_sti_class", Kind: facts.SymbolFunc, File: "app/models/event.rb"},
		{Name: "Model.sti_name", Kind: facts.SymbolFunc, File: "app/models/model.rb"},
		{Name: "Foo.regular_method", Kind: facts.SymbolFunc, File: "app/models/foo.rb"},
	}
	got := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"}))
	for _, excluded := range []string{"Event.find_sti_class", "Model.sti_name"} {
		if _, isOrphan := got[excluded]; isOrphan {
			t.Errorf("STI framework hook %q must be excluded for .rb symbols", excluded)
		}
	}
	if _, isOrphan := got["Foo.regular_method"]; !isOrphan {
		t.Error("an ordinary .rb method should still be reported")
	}
}

// React class-component lifecycle methods are called by React itself, never by name,
// so they are excluded by default like Android lifecycle overrides — but only for
// TS/JS methods, and the caller can opt back in with IncludeEntrypoints.
func TestExclude_ReactLifecycleMethods(t *testing.T) {
	for _, name := range []string{"render", "componentDidMount", "componentWillUnmount", "shouldComponentUpdate", "getDerivedStateFromProps"} {
		m := symInput{Name: "client/components/foo.Foo." + name, Kind: facts.SymbolMethod, Language: "typescript"}
		if !isExcluded(m, options{}) {
			t.Errorf("%s should be excluded by default (React lifecycle, framework-dispatched)", name)
		}
		if isExcluded(m, options{IncludeEntrypoints: true}) {
			t.Errorf("%s should be visible with IncludeEntrypoints", name)
		}
	}
	// A custom (non-lifecycle) method is still reported — it is not framework-dispatched.
	custom := symInput{Name: "client/components/foo.Foo.handleClick", Kind: facts.SymbolMethod, Language: "typescript"}
	if isExcluded(custom, options{}) {
		t.Error("a custom method must not be excluded by the lifecycle rule")
	}
	// A Go function named render is unaffected (rule is TS/JS + method only).
	goRender := symInput{Name: "pkg.render", Kind: facts.SymbolFunc, Language: "go"}
	if isExcluded(goRender, options{}) {
		t.Error("a Go function named render must not be excluded")
	}
}

// TestClassify_ExcludesPythonFrameworkEntryPoints checks that Python framework
// hooks (gunicorn hooks, ASGI lifespan, Airflow cluster policies) and docs/ trees
// are not reported as dead code — they are dispatched by name from config/settings
// or run by the docs toolchain, never by a static call, so they have no incoming
// edge by construction. A genuine production orphan is still reported.
func TestClassify_ExcludesPythonFrameworkEntryPoints(t *testing.T) {
	syms := []symInput{
		{Name: "airflow-core/src/airflow/api_fastapi/gunicorn_config.post_worker_init", Kind: facts.SymbolFunc,
			File: "airflow-core/src/airflow/api_fastapi/gunicorn_config.py", Package: "airflow-core/src/airflow/api_fastapi", Exported: true, Language: "python"},
		{Name: "airflow-core/src/airflow/api_fastapi/app.lifespan", Kind: facts.SymbolFunc,
			File: "airflow-core/src/airflow/api_fastapi/app.py", Package: "airflow-core/src/airflow/api_fastapi", Exported: true, Language: "python"},
		{Name: "airflow-core/src/airflow/settings.task_policy", Kind: facts.SymbolFunc,
			File: "airflow-core/src/airflow/settings.py", Package: "airflow-core/src/airflow", Exported: true, Language: "python"},
		{Name: "airflow-core/docs/conf.setup", Kind: facts.SymbolFunc,
			File: "airflow-core/docs/conf.py", Package: "airflow-core/docs", Exported: true, Language: "python"},
		{Name: "airflow-core/src/airflow/utils/helpers.reallyDead", Kind: facts.SymbolFunc,
			File: "airflow-core/src/airflow/utils/helpers.py", Package: "airflow-core/src/airflow/utils", Exported: true, Language: "python"},
	}
	names := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"}))
	for _, n := range []string{
		"airflow-core/src/airflow/api_fastapi/gunicorn_config.post_worker_init",
		"airflow-core/src/airflow/api_fastapi/app.lifespan",
		"airflow-core/src/airflow/settings.task_policy",
		"airflow-core/docs/conf.setup",
	} {
		if _, ok := names[n]; ok {
			t.Errorf("%s is a Python framework entry point / docs symbol and should be excluded", n)
		}
	}
	if _, ok := names["airflow-core/src/airflow/utils/helpers.reallyDead"]; !ok {
		t.Error("a genuine production orphan should still be reported")
	}

	// Opt back in: include_entrypoints surfaces the hooks; package= surfaces docs.
	withEntry := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, ok := withEntry["airflow-core/src/airflow/settings.task_policy"]; !ok {
		t.Error("include_entrypoints=true should surface Python framework hooks")
	}
}

// TestClassify_ExcludesPythonNonAppTreesAndCLI checks Pass-2 exclusions: shared
// test-support trees (tests_common/test_utils), sphinx extensions, example DAGs,
// alembic migrations, scripts, and click/Typer CLI commands (cli_command prop) are
// framework-loaded/dispatched, never called by name, so they must not be reported.
func TestClassify_ExcludesPythonNonAppTreesAndCLI(t *testing.T) {
	py := func(name, file string) symInput {
		return symInput{Name: name, Kind: facts.SymbolFunc, File: file,
			Package: "p", Exported: true, Language: "python"}
	}
	cli := py("airflow-core/src/airflow/cli/commands/provider_command.providers_list",
		"airflow-core/src/airflow/cli/commands/provider_command.py")
	cli.CLICommand = true
	syms := []symInput{
		py("devel-common/src/tests_common/test_utils/db.clear_db", "devel-common/src/tests_common/test_utils/db.py"),
		py("devel-common/src/sphinx_exts/exampleinclude.setup", "devel-common/src/sphinx_exts/exampleinclude.py"),
		py("airflow-core/src/airflow/example_dags/tutorial.transform", "airflow-core/src/airflow/example_dags/tutorial.py"),
		py("airflow-core/src/airflow/migrations/versions/abc_add_col.upgrade", "airflow-core/src/airflow/migrations/versions/abc_add_col.py"),
		py("scripts/ci/prek/check.main_check", "scripts/ci/prek/check.py"),
		py("performance/src/performance_dags/performance_dag/performance_dag.build", "performance/src/performance_dags/performance_dag/performance_dag.py"),
		{Name: "dev/breeze/src/airflow_breeze/utils/console.get_stderr_console", Kind: facts.SymbolFunc,
			File: "dev/breeze/src/airflow_breeze/utils/console.py", Package: "dev/breeze/src/airflow_breeze/utils", Exported: true, Language: "python"},
		cli,
		py("airflow-core/src/airflow/utils/helpers.reallyDead", "airflow-core/src/airflow/utils/helpers.py"),
	}
	names := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"}))
	for _, n := range []string{
		"devel-common/src/tests_common/test_utils/db.clear_db",
		"devel-common/src/sphinx_exts/exampleinclude.setup",
		"airflow-core/src/airflow/example_dags/tutorial.transform",
		"airflow-core/src/airflow/migrations/versions/abc_add_col.upgrade",
		"scripts/ci/prek/check.main_check",
		"performance/src/performance_dags/performance_dag/performance_dag.build",
		"dev/breeze/src/airflow_breeze/utils/console.get_stderr_console",
		"airflow-core/src/airflow/cli/commands/provider_command.providers_list",
	} {
		if _, ok := names[n]; ok {
			t.Errorf("%s is a non-app/framework-dispatched symbol and should be excluded", n)
		}
	}
	// package=dev opts back in to the developer-tooling tree.
	scopedDev := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all", Package: "dev"}))
	if _, ok := scopedDev["dev/breeze/src/airflow_breeze/utils/console.get_stderr_console"]; !ok {
		t.Error("package=dev should surface the dev/ tooling tree")
	}
	if _, ok := names["airflow-core/src/airflow/utils/helpers.reallyDead"]; !ok {
		t.Error("a genuine production orphan should still be reported")
	}

	// Opt back in: include_tests surfaces test-support; include_entrypoints surfaces CLI.
	withTests := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all", IncludeTests: true}))
	if _, ok := withTests["devel-common/src/tests_common/test_utils/db.clear_db"]; !ok {
		t.Error("include_tests=true should surface test-support trees")
	}
	withEntry := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, ok := withEntry["airflow-core/src/airflow/cli/commands/provider_command.providers_list"]; !ok {
		t.Error("include_entrypoints=true should surface click CLI commands")
	}
}

// TestIsTestPath_ConventionalTestDirSegments pins the three conventional test
// directory segments that perf.isTestPath (perf.go:201) already recognises but
// nonShippableSegments did not: Jest's `__tests__` colocation dir and Xcode's
// default `Tests`/`Mocks` target names. Without them, an XCTest target's classes
// and hand-written spies are offered to the user as dead code to delete — on
// nan/nebenan-iOS that was 1959 of 5671 orphans (34.5%).
//
// `__tests__` is pinned here only: no repo in any corpus tier has such a
// directory, so this test is its sole assertion (a TestGolden fixture cannot
// pin an enterprise analyzer).
func TestIsTestPath_ConventionalTestDirSegments(t *testing.T) {
	for _, p := range []string{
		"Tests/App/TestHelper.swift",                      // Xcode test target
		"Tests/NebenanFeed/Mocks/FeedMock.swift",          // Xcode mock target, nested
		"Tests/Nebenan3P/Helper/SnowplowTestHelper.swift", // real path from nebenan-iOS
		"src/__tests__/testUtils.ts",                      // Jest / CRA / Next.js colocation
	} {
		if !isTestPath(p) {
			t.Errorf("isTestPath(%q) = false, want true (test-support code must not be a cleanup candidate)", p)
		}
	}
}

// TestIsTestPath_ProductionPathsNotExcluded is the other half of the guard: the
// match must stay a case-sensitive *segment* match. A substring match or a
// case-fold would silently delete production code from the graph — which is
// exactly how a production `..._ab_test.rb` ActiveJob was swallowed by a bare
// suffix glob (fixed/28). facts.ModuleRoleForPath was hardened at v60 so genuine
// single-token names (`latest`, `contest`) never misfire; this pins the same
// property here.
func TestIsTestPath_ProductionPathsNotExcluded(t *testing.T) {
	for _, p := range []string{
		"Sources/App/Contest/ContestView.swift", // "Contest" contains "test"
		"src/latest/LatestFeed.ts",              // "latest" contains "test"
		"app/models/Attestation.swift",          // "Attestation" contains "test"
		"Sources/Protest/ProtestBanner.swift",   // "Protest" contains "test"
	} {
		if isTestPath(p) {
			t.Errorf("isTestPath(%q) = true, want false (production code must never be hidden)", p)
		}
	}
}

// TestIsTestPath_SharedWithPerf pins orphans onto the shared facts.IsTestPath.
// The segments fixed/52 added must survive the merge, and the two rules this copy
// carried that were fixed/28 defects — the `_test.` infix and the unqualified
// `test_` prefix — must now leave production code alone.
func TestIsTestPath_SharedWithPerf(t *testing.T) {
	testPaths := []string{
		// fixed/52's segments — must not regress.
		"Tests/Testability/Sources/Assert.swift",
		"Mocks/APIStub.swift",
		"src/components/__tests__/Button.tsx",
		// Long-standing coverage.
		"app/src/androidTest/java/de/nebenan/app/ui/EspressoMocks.kt",
		"spec/models/user_spec.rb",
		"test/models/user_test.rb",
		"tests/unit/test_date_parser.py",
		"internal/engine/cache_test.go",
		"internal/testutil/helpers.go",
		"superset/conftest.py",
		// Only the dotted JS/TS rule catches this — a real test outside any test tree.
		"superset-frontend/cypress-base/cypress/e2e/explore/chart.test.js",
	}
	for _, p := range testPaths {
		if !isTestPath(p) {
			t.Errorf("isTestPath(%q) = false, want true", p)
		}
	}

	// fixed/28 class. The old `strings.Contains(p, "_test.")` claimed the first of
	// these — the very production ActiveJob that the identically-shaped ignore glob
	// once deleted from the graph — and the unqualified `test_` prefix claimed the
	// rest. Excluding production code from dead-code analysis is a silent false
	// negative, so these are pinned.
	prodPaths := []string{
		"app/jobs/hood_message/finish_kindness_reminder_ab_test.rb",
		"app/jobs/test_job.rb",
		"app/jobs/test_fail_job.rb",
		"superset/cli/test_db.py",
		"superset/commands/database/test_connection.py",
		"Sources/NebenanFoundation/Components/KindnessReminderABTest/KindnessReminderABTest.swift",
		"api/src/main/java/de/nebenan/app/api/model/ABTest.kt",
	}
	for _, p := range prodPaths {
		if isTestPath(p) {
			t.Errorf("isTestPath(%q) = true, want false — production code must stay in the graph", p)
		}
	}
}

// --- new/57 / GAP-MC-04: the denominator must be the population ---

// TestPopulation_IsTheCandidateSet pins what total_symbols is supposed to mean.
// collect() returns every symbol in the repo; isExcluded (test paths, entry points,
// framework-loaded) and passesFilters run later, inside classify. So len(syms) — what
// the handler passed as total_symbols — counts symbols that were never orphan
// candidates at all. The denominator was wrong even with NO filter: on nan/nebenan-iOS
// it reported 39241 where isTestPath alone removes 5491, understating the dead-code
// rate from ≥11.0% to 9.5%.
func TestPopulation_IsTheCandidateSet(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.Live", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/live.go"},
		{Name: "pkg.AlsoLive", Kind: facts.SymbolStruct, Package: "pkg", File: "pkg/model.go"},
		// Never a candidate: test-support code.
		{Name: "pkg.Helper", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/testutil/helper.go"},
		{Name: "spec.Case", Kind: facts.SymbolFunc, Package: "spec", File: "spec/case_spec.rb"},
		// Never a candidate: entry point.
		{Name: "main.main", Kind: facts.SymbolFunc, Package: "main", File: "cmd/main.go"},
		// A different package, for the filter case.
		{Name: "other.Thing", Kind: facts.SymbolFunc, Package: "other", File: "other/thing.go"},
	}

	// Unfiltered: the excluded symbols must not be counted. This is the half the
	// bug report missed entirely.
	if got, want := population(syms, options{Mode: "both", Visibility: "all"}), 3; got != want {
		t.Errorf("unfiltered population = %d, want %d (test/entry-point symbols are not candidates)", got, want)
	}

	// Filtered: the denominator narrows with the filter, so total_orphans/total_symbols
	// is a rate for the thing the caller asked about.
	if got, want := population(syms, options{Mode: "both", Visibility: "all", Package: "pkg"}), 2; got != want {
		t.Errorf("population(package=pkg) = %d, want %d", got, want)
	}
	if got, want := population(syms, options{Mode: "both", Visibility: "all", Package: "other"}), 1; got != want {
		t.Errorf("population(package=other) = %d, want %d", got, want)
	}

	// include_tests re-admits the test symbols as candidates, so the denominator grows
	// with them — the population must track the options it was given.
	if got := population(syms, options{Mode: "both", Visibility: "all", IncludeTests: true}); got <= 3 {
		t.Errorf("population(include_tests) = %d, want > 3", got)
	}
}

// TestPopulation_AgreesWithClassify is the anti-drift guard. The population and the
// orphan set must be admitted by the SAME gate — two copies of an admission test is
// exactly how orphans and perf drifted apart (fixed/51), so isCandidate lives once.
// An orphan can never be outside the population it was drawn from.
func TestPopulation_AgreesWithClassify(t *testing.T) {
	syms := []symInput{
		{Name: "pkg.Live", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/live.go"},
		{Name: "pkg.Used", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/used.go"},
		{Name: "pkg.Helper", Kind: facts.SymbolFunc, Package: "pkg", File: "pkg/mocks/helper.go"},
	}
	refs := refsFrom(map[string][]string{"pkg.Live": {"pkg.Used"}})

	for _, opts := range []options{
		{Mode: "both", Visibility: "all"},
		{Mode: "both", Visibility: "all", Package: "pkg"},
		{Mode: "both", Visibility: "all", Kind: facts.SymbolFunc},
	} {
		orphans := classify(syms, refs, opts)
		pop := population(syms, opts)
		if len(orphans) > pop {
			t.Errorf("opts %+v: %d orphans drawn from a population of %d — the gate has drifted",
				opts, len(orphans), pop)
		}
	}
}

// TestSummarize_ScopedDenominator pins the response contract: total_symbols is the
// candidate population, repo_wide_symbols is the repo, and they are distinguishable.
func TestSummarize_ScopedDenominator(t *testing.T) {
	orphans := []Orphan{
		{Name: "pkg.A", Kind: facts.SymbolFunc, Class: classUnreferenced, Confidence: confHigh},
	}
	s := summarize(orphans, 40, 100, "both")
	if s.TotalSymbols != 40 {
		t.Errorf("TotalSymbols = %d, want 40 (the candidate population)", s.TotalSymbols)
	}
	if s.RepoWideSymbols != 100 {
		t.Errorf("RepoWideSymbols = %d, want 100 (the repo, explicitly named)", s.RepoWideSymbols)
	}
	if s.TotalOrphans != 1 {
		t.Errorf("TotalOrphans = %d, want 1 — this fix changes a denominator, not the analysis", s.TotalOrphans)
	}
}

// The demo/benchmark trees and the two new framework signals must all be dropped
// from the default analysis, and each must stay reachable via its documented
// opt-in. A genuine production orphan alongside them must still be reported —
// suppression that also hides real findings is worse than the noise it removes.
func TestClassify_ExcludesSampleTreesGeneratedAndRegistered(t *testing.T) {
	py := func(name, file string) symInput {
		return symInput{Name: name, Kind: facts.SymbolFunc, File: file,
			Package: "p", Exported: true, Language: "python"}
	}
	registered := py("app/api/client.exception_handler", "app/api/client.py")
	registered.FrameworkReg = true
	generated := py("app/baml_client/config.set_log_level", "app/baml_client/config.py")
	generated.Generated = true

	example := py("examples/demos/pipeline.run_simple_pipeline", "examples/demos/pipeline.py")
	example.Package = "examples/demos"
	syms := []symInput{
		example,
		py("evals/src/modal_apps/bench.run_benchmark", "evals/src/modal_apps/bench.py"),
		py("benchmarks/throughput.measure", "benchmarks/throughput.py"),
		py("notebooks/explore.plot_it", "notebooks/explore.py"),
		registered,
		generated,
		py("app/shared/cache.read_cache_file", "app/shared/cache.py"),
	}
	names := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all"}))

	for _, n := range []string{
		"examples/demos/pipeline.run_simple_pipeline",
		"evals/src/modal_apps/bench.run_benchmark",
		"benchmarks/throughput.measure",
		"notebooks/explore.plot_it",
		"app/api/client.exception_handler",
		"app/baml_client/config.set_log_level",
	} {
		if _, ok := names[n]; ok {
			t.Errorf("%s should be excluded (sample tree, generated, or framework-registered)", n)
		}
	}
	if _, ok := names["app/shared/cache.read_cache_file"]; !ok {
		t.Error("a genuine production orphan must still be reported")
	}

	// package= opts back into a sample tree; include_entrypoints surfaces the
	// decorator-registered handler.
	scoped := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all", Package: "examples"}))
	if _, ok := scoped["examples/demos/pipeline.run_simple_pipeline"]; !ok {
		t.Error("package=examples should surface the examples tree")
	}
	withEntry := orphansByName(classify(syms, map[string]map[string]struct{}{}, options{Mode: "both", Visibility: "all", IncludeEntrypoints: true}))
	if _, ok := withEntry["app/api/client.exception_handler"]; !ok {
		t.Error("include_entrypoints=true should surface decorator-registered handlers")
	}
}
