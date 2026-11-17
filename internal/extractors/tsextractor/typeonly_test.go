package tsextractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// An import TypeScript erases names only types: `import type`, a clause whose
// every specifier is `type`, and the same forms re-exported. It couples the
// files for the compiler and runs nothing, so it is marked type-only. An
// import with one value in it, a default or a namespace import, or a bare
// side-effect import is a runtime edge.
func TestTypeOnlyImportsAreMarked(t *testing.T) {
	dir := t.TempDir()
	src := `import type { Order } from './a';
import { type Line, type Total } from './b';
import { type Cart, checkout } from './c';
import Default from './d';
import * as ns from './e';
import './f';
export type { Price } from './g';
export { type Tax } from './h';
export { round } from './i';
import type Whole from './j';
`
	files := map[string]string{"src/x.ts": src}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		files["src/"+n+".ts"] = "export const v = 1;\n"
	}
	for path, body := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	typeOnly := map[string]bool{}
	for _, f := range extractDir(t, dir) {
		if f.Kind != facts.KindDependency || f.File != "src/x.ts" {
			continue
		}
		for _, r := range f.Relations {
			typeOnly[filepath.Base(r.Target)] = f.PropString(facts.PropCouplingKind) == facts.CouplingTypeOnly
		}
	}
	want := map[string]bool{
		"a": true, "b": true, "c": false, "d": false, "e": false,
		"f": false, "g": true, "h": true, "i": false, "j": true,
	}
	for name, w := range want {
		got, ok := typeOnly[name]
		if !ok {
			t.Errorf("no dependency on ./%s among %v", name, typeOnly)
			continue
		}
		if got != w {
			t.Errorf("./%s type-only = %v, want %v", name, got, w)
		}
	}
}
