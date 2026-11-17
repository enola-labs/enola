package tsextractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// k6 runs a script's default export, its lifecycle exports and the functions its
// scenarios name; a helper the script calls itself is an ordinary function, and
// the same names outside a k6 script root nothing.
func TestK6EntrypointsAreRoots(t *testing.T) {
	dir := t.TempDir()
	for path, body := range map[string]string{
		"load/summarise.js": `import http from 'k6/http'
import { check } from 'k6'

export const options = { scenarios: { browse: { executor: 'constant-vus', exec: 'browse' } } }

function url(p) { return 'https://example.test' + p }

export function browse() { http.get(url('/items')) }

export function handleSummary(data) { return { stdout: '' } }

export default function () {
  check(http.get(url('/')), { ok: (r) => r.status === 200 })
}
`,
		"src/report.js": `export function handleSummary(data) { return data }
`,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	roots := map[string]string{}
	for _, f := range extractDir(t, dir) {
		if f.Kind == facts.KindSymbol {
			roots[f.File+" "+lastDot(f.Name)] = f.PropString(facts.PropFrameworkRoot)
		}
	}
	for key, want := range map[string]string{
		"load/summarise.js browse":        "k6",
		"load/summarise.js handleSummary": "k6",
		"load/summarise.js Summarise":     "k6",
		"load/summarise.js url":           "",
		"src/report.js handleSummary":     "",
	} {
		if got, ok := roots[key]; !ok || got != want {
			t.Errorf("%s root = %q (found %v), want %q; symbols %v", key, got, ok, want, roots)
		}
	}
}
