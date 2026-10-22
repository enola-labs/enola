package tsextractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// An import of `./Footer` where both Footer.tsx and a Footer/ folder exist names
// the file: TypeScript tries the extensions before the folder's index. As a bare
// stem the target equalled the folder's module name, so the module graph drew an
// edge to the folder, and a folder nothing imports joined a cycle. A stem with no
// folder of its name keeps its usual spelling.
func TestImportTargetsTheFileBesideASameNamedFolder(t *testing.T) {
	dir := t.TempDir()
	for path, body := range map[string]string{
		"src/components/AppLayout.tsx":                  "import Footer from './Footer';\nimport Button from './Button';\nexport default function AppLayout() { return Footer() + Button(); }\n",
		"src/components/Footer.tsx":                     "export default function Footer() { return ''; }\n",
		"src/components/Footer/index.ts":                "export { default as AuthenticatedFooter } from './AuthenticatedFooter';\n",
		"src/components/Footer/AuthenticatedFooter.tsx": "export default function AuthenticatedFooter() { return ''; }\n",
		"src/components/Button.tsx":                     "export default function Button() { return ''; }\n",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	targets := map[string]bool{}
	for _, f := range extractDir(t, dir) {
		if f.Kind == facts.KindDependency && f.File == "src/components/AppLayout.tsx" {
			for _, r := range f.Relations {
				targets[r.Target] = true
			}
		}
	}
	for _, want := range []string{"src/components/Footer.tsx", "src/components/Button"} {
		if !targets[want] {
			t.Errorf("AppLayout's import targets = %v, want %q among them", targets, want)
		}
	}
	if targets["src/components/Footer"] {
		t.Errorf("the import still names the Footer folder: %v", targets)
	}
}
