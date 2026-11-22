package swiftextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

var _ plugin.TestRefExtractor = (*SwiftExtractor)(nil)

func swiftTestRefTargets(t *testing.T, relFile, src string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range swiftTestRefsFromFile([]byte(src), relFile) {
		for _, r := range f.Relations {
			out[r.Target] = true
		}
	}
	return out
}

func sortedTargets(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSwiftTestRefsCaptureProductionReferences pins what the pass is for: a helper
// written for the module's tests, a type the tests construct or conform a fake to,
// a static member, an implicit member and a property asserted through `$0` all
// keep an inbound edge once test targets are ignored.
func TestSwiftTestRefsCaptureProductionReferences(t *testing.T) {
	src := `@testable import Alamofire
import XCTest

final class SessionTests: BaseTestCase {
    struct TestCredential: AuthenticationCredential { let token: String }

    func testRequest() async throws {
        let session = Session(configuration: .default)
        session.request(url).validate()
        let encoded = try URLEncodedFormEncoder().encode(params)
        XCTAssertEqual(result, RetryResult.retry)
        XCTAssertEqual(AF.download(url).state, .resumed)
        let policy: Outer.RetryPolicy<Payload>? = nil
        let type = MultipartFormData.EncodingCharacters.self
        await store.send(.task) { $0.step = .first }
        let r = try #require(makeInterceptor())
        let n = Request.ResponseDisposition.allow
    }
}
`
	got := swiftTestRefTargets(t, "Tests/SessionTests.swift", src)
	for _, want := range []string{
		"Session",                    // a construction
		"session.request", "request", // a member call on a local, and its bare form
		"validate",              // a call on a call's result
		"URLEncodedFormEncoder", // a construction used as a receiver
		"encode",
		"RetryResult", "RetryResult.retry", "retry", // a static member and its type
		"AF", "AF.download", "download", "state", // a global's member, a property read
		"resumed",                  // an implicit member
		"AuthenticationCredential", // a protocol a fake conforms to
		"BaseTestCase",             // a superclass the test target shares
		"RetryPolicy", "Payload",   // a nested type in an annotation, a type argument
		"EncodingCharacters", "MultipartFormData", // a metatype
		"task", "step", "first", // TestStore actions and a `$0` state assertion
		"makeInterceptor", // a bare call inside a #require macro
		"ResponseDisposition", "allow",
	} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
}

// TestSwiftTestRefsDropHarnessAndOwnDeclarations keeps the reference set about the
// code under test: the harness's words, a harness receiver's members, imports,
// attributes, system types and the test file's own helpers never vouch for a
// production symbol of the same name.
func TestSwiftTestRefsDropHarnessAndOwnDeclarations(t *testing.T) {
	src := `import Testing
@testable import Feature

extension DateGenerator {
  static func runLoop(_ loop: Int) -> Self { Self { loop } }
}

@Suite struct FeatureTests {
  enum Fixture { case sample }
  final class FakeStore: Store { func load() {} }

  @Test @MainActor func loads() async throws {
    let fake = FakeStore()
    #expect(fake.loaded == true)
    let value = try #require(helper())
    XCTAssertEqual(value, .sample)
    Issue.record("x")
    let ex = expectation(description: "done")
    ex.fulfill()
    wait(for: [ex], timeout: 1)
    self.helper()
    super.setUp()
    $0.date = .runLoop(1)
    FakeStore.make()
    let s: String = ""
  }

  func helper() -> Int { 1 }
}
`
	got := swiftTestRefTargets(t, "Tests/FeatureTests/FeatureTests.swift", src)
	for _, banned := range []string{
		"Testing", "Feature", // imports
		"Suite", "Test", "MainActor", "testable", // attributes
		"expect", "require", "XCTAssertEqual", "expectation", "fulfill", "wait",
		"Issue", "Issue.record", "record", // a harness receiver drops the bare name too
		"FakeStore", "Fixture", "sample", "helper", "runLoop", "loads", // the file's own declarations
		"FakeStore.make", "make", // a call on a type the file declares
		"setUp", "self", "super", "String", "Self",
	} {
		if got[banned] {
			t.Errorf("harness or own name %q recorded; have %v", banned, sortedTargets(got))
		}
	}
	for _, want := range []string{
		"DateGenerator", // the production type an extension in the test extends
		"Store",         // the production protocol a fake conforms to
		"fake.loaded", "loaded",
		"date",
	} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
}

// TestSwiftExtractTestRefsEmitsOnlyTestRefFacts checks the plugin.TestRefExtractor
// contract end to end: one test_ref fact per Swift test file, named by the file,
// carrying only calls relations; files the pass does not own are skipped.
func TestSwiftExtractTestRefsEmitsOnlyTestRefFacts(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Tests/CoreTests/CoreTests.swift", "import XCTest\nfinal class CoreTests: XCTestCase { func testX() { Parser.parse(input) } }\n")
	write("Tests/CoreTests/fixture.json", "{}\n")

	got, err := New().ExtractTestRefs(context.Background(), dir,
		[]string{"Tests/CoreTests/CoreTests.swift", "Tests/CoreTests/fixture.json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d facts, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Kind != facts.KindTestRef || f.Name != "Tests/CoreTests/CoreTests.swift" || f.File != f.Name {
		t.Errorf("fact = %s %q %q, want a test_ref named by its file", f.Kind, f.Name, f.File)
	}
	if f.PropString("language") != "swift" {
		t.Errorf("language = %q, want swift", f.PropString("language"))
	}
	targets := map[string]bool{}
	for _, r := range f.Relations {
		if r.Kind != facts.RelCalls {
			t.Errorf("relation kind %q, want only calls", r.Kind)
		}
		targets[r.Target] = true
	}
	for _, want := range []string{"Parser", "Parser.parse", "parse"} {
		if !targets[want] {
			t.Errorf("missing %s in %+v", want, f.Relations)
		}
	}
	if targets["XCTestCase"] {
		t.Errorf("harness superclass recorded: %+v", f.Relations)
	}
}
