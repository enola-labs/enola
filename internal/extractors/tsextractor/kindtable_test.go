package tsextractor

import (
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// This extractor parses two grammars — TypeScript and TSX, the latter also for the
// script blocks lifted out of Vue and Svelte files — and only 12% of their symbol ids
// mean the same thing. A node read against the wrong table yields a real kind name
// for a different node type: no error, just a construct silently unrecognised.
//
// Both tables are therefore checked against Node.Kind() over sources using what each
// grammar has and the other does not.
func TestKindTable_MatchesNodeKind(t *testing.T) {
	cases := []struct {
		name string
		tsx  bool
		src  string
	}{
		{"typescript", false, `
import type { A } from "./a";
export default abstract class C<T extends object = {}> implements I {
  readonly #priv: T | null = null;
  constructor(private readonly dep: Dep) { super(); }
  @Get(":id") async find(@Param() id: string): Promise<T[]> {
    const x = <T>{} as unknown as T;
    for await (const y of gen()) { yield? y; }
    return [x!] satisfies T[];
  }
}
enum E { A = 1 }
namespace N { export const v = 1; }
declare module "m" { export function f(): void; }
type U = { [K in keyof T]?: T[K] } | (() => void);
`},
		{"tsx", true, `
export const App = ({ items }: Props) => (
  <Router>
    <Route path="/x" element={<Page id={1} {...rest} />} />
    <>{items.map((i) => <li key={i}>{i}</li>)}</>
  </Router>
);
function G<T,>(p: T) { return <div data-x="1">{p as string}</div>; }
`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lang := typescript.LanguageTypescript()
			if tc.tsx {
				lang = typescript.LanguageTSX()
			}
			parser := sitter.NewParser()
			defer parser.Close()
			if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
				t.Fatal(err)
			}
			tree := parser.Parse([]byte(tc.src), nil)
			if tree == nil {
				t.Fatal("parse produced no tree")
			}
			defer tree.Close()

			table := tsKindsFor(tc.tsx)
			var checked int
			walkAllTS(tree.RootNode(), func(n *sitter.Node) {
				checked++
				if got, want := kindOf(table, n), n.Kind(); got != want {
					t.Fatalf("kind table disagrees with Node.Kind(): id=%d table=%q Kind()=%q",
						n.KindId(), got, want)
				}
			})
			if checked == 0 {
				t.Fatal("no nodes checked")
			}
			t.Logf("checked %d nodes", checked)
		})
	}
}

// The two tables must be distinct objects. A shared package-level table is exactly
// the bug this file guards, and it would pass every behavioural test on .ts files
// while quietly misreading every .tsx one.
func TestKindTable_PerGrammar(t *testing.T) {
	if tsKindsFor(false) == tsKindsFor(true) {
		t.Fatal("TypeScript and TSX share one kind table; their symbol ids do not agree")
	}
}

func walkAllTS(n *sitter.Node, fn func(*sitter.Node)) {
	fn(n)
	for i := uint(0); i < n.ChildCount(); i++ {
		walkAllTS(n.Child(i), fn)
	}
}
