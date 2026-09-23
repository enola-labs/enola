package config

import (
	"testing"

	"github.com/enola-labs/enola/internal/pathglob"
)

// TestDefaultIgnoresNestedBuildAndPods locks in the ignore-pattern fix: build
// output must be ignored at ANY depth (Gradle/Android emit data/build/...), and
// CocoaPods must be excluded. The bare "build/**" (top-level only) must be gone.
func TestDefaultIgnoresNestedBuildAndPods(t *testing.T) {
	has := func(want string) bool {
		for _, p := range Default().Ignore {
			if p == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"**/build/**", "**/Pods/**"} {
		if !has(want) {
			t.Errorf("Default().Ignore missing %q", want)
		}
	}
	if has("build/**") {
		t.Error("Default().Ignore still has top-level-only \"build/**\"; want \"**/build/**\"")
	}
}

// TestDefaultIgnoresPythonEnvs locks in the .venv fix: Python virtual
// environments and installed dependencies must be ignored at any depth so a
// repo-local .venv (whole dependency tree) is never indexed.
func TestDefaultIgnoresPythonEnvs(t *testing.T) {
	has := func(want string) bool {
		for _, p := range Default().Ignore {
			if p == want {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"**/.venv/**", "**/venv/**", "**/site-packages/**"} {
		if !has(want) {
			t.Errorf("Default().Ignore missing %q", want)
		}
	}
}

// TestDefaultTestGlobsCoverGoAndStayIgnored pins both halves of the test-ref
// contract for Go (GAP-GO-06, v100).
//
// A test file's references survive only if it is BOTH ignored for normal indexing
// AND matched by a TestGlob — engine.walkRepo collects an ignored file for
// reference-only extraction only when matchesTestGlob says so. Adding a glob to
// one list and not the other silently drops the file (ignored, never recovered)
// or indexes test symbols as production code. config.go states the invariant in a
// comment; this asserts it.
//
// Go's bare-suffix form is correct and must stay: unlike Ruby — where
// "**/*_test.rb" swallowed a production job named ..._ab_test.rb and had to become
// directory-scoped (v97) — the Go toolchain DEFINES any *_test.go as a test file,
// so no production file can collide.
func TestDefaultTestGlobsCoverGoAndStayIgnored(t *testing.T) {
	cfg := Default()

	if !contains(cfg.TestGlobs, "**/*_test.go") {
		t.Errorf("Default().TestGlobs missing %q — Go test files are ignored but never recovered", "**/*_test.go")
	}

	for _, g := range cfg.TestGlobs {
		if !contains(cfg.Ignore, g) {
			t.Errorf("TestGlob %q is not in Default().Ignore; a test glob that is not ignored indexes test symbols as production code", g)
		}
	}
}

// TestDefaultTestGlobsCoverTypeScriptAndStayIgnored pins both halves of the
// test-ref contract for TypeScript (GAP-XL-02 TS half, v103): the four
// *.test.ts(x)/*.spec.ts(x) globs must be in TestGlobs (so an ignored test file's
// references are recovered) AND stay in Ignore (so test symbols are never indexed
// as production code). Adding to one list and not the other silently drops the file
// or pollutes the production graph.
//
// Like Go's *_test.go, the dotted suffixes are unambiguous test markers, so no
// production file can collide — no directory scoping needed (contrast Ruby, v97).
func TestDefaultTestGlobsCoverTypeScriptAndStayIgnored(t *testing.T) {
	cfg := Default()

	for _, g := range []string{"**/*.test.ts", "**/*.test.tsx", "**/*.spec.ts", "**/*.spec.tsx"} {
		if !contains(cfg.TestGlobs, g) {
			t.Errorf("Default().TestGlobs missing %q — TS test files are ignored but never recovered", g)
		}
		if !contains(cfg.Ignore, g) {
			t.Errorf("TestGlob %q is not in Default().Ignore; a test glob that is not ignored indexes test symbols as production code", g)
		}
	}
}

// TestDefaultIgnoresPythonTests pins the Python half of the test-ignore contract.
//
// Python test files are not merely noise. A pytest fixture that assembles a
// throwaway app —
//
//	app.include_router(get_cognify_router(), prefix="/cognify")
//
// — is a route-mount fact, and the repo-wide FastAPI prefix fixpoint (v133) folds
// every mount it can see. Indexing tests therefore lets a test-only prefix REWRITE
// production routes: a real corpus gained six phantom endpoints its service never
// served, one of which then matched a client call and mis-attributed the evidence
// on a cross-repo edge. Test files were also ~35% of that repo's total facts.
//
// The patterns are deliberately asymmetric with Ruby's (v97). conftest.py is
// reserved by pytest outright and test_* is its discovery prefix — a production
// module so named would itself be collected as a test — so both are safe at any
// depth. A bare "**/*_test.py" is NOT included: as a suffix it repeats the Ruby
// hazard of swallowing production code that merely ends in the token.
func TestDefaultIgnoresPythonTests(t *testing.T) {
	cfg := Default()

	for _, want := range []string{"**/conftest.py", "**/test_*.py", "**/tests/**/*.py", "**/test/**/*.py"} {
		if !contains(cfg.Ignore, want) {
			t.Errorf("Default().Ignore missing %q", want)
		}
		if !contains(cfg.TestGlobs, want) {
			t.Errorf("Default().TestGlobs missing %q — the file would be ignored and never recoverable", want)
		}
	}

	// The suffix form must stay out: scoping is what keeps production code safe.
	if contains(cfg.Ignore, "**/*_test.py") {
		t.Error("Default().Ignore has bare \"**/*_test.py\"; a suffix glob swallows production code ending in the token (see Ruby, v97)")
	}
}

// TestPythonTestGlobsMatchRealPaths exercises the patterns against the real paths
// that motivated them, and against production paths that must survive. Presence in
// the slice is not the contract — what the globs actually match is.
func TestPythonTestGlobsMatchRealPaths(t *testing.T) {
	ignore := Default().Ignore

	ignored := []string{
		// The fixture that mounted routers at test-only prefixes.
		"cognee/tests/unit/api/test_api_error_responses.py",
		// Fixtures and helpers in a tests tree carry no test_ prefix.
		"cognee/tests/unit/api/conftest.py",
		"cognee/tests/helpers/factories.py",
		"tests/integration/test_search.py",
		// Singular test/ tree, and a co-located test_ file outside any tests dir.
		"src/pkg/test/test_client.py",
		"cognee/modules/search/test_operations.py",
		"conftest.py",
	}
	for _, p := range ignored {
		if !pathglob.MatchAny(p, ignore) {
			t.Errorf("%q should be ignored but is not", p)
		}
	}

	production := []string{
		// Nothing here is a test: no test_ prefix, no tests/ tree.
		"cognee/api/v1/cognify/routers/get_cognify_router.py",
		"cognee/api/client.py",
		"cognee/modules/pipelines/operations/run_tasks.py",
		// "latest" contains "test" but is not a test directory.
		"cognee/modules/latest/handler.py",
		// A production module ending in the token survives — the Ruby v97 hazard.
		"cognee/modules/ab_test.py",
		// A shipped testing-helper package is not a tests tree.
		"cognee/testing/harness.py",
	}
	for _, p := range production {
		if pathglob.MatchAny(p, ignore) {
			t.Errorf("%q is production code but is ignored", p)
		}
	}
}

func TestDefaultIgnores_NestedHeavyDirsExcluded(t *testing.T) {
	cfg := Default()
	match := func(path string) bool {
		return pathglob.MatchAny(path, cfg.Ignore)
	}
	for _, p := range []string{
		"ember_app/node_modules/lodash/index.js",
		"apps/web/dist/bundle.js",
		"sub/app/tmp/cache/x.rb",
		"public/assets/app-abc123.js",
		"packages/ui/vendor/lib.js",
	} {
		if !match(p) {
			t.Errorf("nested heavy path %q escaped the default ignores — a monorepo sub-app's dependency tree would enter the graph", p)
		}
	}
	if match("app/models/build.rb") || match("app/services/distribution.rb") {
		t.Error("a production file merely containing a heavy-dir token must not be ignored")
	}
}

// TestExtractorAlias pins the compatibility promise made when `csharp` became
// `dotnet`. An `extractors:` list REPLACES the default rather than merging, so a
// config written under the old name would otherwise silently disable .NET
// extraction and report zero facts with no error.
func TestExtractorAlias(t *testing.T) {
	old := &Config{Extractors: []string{"go", "csharp"}}
	if !old.IsExtractorEnabled("dotnet") {
		t.Error(`a config listing "csharp" must still enable the dotnet extractor`)
	}
	current := &Config{Extractors: []string{"go", "dotnet"}}
	if !current.IsExtractorEnabled("dotnet") {
		t.Error(`a config listing "dotnet" must enable it`)
	}
	// The alias is one-way: it does not enable extractors the user excluded.
	narrowed := &Config{Extractors: []string{"go"}}
	if narrowed.IsExtractorEnabled("dotnet") {
		t.Error("an extractors list that omits .NET must keep it disabled")
	}
}
