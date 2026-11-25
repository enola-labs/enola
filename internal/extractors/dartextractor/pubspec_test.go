package dartextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// writeRepo lays out a throwaway Dart workspace and returns its root.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestPubspecScanBypassesIgnoreGlobs is the guard on a defect that was invisible from
// inside the extractor and wrong in three places at once.
//
// `**/*.yaml` is in the default ignore globs, so a pubspec.yaml NEVER appears in the
// file list the engine hands an extractor — measured on appflowy, 0 of 4,114 walked
// files. An extractor that reads its package index from that list therefore always
// builds an EMPTY one, and nothing fails: modules simply carry no pub_package (so the
// cycles explainer calls a legal Dart cycle a build defect), the repo's own `package:`
// imports classify as external instead of internal, and the manifest never says
// Flutter. So the pubspecs are read from disk, exactly as the OpenAPI extractor and
// PHP's Symfony route config already do.
//
// The test passes an EMPTY file list for the pubspec deliberately: that is what the
// engine really passes, and a test that fed it in would assert nothing.
func TestPubspecScanBypassesIgnoreGlobs(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"pubspec.yaml":                 "name: app\ndependencies:\n  flutter:\n    sdk: flutter\n",
		"packages/ui/pubspec.yaml":     "name: app_ui\n",
		"lib/main.dart":                "import 'package:app_ui/widgets.dart';\nvoid main() {}\n",
		"packages/ui/lib/widgets.dart": "class Button {}\n",
	})

	found := scanPubspecs(root)
	if len(found) != 2 {
		t.Fatalf("scanPubspecs found %d pubspecs, want 2: %v", len(found), found)
	}

	// The file list carries only .dart, as the engine's really does.
	got, err := New().Extract(context.Background(), root,
		[]string{"lib/main.dart", "packages/ui/lib/widgets.dart"})
	if err != nil {
		t.Fatal(err)
	}

	pkgOf := map[string]string{}
	for _, f := range got {
		if f.Kind == facts.KindModule {
			pkgOf[f.Name] = f.PropString("pub_package")
		}
	}
	if pkgOf["lib"] != "app" {
		t.Errorf("lib should compile into %q, got %q", "app", pkgOf["lib"])
	}
	if pkgOf["packages/ui/lib"] != "app_ui" {
		t.Errorf("packages/ui/lib should compile into %q, got %q", "app_ui", pkgOf["packages/ui/lib"])
	}

	// The workspace's own package resolves to a module, not to a third-party name.
	// This is the second thing the empty index broke: every internal import looked
	// external, so the module graph had no edge where the code plainly has one.
	var internal, external int
	for _, f := range got {
		if f.Kind != facts.KindDependency {
			continue
		}
		switch f.PropString(facts.PropSource) {
		case facts.DepSourceInternal:
			internal++
			// Named "<importer> -> <imported>", the shared convention the
			// package-metrics explainer splits on to recover the importing side.
			if f.Name != "lib -> packages/ui/lib" {
				t.Errorf("internal dependency named %q, want %q", f.Name, "lib -> packages/ui/lib")
			}
		case facts.DepSourceExternal:
			external++
		}
	}
	if internal != 1 {
		t.Errorf("expected the package: import of a workspace package to be internal, got %d internal / %d external",
			internal, external)
	}
}

// TestModulesCarryPubPackage pins the prop the cycles explainer reads.
//
// It is registered in facts.CompilationUnitProps, and Dart is the most permissive entry
// in that table: C# and Rust forbid cycles BETWEEN compilation units, while Dart does
// not even forbid them between libraries inside one — circular imports are legal,
// compile, and are ordinary. Without this prop every Dart cycle is reported as
// something that "can cause initialization issues", which for Dart is untrue.
func TestModulesCarryPubPackage(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"pubspec.yaml": "name: app\n",
		"lib/a/a.dart": "import '../b/b.dart';\nclass A {}\n",
		"lib/b/b.dart": "import '../a/a.dart';\nclass B {}\n",
	})
	got, err := New().Extract(context.Background(), root, []string{"lib/a/a.dart", "lib/b/b.dart"})
	if err != nil {
		t.Fatal(err)
	}
	var modules int
	for _, f := range got {
		if f.Kind != facts.KindModule {
			continue
		}
		modules++
		if u := facts.CompilationUnit(f); u != "app" {
			t.Errorf("module %q: CompilationUnit = %q, want %q", f.Name, u, "app")
		}
	}
	if modules == 0 {
		t.Fatal("no module facts emitted")
	}
}

// TestPublishedPackageLibIsPublished pins which modules carry facts.PropPublished.
// The orphans analyzer reads it to keep a library's exported API, whose callers
// are the projects that install it, out of the test-only dead-code list. Only a
// package pub would publish counts (a version, a license file, no `publish_to:
// none`), and only its lib/, the one directory a `package:` URI reaches. An
// application must stay unpublished, or its real dead code would be hidden.
func TestPublishedPackageLibIsPublished(t *testing.T) {
	root := writeRepo(t, map[string]string{
		// A workspace root that opts out, around packages that do not.
		"pubspec.yaml":                      "name: ws\npublish_to: none\n",
		"lib/ws.dart":                       "class Ws {}\n",
		"packages/core/pubspec.yaml":        "name: core\nversion: 1.2.0\n",
		"packages/core/LICENSE":             "MIT\n",
		"packages/core/lib/core.dart":       "export 'src/impl.dart';\n",
		"packages/core/lib/src/impl.dart":   "void sqlCos() {}\n",
		"packages/core/test/core_test.dart": "void main() {}\n",
		"packages/core/bin/tool.dart":       "void main() {}\n",
		// A private repository is still a repository other projects install from.
		"packages/priv/pubspec.yaml":  "name: priv\nversion: 0.1.0\npublish_to: https://pub.example.com\n",
		"packages/priv/LICENSE.md":    "MIT\n",
		"packages/priv/lib/priv.dart": "class Priv {}\n",
		// `flutter create`'s app: versioned, but opted out.
		"apps/made/pubspec.yaml":  "name: made\nversion: 1.0.0+1\npublish_to: 'none'\n",
		"apps/made/LICENSE":       "MIT\n",
		"apps/made/lib/main.dart": "void main() {}\n",
		// A hand-written app: versioned, no opt-out, but no license pub would demand.
		"apps/hand/pubspec.yaml":  "name: hand\nversion: 1.0.0\n",
		"apps/hand/lib/main.dart": "void main() {}\n",
		// A package with no version cannot be published.
		"packages/nover/pubspec.yaml": "name: nover\n",
		"packages/nover/LICENSE":      "MIT\n",
		"packages/nover/lib/n.dart":   "class N {}\n",
	})
	files := []string{
		"lib/ws.dart",
		"packages/core/lib/core.dart", "packages/core/lib/src/impl.dart",
		"packages/core/test/core_test.dart", "packages/core/bin/tool.dart",
		"packages/priv/lib/priv.dart",
		"apps/made/lib/main.dart", "apps/hand/lib/main.dart",
		"packages/nover/lib/n.dart",
	}
	got, err := New().Extract(context.Background(), root, files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"lib":                   false,
		"packages/core/lib":     true,
		"packages/core/lib/src": true,
		"packages/core/test":    false,
		"packages/core/bin":     false,
		"packages/priv/lib":     true,
		"apps/made/lib":         false,
		"apps/hand/lib":         false,
		"packages/nover/lib":    false,
	}
	seen := map[string]bool{}
	for _, f := range got {
		if f.Kind != facts.KindModule {
			continue
		}
		w, ok := want[f.Name]
		if !ok {
			continue
		}
		seen[f.Name] = true
		if g := f.PropBool(facts.PropPublished); g != w {
			t.Errorf("module %q: published = %v, want %v", f.Name, g, w)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("no module fact for %q", name)
		}
	}
}
