package dotnetextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	csharp "github.com/enola-labs/enola/internal/extractors/dotnetextractor/grammar"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It parses test files for the
// SOLE purpose of capturing their outbound references into production code,
// emitting one facts.KindTestRef fact per file carrying only RelCalls edges — no
// symbols, no modules, no routes.
//
// It exists because excluding test projects (the `**/tests/**/*.cs` globs) has a
// cost: a production symbol whose only caller is a test then has no inbound edge at
// all and reads as dead. This pass restores that one signal without putting test
// code back into the graph, so test classes never become dead-code candidates
// themselves and no symbol/module/route explainer is affected.
//
// Targets are emitted AS WRITTEN — `OrderService.Find`, `Find`, `OrderService` —
// rather than resolved to canonical fact names. That is not a shortcut: resolution
// needs the production symbol index, which is built inside Extract and is not
// available here, and the consumer does not need it. The orphan detector matches a
// test-ref target both exactly and by its last dot-separated segment, which is why
// the Ruby extractor emits the same `Const.method` shape.
//
// prodFiles is unused: nothing here decides whether a referenced module exists.
func (e *CSharpExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, _ []string) ([]facts.Fact, error) {
	var csFiles []string
	for _, relFile := range testFiles {
		if isCSharpFile(relFile) {
			csFiles = append(csFiles, relFile)
		}
	}
	if len(csFiles) == 0 {
		return nil, nil
	}

	perFile := parallel.MapFiles(ctx, csFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[dotnet-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		if isGeneratedSource(relFile, src) {
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

// testFrameworkReceivers are the receivers whose members are assertions and
// mocking scaffolding rather than references into the code under test. A test file
// is mostly these — a hundred `Assert.Equal` calls per file across 18,000 files is
// a lot of edges that can match no production symbol — and dropping them keeps the
// reference set about the subject rather than about the harness.
//
// This is a precision filter, not a correctness one: a name that slips through
// simply matches nothing.
var testFrameworkReceivers = map[string]bool{
	"Assert": true, "Xunit": true, "NUnit": true, "StringAssert": true,
	"CollectionAssert": true, "Mock": true, "It": true, "Times": true,
	"Substitute": true, "A": true, "Arg": true, "Should": true,
	"TestContext": true, "Record": true, "Verify": true,
}

// testRefsFromFile collects one test file's references into a single KindTestRef
// fact. The fact Name is the file path, which never equals a symbol name, so it
// introduces no self-reference — the contract the orphan detector's fold relies on.
func testRefsFromFile(src []byte, relFile string) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(csharp.Language())); err != nil {
		return nil
	}
	src = activeBranches(src)
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	seen := map[string]bool{}
	c := &testRefCollector{src: src, seen: seen}
	c.walk(tree.RootNode())
	if len(seen) == 0 {
		return nil
	}

	targets := make([]string, 0, len(seen))
	for t := range seen {
		targets = append(targets, t)
	}
	// Sorted: the set comes out of a map, and facts.jsonl is hashed into the
	// snapshot id.
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
		Props:     map[string]any{"language": "csharp"},
		Relations: rels,
	}}
}

type testRefCollector struct {
	src  []byte
	seen map[string]bool
}

func (c *testRefCollector) add(target string) {
	if target == "" || testFrameworkReceivers[target] {
		return
	}
	if recv, _, ok := strings.Cut(target, "."); ok && testFrameworkReceivers[recv] {
		return
	}
	c.seen[target] = true
}

func (c *testRefCollector) walk(node *sitter.Node) {
	switch kindOf(node) {
	case "invocation_expression":
		fn := node.ChildByFieldName("function")
		c.addCallTarget(fn)
		// The function subtree is already accounted for; walking it again would
		// re-add the receiver's member access as a separate reference.
		if args := node.ChildByFieldName("arguments"); args != nil {
			c.walk(args)
		}
		return
	case "object_creation_expression":
		c.add(simpleTypeName(typeFullName(node.ChildByFieldName("type"), c.src)))
	case "member_access_expression":
		// A member access NOT under an invocation: a property read, an enum member
		// (`OrderStatus.Draft`), a static field.
		if recv := node.ChildByFieldName("expression"); recv != nil && kindOf(recv) == "identifier" {
			name := nodeText(node.ChildByFieldName("name"), c.src)
			c.add(nodeText(recv, c.src) + "." + name)
		}
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		c.walk(node.Child(i))
	}
}

// addCallTarget records an invocation's target in the shape the main walker uses:
// `Receiver.Method` when the receiver is a plain identifier, the bare method name
// otherwise. Both forms are matched by the orphan detector, which also tries the
// last segment.
func (c *testRefCollector) addCallTarget(fn *sitter.Node) {
	if fn == nil {
		return
	}
	switch kindOf(fn) {
	case "identifier", "generic_name":
		c.add(simpleTypeName(nodeText(fn, c.src)))
	case "member_access_expression":
		name := simpleTypeName(nodeText(fn.ChildByFieldName("name"), c.src))
		recvNode := fn.ChildByFieldName("expression")
		if recvNode != nil && kindOf(recvNode) == "identifier" {
			recv := nodeText(recvNode, c.src)
			// A framework receiver disqualifies the BARE name too. Filtering only
			// the qualified form let `Assert.Equal` through as `Equal`, and `Equal`
			// is a method name production code really has — so the harness would
			// have vouched for a symbol no test actually exercises, suppressing a
			// genuine dead-code finding. That is the opposite of what this pass is
			// for.
			if testFrameworkReceivers[recv] {
				return
			}
			c.add(recv + "." + name)
			// Also the bare name: a variable receiver tells us nothing about the
			// declaring type, and the method name is what matches.
			c.add(name)
			return
		}
		c.add(name)
		// Descend the receiver chain so `factory.Create().Save()` records both.
		if recvNode != nil {
			c.walk(recvNode)
		}
	}
}
