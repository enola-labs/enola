package pythonextractor

import (
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

const nodeKindSrc = `# a comment
import os
import os.path as osp
from typing import List, Optional

CONST = 1


class Service(Base):
    def __init__(self, name: str):
        self.name = name


@app.route("/x")
def handler(a: int, b: str = "d", c=1, *args, **kwargs) -> bool:
    xs = [1, 2, 3]
    d = {"k": 1}
    s = {1, 2}
    lc = [x for x in xs if x]
    dc = {k: v for k, v in d.items()}
    sc = {x for x in xs}
    ge = (x for x in xs)
    t = (1, 2)
    a1, b1 = 1, 2
    [c1, d1] = [3, 4]
    (e1, f1) = (5, 6)
    total = 0
    total += 1
    fn = lambda y: y + 1
    if (n := len(xs)) > 0:
        total = xs[0]
    for i in xs:
        total += i
    with open("f") as fh:
        fh.read()
    try:
        raise ValueError("x")
    except ValueError as e:
        total = 0
    osp.join(os.sep, "a")
    obj.attr.method(1, key=2, *xs, **d)
    return True
`

// TestWalkerNodeKindsStillExist pins the exact grammar node kinds the walker dispatches
// on. A grammar upgrade that renamed one would otherwise degrade extraction silently —
// the walker descends into anything it does not recognize, so a renamed declaration node
// stops producing symbols without producing a single error.
//
// The list is every string this package switches on (or compares Kind() against) that is
// a real node type in the grammar; the sources below are built to produce all of them.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(python.Language())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(nodeKindSrc), nil)
	defer tree.Close()
	if tree.RootNode().HasError() {
		t.Fatalf("the pinning source no longer parses cleanly:\n%s", tree.RootNode().ToSexp())
	}
	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		seen[n.Kind()] = true
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(tree.RootNode())
	want := []string{
		"aliased_import", "as_pattern", "assignment", "attribute", "augmented_assignment",
		"block", "call", "class_definition", "comment", "decorated_definition", "decorator",
		"default_parameter", "dictionary", "dictionary_comprehension", "dictionary_splat_pattern",
		"dotted_name", "except_clause", "expression_list", "expression_statement", "for_in_clause",
		"for_statement", "function_definition", "generator_expression", "identifier",
		"if_statement", "import_from_statement", "import_statement", "integer",
		"keyword_argument", "lambda", "list", "list_comprehension", "list_pattern",
		"list_splat_pattern", "named_expression", "pair", "pattern_list", "raise_statement",
		"set", "set_comprehension", "string", "subscript", "true", "try_statement", "tuple",
		"tuple_pattern", "typed_default_parameter", "typed_parameter", "with_item",
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("grammar no longer produces node kind %q — the walker dispatches on it "+
				"and would silently stop extracting", k)
		}
	}
}

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The go-tree-sitter runtime accepts tree-sitter ABI 13 to 15. A grammar outside that
// range is refused by SetLanguage, and the extractor returns nil on
// that error — so the rejection is SILENT: every Python file parses to nothing, and the
// fact graph becomes indistinguishable from a repository containing none. That is exactly
// how the C# grammar failed once (see dotnetextractor/csharp.go).
//
// If this fails after a dependency bump, the fix is a grammar the runtime accepts, not a
// looser assertion.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(python.Language())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"tree-sitter ABI the runtime does not accept. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that broke
	// one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"module_class", "import os\nfrom typing import List\n\nclass Service:\n    def __init__(self, name: str):\n        self.name = name\n"},
		{"routes", "from fastapi import APIRouter\n\nrouter = APIRouter(prefix='/api')\n\n@router.get('/posts')\nasync def index(limit: int = 10) -> list:\n    return []\n"},
		{"modern", "async def load(url: str) -> dict:\n    async with session.get(url) as r:\n        data = await r.json()\n    if (n := len(data)) > 0:\n        return {'k': n}\n    return {}\n"},
		{"match_statement", "def route(cmd):\n    match cmd:\n        case {'op': 'add', 'x': x}:\n            return x\n        case _:\n            return None\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := parser.Parse([]byte(tc.src), nil)
			if tree == nil {
				t.Fatal("nil tree")
			}
			defer tree.Close()
			root := tree.RootNode()
			if root == nil {
				t.Fatal("nil root node")
			}
			if root.HasError() {
				t.Errorf("a trivial valid file parsed with errors — grammar mismatch:\n%s", root.ToSexp())
			}
			// A grammar the runtime refused yields a root with no children rather than an
			// error, so HasError alone would not catch it.
			if root.ChildCount() == 0 {
				t.Error("root has no children — the grammar was probably rejected")
			}
		})
	}
}
