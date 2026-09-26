package scalaextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	scala "github.com/enola-labs/enola/internal/extractors/scalaextractor/grammar"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It parses test sources for the
// SOLE purpose of capturing their outbound references into production code, emitting
// one facts.KindTestRef fact per file carrying only RelCalls edges — no symbols, no
// modules, no routes.
//
// It exists to pay back the cost of excluding test source sets. Once `src/test/**`
// is ignored, a production symbol whose only caller is a spec has no inbound edge at
// all and reads as dead — and in Scala that is a large class, because a repository's
// public API is frequently exercised only from its own test suite. This pass restores
// that one signal without putting test code back in the graph: the test classes
// themselves never become dead-code candidates, and no symbol/module/route explainer
// sees anything new.
//
// Targets are emitted AS WRITTEN — `OrderService.find`, `find`, `OrderService` —
// rather than resolved to canonical names. Resolution needs the production symbol
// index built inside Extract, which is not available here, and the consumer does not
// need it: the orphan detector matches a test-ref target both exactly and by its last
// dot-separated segment. The Ruby and C# extractors emit the same shape.
//
// prodFiles is unused: nothing here decides whether a referenced module exists.
func (e *ScalaExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, _ []string) ([]facts.Fact, error) {
	var scalaFiles []string
	for _, relFile := range testFiles {
		if isScalaFile(relFile) {
			scalaFiles = append(scalaFiles, relFile)
		}
	}
	if len(scalaFiles) == 0 {
		return nil, nil
	}

	perFile := parallel.MapFiles(ctx, scalaFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[scala-extractor] error reading test file %s: %v", relFile, err)
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

// testHarnessNames are the receivers and bare calls that belong to the test harness
// rather than to the code under test — assertions, matchers, mocking, generators, and
// the spec-structure DSL every Scala framework spells differently (ScalaTest's
// `describe`/`in`, munit's `test`, specs2's `should`, ZIO Test's `suite`/`assertTrue`).
//
// A test file is mostly these, and they match no production symbol, so dropping them
// keeps the reference set about the subject instead of about the framework.
//
// This is a precision filter and not a correctness one — with one exception, inherited
// from the C# pass: a harness receiver disqualifies the BARE method name too. Filtering
// only `Assert.equals` lets `equals` through, and production code really does declare
// `equals`, so the harness would vouch for a symbol no test exercises and suppress a
// genuine dead-code finding. That is the opposite of this pass's purpose.
var testHarnessNames = map[string]bool{
	// Assertions and matchers.
	"assert": true, "assertResult": true, "assertThrows": true, "assertEquals": true,
	"assertNotEquals": true, "assertTrue": true, "assertFalse": true, "assertIO": true,
	"assume": true, "intercept": true, "fail": true, "cancel": true, "succeed": true,
	"should": true, "shouldBe": true, "shouldEqual": true, "shouldNot": true,
	"must": true, "mustBe": true, "mustEqual": true, "be": true, "equal": true,
	"contain": true, "include": true, "have": true, "matchPattern": true,
	"Assert": true, "Assertions": true, "Matchers": true, "Inspectors": true,
	"expect": true, "expecty": true, "diff": true,
	// Spec structure.
	"describe": true, "test": true, "suite": true, "it": true, "they": true,
	"in": true, "when": true, "feature": true, "scenario": true, "property": true,
	"before": true, "after": true, "beforeEach": true, "afterEach": true,
	"beforeAll": true, "afterAll": true, "withFixture": true, "fixture": true,
	"check": true, "checkAll": true, "forAll": true, "forEvery": true, "exactly": true,
	// Mocking and stubbing.
	"mock": true, "Mockito": true, "verify": true, "stub": true, "spy": true,
	"any": true, "anyString": true, "eq": true, "argThat": true, "returns": true,
	"thenReturn": true, "doReturn": true, "smartMock": true,
	// Property generators.
	"Gen": true, "Prop": true, "Arbitrary": true, "arbitrary": true, "sample": true,
	// Waiting and effect runners that appear in every effectful test.
	"eventually": true, "whenReady": true, "Await": true, "await": true,
	"unsafeRunSync": true, "unsafeToFuture": true, "runtime": true,
}

// testRefsFromFile collects one file's references into a single KindTestRef fact.
// The fact Name is the file path, which never equals a symbol name, so the fact
// introduces no self-reference — the contract the orphan detector's fold relies on.
func testRefsFromFile(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(scala.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	c := &scalaTestRefCollector{src: src, seen: map[string]bool{}}
	c.walk(tree.RootNode())
	if len(c.seen) == 0 {
		return nil
	}

	targets := make([]string, 0, len(c.seen))
	for t := range c.seen {
		targets = append(targets, t)
	}
	// Sorted: the set comes out of a map, and facts.jsonl is hashed into the
	// snapshot id, so an unsorted list would make an unchanged tree produce a
	// different snapshot on every run.
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
		Props:     map[string]any{"language": "scala"},
		Relations: rels,
	}}
}

type scalaTestRefCollector struct {
	src  []byte
	seen map[string]bool
}

func (c *scalaTestRefCollector) text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	s, e := n.StartByte(), n.EndByte()
	if e > uint(len(c.src)) {
		e = uint(len(c.src))
	}
	return string(c.src[s:e])
}

func (c *scalaTestRefCollector) add(target string) {
	if target == "" || testHarnessNames[target] {
		return
	}
	if recv, _, ok := strings.Cut(target, "."); ok && testHarnessNames[recv] {
		return
	}
	c.seen[target] = true
}

func (c *scalaTestRefCollector) walk(n *sitter.Node) {
	switch kindOf(n) {
	case "call_expression":
		c.addCallTarget(n)
		// The callee subtree is accounted for; walk only the arguments so a
		// receiver chain is not re-added as a separate bare reference.
		for i := uint(0); i < n.ChildCount(); i++ {
			ch := n.Child(i)
			switch kindOf(ch) {
			case "arguments", "block", "lambda_expression", "case_block":
				c.walk(ch)
			}
		}
		return
	case "instance_expression":
		for i := uint(0); i < n.ChildCount(); i++ {
			ch := n.Child(i)
			if !ch.IsNamed() || kindOf(ch) == "arguments" {
				continue
			}
			if t := baseTypeText(c.text(ch)); t != "" {
				c.add(t)
			}
			break
		}
	case "field_expression":
		// A member access that is not a call: an object's value, an enum case.
		if recv := firstNamedChild(n); recv != nil && kindOf(recv) == "identifier" {
			var last *sitter.Node
			for i := uint(0); i < n.ChildCount(); i++ {
				if ch := n.Child(i); ch.IsNamed() {
					last = ch
				}
			}
			if last != nil && !sameNode(last, recv) {
				c.add(c.text(recv) + "." + c.text(last))
			}
		}
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		c.walk(n.Child(i))
	}
}

// addCallTarget records a call in the shape the main walker emits: `Receiver.method`
// when the receiver is a plain identifier, plus the bare method name, which is what
// matches when the receiver is a local whose type is not known here.
func (c *scalaTestRefCollector) addCallTarget(call *sitter.Node) {
	fn := firstNamedChild(call)
	if fn == nil {
		return
	}
	if kindOf(fn) == "generic_function" {
		fn = firstNamedChild(fn)
	}
	if fn == nil {
		return
	}
	switch kindOf(fn) {
	case "identifier":
		// A bare call, or an apply-form construction (`OrderService(deps)`).
		c.add(c.text(fn))
	case "field_expression":
		var named []*sitter.Node
		for i := uint(0); i < fn.ChildCount(); i++ {
			if ch := fn.Child(i); ch.IsNamed() {
				named = append(named, ch)
			}
		}
		if len(named) == 0 {
			return
		}
		method := c.text(named[len(named)-1])
		if len(named) == 1 {
			c.add(method)
			return
		}
		recvNode := named[len(named)-2]
		if kindOf(recvNode) == "identifier" {
			recv := c.text(recvNode)
			// A harness receiver disqualifies the bare name too — see the comment
			// on testHarnessNames for why this one filter is load-bearing.
			if testHarnessNames[recv] {
				return
			}
			c.add(recv + "." + method)
			c.add(method)
			return
		}
		c.add(method)
		// Descend the receiver chain so `factory.create().save()` records both.
		c.walk(recvNode)
	case "call_expression":
		// A curried application: the inner call carries the callee.
		c.addCallTarget(fn)
	}
}

// baseTypeText strips type arguments from a constructed type, so `new Repo[IO](db)`
// records `Repo`.
func baseTypeText(s string) string {
	if i := strings.IndexAny(s, "[("); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s
}
