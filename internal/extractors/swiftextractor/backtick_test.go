package swiftextractor

import (
	"slices"
	"testing"
)

// A declaration named with a keyword escapes it in backticks; a call after a dot
// writes it bare. The declared name is the name without them, so an explicit call
// binds to it, and a test's call to it is a reference.
func TestSwiftBacktickEscapedNamesBind(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Sources/App/Middleware.swift": "struct ErrorMiddleware {\n" +
			"    static func `default`(environment: String) -> ErrorMiddleware { ErrorMiddleware() }\n" +
			"    static let `extension` = \"txt\"\n" +
			"}\n" +
			"final class App {\n" +
			"    func middlewares() {\n" +
			"        let m = ErrorMiddleware.default(environment: \"dev\")\n" +
			"        let n = ErrorMiddleware.`default`(environment: \"prod\")\n" +
			"    }\n" +
			"}\n",
	})
	names := map[string]bool{}
	for _, f := range ff {
		names[f.Name] = true
	}
	for _, want := range []string{"Sources/App.ErrorMiddleware.default", "Sources/App.ErrorMiddleware.extension"} {
		if !names[want] {
			t.Errorf("no fact %s", want)
		}
	}
	if calls := swCalls(t, ff, "Sources/App.App.middlewares"); !slices.Contains(calls, "Sources/App.ErrorMiddleware.default") {
		t.Errorf("middlewares calls %v, want ErrorMiddleware.default", calls)
	}
	refs := swiftTestRefTargets(t, "Tests/AppTests/MiddlewareTests.swift",
		"func testDefault() { let m = `default`(environment: \"x\"); let c = Config.`for`(1) }\n")
	for _, want := range []string{"default", "for"} {
		if !refs[want] {
			t.Errorf("test refs %v, want %s", sortedTargets(refs), want)
		}
	}
}
