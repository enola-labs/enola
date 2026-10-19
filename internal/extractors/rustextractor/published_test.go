package rustextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A library crate is published when its manifest carries what crates.io requires
// (inherited from the workspace counts) and does not opt out. A crate without
// that metadata, one with publish = false, and a binary-only crate are not.
func TestExtract_PublishedLibraryCrates(t *testing.T) {
	repo, files := writeRustRepo(t, map[string]string{
		"Cargo.toml":             "[workspace]\nmembers = [\"crates/*\"]\n",
		"crates/sdk/Cargo.toml":  "[package]\nname = \"sdk\"\ndescription = \"An SDK\"\nlicense = \"MIT\"\n",
		"crates/sdk/src/lib.rs":  "pub fn connect() {}\n",
		"crates/inh/Cargo.toml":  "[package]\nname = \"inh\"\ndescription.workspace = true\nlicense.workspace = true\n",
		"crates/inh/src/lib.rs":  "pub fn inherit() {}\n",
		"crates/core/Cargo.toml": "[package]\nname = \"core\"\n",
		"crates/core/src/lib.rs": "pub fn internal() {}\n",
		"crates/priv/Cargo.toml": "[package]\nname = \"priv\"\ndescription = \"x\"\nlicense = \"MIT\"\npublish = false\n",
		"crates/priv/src/lib.rs": "pub fn hidden() {}\n",
		"crates/cli/Cargo.toml":  "[package]\nname = \"cli\"\ndescription = \"x\"\nlicense = \"MIT\"\n",
		"crates/cli/src/main.rs": "fn main() {}\n",
	})
	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"crates/sdk/src": true, "crates/inh/src": true,
		"crates/core/src": false, "crates/priv/src": false, "crates/cli/src": false,
	}
	for _, f := range ff {
		if f.Kind != facts.KindModule {
			continue
		}
		if w, ok := want[f.Name]; ok {
			if got := f.PropBool(facts.PropPublished); got != w {
				t.Errorf("%s: published = %v, want %v", f.Name, got, w)
			}
			delete(want, f.Name)
		}
	}
	for name := range want {
		t.Errorf("no module %s", name)
	}
}
