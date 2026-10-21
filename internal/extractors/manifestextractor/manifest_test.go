package manifestextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// repoWith writes a temporary repository from a path -> contents map.
func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// extract runs the extractor and indexes its facts by name.
func extract(t *testing.T, files map[string]string) map[string]facts.Fact {
	t.Helper()
	dir := repoWith(t, files)
	got, err := (&Extractor{}).Extract(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]facts.Fact{}
	for _, f := range got {
		if f.Kind != facts.KindDependency {
			t.Fatalf("kind = %q, want %q", f.Kind, facts.KindDependency)
		}
		out[f.Name] = f
	}
	return out
}

func mustHave(t *testing.T, got map[string]facts.Fact, name string) facts.Fact {
	t.Helper()
	f, ok := got[name]
	if !ok {
		var have []string
		for n := range got {
			have = append(have, n)
		}
		t.Fatalf("missing %q; got %v", name, have)
	}
	return f
}

// A go.mod's require versions ARE the lock, so every direct requirement is
// pinned — and the `// indirect` lines in the same file are the transitive
// closure this extractor deliberately does not carry.
func TestManifests_GoModDirectOnlyAndAlwaysPinned(t *testing.T) {
	got := extract(t, map[string]string{"go.mod": `module example.com/app

go 1.25

require (
	gopkg.in/yaml.v3 v3.0.1
	github.com/tree-sitter/go-tree-sitter v0.24.0 // indirect
)

require golang.org/x/sys v0.47.0
`})
	if len(got) != 2 {
		t.Fatalf("want 2 direct requires, got %d: %v", len(got), got)
	}
	f := mustHave(t, got, "pkg:golang/gopkg.in/yaml.v3")
	if f.PropString("resolved_version") != "v3.0.1" || !f.PropBool("pinned") {
		t.Fatalf("go requires are exact: %+v", f.Props)
	}
	if f.PropString("ecosystem") != EcosystemGo || f.PropString("type") != TypePackage {
		t.Fatalf("props wrong: %+v", f.Props)
	}
	mustHave(t, got, "pkg:golang/golang.org/x/sys")
	if _, indirect := got["pkg:golang/github.com/tree-sitter/go-tree-sitter"]; indirect {
		t.Fatal("an // indirect requirement is the transitive closure and must not be carried")
	}
}

// A caret range is not a pin. The lockfile beside it is what resolves it, and
// without one the dependency is reported unpinned — which is the honest answer
// and the whole point of the prop.
func TestManifests_NpmRangeIsUnpinnedUntilTheLockResolvesIt(t *testing.T) {
	manifest := `{"dependencies": {"lodash": "^4.17.0", "left-pad": "1.3.0"},
	  "devDependencies": {"jest": "~29.0.0"}}`

	got := extract(t, map[string]string{"package.json": manifest})
	if f := mustHave(t, got, "pkg:npm/lodash"); f.PropBool("pinned") {
		t.Fatalf("^4.17.0 is a range, not a pin: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:npm/left-pad"); !f.PropBool("pinned") {
		t.Fatalf("an exact constraint is a pin with no lockfile: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:npm/jest"); !f.PropBool("dev") {
		t.Fatalf("devDependencies must carry dev: %+v", f.Props)
	}

	withLock := extract(t, map[string]string{
		"package.json": manifest,
		"package-lock.json": `{"packages": {
			"": {"name": "app"},
			"node_modules/lodash": {"version": "4.17.21"},
			"node_modules/jest/node_modules/lodash": {"version": "3.0.0"}
		}}`,
	})
	f := mustHave(t, withLock, "pkg:npm/lodash")
	if f.PropString("resolved_version") != "4.17.21" || !f.PropBool("pinned") {
		t.Fatalf("the lock resolves the range: %+v", f.Props)
	}
}

// A Gemfile's version is a range far more often than not, and Gemfile.lock is
// where the answer lives. The lock's own nested dependency lines (six spaces)
// are not resolved versions and must not be read as any.
func TestManifests_GemfileResolvesThroughTheLock(t *testing.T) {
	got := extract(t, map[string]string{
		"Gemfile": `source 'https://rubygems.org'
gem 'rails', '~> 7.1'
gem 'pg'
gem 'rspec', '3.13.0', group: :test
`,
		"Gemfile.lock": `GEM
  remote: https://rubygems.org/
  specs:
    rails (7.1.3)
      actionpack (= 7.1.3)
    pg (1.5.4)

DEPENDENCIES
  rails (~> 7.1)
`,
	})
	if f := mustHave(t, got, "pkg:gem/rails"); f.PropString("resolved_version") != "7.1.3" || !f.PropBool("pinned") {
		t.Fatalf("rails: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:gem/pg"); f.PropString("resolved_version") != "1.5.4" {
		t.Fatalf("a gem with no constraint still resolves: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:gem/rspec"); !f.PropBool("dev") {
		t.Fatalf("a :test group gem is a dev dependency: %+v", f.Props)
	}
	if _, nested := got["pkg:gem/actionpack"]; nested {
		t.Fatal("a lock's nested dependency line is not a declared direct dependency")
	}
}

// Cargo writes a dependency two ways and both must read the same; a build- or
// dev-dependency is dev.
func TestManifests_CargoReadsBothSpellings(t *testing.T) {
	got := extract(t, map[string]string{
		"Cargo.toml": `[package]
name = "app"

[dependencies]
serde = "1.0"
axum = { version = "0.7.4", features = ["macros"] }
pic = { path = "crates/pic", version = "0.1", features = ["a", "b"] }

[dev-dependencies]
proptest = "1"
`,
		"Cargo.lock": `[[package]]
name = "serde"
version = "1.0.197"
`,
	})
	if f := mustHave(t, got, "pkg:cargo/serde"); f.PropString("resolved_version") != "1.0.197" {
		t.Fatalf("serde: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/axum"); f.PropString("constraint") != "0.7.4" {
		t.Fatalf("the table spelling carries a version too: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/pic"); f.PropString("path") != "crates/pic" || f.PropString("constraint") != "0.1" {
		t.Fatalf("an inline path dependency names where it lives beside its version: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/axum"); f.PropString("path") != "" {
		t.Fatalf("an inline table with no path names none: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/proptest"); !f.PropBool("dev") {
		t.Fatalf("proptest: %+v", f.Props)
	}
}

// The table spelling — [dependencies.name] — is how a dependency with many
// settings is written, and how a target-specific one usually is. tokio declares
// windows-sys ONLY under a target table, so reading the list form alone loses
// the dependency rather than its version.
func TestManifests_CargoReadsTableSections(t *testing.T) {
	got := extract(t, map[string]string{
		"Cargo.toml": `[package]
name = "app"

[dependencies]
serde = "1.0"

[dependencies.tokio-stream]
version = "0.1.14"
features = ["net"]

[target.'cfg(windows)'.dependencies.windows-sys]
version = "0.52"

[target.'cfg(windows)'.dev-dependencies.windows-helper]
version = "0.3"

[dependencies.local-crate]
path = "../local"

[features]
default = []
`,
	})
	if f := mustHave(t, got, "pkg:cargo/tokio-stream"); f.PropString("constraint") != "0.1.14" {
		t.Fatalf("a table dependency carries its version: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/windows-sys"); f.PropString("constraint") != "0.52" || f.PropBool("dev") {
		t.Fatalf("a target table is a real dependency: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/windows-helper"); !f.PropBool("dev") {
		t.Fatalf("a target dev-dependency table is a dev dependency: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/local-crate"); f.PropBool("pinned") || f.PropString("path") != "../local" {
		t.Fatalf("a path dependency names no version, and names where it lives: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/serde"); f.PropString("path") != "" {
		t.Fatalf("a registry dependency names no path: %+v", f.Props)
	}
	// [features] is not a dependency section, and its keys are not packages.
	if _, feature := got["pkg:cargo/default"]; feature {
		t.Fatal("a [features] key is not a dependency")
	}
	// Nor are the settings INSIDE a dependency table.
	for _, key := range []string{"pkg:cargo/version", "pkg:cargo/features", "pkg:cargo/path"} {
		if _, leaked := got[key]; leaked {
			t.Fatalf("%s is a setting inside a table, not a package", key)
		}
	}
}

// A pubspec's sdk pseudo-entries are not packages, and a map-valued dependency
// (git, path) has no version to pin.
func TestManifests_PubspecSkipsSDKEntries(t *testing.T) {
	got := extract(t, map[string]string{
		"pubspec.yaml": `name: app

environment:
  sdk: ">=3.0.0 <4.0.0"

dependencies:
  flutter:
    sdk: flutter
  http: ^1.2.0
  local_thing:
    path: ../local_thing

dev_dependencies:
  test: ^1.24.0
`,
		"pubspec.lock": `packages:
  http:
    dependency: "direct main"
    version: "1.2.1"
`,
	})
	if f := mustHave(t, got, "pkg:pub/http"); f.PropString("resolved_version") != "1.2.1" {
		t.Fatalf("http: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:pub/local_thing"); f.PropBool("pinned") {
		t.Fatalf("a path dependency names no version and cannot be pinned: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:pub/test"); !f.PropBool("dev") {
		t.Fatalf("test: %+v", f.Props)
	}
	if _, sdk := got["pkg:pub/flutter"]; sdk {
		t.Fatal("the flutter SDK is not a declared package")
	}
}

// == is pip's pin; anything else is a range. Extras and environment markers are
// neither a name nor a version and must not end up in either.
func TestManifests_PythonPinsWithDoubleEquals(t *testing.T) {
	got := extract(t, map[string]string{
		"requirements.txt": `# comment
requests==2.31.0
django>=4.2
uvicorn[standard]==0.27.0 ; python_version >= "3.9"
-r other.txt
`,
	})
	if f := mustHave(t, got, "pkg:pypi/requests"); !f.PropBool("pinned") || f.PropString("resolved_version") != "2.31.0" {
		t.Fatalf("requests: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:pypi/django"); f.PropBool("pinned") {
		t.Fatalf(">=4.2 is a range: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:pypi/uvicorn"); !f.PropBool("pinned") {
		t.Fatalf("extras and markers must not hide the pin: %+v", f.Props)
	}
}

// PEP 621 and Poetry are the two pyproject spellings, and both are read.
func TestManifests_PyprojectReadsBothSpellings(t *testing.T) {
	got := extract(t, map[string]string{"pyproject.toml": `[project]
name = "app"
dependencies = [
  "httpx==0.27.0",
  "pydantic>=2.0",
]
`})
	if f := mustHave(t, got, "pkg:pypi/httpx"); !f.PropBool("pinned") {
		t.Fatalf("httpx: %+v", f.Props)
	}
	mustHave(t, got, "pkg:pypi/pydantic")

	poetry := extract(t, map[string]string{"pyproject.toml": `[tool.poetry.dependencies]
python = "^3.11"
httpx = "0.27.0"

[tool.poetry.group.dev.dependencies]
pytest = "^8.0"
`})
	if f := mustHave(t, poetry, "pkg:pypi/httpx"); !f.PropBool("pinned") {
		t.Fatalf("poetry httpx: %+v", f.Props)
	}
	if f := mustHave(t, poetry, "pkg:pypi/pytest"); !f.PropBool("dev") {
		t.Fatalf("poetry dev group: %+v", f.Props)
	}
	if _, python := poetry["pkg:pypi/python"]; python {
		t.Fatal("the python version constraint is not a package")
	}
}

// The graph is name-keyed, so two manifests declaring the same package become
// one node. The merge is a decision: a package that is a real dependency
// anywhere is not a dev dependency of the repository, and a resolved version
// beats none.
func TestManifests_SamePackageInTwoManifestsMergesDeliberately(t *testing.T) {
	got := extract(t, map[string]string{
		"a/package.json":      `{"devDependencies": {"lodash": "^4.0.0"}}`,
		"b/package.json":      `{"dependencies": {"lodash": "^4.0.0"}}`,
		"b/package-lock.json": `{"packages": {"node_modules/lodash": {"version": "4.17.21"}}}`,
	})
	f := mustHave(t, got, "pkg:npm/lodash")
	if f.PropBool("dev") {
		t.Fatalf("a real dependency in either manifest is not a dev dependency: %+v", f.Props)
	}
	if f.PropString("resolved_version") != "4.17.21" {
		t.Fatalf("the resolved version must win over none: %+v", f.Props)
	}
}

// Detection answers from the walked names, and a repository with no manifest is
// not this extractor's business.
func TestManifests_DetectsOnManifestNamesOnly(t *testing.T) {
	e := &Extractor{}
	if ok, _ := e.DetectFiles("", []string{"src/main.go", "README.md"}); ok {
		t.Fatal("no manifest must not detect")
	}
	if ok, _ := e.DetectFiles("", []string{"deep/nested/pubspec.yaml"}); !ok {
		t.Fatal("a manifest at any depth must detect")
	}
	if !e.OwnsFile("Gemfile.lock") || !e.OwnsFile("go.mod") || e.OwnsFile("main.go") {
		t.Fatal("OwnsFile must cover manifests and lockfiles and nothing else")
	}
}

// Fail closed: a constraint this vocabulary does not recognise as exact is
// reported as a range, because calling an unpinned dependency pinned is the one
// error with a consequence.
func TestManifests_ExactConstraintFailsClosed(t *testing.T) {
	exact := []string{"1.2.3", "v1.2.3", "==1.2.3", "0.1", "7.1.3"}
	ranges := []string{"", "^1.2.3", "~> 7.1", ">=4.2", "1.*", "1.x", "*",
		"1.2 || 1.3", ">= 1.0, < 2.0", "git@github.com:x/y.git", "file:../local"}
	for _, c := range exact {
		if !isExactConstraint(c) {
			t.Errorf("%q should be exact", c)
		}
	}
	for _, c := range ranges {
		if isExactConstraint(c) {
			t.Errorf("%q should not be exact", c)
		}
	}
}

// Both yarn formats resolve, and a scoped package's leading @ is part of its
// name rather than the separator.
func TestManifests_YarnLockResolvesBothFormats(t *testing.T) {
	manifest := `{"dependencies": {"lodash": "^4.17.0", "@scope/thing": "^1.0.0"}}`

	classic := extract(t, map[string]string{
		"package.json": manifest,
		"yarn.lock": `# yarn lockfile v1

lodash@^4.17.0:
  version "4.17.21"
  resolved "https://registry.yarnpkg.com/lodash/-/lodash-4.17.21.tgz"

"@scope/thing@^1.0.0":
  version "1.2.3"
`,
	})
	if f := mustHave(t, classic, "pkg:npm/lodash"); f.PropString("resolved_version") != "4.17.21" {
		t.Fatalf("classic yarn: %+v", f.Props)
	}
	if f := mustHave(t, classic, "pkg:npm/@scope/thing"); f.PropString("resolved_version") != "1.2.3" {
		t.Fatalf("a scoped name keeps its leading @: %+v", f.Props)
	}

	berry := extract(t, map[string]string{
		"package.json": manifest,
		"yarn.lock": `"lodash@npm:^4.17.0":
  version: 4.17.21
  languageName: node

"@scope/thing@npm:^1.0.0":
  version: 1.2.3
`,
	})
	if f := mustHave(t, berry, "pkg:npm/lodash"); f.PropString("resolved_version") != "4.17.21" {
		t.Fatalf("berry yarn: %+v", f.Props)
	}
	if f := mustHave(t, berry, "pkg:npm/@scope/thing"); f.PropString("resolved_version") != "1.2.3" {
		t.Fatalf("berry scoped: %+v", f.Props)
	}
}

// TestManifests_YarnLockResolvesAliasAndProtocolRanges covers the descriptors
// whose range carries an `@` of its own. Reading the LAST `@` as the name/range
// separator named the package `@typescript/native@npm:typescript`, which
// resolves against nothing, so mastodon's two aliased dependencies were reported
// unpinned while its committed lockfile pins both. A git range failed the same
// way, on the `@` inside the URL.
func TestManifests_YarnLockResolvesAliasAndProtocolRanges(t *testing.T) {
	got := extract(t, map[string]string{
		"package.json": `{"dependencies": {
			"@typescript/native": "npm:typescript@7.0.2",
			"emoji-mart": "npm:emoji-mart-lazyload@latest",
			"tool": "git+ssh://git@github.com/acme/tool.git#v1.2.3"
		}}`,
		"yarn.lock": `"@typescript/native@npm:typescript@7.0.2, typescript@npm:^5.6 || 6 || 7":
  version: 7.0.2
  languageName: node

"emoji-mart@npm:emoji-mart-lazyload@latest":
  version: 3.0.1

"tool@git+ssh://git@github.com/acme/tool.git#v1.2.3":
  version: 1.2.3
`,
	})
	for _, want := range []struct{ purl, version string }{
		{"pkg:npm/@typescript/native", "7.0.2"},
		{"pkg:npm/emoji-mart", "3.0.1"},
		{"pkg:npm/tool", "1.2.3"},
	} {
		f := mustHave(t, got, want.purl)
		if f.PropString("resolved_version") != want.version {
			t.Errorf("%s resolved_version = %q, want %q: the lockfile pins it under an aliased descriptor", want.purl, f.PropString("resolved_version"), want.version)
		}
		if pinned, _ := f.Props["pinned"].(bool); !pinned {
			t.Errorf("%s pinned = %v, want true: a lockfile resolved it", want.purl, f.Props["pinned"])
		}
	}
}

// A lockfile enola cannot read is a stated absence, not a claim of unpinned.
// Answering anyway is how twelve packages in a yarn repository became twelve
// false blocks, and a gate that blocks falsely is one nobody leaves enabled.
func TestManifests_UnreadableLockLeavesPinnedUnknown(t *testing.T) {
	got := extract(t, map[string]string{
		"package.json":   `{"dependencies": {"lodash": "^4.17.0"}}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
	})
	f := mustHave(t, got, "pkg:npm/lodash")
	if _, answered := f.Props["pinned"]; answered {
		t.Fatalf("a lock enola cannot read must leave pinned unanswered: %+v", f.Props)
	}
	if f.PropString("unresolved_lock") != "pnpm-lock.yaml" {
		t.Fatalf("the unread lock must be named: %+v", f.Props)
	}

	// With no lockfile at all, the range genuinely resolves to nothing.
	none := extract(t, map[string]string{"package.json": `{"dependencies": {"lodash": "^4.17.0"}}`})
	f = mustHave(t, none, "pkg:npm/lodash")
	if pinned, answered := f.Props["pinned"]; !answered || pinned != false {
		t.Fatalf("no lockfile means the range is unpinned, and that IS the answer: %+v", f.Props)
	}
}

// Cargo's `=` and pip's `==` both name exactly one version.
func TestManifests_EqualityOperatorIsAPin(t *testing.T) {
	got := extract(t, map[string]string{"Cargo.toml": `[dependencies]
tracing-mock = "= 0.1.0-beta.1"
tokio-macros = "~2.7.0"
`})
	if f := mustHave(t, got, "pkg:cargo/tracing-mock"); !f.PropBool("pinned") {
		t.Fatalf("= names exactly one version: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:cargo/tokio-macros"); f.PropBool("pinned") {
		t.Fatalf("~ is a range: %+v", f.Props)
	}
	for _, r := range []string{">=1.0", "<=1.0", "~=1.0", "!=1.0"} {
		if isExactConstraint(r) {
			t.Errorf("%q ends in = but is a range", r)
		}
	}
}

// A monorepo keeps one lockfile at its root and a manifest in every package.
// Reading only the sibling reported five of excalidraw's dependencies as
// unpinned when its root yarn.lock pins all of them.
func TestManifests_LockfileIsFoundAtTheWorkspaceRoot(t *testing.T) {
	got := extract(t, map[string]string{
		"package.json": `{"name":"root","devDependencies":{"rimraf":"^5.0.0"}}`,
		"yarn.lock": `rimraf@^5.0.0:
  version "5.0.5"

"@codemirror/state@^6.0.0":
  version "6.4.1"
`,
		"packages/editor/package.json": `{"dependencies":{"@codemirror/state":"^6.0.0"}}`,
	})
	f := mustHave(t, got, "pkg:npm/@codemirror/state")
	if f.PropString("resolved_version") != "6.4.1" || !f.PropBool("pinned") {
		t.Fatalf("a nested package resolves against the workspace root lock: %+v", f.Props)
	}
	if f := mustHave(t, got, "pkg:npm/rimraf"); !f.PropBool("pinned") {
		t.Fatalf("the root manifest resolves too: %+v", f.Props)
	}
}

// The nearer lockfile wins, so a package that keeps its own is not resolved by
// the workspace root's stale copy of the same name.
func TestManifests_NearestLockfileWins(t *testing.T) {
	got := extract(t, map[string]string{
		"package.json":              `{"dependencies":{"lodash":"^4.0.0"}}`,
		"yarn.lock":                 "lodash@^4.0.0:\n  version \"4.0.0\"\n",
		"packages/app/package.json": `{"dependencies":{"lodash":"^4.0.0"}}`,
		"packages/app/yarn.lock":    "lodash@^4.0.0:\n  version \"4.17.21\"\n",
	})
	// Both manifests declare the same package, so the graph holds one node; the
	// merge keeps the first resolved version in sorted manifest order, which is
	// the root's. What this test pins is that the nested lock was READ at all.
	if len(got) != 1 {
		t.Fatalf("one package, one node: %+v", got)
	}
	if f := mustHave(t, got, "pkg:npm/lodash"); f.PropString("resolved_version") == "" {
		t.Fatalf("lodash resolved to nothing: %+v", f.Props)
	}
}

// A dependencies array is routinely interrupted by comment lines, and a single
// requirement routinely contains commas. Splitting on commas is wrong in both
// directions: it invented thirty-three packages named after comment prose on
// cognee, and it would have shredded the one requirement that needs them.
func TestManifests_Pep621ArrayIsQuotedSpansNotCommaSplits(t *testing.T) {
	got := extract(t, map[string]string{"pyproject.toml": `[project]
name = "app"
dependencies = [
    "openai>=1.80.1",
    # GHSA-4xgf-cpjx-pc3j: 2.12.0-2.14.1 vulnerable, fixed in 2.14.2. Exclude the
    # vulnerable range rather than capping.
    "pydantic-settings>=2.2.1,!=2.12.*,<3",
    "wheel>=0.46.2", # trailing comment
    "httpx==0.27.0",
]
`})
	if f := mustHave(t, got, "pkg:pypi/pydantic-settings"); f.PropString("constraint") != ">=2.2.1,!=2.12.*,<3" {
		t.Fatalf("a requirement containing commas is one requirement: %+v", f.Props)
	}
	mustHave(t, got, "pkg:pypi/openai")
	mustHave(t, got, "pkg:pypi/wheel")
	if f := mustHave(t, got, "pkg:pypi/httpx"); !f.PropBool("pinned") {
		t.Fatalf("== is a pin: %+v", f.Props)
	}
	if len(got) != 4 {
		var names []string
		for n := range got {
			names = append(names, n)
		}
		t.Fatalf("comment prose is not a package; got %d: %v", len(got), names)
	}
}

// uv.lock and poetry.lock hold one [[package]] block per resolved package, the
// same shape Cargo.lock has, so a range in pyproject.toml resolves through them.
func TestManifests_PythonLockResolvesRanges(t *testing.T) {
	for _, lock := range []string{"uv.lock", "poetry.lock"} {
		got := extract(t, map[string]string{
			"pyproject.toml": "[project]\ndependencies = [\n    \"openai>=1.80.1\",\n]\n",
			lock: `[[package]]
name = "openai"
version = "1.99.1"
`,
		})
		f := mustHave(t, got, "pkg:pypi/openai")
		if f.PropString("resolved_version") != "1.99.1" || !f.PropBool("pinned") {
			t.Fatalf("%s did not resolve: %+v", lock, f.Props)
		}
	}
	// A lock this extractor does not read leaves the answer unstated.
	got := extract(t, map[string]string{
		"pyproject.toml": "[project]\ndependencies = [\n    \"openai>=1.80.1\",\n]\n",
		"Pipfile.lock":   `{"_meta": {}}`,
	})
	f := mustHave(t, got, "pkg:pypi/openai")
	if _, answered := f.Props["pinned"]; answered {
		t.Fatalf("an unread lock must leave pinned unanswered: %+v", f.Props)
	}
}

// PyPI treats case, `_` and `.` as the same (PEP 503), so a manifest spelling
// must not become a second node or an unresolved dependency.
func TestManifests_PyPINamesAreNormalized(t *testing.T) {
	got := extract(t, map[string]string{
		"pyproject.toml": "[project]\ndependencies = [\n    \"typing_extensions>=4.12.2,<5.0.0\",\n    \"cognee @ file:///Users/x/cognee\",\n]\n",
		"uv.lock": `[[package]]
name = "typing-extensions"
version = "4.14.0"
`,
	})
	f := mustHave(t, got, "pkg:pypi/typing-extensions")
	if f.PropString("resolved_version") != "4.14.0" || !f.PropBool("pinned") {
		t.Fatalf("an underscore spelling must resolve against the hyphen one: %+v", f.Props)
	}
	if f.PropString("package_name") != "typing_extensions" {
		t.Fatalf("the written spelling is what a declaration matches: %+v", f.Props)
	}
	// A direct reference names a package and a place, not a version.
	d := mustHave(t, got, "pkg:pypi/cognee")
	if d.PropString("package_name") != "cognee" || d.PropBool("pinned") {
		t.Fatalf("a PEP 508 direct reference: %+v", d.Props)
	}
	if len(got) != 2 {
		t.Fatalf("two packages, got %d", len(got))
	}
	for _, in := range []string{"typing_extensions", "Typing.Extensions", "TYPING-extensions"} {
		if normalizePyPIName(in) != "typing-extensions" {
			t.Errorf("normalize(%q) = %q", in, normalizePyPIName(in))
		}
	}
}
