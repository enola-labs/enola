package tsextractor

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// TestExtractTestRefs_SvelteKitAliasFallbacks pins that a test resolves `$lib` the
// way production code does in a SvelteKit project with no generated tsconfig. The
// file index alone cannot tell src/lib/util.ts from pkg/util.ts for a test outside
// both directories.
func TestExtractTestRefs_SvelteKitAliasFallbacks(t *testing.T) {
	files := map[string]string{
		"svelte.config.js":   `export default {}`,
		"src/lib/util.ts":    `export function f() { return 1 }`,
		"pkg/util.ts":        `export function f() { return 2 }`,
		"tests/util.test.ts": `import { f } from '$lib/util'` + "\n" + `f()`,
	}
	dir := setupTSProject(t, files, false)
	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"tests/util.test.ts"}, []string{"src/lib/util.ts", "pkg/util.ts"})
	if err != nil {
		t.Fatal(err)
	}
	got := testRefTargets(t, ff, "tests/util.test.ts")
	if !got["src/lib.f"] {
		t.Errorf("missing target src/lib.f; got %v", got)
	}
	if got["tests.f"] || got["pkg.f"] {
		t.Errorf("import through $lib bound elsewhere; got %v", got)
	}
}

// TestBarrelExports_EmittedExtensionSpecifier pins that a re-export written with the
// extension the module compiles to (`./lib/a.js` for lib/a.ts) is followed, for a
// star and a named re-export alike.
func TestBarrelExports_EmittedExtensionSpecifier(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/index.ts":   `export * from './lib/a.js'` + "\n" + `export { g } from './other/b.js'`,
		"src/lib/a.ts":   `export function f() { return 1 }`,
		"src/other/b.ts": `export function g() { return 1 }`,
		"main.ts":        `import { f, g } from './src/index.js'` + "\n" + `export function main() { return f() + g() }`,
	}, false)
	m, ok := findFact(ff, "..main")
	if !ok {
		t.Fatal("main not extracted")
	}
	for _, want := range []string{"src/lib.f", "src/other.g"} {
		if !hasRelation(m, facts.RelCalls, want) {
			t.Errorf("missing call to %s: %v", want, m.Relations)
		}
	}
}

// TestBarrelExports_NonIndexModule pins that a module other than a folder index that
// re-exports (`src/api.ts` with `export * from './impl/impl'`) binds the names it
// passes on to their declarations. A name another file of the folder declares stays
// the folder's.
func TestBarrelExports_NonIndexModule(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/api.ts":       `export * from './impl/impl'` + "\n" + `export { h as hh } from './impl/h'`,
		"src/impl/impl.ts": `export function f() { return 1 }` + "\n" + `export function own() { return 1 }`,
		"src/impl/h.ts":    `export function h() { return 1 }`,
		"src/own.ts":       `export function own() { return 2 }`,
		"main.ts":          `import { f, hh, own } from './src/api'` + "\n" + `export function main() { return f() + hh() + own() }`,
	}, false)
	m, ok := findFact(ff, "..main")
	if !ok {
		t.Fatal("main not extracted")
	}
	for _, want := range []string{"src/impl.f", "src/impl.h", "src.own"} {
		if !hasRelation(m, facts.RelCalls, want) {
			t.Errorf("missing call to %s: %v", want, m.Relations)
		}
	}
	if hasRelation(m, facts.RelCalls, "src/impl.own") {
		t.Errorf("a name the folder declares moved: %v", m.Relations)
	}
}

// TestBarrelExports_NamesEdgeRewritten pins that a function passed as a value through
// a barrel names its declaration, as a call through it does.
func TestBarrelExports_NamesEdgeRewritten(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/index.ts": `export { f } from './a/a'`,
		"src/a/a.ts":   `export function f() { return 1 }`,
		"main.ts":      `import { f } from './src'` + "\n" + `export function main() { return [1].map(f) }`,
	}, false)
	m, ok := findFact(ff, "..main")
	if !ok {
		t.Fatal("main not extracted")
	}
	if !hasRelation(m, facts.RelNames, "src/a.f") {
		t.Errorf("missing names src/a.f: %v", m.Relations)
	}
	if hasRelation(m, facts.RelNames, "src.f") {
		t.Errorf("names edge kept the barrel target: %v", m.Relations)
	}
}

// TestNamespaceLookup_DynamicKeyFollowsReexports pins that a namespace module that
// only re-exports passes reached_dynamically to the declarations it re-exports.
func TestNamespaceLookup_DynamicKeyFollowsReexports(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"tpl/index.ts":  `export { A } from './A'` + "\n" + `export * from './more/B'`,
		"tpl/A.ts":      `export const A = () => 1`,
		"tpl/more/B.ts": `export function B() {}` + "\n" + `function hidden() {}`,
		"app/render.ts": `import * as templates from '../tpl'
export function render(k: string) { return templates[k] }`,
	}, false)
	for _, name := range []string{"tpl.A", "tpl/more.B"} {
		sym, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("%s not extracted", name)
		}
		if sym.Props[propReachedDynamically] != true {
			t.Errorf("%s: reached_dynamically = %v", name, sym.Props[propReachedDynamically])
		}
		from, _ := sym.Props[propReachedDynamicallyFrom].([]string)
		if !slices.Equal(from, []string{"app/render.ts"}) {
			t.Errorf("%s: reached_dynamically_from = %v", name, sym.Props[propReachedDynamicallyFrom])
		}
	}
	if h, _ := findFact(ff, "tpl/more.hidden"); h.Props[propReachedDynamically] != nil {
		t.Error("an unexported declaration was marked")
	}
}
