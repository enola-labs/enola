package kotlinextractor

import (
	"os"
	"path/filepath"
	"testing"
)

// A multi-module Android build has no app/ module: its manifest sits in a module
// named anything, or a feature module two levels down.
func TestDetectAndroidProject_AnyModuleManifest(t *testing.T) {
	for _, manifest := range []string{
		"app/src/main/AndroidManifest.xml",
		"application/src/main/AndroidManifest.xml",
		"app-features/cart/impl/src/main/AndroidManifest.xml",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, filepath.FromSlash(manifest))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("<manifest/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !detectAndroidProject(dir) {
			t.Errorf("%s: not detected as Android", manifest)
		}
	}
	if detectAndroidProject(t.TempDir()) {
		t.Error("a repository without a manifest was detected as Android")
	}
	for _, manifest := range []string{
		"examples/product-sample-kt/app/src/main/AndroidManifest.xml",
		"samples/app/src/main/AndroidManifest.xml",
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, filepath.FromSlash(manifest))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("<manifest/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if detectAndroidProject(dir) {
			t.Errorf("%s: a sample app made the repository Android", manifest)
		}
	}
}
