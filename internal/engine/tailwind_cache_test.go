package engine_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/pkg/bootstrap"
)

type themeToken struct {
	line     int
	value    string
	bodyHash string
}

// tailwindSnapshot snapshots root with the built-in defaults, which ignore vendor/,
// and returns the Tailwind tokens by name.
func tailwindSnapshot(t *testing.T, root string) map[string]themeToken {
	t.Helper()
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{})
	if err != nil {
		t.Fatalf("bootstrap.NewEngine: %v", err)
	}
	if _, err := eng.GenerateSnapshot(context.Background(), root, false); err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	var buf bytes.Buffer
	if err := eng.Store().WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	out := map[string]themeToken{}
	sc := bufio.NewScanner(&buf)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var f struct {
			Name  string         `json:"name"`
			Line  int            `json:"line"`
			Props map[string]any `json:"props"`
		}
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatal(err)
		}
		if f.Props["token_source"] != "tailwind" {
			continue
		}
		value, _ := f.Props["value"].(string)
		hash, _ := f.Props["body_hash"].(string)
		out[f.Name] = themeToken{line: f.Line, value: value, bodyHash: hash}
	}
	return out
}

// A Tailwind theme imported from an ignored folder is read outside the walk. A warm
// snapshot must still see an edit to it, and its deletion, the way a cold one does.
func TestTailwindOffWalkThemeTracksEditsAcrossWarmRuns(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"package.json":       `{"name": "tw", "dependencies": {"tailwindcss": "^3.4.0"}}`,
		"tailwind.config.ts": "import { theme } from \"./vendor/theme\";\n\nexport default {\n  content: [\"./src/**/*.tsx\"],\n  theme: { extend: theme.extend },\n};\n",
		"vendor/theme.ts":    "export const theme = {\n  extend: {\n    colors: {\n      // palette\n      paper: \"#fff\",\n    },\n  },\n};\n",
		"src/App.tsx":        "export function App() {\n  return <div className=\"bg-paper\">hi</div>;\n}\n",
	})
	const paper = "tailwind/colors.paper"

	cold := tailwindSnapshot(t, root)
	if got := cold[paper]; got.line != 5 || got.value != "#fff" {
		t.Fatalf("cold: paper = %+v, want line 5 value #fff", got)
	}
	if warm := tailwindSnapshot(t, root); warm[paper] != cold[paper] {
		t.Fatalf("warm, unchanged: paper = %+v, want %+v", warm[paper], cold[paper])
	}

	writeFiles(t, root, map[string]string{
		"vendor/theme.ts": "export const theme = {\n  extend: {\n    colors: {\n      // palette\n      // edited\n      paper: \"#123\",\n    },\n  },\n};\n",
	})
	edited := tailwindSnapshot(t, root)
	if got := edited[paper]; got.line != 6 || got.value != "#123" {
		t.Errorf("warm, theme edited: paper = %+v, want line 6 value #123", got)
	}
	if err := os.RemoveAll(filepath.Join(root, ".enola")); err != nil {
		t.Fatal(err)
	}
	if recold := tailwindSnapshot(t, root); recold[paper] != edited[paper] {
		t.Errorf("warm, theme edited: paper = %+v, a cold run reads %+v", edited[paper], recold[paper])
	}

	if err := os.Remove(filepath.Join(root, "vendor", "theme.ts")); err != nil {
		t.Fatal(err)
	}
	if got, ok := tailwindSnapshot(t, root)[paper]; ok {
		t.Errorf("warm, theme deleted: paper still present: %+v", got)
	}
}
