package tsextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// starBarrelFiles is a folder whose index re-exports with `export *`: a name one
// star source declares binds there (followed through a nested star, and past a
// cycle), a name the index declares itself stays the index's, and a name two star
// sources declare binds to neither. A name another file of the folder declares
// stays the folder's too.
var starBarrelFiles = map[string]string{
	"lib/index.ts": `export * from './a/x'
export * from './b/y'
export * as ns from './c/z'
export function own() {}
`,
	"lib/a/x.ts":    `export function translate() {}` + "\n" + `export function dup() {}` + "\n" + `export function own() {}` + "\n" + `export function shade() {}` + "\n" + `export default function dflt() {}`,
	"lib/shade.ts":  `export function shade() {}`,
	"lib/b/y.ts":    `export function dup() {}` + "\n" + `export * from '../d/deep'`,
	"lib/c/z.ts":    `export function hidden() {}`,
	"lib/d/deep.ts": `export const deep = () => 1` + "\n" + `export * from '../b/y'`,
}

func TestBarrelExports_StarReexportFollowedToTheDeclaringModule(t *testing.T) {
	files := map[string]string{
		"app/use.ts": `import { translate, dup, own, deep, hidden, shade } from '../lib'
export function run() { translate(); dup(); own(); deep(); hidden(); shade() }`,
	}
	for k, v := range starBarrelFiles {
		files[k] = v
	}
	ff := extractAll(t, files, false)
	run, ok := findFact(ff, "app.run")
	if !ok {
		t.Fatal("app.run not extracted")
	}
	for _, want := range []string{"lib/a.translate", "lib/d.deep", "lib.own", "lib.dup", "lib.hidden", "lib.shade"} {
		if !hasRelation(run, facts.RelCalls, want) {
			t.Errorf("missing call to %s: %v", want, run.Relations)
		}
	}
	for _, wrong := range []string{"lib/a.own", "lib/a.dup", "lib/b.dup", "lib/c.hidden", "lib.translate", "lib/a.shade"} {
		if hasRelation(run, facts.RelCalls, wrong) {
			t.Errorf("call bound to %s: %v", wrong, run.Relations)
		}
	}
}

func TestExtractTestRefs_StarBarrelResolvesToTheDeclaringModule(t *testing.T) {
	files := map[string]string{
		"tests/use.ts": `import { translate, dup, own, deep, shade } from '../lib'
translate(); dup(); own(); deep(); shade()`,
	}
	var prod []string
	for k, v := range starBarrelFiles {
		files[k] = v
		prod = append(prod, k)
	}
	dir := setupTSProject(t, files, false)
	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"tests/use.ts"}, prod)
	if err != nil {
		t.Fatal(err)
	}
	got := testRefTargets(t, ff, "tests/use.ts")
	for _, want := range []string{"lib/a.translate", "lib/d.deep", "lib.own", "lib.dup", "lib.shade"} {
		if !got[want] {
			t.Errorf("missing target %s; got %v", want, got)
		}
	}
	for _, wrong := range []string{"lib/a.own", "lib/a.dup", "lib/b.dup", "lib.translate", "lib/a.shade"} {
		if got[wrong] {
			t.Errorf("target %s; got %v", wrong, got)
		}
	}
}
