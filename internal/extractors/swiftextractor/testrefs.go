package swiftextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	swift "github.com/enola-labs/enola/internal/extractors/swiftextractor/grammar"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It parses Swift test sources
// (XCTest and Swift Testing targets under a *Tests directory, the glob
// config.Default().TestGlobs names) for the SOLE purpose of capturing their
// outbound references into production code, emitting one facts.KindTestRef fact per
// file that carries only RelCalls edges: no symbols, no modules, no routes.
//
// It pays back the cost of ignoring test targets. Once they are ignored, a
// production function whose only caller is its test has no inbound edge and reads
// as dead; `@testable import` makes that common in Swift, because an internal
// helper written for the module's own tests is visible to nothing else.
//
// Targets are emitted as written (`Session.request`, `request`, `Session`), not
// resolved to canonical names: resolution needs the production index Extract
// builds, and the orphan detector matches a test-ref target both exactly and by
// its last segment. This is the shape the Kotlin, Java and Scala passes emit.
//
// The engine scopes testFiles to .swift files through plugin.FileOwner. prodFiles
// is unused: nothing here decides whether a referenced module exists.
func (e *SwiftExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, _ []string) ([]facts.Fact, error) {
	var swiftFiles []string
	for _, relFile := range testFiles {
		if isSwiftFile(relFile) {
			swiftFiles = append(swiftFiles, relFile)
		}
	}
	if len(swiftFiles) == 0 {
		return nil, nil
	}

	perFile := parallel.MapFiles(ctx, swiftFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[swift-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		return swiftTestRefsFromFile(src, relFile)
	})

	var out []facts.Fact
	for _, ff := range perFile {
		out = append(out, ff...)
	}
	return out, nil
}

// swiftTestHarnessNames are the test frameworks' own vocabulary: XCTest, Swift
// Testing, XCUITest, the Point-Free testing libraries (swift-snapshot-testing,
// swift-custom-dump, swift-dependencies) and Quick/Nimble. A test file is mostly
// these, and none is a symbol of the code under test.
//
// A harness receiver disqualifies the bare member name too, because a production
// symbol sharing a harness word would otherwise be vouched for by a test that never
// touches it, and a genuine dead-code finding would turn into a test-only one.
// TestStore's send and receive are left out on purpose: they are ordinary words a
// production API uses too (a socket's send), and a test calling one is far more
// often the real use than the harness one.
var swiftTestHarnessNames = map[string]bool{
	// XCTest.
	"XCTest": true, "XCTAssert": true, "XCTAssertEqual": true, "XCTAssertNotEqual": true,
	"XCTAssertTrue": true, "XCTAssertFalse": true, "XCTAssertNil": true,
	"XCTAssertNotNil": true, "XCTAssertIdentical": true, "XCTAssertNotIdentical": true,
	"XCTAssertGreaterThan": true, "XCTAssertGreaterThanOrEqual": true,
	"XCTAssertLessThan": true, "XCTAssertLessThanOrEqual": true,
	"XCTAssertEqualObjects": true, "XCTAssertThrowsError": true, "XCTAssertNoThrow": true,
	"XCTFail": true, "XCTUnwrap": true, "XCTSkip": true, "XCTSkipIf": true,
	"XCTSkipUnless": true, "XCTExpectFailure": true, "XCTContext": true,
	"XCTestCase": true, "XCTestExpectation": true, "XCTWaiter": true,
	"XCTNSPredicateExpectation": true, "XCTKVOExpectation": true,
	"XCTNSNotificationExpectation": true, "XCTestObservationCenter": true,
	"XCUIApplication": true, "XCUIElement": true, "XCUIElementQuery": true,
	"expectation": true, "fulfill": true, "fulfillment": true, "wait": true,
	"measure": true, "addTeardownBlock": true, "setUp": true, "tearDown": true,
	"setUpWithError": true, "tearDownWithError": true, "continueAfterFailure": true,
	"executionTimeAllowance": true, "isInverted": true, "expectedFulfillmentCount": true,
	"assertForOverFulfill": true,
	// Swift Testing.
	"expect": true, "require": true, "Test": true, "Suite": true, "Issue": true,
	"confirmation": true, "withKnownIssue": true, "Tag": true,
	// Point-Free testing libraries.
	"TestStore": true, "expectNoDifference": true, "expectDifference": true,
	"XCTAssertNoDifference": true, "XCTAssertDifference": true, "assertSnapshot": true,
	"assertSnapshots": true, "verifySnapshot": true, "assertInlineSnapshot": true,
	"withSnapshotTesting": true, "assertCustomDump": true, "customDump": true,
	"withDependencies": true, "withMainSerialExecutor": true, "prepareDependencies": true,
	"exhaustivity": true, "skipReceivedActions": true, "skipInFlightEffects": true,
	"XCTExpectFailureIf": true, "isRecording": true,
	// Quick and Nimble.
	"QuickSpec": true, "describe": true, "context": true, "it": true, "fit": true,
	"xit": true, "beforeEach": true, "afterEach": true, "beforeSuite": true,
	"afterSuite": true, "toEventually": true, "toNot": true, "notTo": true,
	"equal": true, "beNil": true, "beTrue": true, "beFalse": true, "beEmpty": true,
	"beAKindOf": true, "beAnInstanceOf": true, "throwError": true,
}

// swiftTestRefsFromFile collects one test file's references into a single
// KindTestRef fact. The fact Name is the file path, which never equals a symbol
// name, so the fact introduces no self-reference: the contract the orphan
// detector's fold relies on.
func swiftTestRefsFromFile(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(swift.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	root := tree.RootNode()
	c := &swiftTestRefCollector{src: src, seen: map[string]bool{},
		declared: map[string]bool{}, declaredTypes: map[string]bool{}}
	c.collectDeclared(root)
	c.walk(root)
	if len(c.seen) == 0 {
		return nil
	}

	targets := make([]string, 0, len(c.seen))
	for t := range c.seen {
		targets = append(targets, t)
	}
	// Sorted: the set comes out of a map, and facts.jsonl is hashed into the
	// snapshot id, so an unsorted list would change the id on every run.
	sort.Strings(targets)

	rels := make([]facts.Relation, 0, len(targets))
	for _, t := range targets {
		rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: t})
	}
	return []facts.Fact{{
		Kind:      facts.KindTestRef,
		Name:      relFile,
		File:      relFile,
		Line:      1,
		Props:     map[string]any{"language": "swift"},
		Relations: rels,
	}}
}

type swiftTestRefCollector struct {
	src  []byte
	seen map[string]bool
	// declared holds the names the test file declares itself: its test classes,
	// fakes, helper functions, their enum cases and the members an extension in the
	// file adds. A bare call, an implicit member (`.helper`) or a type naming one of
	// them is the test using its own code, and must not vouch for a production symbol
	// that happens to share the name. A member reached through a receiver is kept
	// whatever the file declares, because a fake implementing the production
	// protocol it stands in for declares the same names.
	declared map[string]bool
	// declaredTypes is the subset of declared that names a type. Only a type can be
	// a receiver the file owns: `TestCredential.make()` is the test's own call.
	declaredTypes map[string]bool
}

func (c *swiftTestRefCollector) collectDeclared(n *sitter.Node) {
	switch kindOf(n) {
	case "class_declaration", "protocol_declaration", "typealias_declaration":
		// An extension's name is the production type it extends (a user_type), not a
		// declaration of the file's own: only a plain type_identifier name counts.
		if name := n.ChildByFieldName("name"); name != nil && kindOf(name) == "type_identifier" {
			c.declared[nodeText(name, c.src)] = true
			c.declaredTypes[nodeText(name, c.src)] = true
		}
	case "function_declaration":
		if name := n.ChildByFieldName("name"); name != nil && kindOf(name) == "simple_identifier" {
			c.declared[nodeText(name, c.src)] = true
		}
	case "enum_entry":
		for i := uint(0); i < n.NamedChildCount(); i++ {
			if ch := n.NamedChild(i); kindOf(ch) == "simple_identifier" {
				c.declared[nodeText(ch, c.src)] = true
			}
		}
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.collectDeclared(n.NamedChild(i))
	}
}

// ignoredName reports a name no production symbol can stand for: the harness,
// Swift's own spellings for the current type and instance, initializers and
// closure shorthand arguments ($0).
func (c *swiftTestRefCollector) ignoredName(name string) bool {
	switch name {
	case "", "self", "Self", "super", "init", "Type", "Protocol":
		return true
	}
	return swiftTestHarnessNames[name] || strings.HasPrefix(name, "$")
}

// add records a bare name: a call, a construction, a type. A keyword escaped in
// backticks (`default`) is named without them, as its declaration is.
func (c *swiftTestRefCollector) add(name string) {
	name = strings.Trim(name, "`")
	if c.ignoredName(name) || isSystemType(name) || c.declared[name] {
		return
	}
	c.seen[name] = true
}

// addMember records a member reached through a plain identifier receiver.
//
// A call records `Recv.member` and the bare member, which is what matches when the
// receiver is a local whose type is not known here. A read that is not a call
// records no bare member: a property name (`id`, `viewModel`, `database`) is
// shared by many unrelated types, the orphan detector credits a type with any use
// of its members, and a bare read would turn every type declaring the name into
// test-only code. A read through a capitalized receiver names that type
// (`Mode.fast`, `Policy.self`), and a capitalized member of it a nested type.
//
// A harness receiver or a type the file declares drops all of it: see
// swiftTestHarnessNames.
func (c *swiftTestRefCollector) addMember(recv, member string, call bool) {
	if strings.HasPrefix(recv, "$") {
		// A closure's shorthand argument: a call through it is still a call.
		if call && !c.ignoredName(member) {
			c.seen[member] = true
		}
		return
	}
	if c.ignoredName(recv) || c.declaredTypes[recv] || (isCapitalized(recv) && isSystemType(recv)) {
		return
	}
	if isCapitalized(recv) {
		c.add(recv)
	} else if !call {
		return
	}
	if c.ignoredName(member) {
		return
	}
	c.seen[recv+"."+member] = true
	switch {
	case call:
		c.seen[member] = true
	case isCapitalized(member):
		c.add(member)
	}
}

func (c *swiftTestRefCollector) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "import_declaration", "attribute", "directive":
		// An import alone is not a use, a test's attributes (@Test, @MainActor,
		// @testable) are the harness, and `#if canImport(XCTest)` names a module.
		return
	case "call_expression", "macro_invocation":
		// `#expect(...)` and `#require(...)` are the harness: only their arguments
		// can reach production code. A subscript (`items[0]`) parses as a call and
		// is a read of its base, not a call.
		callee := firstNamedChild(n)
		switch {
		case kindOf(n) == "macro_invocation":
		case isSubscriptCall(n, c.src):
			c.walk(callee)
		default:
			c.addCallee(callee)
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			if ch := n.NamedChild(i); callee == nil || ch.Id() != callee.Id() {
				c.walk(ch)
			}
		}
		return
	case "navigation_expression":
		// A member read that is not a call: a property, a static value, an enum
		// case, a key path's component.
		c.addNavigation(n, false)
		return
	case "prefix_expression":
		// `.member` read without a call: an enum case or a static value of a type
		// the context infers. Like any other bare read it names nothing here (see
		// addMember). `-x`, `!x` are operators on a value and are walked.
		if implicitMember(n, c.src) != "" {
			return
		}
	case "simple_identifier":
		// A type or a global used as a value: passed on, compared, assigned. A
		// lower-case name in those places is a local.
		if name := nodeText(n, c.src); isCapitalized(name) {
			c.add(name)
		}
		return
	case "user_type":
		// A type written in the test: a declared variable's type, a cast, a type
		// argument, a protocol a fake conforms to, the type an extension extends.
		// `Outer.Inner` is Inner.
		if last := lastTypeIdentifier(n); last != nil {
			c.add(nodeText(last, c.src))
		}
	}
	c.walkNamed(n)
}

func (c *swiftTestRefCollector) walkNamed(n *sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.walk(n.NamedChild(i))
	}
}

// addCallee records what a call invokes.
func (c *swiftTestRefCollector) addCallee(fn *sitter.Node) {
	if fn == nil {
		return
	}
	switch kindOf(fn) {
	case "simple_identifier":
		// A bare call, or a construction (`Session(configuration:)`).
		c.add(nodeText(fn, c.src))
	case "navigation_expression":
		c.addNavigation(fn, true)
	case "prefix_expression":
		// An implicit member call: a static factory (`.mock(...)`) of the type the
		// context infers. `.init(...)` is a construction of that unnamed type.
		if name := implicitMember(fn, c.src); name != "" {
			c.add(name)
			return
		}
		c.walk(fn)
	default:
		// A call on a call's result or on a parenthesised expression.
		c.walk(fn)
	}
}

// addNavigation records `recv.member`, descending into a receiver that is itself
// an expression so `factory.create().save()` records both calls.
func (c *swiftTestRefCollector) addNavigation(nav *sitter.Node, call bool) {
	recv := nav.ChildByFieldName("target")
	member := swiftNavigationMember(nav, c.src)
	if recv == nil || member == "" {
		c.walkNamed(nav)
		return
	}
	switch kindOf(recv) {
	case "simple_identifier":
		c.addMember(nodeText(recv, c.src), member, call)
	case "self_expression", "super_expression":
		// The test class's own member, or the XCTestCase lifecycle.
	default:
		// A called member is kept even when the file declares the same name: a fake
		// implementing `apply` does not make `interceptor.adapter.apply()` the
		// fake's call.
		if call && !c.ignoredName(member) {
			c.seen[member] = true
		}
		c.walk(recv)
	}
}

// implicitMember returns the name of an implicit member expression (`.member`),
// or "" when n is another prefix operator.
func implicitMember(n *sitter.Node, src []byte) string {
	op := n.ChildByFieldName("operation")
	if op == nil || nodeText(op, src) != "." {
		return ""
	}
	if t := n.ChildByFieldName("target"); t != nil && kindOf(t) == "simple_identifier" {
		return nodeText(t, src)
	}
	return ""
}

// swiftNavigationMember returns the name a navigation_expression's suffix selects,
// or "" for a tuple index (`pair.0`) or a suffix the grammar spells otherwise.
func swiftNavigationMember(nav *sitter.Node, src []byte) string {
	suffix := nav.ChildByFieldName("suffix")
	if suffix == nil {
		return ""
	}
	if id := suffix.ChildByFieldName("suffix"); id != nil && kindOf(id) == "simple_identifier" {
		return swiftIdentifier(id, src)
	}
	return ""
}

// lastTypeIdentifier returns a user_type's trailing name: `Outer.Inner` is Inner.
func lastTypeIdentifier(n *sitter.Node) *sitter.Node {
	var last *sitter.Node
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if ch := n.NamedChild(i); kindOf(ch) == "type_identifier" {
			last = ch
		}
	}
	return last
}
