package javaextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/jvmsrc"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	java "github.com/tree-sitter/tree-sitter-java/bindings/go"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It parses Java test sources
// (Maven's and Gradle's src/test, src/androidTest, src/integrationTest,
// src/testFixtures and the flavor sets config.Default().TestGlobs names) for the
// SOLE purpose of capturing their outbound references into production code,
// emitting one facts.KindTestRef fact per file that carries only RelCalls edges: no
// symbols, no modules, no routes.
//
// It pays back the cost of ignoring test source sets. Once they are ignored, a
// production method whose only caller is its test has no inbound edge and reads as
// dead; with this pass it reads as test-only, which is what it is.
//
// Targets are emitted as written (`Repo.load`, `load`, `Repo`), not resolved to
// canonical names: resolution needs the production index Extract builds, and the
// orphan detector matches a test-ref target both exactly and by its last segment.
// That is the shape the Kotlin, Scala, C# and Ruby passes emit.
//
// The engine scopes testFiles to .java files through plugin.FileOwner. prodFiles
// decides one thing: whether an annotation a test applies names a production type
// (javaProdTypes), since an annotation is otherwise the harness (@Test, @Mock).
func (e *JavaExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, prodFiles []string) ([]facts.Fact, error) {
	var javaFiles []string
	for _, relFile := range testFiles {
		if isJavaFile(relFile) {
			javaFiles = append(javaFiles, relFile)
		}
	}
	if len(javaFiles) == 0 {
		return nil, nil
	}
	prodTypes := javaProdTypes(ctx, repoPath, prodFiles)

	perFile := parallel.MapFiles(ctx, javaFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[java-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		return testRefsFromFileIn(src, relFile, prodTypes)
	})

	var out []facts.Fact
	for _, ff := range perFile {
		out = append(out, ff...)
	}
	return out, nil
}

// javaTestHarnessNames are the test framework's own vocabulary: JUnit 4/5, TestNG,
// Mockito and BDDMockito, AssertJ, Hamcrest, Truth, Awaitility, Spring's test
// support and Espresso. A test file is mostly these, and none is a symbol of the
// code under test.
//
// A harness receiver disqualifies the bare member name too (`Mockito.verify` does
// not let `verify` through), because a production symbol sharing a harness word
// would otherwise be vouched for by a test that never touches it, and a genuine
// dead-code finding would turn into a test-only one.
var javaTestHarnessNames = map[string]bool{
	// Assertions.
	"assertEquals": true, "assertNotEquals": true, "assertTrue": true, "assertFalse": true,
	"assertNull": true, "assertNotNull": true, "assertSame": true, "assertNotSame": true,
	"assertThat": true, "assertThatThrownBy": true, "assertThatCode": true,
	"assertThatExceptionOfType": true, "assertThatNoException": true, "assertThrows": true,
	"assertThrowsExactly": true, "assertArrayEquals": true, "assertAll": true,
	"assertDoesNotThrow": true, "assertTimeout": true, "assertTimeoutPreemptively": true,
	"assertIterableEquals": true, "assertLinesMatch": true, "assertInstanceOf": true,
	"assertWithMessage": true, "assumeTrue": true, "assumeFalse": true, "assumeThat": true,
	"assertSoftly": true, "fail": true, "expectThrows": true, "catchThrowable": true,
	"Assert": true, "Assertions": true, "Assume": true, "Assumptions": true, "Truth": true,
	"MatcherAssert": true, "Matchers": true, "CoreMatchers": true, "SoftAssertions": true,
	"isEqualTo": true, "isNotEqualTo": true, "isTrue": true, "isFalse": true, "isNull": true,
	"isNotNull": true, "isEmpty": true, "isNotEmpty": true, "isSameAs": true,
	"isNotSameAs": true, "isInstanceOf": true, "isExactlyInstanceOf": true, "hasSize": true,
	"hasMessage": true, "hasMessageContaining": true, "hasCauseInstanceOf": true,
	"hasRootCauseInstanceOf": true, "hasFieldOrPropertyWithValue": true, "hasLength": true,
	"contains": true, "containsExactly": true, "containsExactlyInAnyOrder": true,
	"containsOnly": true, "containsKey": true, "containsKeys": true, "containsEntry": true,
	"containsValue": true, "doesNotContain": true, "doesNotContainKey": true,
	"isGreaterThan": true, "isGreaterThanOrEqualTo": true, "isLessThan": true,
	"isLessThanOrEqualTo": true, "isCloseTo": true, "isBetween": true, "startsWith": true,
	"endsWith": true, "matches": true, "isIn": true, "isNotIn": true, "isZero": true,
	"isPositive": true, "isNegative": true, "isEqualToIgnoringCase": true,
	"extracting": true, "satisfies": true, "allMatch": true, "anyMatch": true,
	"as": true, "describedAs": true, "withFailMessage": true, "isThrownBy": true,
	"equalTo": true, "instanceOf": true, "notNullValue": true, "nullValue": true,
	"is": true, "not": true, "hasItem": true, "hasItems": true, "hasEntry": true,
	"hasKey": true, "hasProperty": true, "containsString": true, "greaterThan": true,
	"lessThan": true, "closeTo": true, "everyItem": true, "allOf": true, "anyOf": true,
	"sameInstance": true, "emptyIterable": true, "isA": true,
	// Mocking.
	"mock": true, "spy": true, "when": true, "verify": true, "verifyNoMoreInteractions": true,
	"verifyNoInteractions": true, "verifyZeroInteractions": true, "reset": true,
	"Mockito": true, "BDDMockito": true, "ArgumentMatchers": true, "ArgumentCaptor": true,
	"MockitoAnnotations": true, "openMocks": true, "initMocks": true, "mockStatic": true,
	"given": true, "willReturn": true, "willThrow": true, "willAnswer": true, "will": true,
	"willDoNothing": true, "then": true, "should": true, "shouldHaveNoMoreInteractions": true,
	"doReturn": true, "doThrow": true, "doAnswer": true, "doNothing": true,
	"doCallRealMethod": true, "thenReturn": true, "thenThrow": true, "thenAnswer": true,
	"thenCallRealMethod": true, "any": true, "anyString": true, "anyInt": true,
	"anyLong": true, "anyBoolean": true, "anyDouble": true, "anyList": true, "anyMap": true,
	"anySet": true, "anyCollection": true, "anyIterable": true,
	"eq": true, "same": true, "argThat": true, "refEq": true, "nullable": true,
	"forClass": true, "capture": true, "getAllValues": true,
	"times": true, "never": true, "atLeast": true, "atLeastOnce": true, "atMost": true,
	"only": true, "inOrder": true, "lenient": true,
	"withSettings": true, "RETURNS_DEEP_STUBS": true, "CALLS_REAL_METHODS": true,
	// Structure, lifecycle and async waiting.
	"Awaitility": true, "await": true, "until": true, "untilAsserted": true,
	"pollInterval": true, "pollDelay": true,
	"DynamicTest": true, "dynamicTest": true, "Arguments": true, "arguments": true,
	"TestInstance": true,
	// Spring test support.
	"MockMvc": true, "MockMvcRequestBuilders": true, "MockMvcResultMatchers": true,
	"MockMvcResultHandlers": true, "perform": true, "andExpect": true, "andDo": true,
	"andReturn": true, "jsonPath": true, "isOk": true, "ReflectionTestUtils": true,
	// Android and Espresso UI tests.
	"ApplicationProvider": true, "InstrumentationRegistry": true,
	"getApplicationContext": true, "getInstrumentation": true, "Robolectric": true,
	"ActivityScenario": true, "Espresso": true, "onView": true, "onData": true,
	"withId": true, "withText": true, "click": true, "check": true, "isDisplayed": true,
	"typeText": true, "closeSoftKeyboard": true, "scrollTo": true,
}

// javaTestBuiltins are the JDK types a test leans on that no repository declares:
// java.lang (javaLangTypes) and the java.util and concurrency types tests build
// fixtures from. A receiver among them disqualifies its member, as a harness one
// does: `String.valueOf` and `List.of` do not vouch for a production valueOf or of.
var javaTestBuiltins = map[string]bool{
	"List": true, "Map": true, "Set": true, "Collection": true, "ArrayList": true,
	"LinkedList": true, "HashMap": true, "LinkedHashMap": true, "TreeMap": true,
	"HashSet": true, "LinkedHashSet": true, "TreeSet": true, "Arrays": true,
	"Collections": true, "Optional": true, "Objects": true, "Iterator": true,
	"UUID": true, "Random": true, "Stream": true, "IntStream": true, "LongStream": true,
	"Collectors": true, "Duration": true, "Instant": true, "LocalDate": true,
	"LocalDateTime": true, "TimeUnit": true, "CountDownLatch": true,
	"CompletableFuture": true, "Executors": true, "ExecutorService": true,
	"AtomicInteger": true, "AtomicLong": true, "AtomicBoolean": true,
	"AtomicReference": true, "ConcurrentHashMap": true, "BigDecimal": true,
	"BigInteger": true, "Files": true, "Paths": true, "Path": true, "File": true,
	"StandardCharsets": true, "IOException": true, "Function": true, "BiFunction": true,
	"Supplier": true, "Consumer": true, "Predicate": true, "Callable": true,
}

func javaTestIgnored(name string) bool {
	return javaTestHarnessNames[name] || javaLangTypes[name] || javaTestBuiltins[name]
}

// javaProdTypes is the set of top-level type FQNs the production Java sources
// declare: each file's package plus its base name, the public type javac requires
// the file to be named after. Only an annotation type's existence is asked of it,
// and an annotation type is public to be applied from a test in the first place.
func javaProdTypes(ctx context.Context, repoPath string, prodFiles []string) map[string]bool {
	var javaFiles []string
	for _, relFile := range prodFiles {
		if isJavaFile(relFile) {
			javaFiles = append(javaFiles, relFile)
		}
	}
	fqns := parallel.MapFiles(ctx, javaFiles, func(relFile string) string {
		pkg, _ := jvmsrc.ReadPackage(filepath.Join(repoPath, relFile))
		return qualifyIn(pkg, strings.TrimSuffix(filepath.Base(relFile), filepath.Ext(relFile)))
	})
	out := make(map[string]bool, len(fqns))
	for _, fqn := range fqns {
		out[fqn] = true
	}
	return out
}

func testRefsFromFile(src []byte, relFile string) []facts.Fact {
	return testRefsFromFileIn(src, relFile, nil)
}

// testRefsFromFileIn collects one test file's references into a single KindTestRef
// fact. The fact Name is the file path, which never equals a symbol name, so the
// fact introduces no self-reference: the contract the orphan detector's fold
// relies on.
func testRefsFromFileIn(src []byte, relFile string, prodTypes map[string]bool) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(java.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	root := tree.RootNode()
	c := &javaTestRefCollector{src: src, seen: map[string]bool{},
		declared: map[string]bool{}, declaredTypes: map[string]bool{},
		prodTypes: prodTypes, imports: map[string]string{}}
	c.collectHeader(root)
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
		Props:     map[string]any{"language": "java"},
		Relations: rels,
	}}
}

type javaTestRefCollector struct {
	src  []byte
	seen map[string]bool
	// declared holds the names the test file declares itself: its test classes,
	// helper methods and fakes. A bare call to one of them, or a call on a declared
	// type as the receiver, is the test calling its own code, and must not vouch for
	// a production symbol that happens to share the name. A member reached through a
	// receiver is kept whatever the file declares, because a fake overriding the
	// production member it stands in for declares the same name.
	declared map[string]bool
	// declaredTypes is the subset of declared that names a type. Only a type can be
	// a receiver: in `histogram.setPercentiles(...)` inside a test method named
	// histogram(), the receiver is a local, not the method.
	declaredTypes map[string]bool
	// The file's package and imports, and the production types they may name: an
	// annotation counts only when its name resolves to one of those (addAnnotation).
	prodTypes map[string]bool
	pkg       string
	imports   map[string]string
	wildcards []string
}

// collectHeader reads the package and the non-static imports: what an annotation
// name resolves against.
func (c *javaTestRefCollector) collectHeader(root *sitter.Node) {
	for i := uint(0); i < root.NamedChildCount(); i++ {
		n := root.NamedChild(i)
		switch kindOf(n) {
		case "package_declaration":
			if p := firstNamedChild(n); p != nil {
				c.pkg = nodeText(p, c.src)
			}
		case "import_declaration":
			var path string
			static, wildcard := false, false
			for j := uint(0); j < n.ChildCount(); j++ {
				switch ch := n.Child(j); kindOf(ch) {
				case "static":
					static = true
				case "asterisk":
					wildcard = true
				case "scoped_identifier", "identifier":
					path = nodeText(ch, c.src)
				}
			}
			switch {
			case path == "" || static:
			case wildcard:
				c.wildcards = append(c.wildcards, path)
			default:
				c.imports[lastTypeComponent(path)] = path
			}
		}
	}
}

// addAnnotation records an annotation the test applies when its name resolves, as
// javac would resolve it, to a production type of the repository (or a member type
// of one). Anything else is the harness or a library: @Test, @Mock, @DisplayName.
// The recorded target is the type's simple name, the shape of every other type
// reference this pass emits.
func (c *javaTestRefCollector) addAnnotation(n *sitter.Node) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil || len(c.prodTypes) == 0 {
		return
	}
	for _, cand := range annotationCandidates(nodeText(nameNode, c.src), c.pkg, nil, c.imports, c.wildcards) {
		if !c.prodTypes[cand] && !c.prodTypes[parentName(cand)] {
			continue
		}
		if simple := lastTypeComponent(cand); !c.declared[simple] {
			c.seen[simple] = true
		}
		return
	}
}

func (c *javaTestRefCollector) collectDeclared(n *sitter.Node) {
	switch kindOf(n) {
	case "class_declaration", "interface_declaration", "enum_declaration",
		"record_declaration", "annotation_type_declaration", "method_declaration":
		if name := n.ChildByFieldName("name"); name != nil {
			c.declared[nodeText(name, c.src)] = true
			if kindOf(n) != "method_declaration" {
				c.declaredTypes[nodeText(name, c.src)] = true
			}
		}
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.collectDeclared(n.NamedChild(i))
	}
}

// add records a bare name: a call, a constructor, a type, a method reference.
func (c *javaTestRefCollector) add(name string) {
	if name == "" || javaTestIgnored(name) || c.declared[name] {
		return
	}
	c.seen[name] = true
}

// addMember records a member reached through a plain identifier receiver as
// `Recv.member` and as the bare member, which is what matches when the receiver is
// a local whose type is not known here. A harness or JDK receiver, or one the file
// declares, drops both: see javaTestHarnessNames.
func (c *javaTestRefCollector) addMember(recv, member string) {
	if recv == "" || member == "" || javaTestIgnored(recv) || c.declaredTypes[recv] {
		return
	}
	if javaTestHarnessNames[member] {
		return
	}
	c.seen[recv+"."+member] = true
	c.seen[member] = true
}

// addBareMember records a member reached through a receiver that is itself an
// expression (a call's result, a field of a field): only the member name is known.
func (c *javaTestRefCollector) addBareMember(member string) {
	if member != "" && !javaTestHarnessNames[member] {
		c.seen[member] = true
	}
}

func (c *javaTestRefCollector) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "package_declaration", "import_declaration":
		// An import alone is not a use.
		return
	case "marker_annotation", "annotation":
		// Mostly the harness (@Test, @BeforeEach, @ExtendWith); a repository's own
		// annotation type is a use. The arguments are not walked.
		c.addAnnotation(n)
		return
	case "method_invocation":
		c.addInvocation(n)
		return
	case "field_access":
		c.addAccess(n.ChildByFieldName("object"), n.ChildByFieldName("field"))
		return
	case "method_reference":
		c.addMethodReference(n)
		return
	case "type_identifier":
		// A type written in the test: a declared variable's type, a constructor, a
		// cast, an instanceof check, a class literal, a type argument, a supertype
		// of a fake.
		c.add(nodeText(n, c.src))
		return
	case "scoped_type_identifier":
		// `Outer.Inner` is Inner; the qualifier is a package or an enclosing type.
		if last := lastNamedChild(n); last != nil && kindOf(last) == "type_identifier" {
			c.add(nodeText(last, c.src))
		}
		return
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.walk(n.NamedChild(i))
	}
}

// addInvocation records what a method call invokes and walks its receiver chain,
// type arguments and arguments.
func (c *javaTestRefCollector) addInvocation(n *sitter.Node) {
	obj := n.ChildByFieldName("object")
	nameNode := n.ChildByFieldName("name")
	if nameNode != nil {
		if obj == nil {
			// A bare call: an inherited or statically imported method.
			c.add(nodeText(nameNode, c.src))
		} else {
			c.addAccess(obj, nameNode)
		}
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		ch := n.NamedChild(i)
		if (obj != nil && ch.Id() == obj.Id()) || (nameNode != nil && ch.Id() == nameNode.Id()) {
			continue
		}
		c.walk(ch)
	}
}

// addAccess records `obj.member`, a call or a field read, descending into a
// receiver that is itself an expression so `factory.create().save()` records both.
func (c *javaTestRefCollector) addAccess(obj, memberNode *sitter.Node) {
	if obj == nil || memberNode == nil {
		c.walk(obj)
		return
	}
	member := nodeText(memberNode, c.src)
	switch kindOf(obj) {
	case "identifier":
		c.addMember(nodeText(obj, c.src), member)
	case "this", "super":
		// The test class's own member, or its supertype's.
	default:
		// A member is kept even when the file declares the same name: a fake
		// overriding `load` does not make `repo().load()` the fake's call.
		c.addBareMember(member)
		c.walk(obj)
	}
}

// addMethodReference records `Type::method`, `local::method` and `Type::new`.
func (c *javaTestRefCollector) addMethodReference(n *sitter.Node) {
	recv := firstNamedChild(n)
	last := lastNamedChild(n)
	if recv == nil {
		return
	}
	if last == nil || last.Id() == recv.Id() || kindOf(last) != "identifier" {
		// `Type::new` names the constructor: the type is the use.
		c.walk(recv)
		return
	}
	member := nodeText(last, c.src)
	switch kindOf(recv) {
	case "identifier", "type_identifier":
		c.addMember(nodeText(recv, c.src), member)
		c.add(nodeText(recv, c.src))
	case "this", "super":
	default:
		c.addBareMember(member)
		c.walk(recv)
	}
}

func lastNamedChild(n *sitter.Node) *sitter.Node {
	if n == nil || n.NamedChildCount() == 0 {
		return nil
	}
	return n.NamedChild(n.NamedChildCount() - 1)
}
