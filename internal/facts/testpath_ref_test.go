package facts

import (
	"path/filepath"
	"strings"
	"testing"
)

// refIsTestPath is IsTestPath as it was before it stopped splitting the path, kept
// to pin that the rewrite answers exactly as the original did.
func refIsTestPath(p string) bool {
	if p == "" {
		return false
	}
	p = filepath.ToSlash(p)
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if testSegments[seg] || IsTestSourceSet(segs, i) || IsTestSupportModule(seg) {
			return true
		}
		for _, suf := range testProjectSuffixes {
			if strings.HasSuffix(seg, suf) && len(seg) > len(suf) {
				return true
			}
		}
	}
	base := p
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, suf := range testFileSuffixes {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return base == "conftest.py" || testRunnerConfigs[base]
}

// Every path built from three segments drawn from a pool that hits each rule, plus
// the edge shapes (leading, trailing and doubled slashes, a source set first or
// last), must classify as the original did.
func TestIsTestPathMatchesReference(t *testing.T) {
	pool := []string{
		"", "src", "app", "test", "Tests", "jsTest", "androidHostTest", "Test", "xTest1",
		"core-test-utils", "ab-testing", "shop-e2e", "-e2e", "contest", "lib",
		"foo_test.go", "user_spec.rb", "Button.cy.tsx", "conftest.py", "cypress.config.ts",
		"main.go", "test_db.py", "a.e2e-spec.ts", "__tests__", "benches",
	}
	n := 0
	for _, a := range pool {
		for _, b := range pool {
			for _, c := range pool {
				for _, p := range []string{a + "/" + b + "/" + c, a + "/" + b, "/" + a + "/" + c, a + "//" + b, b + "/", a} {
					n++
					if got, want := IsTestPath(p), refIsTestPath(p); got != want {
						t.Errorf("IsTestPath(%q) = %v, reference %v", p, got, want)
					}
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no paths checked")
	}
}

func TestIsTestPathAllocatesNothing(t *testing.T) {
	paths := []string{"src/main/kotlin/okhttp3/Foo.kt", "shared/src/commonTest/kotlin/Foo.kt", "web/app/components/button.tsx"}
	if n := testing.AllocsPerRun(100, func() {
		for _, p := range paths {
			IsTestPath(p)
		}
	}); n != 0 {
		t.Errorf("IsTestPath allocates %v times per run, want 0", n)
	}
}

func BenchmarkIsTestPath(b *testing.B) {
	paths := []string{
		"src/coreclr/vm/jitinterface.cpp",
		"packages/core/src/render3/instructions/shared.ts",
		"app/models/concerns/user_helpers.rb",
		"okhttp-testing-support/src/main/kotlin/okhttp3/Foo.kt",
	}
	for b.Loop() {
		for _, p := range paths {
			IsTestPath(p)
		}
	}
}
