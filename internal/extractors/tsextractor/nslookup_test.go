package tsextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const sortModule = `export const byName = () => 0
export const byTitle = () => 0
export function byTime() {}
export function byOrder() {}
export function byIndex() {}
export function bySize() {}
function helper() {}
`

// TestNamespaceLookup_StaticKeyIsAUse pins that `ns[key]` on a namespace import is a
// use of the export the key names when the key is known where it is written: a
// string literal, a const bound to one, a parameter typed as a union of string
// literals (inline or through a type alias), and `keyof typeof ns`, which spans
// every export.
func TestNamespaceLookup_StaticKeyIsAUse(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"util/sort.ts": sortModule,
		"app/lit.ts": `import * as s from '../util/sort'
export const a = s['byName']`,
		"app/cst.ts": `import * as s from '../util/sort'
const K = 'byTitle'
export const b = s[K]`,
		"app/union.ts": `import * as s from '../util/sort'
type Key = 'byTime' | 'byOrder'
export function pick(k: Key) { return s[k] }
export function pick2(k: 'byIndex') { return s[k] }`,
	}, false)
	for file, want := range map[string][]string{
		"app/lit.ts":   {"util.byName"},
		"app/cst.ts":   {"util.byTitle"},
		"app/union.ts": {"util.byTime", "util.byOrder", "util.byIndex"},
	} {
		ref, ok := findFact(ff, file)
		if !ok {
			t.Fatalf("%s: no file_ref", file)
		}
		for _, w := range want {
			if !hasRelation(ref, facts.RelCalls, w) {
				t.Errorf("%s: missing use of %s: %v", file, w, ref.Relations)
			}
		}
		if hasRelation(ref, facts.RelCalls, "util.bySize") {
			t.Errorf("%s: a key the code cannot take credited bySize", file)
		}
	}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Props["reached_dynamically"] != nil {
			t.Errorf("%s marked reached_dynamically by a static key", f.Name)
		}
	}

	ff = extractAll(t, map[string]string{
		"util/sort.ts": sortModule,
		"app/all.ts": `import * as s from '../util/sort'
export function any(k: keyof typeof s) { return s[k] }`,
	}, false)
	ref, _ := findFact(ff, "app/all.ts")
	for _, w := range []string{"util.byName", "util.byTitle", "util.byTime", "util.byOrder", "util.byIndex", "util.bySize"} {
		if !hasRelation(ref, facts.RelCalls, w) {
			t.Errorf("keyof typeof: missing use of %s: %v", w, ref.Relations)
		}
	}
	if hasRelation(ref, facts.RelCalls, "util.helper") {
		t.Error("keyof typeof credited an unexported declaration")
	}
}

// TestNamespaceLookup_DynamicKeyMarksExportsWithoutEdges pins that `ns[expr]` with a
// key the code does not pin down draws no edge, and marks every export of the module
// as reached_dynamically, naming the file of the lookup. An unexported declaration
// is not marked: the namespace object cannot reach it.
func TestNamespaceLookup_DynamicKeyMarksExportsWithoutEdges(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"util/sort.ts": sortModule,
		"app/store.js": `import * as s from '../util/sort'
export function sorter(state, k) { return s[state.order] || s[k] }`,
		"app/other.js": `import * as s from '../util/sort'
export const x = (k) => s[k]`,
	}, false)
	for _, f := range ff {
		for _, r := range f.Relations {
			if (r.Kind == facts.RelCalls || r.Kind == facts.RelNames) && slices.Contains([]string{"util.byName", "util.byTitle", "util.byTime", "util.byOrder", "util.byIndex", "util.bySize"}, r.Target) {
				t.Errorf("%s draws %s %s from a dynamic key", f.Name, r.Kind, r.Target)
			}
		}
	}
	for _, name := range []string{"util.byName", "util.byTitle", "util.byTime", "util.byOrder", "util.byIndex", "util.bySize"} {
		sym, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("%s not extracted", name)
		}
		if sym.Props["reached_dynamically"] != true {
			t.Errorf("%s: reached_dynamically = %v", name, sym.Props["reached_dynamically"])
		}
		from, _ := sym.Props["reached_dynamically_from"].([]string)
		if !slices.Equal(from, []string{"app/other.js", "app/store.js"}) {
			t.Errorf("%s: reached_dynamically_from = %v", name, sym.Props["reached_dynamically_from"])
		}
	}
	helper, _ := findFact(ff, "util.helper")
	if helper.Props["reached_dynamically"] != nil {
		t.Error("an unexported declaration was marked reached_dynamically")
	}
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && (f.Props[propNamespaceDynamic] != nil || f.Props[propNamespaceAll] != nil) {
			t.Errorf("%s keeps a transient prop: %v", f.Name, f.Props)
		}
	}
}
