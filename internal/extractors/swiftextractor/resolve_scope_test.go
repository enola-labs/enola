package swiftextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func swRelTargets(t *testing.T, ff []facts.Fact, name string) []string {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Name == name {
			var out []string
			for _, r := range f.Relations {
				out = append(out, r.Kind+" "+r.Target)
			}
			return out
		}
	}
	t.Fatalf("no symbol %s", name)
	return nil
}

// A production target cannot import a test target, so a production reference never
// binds to a declaration under a test target's directory: not a type the tests also
// declare, not a method only a test declares, not a test's extension of a framework
// type. A test still binds to its own declarations.
func TestSwiftProductionReferenceSkipsTestDeclarations(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Net/Clock.swift": `
struct Clock {
    func now() {}
}
`,
		"Sources/Net/Uploader.swift": `
import Foundation

class Uploader {
    let fileManager: FileManager
    func run(helper: AnyObject) {
        let c = Clock()
        c.now()
        helper.stamp()
        fileManager.removeItem(at: url)
    }
}
`,
		"Tests/NetTests/Fixtures.swift": `
import Foundation

struct Clock {
    func tick() {}
}

final class Recorder {
    func stamp() {}
}

extension FileManager {
    func removeItem(at url: URL) {}
}

final class UploaderTests {
    func testRun() {
        let c = Clock()
        Recorder().stamp()
    }
}
`,
	})
	prod := swRelTargets(t, ff, "Sources/Net.Uploader.run")
	if !slices.Contains(prod, "instantiates Sources/Net.Clock") {
		t.Errorf("Uploader.run relations %v, want instantiates Sources/Net.Clock", prod)
	}
	for _, r := range prod {
		if strings.Contains(r, "Tests/") {
			t.Errorf("production Uploader.run binds to a test declaration: %s", r)
		}
	}
	test := swRelTargets(t, ff, "Tests/NetTests.UploaderTests.testRun")
	for _, want := range []string{"instantiates Tests/NetTests.Clock", "calls Tests/NetTests.Recorder.stamp"} {
		if !slices.Contains(test, want) {
			t.Errorf("UploaderTests.testRun relations %v, want %s", test, want)
		}
	}
}

// A member call rooted at a type the repository neither declares nor extends
// (Bundle.module.path(...), URL.temporaryDirectory.render()) calls the framework, so
// a method of an in-repo struct that merely shares the name does not capture it. A
// method the repository adds to a framework type or protocol still binds, so does a
// module-qualified top-level function, a chain through a member a protocol extension
// of the repository adds, and a call rooted at a type the repository declares or at
// an untyped value.
func TestSwiftCallOnAForeignTypeRootDoesNotBindByName(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/Style/Bezier.swift": `
struct Bezier {
    func path(in rect: CGRect) -> Path { Path() }
    func render() {}
    func score() {}
}
`,
		"Sources/Store/Repo.swift": `
final class Repo {
    static let shared = Repo()
    func fetch() {}
}

extension Bundle {
    func asset() {}
}

extension View {
    func shine() -> some View { self }
}

extension Color {
    static func shade(_ hex: UInt) -> Color { .red }
}

protocol Extended {}

extension Extended {
    static var ext: Revoker { Revoker() }
}

struct Revoker {
    func revoke() {}
}

func score(_ word: String) -> Int { 0 }

final class Coordinator {
    func show() {}
}
`,
		"Sources/Store/Palette.swift": `
import SwiftUI

let accent = Color.shade(0xF1DC9C)
let glyph = SHA256.render(data: payload)
`,
		"Sources/Store/Client.swift": `
import Foundation

final class Client {
    func load(coordinator: AnyObject) {
        let p = Bundle.module.path(forResource: "Words", ofType: "db")
        URL.temporaryDirectory.render()
        Repo.shared.fetch()
        Bundle.main.asset()
        _ = Text("Title").shine()
        SecPolicy.ext.revoke()
        _ = Store.score("CAB")
        coordinator.show()
    }
}
`,
	})
	calls := swCalls(t, ff, "Sources/Store.Client.load")
	for _, bad := range []string{"Sources/Style.Bezier.path", "Sources/Style.Bezier.render"} {
		if slices.Contains(calls, bad) {
			t.Errorf("Client.load calls %v, must not bind %s", calls, bad)
		}
	}
	// At file scope too: the file's statements keep the extension's method and not
	// the struct's.
	var palette []string
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.Name == "Sources/Store/Palette.swift" {
			for _, r := range f.Relations {
				palette = append(palette, r.Target)
			}
		}
	}
	if !slices.Contains(palette, "Sources/Store.Color.shade") || slices.Contains(palette, "Sources/Style.Bezier.render") {
		t.Errorf("Palette.swift calls %v, want Sources/Store.Color.shade and not Sources/Style.Bezier.render", palette)
	}
	for _, want := range []string{"Sources/Store.Repo.fetch", "Sources/Store.Bundle.asset", "Sources/Store.View.shine", "Sources/Store.Revoker.revoke", "Sources/Store.score", "Sources/Store.Coordinator.show"} {
		if !slices.Contains(calls, want) {
			t.Errorf("Client.load calls %v, want %s", calls, want)
		}
	}
}
