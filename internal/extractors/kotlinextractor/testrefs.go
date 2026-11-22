package kotlinextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It parses Kotlin test sources
// (src/test, src/androidTest, src/commonTest and the other Gradle test source sets
// config.Default().TestGlobs names) for the SOLE purpose of capturing their outbound
// references into production code, emitting one facts.KindTestRef fact per file that
// carries only RelCalls edges: no symbols, no modules, no routes.
//
// It pays back the cost of ignoring test source sets. Once they are ignored, a
// production function whose only caller is its test has no inbound edge and reads
// as dead; Kotlin makes that common, because an `internal` accessor written for the
// module's own tests is visible to nothing else. This pass restores the one signal
// without putting test code back in the graph.
//
// Targets are emitted as written (`Repo.load`, `load`, `Repo`), not resolved to
// canonical names: resolution needs the production index Extract builds, and the
// orphan detector matches a test-ref target both exactly and by its last segment.
// That is the shape the main walker already gives a navigation call, and the one
// the Scala, C# and Ruby passes emit.
//
// The engine scopes testFiles to .kt files through plugin.FileOwner. prodFiles is
// unused: nothing here decides whether a referenced module exists.
func (e *KotlinExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, _ []string) ([]facts.Fact, error) {
	var ktFiles []string
	for _, relFile := range testFiles {
		if isKotlinFile(relFile) {
			ktFiles = append(ktFiles, relFile)
		}
	}
	if len(ktFiles) == 0 {
		return nil, nil
	}

	perFile := parallel.MapFiles(ctx, ktFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[kotlin-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		return testRefsFromFile(src, relFile)
	})

	var out []facts.Fact
	for _, ff := range perFile {
		out = append(out, ff...)
	}
	return out, nil
}

// kotlinTestHarnessNames are the test framework's own vocabulary: JUnit 4/5,
// kotlin.test, Truth, AssertJ, assertk, Hamcrest, Kotest, MockK, Mockito-Kotlin,
// Turbine, kotlinx-coroutines-test, Robolectric and the Compose and Espresso test
// APIs. A test file is mostly these, and none is a symbol of the code under test.
//
// A harness receiver disqualifies the bare member name too (`Mockito.verify` does
// not let `verify` through), because a production symbol sharing a harness word
// would otherwise be vouched for by a test that never touches it, and a genuine
// dead-code finding would turn into a test-only one.
var kotlinTestHarnessNames = map[string]bool{
	// Assertions.
	"assert": true, "assertEquals": true, "assertNotEquals": true, "assertTrue": true,
	"assertFalse": true, "assertNull": true, "assertNotNull": true, "assertSame": true,
	"assertNotSame": true, "assertThat": true, "assertThrows": true, "assertFailsWith": true,
	"assertFails": true, "assertIs": true, "assertIsNot": true, "assertContains": true,
	"assertContentEquals": true, "assertArrayEquals": true, "assertAll": true,
	"assertDoesNotThrow": true, "assertTimeout": true, "assertIterableEquals": true,
	"assertLinesMatch": true, "assertSuccess": true, "assertFailure": true,
	"assertWithMessage": true, "assumeTrue": true, "assumeFalse": true, "assumeThat": true,
	"fail": true, "expect": true, "expectThat": true, "expectThrows": true,
	"Assert": true, "Assertions": true, "Assume": true, "Truth": true, "MatcherAssert": true,
	"isEqualTo": true, "isNotEqualTo": true, "isTrue": true, "isFalse": true, "isNull": true,
	"isNotNull": true, "isEmpty": true, "isNotEmpty": true, "isSameAs": true,
	"isSameInstanceAs": true, "isNotSameAs": true, "isInstanceOf": true, "isA": true,
	"hasSize": true, "hasMessage": true, "hasMessageThat": true, "hasLength": true,
	"contains": true, "containsExactly": true, "containsExactlyElementsIn": true,
	"containsAtLeast": true, "containsOnly": true, "containsNoneOf": true,
	"containsKey": true, "containsEntry": true, "doesNotContain": true, "inOrder": true,
	"isGreaterThan": true, "isLessThan": true, "isAtLeast": true, "isAtMost": true,
	"isCloseTo": true, "isBetween": true, "startsWith": true, "endsWith": true,
	"matches": true, "isIn": true, "isNotIn": true, "isAnyOf": true, "isEqualToIgnoringCase": true,
	"equalTo": true, "instanceOf": true, "notNullValue": true, "nullValue": true, "`is`": true,
	"should": true, "shouldBe": true, "shouldNotBe": true, "shouldThrow": true,
	"shouldContain": true, "shouldHaveSize": true, "shouldBeNull": true,
	"shouldNotBeNull": true, "shouldBeInstanceOf": true, "shouldBeTrue": true,
	"shouldBeFalse": true, "shouldBeEmpty": true,
	// Structure and lifecycle.
	"describe": true, "context": true, "it": true, "test": true, "given": true,
	"then": true, "feature": true, "scenario": true, "runTest": true, "runBlockingTest": true,
	"runBlocking": true, "advanceUntilIdle": true, "advanceTimeBy": true, "runCurrent": true,
	"StandardTestDispatcher": true, "UnconfinedTestDispatcher": true, "TestScope": true,
	"Dispatchers": true, "setMain": true, "resetMain": true,
	// Mocking.
	"mock": true, "mockk": true, "spyk": true, "mockkStatic": true, "mockkObject": true,
	"unmockkAll": true, "every": true, "coEvery": true, "verify": true, "coVerify": true,
	"confirmVerified": true, "slot": true, "capture": true, "answers": true, "returns": true,
	"throws": true, "just": true, "Runs": true, "relaxed": true, "Mockito": true,
	"whenever": true, "doReturn": true, "doThrow": true, "doAnswer": true, "thenReturn": true,
	"thenThrow": true, "thenAnswer": true, "any": true, "anyOrNull": true, "eq": true,
	"argThat": true, "argumentCaptor": true, "times": true, "never": true, "atLeast": true,
	"atLeastOnce": true, "atMost": true, "verifyNoMoreInteractions": true,
	"verifyNoInteractions": true, "reset": true, "spy": true,
	// Flow testing.
	"awaitItem": true, "awaitComplete": true, "awaitError": true, "expectNoEvents": true,
	"cancelAndIgnoreRemainingEvents": true, "cancelAndConsumeRemainingEvents": true,
	"expectMostRecentItem": true, "skipItems": true,
	// Android, Compose and Espresso UI tests.
	"ApplicationProvider": true, "InstrumentationRegistry": true, "getApplicationContext": true,
	"getInstrumentation": true, "Robolectric": true, "ActivityScenario": true,
	"composeTestRule": true, "composeRule": true, "onNodeWithText": true, "onNodeWithTag": true,
	"onNodeWithContentDescription": true, "onAllNodesWithText": true, "onAllNodesWithTag": true,
	"onNode": true, "onAllNodes": true, "onRoot": true, "onFirst": true, "onChildren": true,
	"assertIsDisplayed": true, "assertExists": true, "assertDoesNotExist": true,
	"assertIsNotDisplayed": true, "assertIsEnabled": true, "assertIsNotEnabled": true,
	"assertIsSelected": true, "assertIsNotSelected": true, "assertIsOn": true,
	"assertIsOff": true, "assertTextEquals": true, "assertTextContains": true,
	"assertCountEquals": true, "assertHasClickAction": true, "performClick": true,
	"performTextInput": true, "performScrollTo": true, "performTouchInput": true,
	"performScrollToNode": true, "performScrollToIndex": true, "waitForIdle": true,
	"waitUntil": true, "mainClock": true, "hasText": true, "hasTestTag": true,
	"hasContentDescription": true, "hasClickAction": true, "onView": true, "withId": true,
	"withText": true, "perform": true, "check": true, "click": true,
	"isDisplayed": true, "Espresso": true,
}

// testRefsFromFile collects one test file's references into a single KindTestRef
// fact. The fact Name is the file path, which never equals a symbol name, so the
// fact introduces no self-reference: the contract the orphan detector's fold
// relies on.
func testRefsFromFile(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(kotlin.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	if tree.RootNode().HasError() {
		tree = reparseSameLineCloses(parser, src, tree)
	}
	defer tree.Close()

	root := tree.RootNode()
	c := &kotlinTestRefCollector{src: src, seen: map[string]bool{},
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
		Props:     map[string]any{"language": "kotlin"},
		Relations: rels,
	}}
}

type kotlinTestRefCollector struct {
	src  []byte
	seen map[string]bool
	// declared holds the names the test file declares itself: its test classes,
	// helper functions and fakes. A bare call to one of them, or a call on a
	// declared type as the receiver, is the test calling its own code, and must not
	// vouch for a production symbol that happens to share the name. A member reached
	// through a receiver is kept whatever the file declares, because a fake
	// overriding the production member it stands in for declares the same name.
	declared map[string]bool
	// declaredTypes is the subset of declared that names a class or an object. Only
	// a type can be a receiver: in `histogram.setPercentiles(...)` inside a test
	// function named histogram(), the receiver is a local, not the function.
	declaredTypes map[string]bool
}

func (c *kotlinTestRefCollector) collectDeclared(n *sitter.Node) {
	switch kindOf(n) {
	case "class_declaration", "object_declaration", "function_declaration":
		if name := findChildByKind(n, "identifier"); name != nil {
			c.declared[nodeText(name, c.src)] = true
			if kindOf(n) != "function_declaration" {
				c.declaredTypes[nodeText(name, c.src)] = true
			}
		}
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.collectDeclared(n.NamedChild(i))
	}
}

// add records a bare name: a call, a constructor, a type, a callable reference.
func (c *kotlinTestRefCollector) add(name string) {
	if name == "" || kotlinTestHarnessNames[name] || kotlinBuiltins[name] || c.declared[name] {
		return
	}
	c.seen[name] = true
}

// addMember records a member reached through a plain identifier receiver as
// `Recv.member` and as the bare member, which is what matches when the receiver is
// a local whose type is not known here. A harness receiver or a type the file
// declares drops both: see kotlinTestHarnessNames.
func (c *kotlinTestRefCollector) addMember(recv, member string) {
	if recv == "" || member == "" || kotlinTestHarnessNames[recv] || c.declaredTypes[recv] {
		return
	}
	if kotlinTestHarnessNames[member] {
		return
	}
	c.seen[recv+"."+member] = true
	c.seen[member] = true
}

func (c *kotlinTestRefCollector) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "package_header", "import", "annotation":
		// An import alone is not a use, and a test's annotations (@Test, @Before,
		// @RunWith) are the harness.
		return
	case "call_expression":
		c.addCallee(kotlinUnprefixed(firstNamedChild(n)))
		// The callee is accounted for; walk the rest (arguments, trailing lambda,
		// type arguments) so the receiver chain is not re-added as a property read.
		callee := firstNamedChild(n)
		for i := uint(0); i < n.NamedChildCount(); i++ {
			if ch := n.NamedChild(i); callee == nil || ch.Id() != callee.Id() {
				c.walk(ch)
			}
		}
		return
	case "navigation_expression":
		// A member read that is not a call: a property, an object's value, an enum
		// entry, a bound reference `recv::member`. `Foo::class` is reflection.
		if kotlinIsClassLiteral(n, c.src) {
			c.walkNamed(n)
			return
		}
		c.addNavigation(n)
		return
	case "callable_reference":
		// `::helper`, `Type::member`: the trailing name is used without a call, and
		// a receiver type written before it is walked like any other type.
		last := lastNamedChild(n)
		if last != nil && kindOf(last) == "identifier" {
			c.add(nodeText(last, c.src))
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			if ch := n.NamedChild(i); last == nil || ch.Id() != last.Id() {
				c.walk(ch)
			}
		}
		return
	case "identifier":
		// An object or a constant used as a value: passed on, compared, paired with
		// `to`. A lower-case name in those places is a local.
		if p := n.Parent(); p != nil && kotlinValuePosition(n, p) {
			if name := nodeText(n, c.src); isCapitalized(name) {
				c.add(name)
			}
		}
		return
	case "user_type":
		// A type written in the test: a declared variable's type, a cast, an `is`
		// check, a type argument, a supertype of a fake.
		if last := lastIdentifierChild(n); last != nil {
			c.add(nodeText(last, c.src))
		}
	}
	c.walkNamed(n)
}

func (c *kotlinTestRefCollector) walkNamed(n *sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.walk(n.NamedChild(i))
	}
}

// addCallee records what a call invokes.
func (c *kotlinTestRefCollector) addCallee(fn *sitter.Node) {
	if fn == nil {
		return
	}
	switch kindOf(fn) {
	case "identifier":
		// A bare call, or a construction (`Repo(db)`).
		c.add(nodeText(fn, c.src))
	case "navigation_expression":
		c.addNavigation(fn)
	default:
		// A call on a call's result or on a parenthesised expression.
		c.walk(fn)
	}
}

// addNavigation records `recv.member`, descending into a receiver that is itself
// an expression so `factory.create().save()` records both calls.
func (c *kotlinTestRefCollector) addNavigation(nav *sitter.Node) {
	recv := kotlinUnprefixed(firstNamedChild(nav))
	last := lastNamedChild(nav)
	if recv == nil || last == nil || recv.Id() == last.Id() {
		c.walkNamed(nav)
		return
	}
	member := nodeText(last, c.src)
	if kindOf(last) != "identifier" {
		c.walk(recv)
		return
	}
	switch kindOf(recv) {
	case "identifier":
		c.addMember(nodeText(recv, c.src), member)
	case "this_expression", "super_expression":
		// The test class's own member.
	default:
		// A member is kept even when the file declares the same name: a fake
		// overriding `load` does not make `repo().load()` the fake's call.
		if !kotlinTestHarnessNames[member] {
			c.seen[member] = true
		}
		c.walk(recv)
	}
}

// lastIdentifierChild returns a user_type's trailing name: `Outer.Inner` is Inner.
func lastIdentifierChild(n *sitter.Node) *sitter.Node {
	var last *sitter.Node
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if ch := n.NamedChild(i); kindOf(ch) == "identifier" {
			last = ch
		}
	}
	return last
}
