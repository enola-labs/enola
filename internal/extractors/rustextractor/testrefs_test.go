package rustextractor

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

var _ plugin.TestRefExtractor = (*RustExtractor)(nil)

// TestRustExtractTestRefsEmitsOnlyTestRefFacts pins the reference-only pass over
// ignored Cargo test targets: each integration test, bench and test helper module
// yields one test_ref fact naming the production code it reaches, resolved the way
// the production walk resolves a call (symbols are named by module directory, so
// src/engine.rs declares src.spin), and nothing else: no symbol, no dependency,
// no module.
func TestRustExtractTestRefsEmitsOnlyTestRefFacts(t *testing.T) {
	repo, _ := writeRustRepo(t, map[string]string{
		"Cargo.toml":          "[package]\nname = \"pkg\"\n",
		"src/lib.rs":          "pub mod engine;\npub fn run() {}\n",
		"src/engine.rs":       "pub fn spin() {}\npub struct Engine;\n",
		"tests/it.rs":         "use pkg::engine::spin;\nmod common;\nstruct Fixture;\nfn setup() { pkg::run(); }\n#[test]\nfn t() { setup(); spin(); common::helper(); }\n",
		"tests/common/mod.rs": "pub fn helper() { let _ = pkg::engine::Engine; }\n",
		"benches/spin.rs":     "fn main() { pkg::engine::spin(); }\n",
		"notes.txt":           "not rust\n",
	})
	prod := []string{"Cargo.toml", "src/lib.rs", "src/engine.rs"}
	tests := []string{"tests/it.rs", "tests/common/mod.rs", "benches/spin.rs", "notes.txt"}

	got, err := New().ExtractTestRefs(context.Background(), repo, tests, prod)
	if err != nil {
		t.Fatal(err)
	}
	byFile := map[string]facts.Fact{}
	for _, f := range got {
		if f.Kind != facts.KindTestRef {
			t.Errorf("%s emitted a %s fact %q: the pass is reference-only", f.File, f.Kind, f.Name)
			continue
		}
		if f.Name != f.File {
			t.Errorf("test_ref named %q for file %q, want the file path", f.Name, f.File)
		}
		if f.PropString("language") != "rust" {
			t.Errorf("%s: language = %q, want rust", f.File, f.PropString("language"))
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls {
				t.Errorf("%s: relation kind %q, want only calls", f.File, r.Kind)
			}
		}
		byFile[f.File] = f
	}
	want := map[string][]string{
		"tests/it.rs":         {"src.run", "src.spin"},
		"tests/common/mod.rs": {"Engine"},
		"benches/spin.rs":     {"src.spin"},
	}
	for file, targets := range want {
		f, ok := byFile[file]
		if !ok {
			t.Errorf("%s emitted no test_ref", file)
			continue
		}
		have := map[string]bool{}
		for _, r := range f.Relations {
			have[r.Target] = true
		}
		for _, tg := range targets {
			if !have[tg] {
				t.Errorf("%s: missing %s in %v", file, tg, f.Relations)
			}
		}
	}
	if _, ok := byFile["notes.txt"]; ok {
		t.Error("a non-Rust file produced a test_ref")
	}
}

// TestRustExtractTestRefsSameAsCfgTestModuleWalk keeps the two routes to test code
// in step: a `#[cfg(test)] mod tests;` file under src/ used to be walked in test mode
// by Extract, and is now ignored by the tests/ glob and handed to ExtractTestRefs. Its
// test reference must come out the same either way.
func TestRustExtractTestRefsSameAsCfgTestModuleWalk(t *testing.T) {
	files := map[string]string{
		"Cargo.toml":       "[package]\nname = \"pkg\"\n",
		"src/lib.rs":       "#[cfg(test)]\nmod tests;\npub mod engine;\npub fn run() {}\n",
		"src/engine.rs":    "pub fn spin() {}\n",
		"src/tests/mod.rs": "use super::*;\nuse crate::engine::spin;\n#[test]\nfn t() { run(); spin(); crate::run(); }\n",
	}
	repo, all := writeRustRepo(t, files)
	whole, err := New().Extract(context.Background(), repo, all)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := New().ExtractTestRefs(context.Background(), repo, []string{"src/tests/mod.rs"},
		[]string{"Cargo.toml", "src/lib.rs", "src/engine.rs"})
	if err != nil {
		t.Fatal(err)
	}
	pick := func(ff []facts.Fact) string {
		for _, f := range ff {
			if f.Kind == facts.KindTestRef && f.File == "src/tests/mod.rs" {
				var b strings.Builder
				for _, r := range f.Relations {
					b.WriteString(r.Kind + ">" + r.Target + " ")
				}
				return b.String()
			}
		}
		return "<none>"
	}
	if a, b := pick(whole), pick(refs); a != b || a == "<none>" {
		t.Errorf("test_ref differs:\n Extract:         %s\n ExtractTestRefs: %s", a, b)
	}
}

// TestRustTestCodeImplAndConstBodiesAreTestRefs: test code is more than its test
// functions. A fixture implementing a production trait, its methods' calls into
// production code, and a constant built by a production function are all uses, in
// an inline #[cfg(test)] module and in an ignored integration test alike. They used
// to be skipped, so production code reached only from them read as dead. The trait
// is named as a type reference is, by its last segment, which the orphan detector
// matches.
func TestRustTestCodeImplAndConstBodiesAreTestRefs(t *testing.T) {
	fixture := "struct Fake;\n" +
		"impl crate::Codec for Fake {\n    fn encode(&self) { crate::engine::spin(); self.own(); }\n}\n" +
		"impl Fake {\n    fn own(&self) {}\n}\n" +
		"const SEED: u8 = crate::make_seed();\n" +
		"static NAME: &str = crate::seed_name();\n"
	files := map[string]string{
		"Cargo.toml":    "[package]\nname = \"pkg\"\n",
		"src/lib.rs":    "pub mod engine;\npub trait Codec { fn encode(&self); }\npub fn make_seed() -> u8 { 0 }\npub fn seed_name() -> &'static str { \"\" }\n#[cfg(test)]\nmod tests {\n" + fixture + "}\n",
		"src/engine.rs": "pub fn spin() {}\n",
		"tests/it.rs":   strings.ReplaceAll(fixture, "crate::", "pkg::"),
	}
	repo, all := writeRustRepo(t, files)
	var prod []string
	for _, f := range all {
		if f != "tests/it.rs" {
			prod = append(prod, f)
		}
	}
	whole, err := New().Extract(context.Background(), repo, prod)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := New().ExtractTestRefs(context.Background(), repo, []string{"tests/it.rs"}, prod)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		file string
		ff   []facts.Fact
	}{{"src/lib.rs", whole}, {"tests/it.rs", refs}} {
		have := map[string]bool{}
		for _, f := range tc.ff {
			if f.Kind == facts.KindTestRef && f.File == tc.file {
				for _, r := range f.Relations {
					have[r.Target] = true
				}
			}
			if f.Kind == facts.KindSymbol && f.File == tc.file && strings.Contains(f.Name, "Fake") {
				t.Errorf("%s: test fixture emitted symbol %s", tc.file, f.Name)
			}
		}
		for _, want := range []string{"Codec", "src.spin", "src.make_seed", "src.seed_name"} {
			if !have[want] {
				t.Errorf("%s: test_ref lacks %s; has %v", tc.file, want, sortedTargets(have))
			}
		}
		for tg := range have {
			if strings.HasSuffix(tg, ".encode") || tg == "encode" {
				t.Errorf("%s: the fixture's own method %s vouches for the production trait method", tc.file, tg)
			}
		}
	}
}

func sortedTargets(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
