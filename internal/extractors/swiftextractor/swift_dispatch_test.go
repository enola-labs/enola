package swiftextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// extractRepo writes the given files into a temp repo and runs the full Extract
// pipeline (including canonicalizeTargets + resolveMethodCalls), so tentative
// member-call edges are resolved/dropped as in a real snapshot.
func extractRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	repo := t.TempDir()
	var names []string
	for rel, content := range files {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, rel)
	}
	sort.Strings(names) // the walk sees files in path order, as a snapshot does
	ff, err := New().Extract(context.Background(), repo, names)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

// --- Prong 2: methods vs free functions ---

func TestSymbolKind_MethodVsFunc(t *testing.T) {
	ff := extractAST(t, `
func topLevel() {}
class C {
    func member() {}
}
`, false)

	top, ok := findFact(ff, "pkg.topLevel")
	if !ok {
		t.Fatal("expected fact for pkg.topLevel")
	}
	if got := top.Props["symbol_kind"]; got != facts.SymbolFunc {
		t.Errorf("topLevel symbol_kind = %v, want %q", got, facts.SymbolFunc)
	}

	m, ok := findFact(ff, "pkg.C.member")
	if !ok {
		t.Fatal("expected fact for pkg.C.member")
	}
	if got := m.Props["symbol_kind"]; got != facts.SymbolMethod {
		t.Errorf("member symbol_kind = %v, want %q", got, facts.SymbolMethod)
	}
	if got := m.Props["receiver"]; got != "C" {
		t.Errorf("member receiver = %v, want C", got)
	}
}

// --- Prong 1: self / self? dispatch (walker-level) ---

func TestRelCalls_SelfOptionalChaining(t *testing.T) {
	// self?.foo() inside a [weak self] closure must produce the same edge as
	// self.foo(); the grammar folds "?." into the navigation token.
	ff := extractAST(t, `
class Coord {
    func start() {
        handler = { [weak self] in
            self?.foo()
        }
    }
    func foo() {}
}
`, false)

	f, ok := findFact(ff, "pkg.Coord.start")
	if !ok {
		t.Fatal("expected fact for pkg.Coord.start")
	}
	if !hasRelation(f, facts.RelCalls, "pkg.Coord.foo") {
		t.Errorf("expected calls pkg.Coord.foo via self?.foo(); relations=%v", f.Relations)
	}
}

func TestRelCalls_LowercaseReceiverTentative(t *testing.T) {
	// A member call on a lowercase receiver emits a tentative bare short-name edge
	// at walk time (resolved in the post-pass).
	ff := extractAST(t, `
class Coord {
    func start() {
        coordinator?.showRecoverPassword()
        delegate?.infoTouched()
    }
}
`, false)

	f, ok := findFact(ff, "pkg.Coord.start")
	if !ok {
		t.Fatal("expected fact for pkg.Coord.start")
	}
	if !hasRelation(f, facts.RelCalls, "showRecoverPassword") {
		t.Errorf("expected tentative calls showRecoverPassword; relations=%v", f.Relations)
	}
	if !hasRelation(f, facts.RelCalls, "infoTouched") {
		t.Errorf("expected tentative calls infoTouched; relations=%v", f.Relations)
	}
}

func TestRelCalls_TypeDotMethod(t *testing.T) {
	// Foo.bar() credits both the root type and the invoked method short name.
	ff := extractAST(t, `
class C {
    func go() {
        AppComposition.shared.makeRepo()
    }
}
`, false)

	f, ok := findFact(ff, "pkg.C.go")
	if !ok {
		t.Fatal("expected fact for pkg.C.go")
	}
	if !hasRelation(f, facts.RelCalls, "AppComposition") {
		t.Errorf("expected calls AppComposition (root type); relations=%v", f.Relations)
	}
	// The method is noted with its root, for resolveRootedCalls to bind once the
	// repository's declarations are known.
	notes, _ := f.PropAny(propRootedCalls).([]swiftRootedCall)
	if !slices.ContainsFunc(notes, func(n swiftRootedCall) bool {
		return n.root == "AppComposition" && n.method == "makeRepo" && slices.Equal(n.via, []string{"shared"})
	}) {
		t.Errorf("expected rooted call AppComposition.makeRepo; notes=%v", notes)
	}
}

func TestRelCalls_DeferSuppressed(t *testing.T) {
	ff := extractAST(t, `
class C {
    func go() {
        defer { cleanup() }
    }
    func cleanup() {}
}
`, false)

	f, ok := findFact(ff, "pkg.C.go")
	if !ok {
		t.Fatal("expected fact for pkg.C.go")
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && (r.Target == "defer" || r.Target == "pkg.C.defer" || r.Target == "pkg.defer") {
			t.Errorf("unexpected phantom defer edge: %v", r)
		}
	}
	// the closure body's real call is still captured
	if !hasRelation(f, facts.RelCalls, "pkg.C.cleanup") {
		t.Errorf("expected calls pkg.C.cleanup inside defer; relations=%v", f.Relations)
	}
}

// --- Prong 1: post-pass resolution (full Extract pipeline) ---

func TestExtract_CrossExtensionAndClosureRouting(t *testing.T) {
	// The dominant coordinator idiom: start() dispatches enum routing via
	// self?.method() inside a [weak self] switch closure, where the routing
	// methods live in a separate extension of the same type. The old
	// currentMethods gate dropped these; they must now resolve.
	ff := extractRepo(t, map[string]string{
		"Feed/FeedSearchCoordinator.swift": `
final class FeedSearchCoordinator {
    func start() {
        controller.viewModel.transitionHandler = { [weak self] in
            switch $0 {
            case .createContentButtonTapped: self?.createContentButtonTapped()
            case .showPostingDetail:         self?.showPostingDetail()
            }
        }
    }
}
extension FeedSearchCoordinator {
    func createContentButtonTapped() {}
    func showPostingDetail() {}
}
`,
	})

	start, ok := findFact(ff, "Feed.FeedSearchCoordinator.start")
	if !ok {
		t.Fatalf("expected start fact; facts=%v", factNames(ff))
	}
	for _, want := range []string{
		"Feed.FeedSearchCoordinator.createContentButtonTapped",
		"Feed.FeedSearchCoordinator.showPostingDetail",
	} {
		if !hasRelation(start, facts.RelCalls, want) {
			t.Errorf("expected resolved calls %s; relations=%v", want, start.Relations)
		}
	}
}

func TestExtract_LowercaseReceiverResolvesUnique(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Onb/OnboardingCoordinator.swift": `
final class OnboardingCoordinator {
    func go() {
        coordinator?.showRecoverPassword()
    }
}
`,
		"Onb/PasswordCoordinator.swift": `
final class PasswordCoordinator {
    func showRecoverPassword() {}
}
`,
	})

	f, ok := findFact(ff, "Onb.OnboardingCoordinator.go")
	if !ok {
		t.Fatalf("expected go fact; facts=%v", factNames(ff))
	}
	if !hasRelation(f, facts.RelCalls, "Onb.PasswordCoordinator.showRecoverPassword") {
		t.Errorf("expected resolved calls to PasswordCoordinator.showRecoverPassword; relations=%v", f.Relations)
	}
}

func TestExtract_AmbiguousStaysBare(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"A/A.swift": `
final class A {
    func caller() { thing?.refresh() }
}
`,
		"B/B.swift": `final class B { func refresh() {} }`,
		"C/C.swift": `final class C { func refresh() {} }`,
	})

	f, ok := findFact(ff, "A.A.caller")
	if !ok {
		t.Fatalf("expected caller fact; facts=%v", factNames(ff))
	}
	if !hasRelation(f, facts.RelCalls, "refresh") {
		t.Errorf("expected ambiguous edge to stay bare 'refresh'; relations=%v", f.Relations)
	}
}

func TestExtract_UnknownFrameworkCallDropped(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"A/A.swift": `
final class A {
    func caller() {
        view.someFrameworkOnlyMethod()
    }
}
`,
	})

	f, ok := findFact(ff, "A.A.caller")
	if !ok {
		t.Fatalf("expected caller fact; facts=%v", factNames(ff))
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && r.Target == "someFrameworkOnlyMethod" {
			t.Errorf("expected unknown framework call to be dropped, found %v", r)
		}
	}
}

// --- Pure post-pass function ---

func TestResolveMethodCalls(t *testing.T) {
	methodIndex := map[string][]string{
		"unique": {"Mod.T.unique"},
		"ambig":  {"Mod.A.ambig", "Mod.B.ambig"},
	}
	funcIndex := map[string][]string{
		"flattened": {"Mod.flattened"}, // a method flattened to a top-level function
		"unique":    {"Mod.unique"},    // also a func, but methodIndex wins
	}
	ff := []facts.Fact{{
		Kind: facts.KindSymbol,
		Name: "Mod.Caller.go",
		Relations: []facts.Relation{
			{Kind: facts.RelCalls, Target: "unique"},         // -> methodIndex wins
			{Kind: facts.RelCalls, Target: "ambig"},          // -> keep bare
			{Kind: facts.RelCalls, Target: "flattened"},      // -> funcIndex fallback
			{Kind: facts.RelCalls, Target: "missing"},        // -> drop
			{Kind: facts.RelCalls, Target: "Mod.T.explicit"}, // dotted, untouched
			{Kind: facts.RelCalls, Target: "SomeType"},       // capitalized, untouched
			{Kind: facts.RelInstantiates, Target: "unique"},  // non-call, untouched
		},
	}}

	resolveMethodCalls(ff, &swiftIndexes{prod: swiftNameIndex{methods: methodIndex, funcs: funcIndex}})

	got := ff[0].Relations
	want := []facts.Relation{
		{Kind: facts.RelCalls, Target: "Mod.T.unique"},
		{Kind: facts.RelCalls, Target: "ambig"},
		{Kind: facts.RelCalls, Target: "Mod.flattened"},
		{Kind: facts.RelCalls, Target: "Mod.T.explicit"},
		{Kind: facts.RelCalls, Target: "SomeType"},
		{Kind: facts.RelInstantiates, Target: "unique"},
	}
	if len(got) != len(want) {
		t.Fatalf("relations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("relation[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExtract_FlattenedTypeMethodResolves is the end-to-end guard for the Round-3
// fix: when a type fails to parse and its methods are emitted as top-level
// functions, a member call on that type still resolves via the funcIndex fallback.
// We simulate the flattened output directly (a top-level `func` + a caller doing
// `obj.method()`), since reproducing the tree-sitter parse failure in a fixture is
// brittle.
func TestExtract_FlattenedTypeMethodResolves(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		// Flattened: addAsyncImageUploads emitted as a top-level function.
		"NCC/ImageUploadModel.swift": `
public func addAsyncImageUploads(_ images: [Int]) {}
`,
		"NCC/ImagePickerController.swift": `
final class ImagePickerController {
    func pick() {
        uploadModel.addAsyncImageUploads([1])
    }
}
`,
	})

	f, ok := findFact(ff, "NCC.ImagePickerController.pick")
	if !ok {
		t.Fatalf("expected pick fact; facts=%v", factNames(ff))
	}
	if !hasRelation(f, facts.RelCalls, "NCC.addAsyncImageUploads") {
		t.Errorf("expected member call to resolve to flattened top-level function; relations=%v", f.Relations)
	}
}

// TestExtensionPropertyGetterCalls guards the Round-3 computed-getter fix: a call
// in a property initializer/getter inside an `extension` (which pushes no type
// owner) must attach to the property, not be dropped — else a helper called only
// from an extension property looks dead.
func TestExtensionPropertyGetterCalls(t *testing.T) {
	ff := extractAST(t, `
extension Bar {
    public static var item: Self {
        return helper()
    }
}
func helper() -> Bar { Bar() }
`, false)

	f, ok := findFact(ff, "pkg.Bar.item")
	if !ok {
		t.Fatalf("expected pkg.Bar.item fact; facts=%v", factNames(ff))
	}
	if !hasRelation(f, facts.RelCalls, "pkg.helper") {
		t.Errorf("expected extension computed-property getter to emit calls -> pkg.helper; relations=%v", f.Relations)
	}
}

// --- Round 2, Gap A: file-scope call edges (KindFileRef) ---

func TestFileScope_TopLevelCallsEmitFileRef(t *testing.T) {
	// A #!/usr/bin/swift-style script: top-level funcs invoked at file scope must be
	// recorded as used via a KindFileRef fact, or they look orphaned.
	ff := extractAST(t, `
func analyzeLocalizations() {}
func updateStrings() {}
let r = readPropertyList()
analyzeLocalizations()
updateStrings()
`, false)

	var fileRef *facts.Fact
	for i := range ff {
		if ff[i].Kind == facts.KindFileRef {
			fileRef = &ff[i]
			break
		}
	}
	if fileRef == nil {
		t.Fatalf("expected a file_ref fact; facts=%v", factNames(ff))
	}
	for _, want := range []string{"pkg.analyzeLocalizations", "pkg.updateStrings", "pkg.readPropertyList"} {
		if !hasRelation(*fileRef, facts.RelCalls, want) {
			t.Errorf("expected file_ref calls %s; relations=%v", want, fileRef.Relations)
		}
	}
}

func TestFileScope_NoFileRefWhenOnlyDeclarations(t *testing.T) {
	// A normal source file with only declarations must NOT emit a file_ref fact.
	ff := extractAST(t, `
import Foundation
class C {
    func go() { helper() }
    func helper() {}
}
`, false)
	for _, f := range ff {
		if f.Kind == facts.KindFileRef {
			t.Errorf("did not expect a file_ref fact for a declaration-only file; got %+v", f)
		}
	}
}

// --- Round 2, Gap B-2: custom operator usage ---

func TestOperator_CustomInfixTracked(t *testing.T) {
	// `a <- b` where `func <-` is a project operator must emit a resolved calls edge
	// to the operator; a standard `x + y` must NOT (would flood arithmetic sites).
	ff := extractRepo(t, map[string]string{
		"F/Operators.swift": `
infix operator <-
func <- (lhs: Observable, rhs: Int) {}
func + (lhs: [String: Int], rhs: [String: Int]) -> [String: Int] { lhs }
`,
		"F/Use.swift": `
final class Binder {
    func bind() {
        observable <- 5
        let merged = dictA + dictB
    }
}
`,
	})

	f, ok := findFact(ff, "F.Binder.bind")
	if !ok {
		t.Fatalf("expected bind fact; facts=%v", factNames(ff))
	}
	if !hasRelation(f, facts.RelCalls, "F.<-") {
		t.Errorf("expected resolved calls to F.<- (custom operator); relations=%v", f.Relations)
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && (r.Target == "+" || r.Target == "F.+") {
			t.Errorf("standard-token operator + must NOT be tracked; found %v", r)
		}
	}
}

func TestOperator_StandardMulticharNotTracked(t *testing.T) {
	// `<=`/`>=`/`??` tokenize as custom_operator in this grammar but are stdlib
	// operators — overloading them must NOT emit usage edges (would flood fan-in).
	ff := extractRepo(t, map[string]string{
		"T/Time.swift": `
struct Time {
    static func <= (lhs: Time, rhs: Time) -> Bool { true }
    static func >= (lhs: Time, rhs: Time) -> Bool { true }
}
`,
		"T/Use.swift": `
final class Clock {
    func compare() -> Bool {
        return a <= b && c >= d
    }
}
`,
	})

	f, ok := findFact(ff, "T.Clock.compare")
	if !ok {
		t.Fatalf("expected compare fact; facts=%v", factNames(ff))
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && (isStdOpTarget(r.Target)) {
			t.Errorf("standard operator overload must NOT be tracked; found %v", r)
		}
	}
}

func isStdOpTarget(target string) bool {
	switch target {
	case "<=", ">=", "T.Time.<=", "T.Time.>=", "Time.<=", "Time.>=":
		return true
	}
	return false
}

// TestClassPropertyInitAttachesToType covers v43: a class/struct stored-property
// initializer's call edge attaches to the enclosing TYPE (not the property) — the
// mirror of the extension case (v42), which pushes the property as owner. This keeps
// class-property attribution (and the coupling graph) unchanged.
func TestClassPropertyInitAttachesToType(t *testing.T) {
	ff := extractAST(t, `
final class C {
    var item = helper()
}
func helper() -> Int { 0 }
`, false)

	typ, ok := findFact(ff, "pkg.C")
	if !ok {
		t.Fatalf("expected pkg.C fact; facts=%v", factNames(ff))
	}
	if !hasRelation(typ, facts.RelCalls, "pkg.helper") {
		t.Errorf("class property init should attach calls -> pkg.helper to the type; relations=%v", typ.Relations)
	}
	if prop, ok := findFact(ff, "pkg.C.item"); ok && hasRelation(prop, facts.RelCalls, "pkg.helper") {
		t.Errorf("class property fact must NOT own the init call edge (v43); relations=%v", prop.Relations)
	}
}
