package rustextractor

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A file a parent declares as `#[cfg(test)] mod name;` is test code: it emits no
// symbol and its calls become a test reference, and so do the files it declares
// in turn. A `mod name;` nested in an inline test module names a file in a
// directory of that module's name and leaves a same-named sibling alone.
func TestExtract_OutOfLineCfgTestModuleIsTestCode(t *testing.T) {
	repo, files := writeRustRepo(t, map[string]string{
		"Cargo.toml":           "[package]\nname = \"pkg\"\n",
		"src/lib.rs":           "#[cfg(test)]\nmod tests;\nmod engine;\nmod other;\npub fn run() {}\n",
		"src/tests.rs":         "fn new_handle() -> u8 { 0 }\nmod helpers;\n#[test]\nfn t() { crate::run(); new_handle(); }\n",
		"src/tests/helpers.rs": "pub fn helper() {}\n",
		"src/engine.rs":        "#[cfg(all(test, feature = \"x\"))]\nmod mocks;\npub fn spin() {}\n",
		"src/engine/mocks.rs":  "pub fn run_one() {}\n",
		"src/other.rs":         "#[cfg(test)]\nmod t {\n    mod deep;\n}\npub fn other() {}\n",
		"src/other/deep.rs":    "pub fn deep() {}\n",
	})
	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}
	symbolFiles := map[string]bool{}
	var testRef *facts.Fact
	for i, f := range ff {
		switch f.Kind {
		case facts.KindSymbol:
			symbolFiles[f.File] = true
		case facts.KindTestRef:
			if f.File == "src/tests.rs" {
				testRef = &ff[i]
			}
		}
	}
	for _, f := range []string{"src/tests.rs", "src/tests/helpers.rs", "src/engine/mocks.rs"} {
		if symbolFiles[f] {
			t.Errorf("%s is test code but emitted symbols", f)
		}
	}
	for _, f := range []string{"src/lib.rs", "src/engine.rs", "src/other/deep.rs"} {
		if !symbolFiles[f] {
			t.Errorf("%s is production code but emitted no symbol", f)
		}
	}
	if testRef == nil {
		t.Fatal("src/tests.rs emitted no test reference")
	}
	called := false
	for _, r := range testRef.Relations {
		if r.Target == "src.run" {
			called = true
		}
	}
	if !called {
		t.Errorf("test reference from src/tests.rs does not name src.run: %+v", testRef.Relations)
	}
}

// A cfg that excludes the test build, or also builds without it, is not test code:
// `not(all(test, loom))` is tokio's production loom shim, and `any(feature = "x",
// test)` is a module a feature turns on.
func TestCfgThatDoesNotRequireTestIsProduction(t *testing.T) {
	repo, files := writeRustRepo(t, map[string]string{
		"Cargo.toml":     "[package]\nname = \"pkg\"\n",
		"src/lib.rs":     "#[cfg(not(all(test, loom)))]\nmod shim;\n#[cfg(any(feature = \"builder\", test))]\nmod builder;\n#[cfg(all(test, not(miri)))]\nmod tests;\n#[cfg(not(test))]\nmod real {\n    pub fn real_fn() {}\n}\n",
		"src/shim.rs":    "pub fn atomic() {}\n",
		"src/builder.rs": "pub fn build() {}\n",
		"src/tests.rs":   "fn helper() {}\n",
	})
	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}
	symbolFiles := map[string]bool{}
	names := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			symbolFiles[f.File] = true
			names[f.Name] = true
		}
	}
	for _, f := range []string{"src/shim.rs", "src/builder.rs"} {
		if !symbolFiles[f] {
			t.Errorf("%s is production code but emitted no symbol", f)
		}
	}
	if symbolFiles["src/tests.rs"] {
		t.Errorf("src/tests.rs requires a test build but emitted symbols")
	}
	found := false
	for n := range names {
		if strings.HasSuffix(n, "real_fn") {
			found = true
		}
	}
	if !found {
		t.Errorf("an inline #[cfg(not(test))] module is production code; got %v", names)
	}
}
