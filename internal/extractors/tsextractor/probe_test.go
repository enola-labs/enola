package tsextractor

import (
	"testing"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

const tsSrc = `// a comment
import { a, b } from './x';
import * as ns from './y';
import def from './z';

export interface Handler { handle(s: string): boolean }
export type Alias = string | number;
export enum Status { Active, Archived }

namespace Inner { export const x = 1; }
namespace Outer.Nested { export const y = 2; }
module Legacy { }

abstract class AbstractThing { abstract run(): void }

@Injectable()
export class Svc {}

export default class Impl extends Base implements Handler {
  private readonly name: string = 'n';
  constructor(name: string) { super(); this.name = name; }
  handle(s: string): boolean { return s.length > 0; }
}

export function* gen(): Generator<number> { yield 1; }

export function run(input: string): number {
  var legacy = 1;
  const arr = [1, 2, 3];
  const obj = { k: 1, v: 'two' };
  const { k, v } = obj;
  const f = function () { return 1; };
  const g = function* () { yield 2; };
  const arrow = (x: number) => x + 1;
  let total = 0;
  for (let i = 0; i < arr.length; i++) { total += i; }
  for (const key in obj) { total += 1; }
  if (total > 0) { total = 1; }
  while (total > 0) { total--; }
  do { total++; } while (total < 0);
  switch (total) { case 1: break; default: break; }
  const t = total > 1 ? 'a' : 'b';
  try { ns.call(); } catch (e) { total = 0; }
  return (total) + f() + g().next().value + arrow(1) + legacy + Number(true) + k + v + t;
}
`

const tsxSrc = `import React from 'react';

export const View = ({ id }: { id: number }) => (
  <div className="x">
    <Widget.Inner id={id} />
    <span>hi</span>
  </div>
);
`

// TestWalkerNodeKindsStillExist pins the exact grammar node kinds the walker dispatches
// on. A grammar upgrade that renamed one would otherwise degrade extraction silently —
// the walker descends into anything it does not recognize, so a renamed declaration node
// stops producing symbols without producing a single error.
//
// The list is every string this package switches on (or compares Kind() against) that is
// a real node type in the grammar; the sources below are built to produce all of them.
func TestWalkerNodeKindsStillExist(t *testing.T) {
	seen := map[string]bool{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		seen[n.Kind()] = true
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	for _, c := range []struct {
		name string
		lang *sitter.Language
		src  string
	}{
		{"ts", sitter.NewLanguage(typescript.LanguageTypescript()), tsSrc},
		{"tsx", sitter.NewLanguage(typescript.LanguageTSX()), tsxSrc},
	} {
		parser := sitter.NewParser()
		if err := parser.SetLanguage(c.lang); err != nil {
			t.Fatalf("SetLanguage %s: %v", c.name, err)
		}
		tree := parser.Parse([]byte(c.src), nil)
		if tree.RootNode().HasError() {
			t.Fatalf("pinning source %v no longer parses cleanly:\n%s", c.name, tree.RootNode().ToSexp())
		}
		walk(tree.RootNode())
		tree.Close()
		parser.Close()
	}
	want := []string{
		"abstract_class_declaration", "accessibility_modifier", "array", "arrow_function",
		"binary_expression", "call_expression", "catch_clause", "class", "class_declaration",
		"class_heritage", "comment", "decorator", "default", "do_statement", "enum_declaration",
		"export_statement", "extends_clause", "for_in_statement", "for_statement", "function",
		"function_declaration", "function_expression", "generator_function",
		"generator_function_declaration", "identifier", "if_statement", "implements_clause",
		"import", "import_statement", "interface_declaration", "internal_module", "jsx_element",
		"jsx_opening_element", "jsx_self_closing_element", "lexical_declaration",
		"member_expression", "method_definition", "module", "named_imports", "namespace_import",
		"nested_identifier", "number", "object", "object_pattern", "parenthesized_expression",
		"return", "shorthand_property_identifier_pattern", "string", "switch_case",
		"ternary_expression", "this", "true", "type_alias_declaration", "type_identifier",
		"variable_declaration", "variable_declarator", "while_statement",
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
// that error — so the rejection is SILENT: every file parses to nothing, and the fact
// graph becomes indistinguishable from a repository containing no TypeScript. That is
// exactly how the C# grammar failed once (see dotnetextractor/csharp.go).
//
// This package loads BOTH dialects — LanguageTypescript for .ts and LanguageTSX for .tsx,
// and the Vue and Svelte readers pick between them the same way — so both are asserted
// here; they ship as separate generated parsers and could diverge.
//
// tree-sitter-typescript v0.23.2 is the newest release (ABI 14 in both dialects), so
// nothing is pinned back and .github/dependabot.yml carries no bound for it. This test
// exists for the day upstream regenerates beyond what the runtime accepts: the bump then
// fails loudly here instead of quietly deleting every TypeScript fact from the graph, and
// the fix is to add that bound.
func TestGrammarSmoke(t *testing.T) {
	for _, g := range []struct {
		name string
		lang *sitter.Language
		srcs []struct{ name, src string }
	}{
		{
			name: "typescript", lang: sitter.NewLanguage(typescript.LanguageTypescript()),
			srcs: []struct{ name, src string }{
				{"imports_class", "import { Helper } from './helper';\n\nexport class Service extends Base implements Handler {\n  constructor(private readonly helper: Helper) { super(); }\n  handle(s: string): boolean { return s.length > 0; }\n}\n"},
				{"types_interfaces", "export interface Post { id: number; title: string }\nexport type Maybe<T> = T | null;\nexport enum Status { Active, Archived }\n"},
				// NestJS decorators and Express route calls are what the extractor reads for
				// routes; a shredded decorator would lose every route fact.
				{"decorators_routes", "@Controller('posts')\nexport class PostsController {\n  @Get(':id')\n  findOne(@Param('id') id: string) { return id; }\n}\n\nrouter.get('/health', (req, res) => res.send('ok'));\n"},
				{"modern_ts", "export async function load(url: string): Promise<Post[]> {\n  const r = await fetch(url);\n  return (await r.json()) satisfies Post[];\n}\n"},
			},
		},
		{
			name: "tsx", lang: sitter.NewLanguage(typescript.LanguageTSX()),
			srcs: []struct{ name, src string }{
				{"component", "import React from 'react';\n\nexport const View = ({ id }: { id: number }) => (\n  <div className=\"x\">\n    <Widget.Inner id={id} />\n    <span>hi</span>\n  </div>\n);\n"},
				{"hooks", "export function useThing() {\n  const [v, setV] = React.useState<number>(0);\n  React.useEffect(() => { setV(1); }, []);\n  return v;\n}\n"},
			},
		},
	} {
		t.Run(g.name, func(t *testing.T) {
			parser := sitter.NewParser()
			defer parser.Close()
			if err := parser.SetLanguage(g.lang); err != nil {
				t.Fatalf("SetLanguage failed for the %s dialect — it is almost certainly built "+
					"against a tree-sitter ABI the runtime does not accept. Error: %v",
					g.name, err)
			}
			for _, tc := range g.srcs {
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
					// A grammar the runtime refused yields a root with no children rather than
					// an error, so HasError alone would not catch it.
					if root.ChildCount() == 0 {
						t.Error("root has no children — the grammar was probably rejected")
					}
				})
			}
		})
	}
}
