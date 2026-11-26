package swiftextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func symbolLines(ff []facts.Fact) map[string]int {
	out := map[string]int{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			out[f.Name] = f.Line
		}
	}
	return out
}

func anyRelation(ff []facts.Fact, from, kind, to string) bool {
	for _, f := range ff {
		if f.Name == from && hasRelation(f, kind, to) {
			return true
		}
	}
	return false
}

// A `sending` type specifier and a `consume` operator are unknown to the grammar,
// and the first one used to turn the whole class into one error node, so the file
// had no symbol and the errors it throws were never constructed.
func TestSwiftSendingAndConsumeKeepTheType(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Lib/Stream.swift": `
struct ReadFailed: Error {}
struct AlreadyReading: Error {}

final class Stream: Sendable {
    private enum Checkout: ~Copyable {
        case busy
        case failed
    }

    init(reader: consuming sending Reader) {}

    func read() throws {
        let checkout = lock { state -> sending Checkout in
            return .busy
        }
        switch consume checkout {
        case .busy:
            throw AlreadyReading()
        case .failed:
            throw ReadFailed()
        }
    }

    func dup(_ x: Int) -> Int {
        let y = copy x
        return y
    }
}
`,
	})
	lines := symbolLines(ff)
	for name, line := range map[string]int{
		"Sources/Lib.Stream":          5,
		"Sources/Lib.Stream.Checkout": 6,
		"Sources/Lib.Stream.read":     13,
		"Sources/Lib.Stream.dup":      25,
	} {
		if got, ok := lines[name]; !ok || got != line {
			t.Errorf("%s: line %d (present %v), want %d", name, got, ok, line)
		}
	}
	for _, target := range []string{"Sources/Lib.AlreadyReading", "Sources/Lib.ReadFailed"} {
		if !anyRelation(ff, "Sources/Lib.Stream.read", "instantiates", target) &&
			!anyRelation(ff, "Sources/Lib.Stream.read", "calls", target) {
			t.Errorf("Stream.read records no construction of %s", target)
		}
	}
}

// A freestanding macro with a trailing closure in a type body closed the type at
// its `#`: the type lost its symbol and its methods surfaced at file scope.
func TestSwiftTypeBodyMacroKeepsTheType(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Controllers.swift": `
struct User {}

@Controller
private struct KeyPathController {
    let client: Int

    #Middleware(using: \Self.client) {
        #Auth(User.self) {
            @GET("me")
            func me(user: User) -> User { user }
        }
    }

    #warning("todo")

    @GET("attached")
    func attached(user: User) -> User { user }
}

func helper() {
    #expect(1 == 1)
}
`,
	})
	lines := symbolLines(ff)
	for name, line := range map[string]int{
		"Sources/App.KeyPathController":          4,
		"Sources/App.KeyPathController.me":       10,
		"Sources/App.KeyPathController.attached": 17,
		"Sources/App.helper":                     21,
	} {
		if got, ok := lines[name]; !ok || got != line {
			t.Errorf("%s: line %d (present %v), want %d", name, got, ok, line)
		}
	}
	if _, ok := lines["Sources/App.attached"]; ok {
		t.Errorf("attached still surfaces at file scope")
	}
}

// The rewrites keep every byte offset and touch only the forms they target: a
// variable named `sending` or `copy`, a macro inside a function body, strings and
// comments stay as they are.
func TestSwiftRecoveryRewritesPreserveOffsets(t *testing.T) {
	src := `let sending = true
let copy = 1
func f(x: sending Foo) -> sending Bar {
    let s = "consume x \(copy) sending y"
    // switch consume x
    if sending { _ = copy }
    #expect(x)
    switch consume x { default: break }
}
struct S {
    #Macro<T>(a: "}", b: (1, 2)) {
        func m() {}
    }
}
`
	out, changed := blankOwnershipSpecifiers([]byte(src))
	if !changed || len(out) != len(src) {
		t.Fatalf("changed=%v len %d, want true and %d", changed, len(out), len(src))
	}
	want := `let sending = true
let copy = 1
func f(x:         Foo) ->         Bar {
    let s = "consume x \(copy) sending y"
    // switch consume x
    if sending { _ = copy }
    #expect(x)
    switch         x { default: break }
}
struct S {
    #Macro<T>(a: "}", b: (1, 2)) {
        func m() {}
    }
}
`
	if string(out) != want {
		t.Errorf("ownership rewrite:\n%s\nwant:\n%s", out, want)
	}
	out, changed = blankTypeBodyMacros([]byte(src))
	if !changed || len(out) != len(src) {
		t.Fatalf("macro rewrite changed=%v len %d", changed, len(out))
	}
	got := strings.Split(string(out), "\n")
	if got[6] != "    #expect(x)" {
		t.Errorf("macro in a function body was blanked: %q", got[6])
	}
	if strings.TrimSpace(got[10]) != "" || strings.TrimSpace(got[12]) != "" {
		t.Errorf("type-body macro not blanked: %q / %q", got[10], got[12])
	}
	if got[11] != "        func m() {}" {
		t.Errorf("macro body declaration changed: %q", got[11])
	}
}

// `try await` in an if condition made the grammar read the body as a trailing
// closure and the whole file as one error, so the class had no symbol and its
// methods surfaced as top-level functions.
func TestSwiftTryAwaitInIfConditionKeepsTheType(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Store.swift": `
@MainActor
final class Store {
    func bootstrap() async {
        if let result = try? await provider.reconcile(dataset) {
            apply(result)
        }
        if let file = try await decrypt(item) {
            apply(file)
        }
        for try await line in lines {
            apply(line)
        }
    }
    func apply(_ x: Int) {}
}
`,
	})
	calls := swCalls(t, ff, "Sources/App.Store.bootstrap")
	if !slices.Contains(calls, "Sources/App.Store.apply") {
		t.Errorf("Store.bootstrap calls %v, want Sources/App.Store.apply", calls)
	}
	out, changed := blankAwaitAfterTry([]byte("a = try? await f()\nb = try await g()\nfor try await x in s {}\nc = await h()\n"))
	if !changed || string(out) != "a = try?       f()\nb = try       g()\nfor try await x in s {}\nc = await h()\n" {
		t.Errorf("blankAwaitAfterTry = %q", out)
	}
}
