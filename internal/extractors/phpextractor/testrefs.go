package phpextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It parses PHP test sources
// (PHPUnit, Pest and Codeception under tests/, Laminas under test/, the globs
// config.Default().TestGlobs names) for the SOLE purpose of capturing their
// outbound references into production code, emitting one facts.KindTestRef fact per
// file that carries only RelCalls edges: no symbols, no modules, no routes.
//
// It pays back the cost of ignoring test directories. Once they are ignored, a
// production method whose only caller is its test has no inbound edge and reads as
// dead; with this pass it reads as test-only, which is what it is.
//
// Class names are resolved the way the production walker resolves them, through the
// file's namespace and `use` imports, so a test naming `User` under
// `use App\Models\User;` refers to App\Models\User and to no other class of that
// name. A static call is `Class::method` on the resolved class. An instance call
// keeps the bare method name, which is the production walker's shape for a call
// whose receiver type it does not know.
//
// The engine scopes testFiles to PHP files through plugin.FileOwner. prodFiles is
// unused: nothing here decides whether a referenced class exists.
func (e *PHPExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, _ []string) ([]facts.Fact, error) {
	var phpFiles []string
	for _, relFile := range testFiles {
		if isPHPFile(relFile) {
			phpFiles = append(phpFiles, relFile)
		}
	}
	if len(phpFiles) == 0 {
		return nil, nil
	}

	perFile := parallel.MapFiles(ctx, phpFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[php-extractor] error reading test file %s: %v", relFile, err)
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

// phpTestHarnessFuncs are the test frameworks' global functions: Pest's structure,
// hooks and expectations, and the Mockery and PHPUnit helpers a test calls bare.
var phpTestHarnessFuncs = map[string]bool{
	"it": true, "test": true, "describe": true, "expect": true, "beforeEach": true,
	"afterEach": true, "beforeAll": true, "afterAll": true, "uses": true, "dataset": true,
	"covers": true, "mutates": true, "todo": true, "arch": true, "pest": true,
	"mock": true, "spy": true, "fake": true,
}

// phpTestHarnessMethods are the method names a test calls on the harness rather
// than on the code under test: PHPUnit's mock builder and invocation matchers,
// Mockery's expectations, and Pest's expectation chain. An instance call keeps only
// its bare name, so a production method sharing one of these words would otherwise
// be vouched for by a test that never touches it. Assertions (`assert...`) and
// Pest matchers (`toBe...`, `toHave...`) are dropped by prefix in isHarnessMethod.
var phpTestHarnessMethods = map[string]bool{
	// PHPUnit mocks.
	"getMockBuilder": true, "createMock": true, "createStub": true,
	"createPartialMock": true, "createConfiguredMock": true, "getMockForAbstractClass": true,
	"getMockForTrait": true, "onlyMethods": true, "addMethods": true, "setMethods": true,
	"disableOriginalConstructor": true, "disableOriginalClone": true,
	"setConstructorArgs": true, "getMock": true, "expects": true, "method": true,
	"willReturn": true, "willReturnMap": true, "willReturnCallback": true,
	"willReturnSelf": true, "willReturnArgument": true, "willThrowException": true,
	"willReturnOnConsecutiveCalls": true, "withConsecutive": true, "will": true,
	"returnValue": true, "returnCallback": true, "once": true, "twice": true,
	"never": true, "any": true, "exactly": true, "atLeastOnce": true,
	"expectException": true, "expectExceptionMessage": true, "expectExceptionCode": true,
	"markTestSkipped": true, "markTestIncomplete": true,
	// Mockery.
	"shouldReceive": true, "shouldNotReceive": true, "shouldHaveReceived": true,
	"shouldNotHaveReceived": true, "shouldIgnoreMissing": true, "andReturn": true,
	"andReturns": true, "andReturnSelf": true, "andReturnNull": true,
	"andReturnUsing": true, "andReturnValues": true, "andThrow": true, "andThrows": true,
	"with": true, "withArgs": true, "withAnyArgs": true, "withNoArgs": true,
	"times": true, "zeroOrMoreTimes": true, "atLeast": true, "atMost": true,
	"byDefault": true, "makePartial": true, "shouldAllowMockingProtectedMethods": true,
	"passthru": true, "ordered": true,
	// Pest's expectation chain.
	"not": true, "and": true, "each": true, "sequence": true, "toBe": true,
	"toEqual": true, "toThrow": true, "toMatchArray": true, "toMatchObject": true,
}

// isHarnessMethod reports a method name the harness owns.
func isHarnessMethod(name string) bool {
	if phpTestHarnessMethods[name] {
		return true
	}
	for _, p := range []string{"assert", "toBe", "toHave", "toContain", "toEqual", "toMatch", "toStartWith", "toEndWith"} {
		if rest, ok := strings.CutPrefix(name, p); ok && (rest == "" || isUpperASCII(rest[0])) {
			return true
		}
	}
	return false
}

func isUpperASCII(b byte) bool { return b >= 'A' && b <= 'Z' }

// testRefsFromFile collects one test file's references into a single KindTestRef
// fact. The fact Name is the file path, which never equals a symbol name, so the
// fact introduces no self-reference: the contract the orphan detector's fold
// relies on.
func testRefsFromFile(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	root := tree.RootNode()
	c := &phpTestRefCollector{
		w:        &phpWalker{src: src, relFile: relFile, importMap: map[string]string{}},
		seen:     map[string]bool{},
		declared: map[string]bool{},
		required: map[string]bool{},
	}
	c.collectDeclared(root)
	c.w.namespace = ""
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
		Props:     map[string]any{"language": "php"},
		Relations: rels,
	}}
}

type phpTestRefCollector struct {
	// w resolves names: it carries the file's current namespace, its `use` imports
	// and the enclosing class, exactly as the production walk does. Its out slice
	// collects the dependency facts handleUse writes, which are discarded.
	w    *phpWalker
	seen map[string]bool
	// declared holds the fully-qualified classes and functions the test file
	// declares itself: its test cases, fakes and helpers. A reference to one is the
	// test using its own code, never a production symbol.
	declared map[string]bool
	// required holds the variables the file assigns from require or include.
	required map[string]bool
}

// collectDeclared records the file's own class-likes and functions under the
// namespace each is declared in.
func (c *phpTestRefCollector) collectDeclared(n *sitter.Node) {
	switch kindOf(n) {
	case "namespace_definition":
		name := phpText(n.ChildByFieldName("name"), c.w.src)
		if body := n.ChildByFieldName("body"); body != nil {
			prev := c.w.namespace
			c.w.namespace = name
			c.collectDeclared(body)
			c.w.namespace = prev
			return
		}
		c.w.namespace = name
		return
	case "class_declaration", "interface_declaration", "trait_declaration", "enum_declaration", "function_definition":
		if name := phpText(n.ChildByFieldName("name"), c.w.src); name != "" {
			c.declared[c.w.qualify(name)] = true
		}
	case "assignment_expression":
		switch kindOf(n.ChildByFieldName("right")) {
		case "require_expression", "require_once_expression", "include_expression", "include_once_expression":
			if name := variableName(n.ChildByFieldName("left"), c.w.src); name != "" {
				c.required[name] = true
			}
		}
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.collectDeclared(n.NamedChild(i))
	}
}

// addClass records a class the test names, resolved through the file's imports and
// namespace. A class the file declares names nothing in production.
func (c *phpTestRefCollector) addClass(raw string) {
	switch strings.TrimSpace(raw) {
	case "", "self", "static", "parent":
		return
	}
	if cls := c.w.className(raw); cls != "" && !c.declared[cls] {
		c.seen[cls] = true
	}
}

func (c *phpTestRefCollector) walk(n *sitter.Node) {
	if n == nil {
		return
	}
	switch kindOf(n) {
	case "namespace_definition":
		name := phpText(n.ChildByFieldName("name"), c.w.src)
		body := n.ChildByFieldName("body")
		if body == nil {
			c.w.namespace = name
			return
		}
		prev, prevImports := c.w.namespace, c.w.importMap
		c.w.namespace, c.w.importMap = name, map[string]string{}
		c.walkNamed(body)
		c.w.namespace, c.w.importMap = prev, prevImports
		return
	case "namespace_use_declaration":
		c.w.handleUse(n)
		c.w.out = c.w.out[:0]
		return
	case "attribute_list":
		// #[Test], #[DataProvider], #[CoversClass]: the harness.
		return
	case "class_declaration", "interface_declaration", "trait_declaration", "enum_declaration":
		// A fake extending or implementing a production type names it; the
		// class's own name is the test's.
		fqn := c.w.qualify(phpText(n.ChildByFieldName("name"), c.w.src))
		c.w.typeStack = append(c.w.typeStack, fqn)
		c.walkNamed(n)
		c.w.typeStack = c.w.typeStack[:len(c.w.typeStack)-1]
		return
	case "base_clause", "class_interface_clause", "use_declaration":
		// extends, implements, and a trait a test class uses.
		for i := uint(0); i < n.NamedChildCount(); i++ {
			if ch := n.NamedChild(i); kindOf(ch) == "name" || kindOf(ch) == "qualified_name" {
				c.addClass(phpText(ch, c.w.src))
			}
		}
		return
	case "named_type":
		// A declared parameter, property, return or catch type.
		c.addClass(phpText(n, c.w.src))
		return
	case "object_creation_expression":
		c.addClass(c.w.creationClass(n))
	case "class_constant_access_expression", "scoped_property_access_expression":
		// `Foo::class`, `Foo::BAR`, an enum case, a static property.
		if cls := c.w.namedClass(n); cls != "" && !c.declared[cls] {
			c.seen[cls] = true
		}
	case "binary_expression":
		if phpText(n.ChildByFieldName("operator"), c.w.src) == "instanceof" {
			if r := n.ChildByFieldName("right"); r != nil && (kindOf(r) == "name" || kindOf(r) == "qualified_name") {
				c.addClass(phpText(r, c.w.src))
			}
		}
	case "function_call_expression":
		fn := n.ChildByFieldName("function")
		if fn != nil && (kindOf(fn) == "name" || kindOf(fn) == "qualified_name") {
			name := phpText(fn, c.w.src)
			if target := c.w.resolveRef(name); !phpTestHarnessFuncs[name] && !c.declared[target] && !c.declared[c.w.qualify(name)] {
				c.seen[target] = true
			}
		}
		c.walkExcept(n, fn)
		return
	case "scoped_call_expression":
		scope, method := n.ChildByFieldName("scope"), n.ChildByFieldName("name")
		if scope != nil && method != nil && kindOf(method) == "name" && (kindOf(scope) == "name" || kindOf(scope) == "qualified_name") {
			switch raw := phpText(scope, c.w.src); raw {
			case "self", "static", "parent":
				// The test class's own or inherited member.
			default:
				if cls := c.w.className(raw); cls != "" && !c.declared[cls] {
					c.seen[cls+"::"+phpText(method, c.w.src)] = true
				}
			}
		}
		c.walkExcept(n, method)
		return
	case "member_call_expression", "nullsafe_member_call_expression":
		obj, method := n.ChildByFieldName("object"), n.ChildByFieldName("name")
		if method != nil && kindOf(method) == "name" {
			if name := phpText(method, c.w.src); !isHarnessMethod(name) && !c.harnessReceiver(obj, name) {
				c.seen[name] = true
			}
		}
		c.walkExcept(n, method)
		return
	}
	c.walkNamed(n)
}

// phpFactoryChainMethods are the methods of a Laravel model factory, the builder
// `Model::factory()` returns.
var phpFactoryChainMethods = map[string]bool{
	"create": true, "createOne": true, "createMany": true, "createQuietly": true,
	"make": true, "makeOne": true, "makeMany": true, "raw": true, "for": true,
	"has": true, "hasAttached": true, "state": true, "count": true, "times": true,
	"sequence": true, "forEachSequence": true, "recycle": true, "afterCreating": true,
	"afterMaking": true, "trashed": true,
}

// harnessReceiver reports an instance call made on the test case or on a builder
// the harness owns, whose method names nothing in production even though the name
// is not harness vocabulary by itself:
//
//   - `$this->get(...)` and the test's own helpers are the test case's members, and
//     so is a fluent chain on one, `$this->actingAs($user)->post(...)`. A call
//     through a property, `$this->service->run()`, is on the code under test.
//   - `Model::factory()->for($team)->create()`: the factory's own methods.
//   - `$migration = require '...'; $migration->up()`: the receiver is the object
//     one file returns, whose class the walk cannot name, so the bare method would
//     vouch for every same-named method in the repository.
func (c *phpTestRefCollector) harnessReceiver(obj *sitter.Node, method string) bool {
	for obj != nil {
		switch kindOf(obj) {
		case "variable_name":
			name := variableName(obj, c.w.src)
			return name == "this" || c.required[name]
		case "member_call_expression", "nullsafe_member_call_expression":
			obj = obj.ChildByFieldName("object")
		case "scoped_call_expression":
			name := obj.ChildByFieldName("name")
			return phpFactoryChainMethods[method] && name != nil && phpText(name, c.w.src) == "factory"
		default:
			return false
		}
	}
	return false
}

func (c *phpTestRefCollector) walkNamed(n *sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c.walk(n.NamedChild(i))
	}
}

// walkExcept walks n's named children except skip, the callee already recorded.
func (c *phpTestRefCollector) walkExcept(n, skip *sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		ch := n.NamedChild(i)
		if skip != nil && ch.StartByte() == skip.StartByte() && ch.EndByte() == skip.EndByte() {
			continue
		}
		c.walk(ch)
	}
}
