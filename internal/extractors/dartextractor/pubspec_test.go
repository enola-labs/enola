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
