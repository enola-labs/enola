package swiftextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// callTargets returns the calls targets of the fact named name.
func valueCallTargets(t *testing.T, ff []facts.Fact, name string) []string {
	t.Helper()
	for _, f := range ff {
		if f.Name == name && f.Kind != facts.KindModule {
			var out []string
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls {
					out = append(out, r.Target)
				}
			}
			return out
		}
	}
	t.Fatalf("no fact %s", name)
	return nil
}

// A function or method handed over by name, not called, is a use of it: an
// argument, a dictionary or array value, the right side of an assignment or a let,
// an operand of a custom operator, a returned value.
func TestFunctionValuesRecordUse(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/funcs.swift": `
func mapE(_ x: Int) -> Int { x }
func routeD() {}
func routeE() {}
func styleL(_ l: Label) -> Label { l }
func isOk(_ x: Int) -> Bool { true }
func pick() -> (Int) -> Bool { return isOk }
`,
		"Sources/App/use.swift": `
let aliasStyle = styleL

func run(xs: [Int], l: Label) {
    _ = xs.map(mapE)
    let table = ["/a": routeD]
    let list = [routeE]
    _ = l |> styleL
}

final class Cell {
    func configure(_ x: Int) {}
    func bind(obs: Signal) {
        obs.observeValues(self.configure)
        obs.observeValues(configure)
        obs.map(Helper.make)
    }
}

struct Helper {
    static func make(_ x: Int) -> Int { x }
}
`,
	})
	cases := map[string]string{
		"Sources/App.run":        "Sources/App.mapE",
		"Sources/App.pick":       "Sources/App.isOk",
		"Sources/App.aliasStyle": "Sources/App.styleL",
		"Sources/App.Cell.bind":  "Sources/App.Cell.configure",
	}
	for from, to := range cases {
		if got := valueCallTargets(t, ff, from); !slices.Contains(got, to) {
			t.Errorf("%s: calls %v, want %s", from, got, to)
		}
	}
	run := valueCallTargets(t, ff, "Sources/App.run")
	for _, want := range []string{"Sources/App.routeD", "Sources/App.routeE", "Sources/App.styleL"} {
		if !slices.Contains(run, want) {
			t.Errorf("run: calls %v, want %s", run, want)
		}
	}
	if got := valueCallTargets(t, ff, "Sources/App.Cell.bind"); !slices.Contains(got, "Sources/App.Helper.make") {
		t.Errorf("bind: calls %v, want Sources/App.Helper.make", got)
	}
	f, _ := findFact(ff, "Sources/App.run")
	if refs, _ := f.PropAny("value_refs").([]string); !slices.Contains(refs, "Sources/App.mapE") {
		t.Errorf("run value_refs = %v, want Sources/App.mapE among them", refs)
	}
}

// A name that is a parameter, a local, a closure parameter, a nested function or a
// property is not the function of the same name.
func TestFunctionValuesSkipNonFunctions(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/funcs.swift": `
func mapE(_ x: Int) -> Int { x }
func routeD() {}
func styleL(_ l: Int) -> Int { l }
func title() -> String { "" }
func helper() {}
`,
		"Sources/App/use.swift": `
func param(mapE: Int) { use(mapE) }
func local() {
    let routeD = 1
    use(routeD)
}
func closure(xs: [Int]) {
    xs.forEach { styleL in use(styleL) }
}
func nested(xs: [Int]) {
    func helper(_ x: Int) -> Int { x }
    _ = xs.map(helper)
}

final class Screen {
    var title: String = ""
    func show() { use(title) }
}

final class Item {
    var label: String = ""
    func label(for x: Int) -> String { "" }
    func read() { use(self.label) }
}

enum Mode { case fast }
func mode() { use(Mode.fast) }
`,
	})
	for from, bad := range map[string]string{
		"Sources/App.param":       "Sources/App.mapE",
		"Sources/App.local":       "Sources/App.routeD",
		"Sources/App.closure":     "Sources/App.styleL",
		"Sources/App.nested":      "Sources/App.helper",
		"Sources/App.Screen.show": "Sources/App.title",
		"Sources/App.Item.read":   "Sources/App.Item.label",
	} {
		if got := valueCallTargets(t, ff, from); slices.Contains(got, bad) {
			t.Errorf("%s: calls %v, must not reach %s", from, got, bad)
		}
	}
	for _, f := range ff {
		if f.Name == "Sources/App.mode" {
			if refs, ok := f.PropAny("value_refs").([]string); ok {
				t.Errorf("an enum case read as a function value: %v", refs)
			}
		}
	}
}

// A function handed over is reachable, not run: the performs_io closure does not
// follow a value reference.
func TestFunctionValuesDoNotCarryIO(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/io.swift": `
import Foundation
func load(_ u: URL) async throws -> Data {
    let (d, _) = try await URLSession.shared.data(from: u)
    return d
}
func register(router: Router) {
    router.on("/load", load)
}
`,
	})
	if got := valueCallTargets(t, ff, "Sources/App.register"); !slices.Contains(got, "Sources/App.load") {
		t.Fatalf("register: calls %v, want Sources/App.load", got)
	}
	l, _ := findFact(ff, "Sources/App.load")
	if io, _ := l.PropAny("performs_io").(bool); !io {
		t.Fatalf("load should perform I/O: %v", l.Props)
	}
	r, _ := findFact(ff, "Sources/App.register")
	if io, _ := r.PropAny("performs_io").(bool); io {
		t.Errorf("register only hands load over, yet performs_io is set")
	}
}

// A member a framework supertype may declare is not a protocol extension's method
// of the same name: `self.view` in a view controller is UIKit's property.
func TestFunctionValuesIgnoreInheritedNames(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/nib.swift": `
protocol NibLoading {}
extension NibLoading {
    func view(fromNib nib: String) -> UIView? { nil }
}
final class BannerViewController: UIViewController, NibLoading {
    func show() { present(self.view) }
}
func reload(vc: UIViewController) { use(vc.view) }
`,
	})
	for _, from := range []string{"Sources/App.BannerViewController.show", "Sources/App.reload"} {
		if got := valueCallTargets(t, ff, from); slices.Contains(got, "Sources/App.NibLoading.view") {
			t.Errorf("%s: calls %v, must not reach NibLoading.view", from, got)
		}
	}
}

// A file-private function declared in two files of a module is one name, and a
// property of an unrelated type cannot be reached by a bare name.
func TestFunctionValuesBindPrivateDuplicates(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/a.swift": `
final class A {
    func bind(xs: Signal) { _ = xs.map(isValid) }
}
private func isValid(_ x: Int) -> Bool { true }
`,
		"Sources/App/b.swift": `
final class B {
    func bind(xs: Signal) { _ = xs.map(posts) }
}
private func isValid(_ x: Int) -> Bool { false }
private func posts(_ x: Int) -> Int { x }
`,
		"Sources/App/model.swift": `
struct Project { var posts: Int = 0 }
`,
	})
	if got := valueCallTargets(t, ff, "Sources/App.A.bind"); !slices.Contains(got, "Sources/App.isValid") {
		t.Errorf("A.bind: calls %v, want Sources/App.isValid", got)
	}
	if got := valueCallTargets(t, ff, "Sources/App.B.bind"); !slices.Contains(got, "Sources/App.posts") {
		t.Errorf("B.bind: calls %v, want Sources/App.posts", got)
	}
}

// `self.tableView` in a table view controller is UIKit's property, not the
// delegate callback the class overrides under the same base name.
func TestFunctionValuesSkipDelegateCallbacks(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/list.swift": `
final class ListViewController: UITableViewController {
    override func viewDidLoad() {
        _ = self.tableView |> tableStyle
    }
    override func tableView(_ tableView: UITableView, didSelectRowAt indexPath: IndexPath) {}
}
func tableStyle(_ t: UITableView) -> UITableView { t }
`,
	})
	got := valueCallTargets(t, ff, "Sources/App.ListViewController.viewDidLoad")
	if slices.Contains(got, "Sources/App.ListViewController.tableView") {
		t.Errorf("viewDidLoad: calls %v, must not reach the tableView delegate callback", got)
	}
	if !slices.Contains(got, "Sources/App.tableStyle") {
		t.Errorf("viewDidLoad: calls %v, want Sources/App.tableStyle", got)
	}
	for _, f := range ff {
		if _, ok := f.Props[propDelegateCallback]; ok {
			t.Errorf("%s keeps the internal %s prop", f.Name, propDelegateCallback)
		}
	}
}

// A file's top-level fact kept only for names that bind nothing is dropped, as one
// whose statements call nothing is; one that binds keeps its edge.
func TestFunctionValuesFileScopeFact(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/routes.swift": `
private func home() {}
let table = ["/": home]
_ = ["/x": unknownThing]
`,
		"Sources/App/empty.swift": `
_ = [1: someLocalName]
`,
	})
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.File == "Sources/App/empty.swift" {
			t.Errorf("file fact kept with nothing bound: %+v", f)
		}
		for k := range f.Props {
			if k == propValueNotes || k == propDropUnlessValues || k == propDelegateCallback {
				t.Errorf("%s keeps internal prop %s", f.Name, k)
			}
		}
	}
	if got := valueCallTargets(t, ff, "Sources/App.table"); !slices.Contains(got, "Sources/App.home") {
		t.Errorf("table: calls %v, want Sources/App.home", got)
	}
}
