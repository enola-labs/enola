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
