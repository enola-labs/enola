package phpextractor

import (
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// TestGrammarSmoke is the ABI guard, and it is the most important test in this package.
//
// The go-tree-sitter runtime accepts tree-sitter ABI 13 to 15. A grammar outside that
// range is refused by SetLanguage, and the extractors return nil on
// that error — so the rejection is SILENT: every file parses to nothing, and the fact
// graph becomes indistinguishable from a repository containing no PHP. That is exactly
// how the C# grammar failed once (see dotnetextractor/csharp.go).
//
// tree-sitter-php is pinned at v0.23.12: every later release requires go-tree-sitter
// v0.25.0, a tag upstream deleted, so it builds only through the module proxy. The bound
// is recorded in .github/dependabot.yml. If this fails after a dependency bump, the fix
// is to pin the grammar back, not to loosen the assertion.
//
// LanguagePHP (not LanguagePHPOnly) is the dialect under test because that is what the
// extractors load: template files interleaving HTML and <?php must still parse.
func TestGrammarSmoke(t *testing.T) {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		t.Fatalf("SetLanguage failed — the grammar is almost certainly built against a "+
			"tree-sitter ABI the runtime does not accept. Pin tree-sitter-php "+
			"to v0.23.12. Error: %v", err)
	}

	// Each case exercises a different half of the grammar, so a regeneration that broke
	// one would not pass on the others.
	for _, tc := range []struct{ name, src string }{
		{"namespace_class", "<?php\n\nnamespace Acme\\Http;\n\nuse Acme\\Support\\Helper;\n\n" +
			"class Controller extends Base implements Handler\n{\n" +
			"    public function handle(string $in): string { return $in; }\n}\n"},
		// The Laravel/Symfony shapes the extractor reads for routes and DI. A grammar that
		// parsed the class but shredded attributes or static calls would lose every route.
		{"routes_and_attributes", "<?php\n\n#[Route('/api')]\nclass Api\n{\n" +
			"    #[Get('/posts')]\n    public function index(PostRepo $repo): array { return $repo->all(); }\n}\n\n" +
			"Route::get('/health', [Api::class, 'index']);\n"},
		{"traits_enums_interfaces", "<?php\n\ninterface Handler { public function handle(): void; }\n" +
			"trait Loggable { public function log(string $m): void {} }\n" +
			"enum Status: string { case Active = 'active'; }\n"},
		// Modern syntax the older grammar handled poorly; if it regressed the walker would
		// quietly stop seeing these call edges.
		{"modern_php", "<?php\n\nclass C {\n  public function run(?Helper $h): int {\n" +
			"    $h?->maybe();\n    $fn = fn($x) => $x * 2;\n    return match(true) { default => $fn(1) };\n  }\n}\n"},
		// A template file: HTML around a <?php island. This is why LanguagePHP is used.
		{"template_file", "<div class=\"x\">\n<?php foreach ($items as $item): ?>\n" +
			"  <span><?= $item->name ?></span>\n<?php endforeach; ?>\n</div>\n"},
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

// TestWalkerNodeKindsStillExist pins the exact grammar node kinds the walkers dispatch
// on. A grammar upgrade that renamed one would otherwise degrade extraction silently —
// the walkers descend into anything they do not recognize, so a renamed
// `member_call_expression` would stop producing call edges without producing an error.
//
// The list is every string this package switches on (or compares Kind() against) that is
// a real node type in the grammar; the source below is built to produce all of them.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	const src = `<?php

namespace Acme\Http;

use Acme\Support\Helper;

interface Handler { public function handle(string $in): string; }

trait Loggable { public function log(string $m): void {} }

enum Status: string {
    case Active = 'active';
    case Archived = 'archived';
}

function helper(int $x): int { return $x; }

#[Route('/api')]
class Controller extends Base implements Handler
{
    use Loggable;

    public const MAX = 10;
    private static int $count = 0;
    protected array $items = [];

    public function handle(string $in): string
    {
        $n = strlen($in) + self::MAX;
        $obj = new Helper();
        $obj->run();
        $obj?->maybe();
        static::boot();
        parent::handle($in);
        $s = "hi $in";
        $fn = function ($x) { return $x + 1; };
        $arrow = fn($x) => $x * 2;
        $list = [1, 2, 3];
        foreach ($list as $item) { $n += $item; }
        for ($i = 0; $i < $n; $i++) { $n--; }
        while ($n > 0) { $n--; }
        do { $n++; } while ($n < 0);
        return Status::Active->value . $s . $fn(1) . $arrow(2) . helper($n);
    }
}
`
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}
	tree := parser.Parse([]byte(src), nil)
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

	for _, kind := range []string{
		"anonymous_function", "argument", "array_creation_expression", "arrow_function",
		"attribute", "binary_expression", "class_constant_access_expression",
		"class_declaration", "const_declaration", "do_statement", "encapsed_string",
		"enum_case", "enum_declaration", "for_statement", "foreach_statement",
		"function_call_expression", "function_definition", "integer", "interface_declaration",
		"member_call_expression", "method_declaration", "name", "namespace_definition",
		"namespace_name", "namespace_use_declaration", "nullsafe_member_call_expression",
		"object_creation_expression", "parent", "property_declaration", "qualified_name",
		"scoped_call_expression", "self", "static", "string", "trait_declaration",
		"use_declaration", "visibility_modifier", "while_statement",
	} {
		if !seen[kind] {
			t.Errorf("grammar no longer produces node kind %q — the walker dispatches on it "+
				"and would silently stop extracting", kind)
		}
	}
}
