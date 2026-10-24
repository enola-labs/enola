package jvmsrc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// writeRepo materializes files (relpath -> contents) under a temp dir and returns
// the repo root and the relative file list.
func writeRepo(t *testing.T, files map[string]string) (string, []string) {
	t.Helper()
	root := t.TempDir()
	var rel []string
	for p, content := range files {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, p)
	}
	return root, rel
}

// TestBuildPackageIndex_MultiModuleSamePrefix is the core regression: two Gradle
// modules (app, api) both root packages at com.example.app.*. Each package must
// map to ITS OWN module directory, not collapse onto one source root.
func TestBuildPackageIndex_MultiModuleSamePrefix(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"app/src/main/java/com/example/app/ui/Home.kt":                       "package com.example.app.ui\n",
		"api/src/main/java/com/example/app/api/model/search/SearchResp.java": "package com.example.app.api.model.search;\n",
	})
	idx := BuildPackageIndex(root, files)

	if got, want := idx["com.example.app.ui"], "app/src/main/java/com/example/app/ui"; got != want {
		t.Errorf("com.example.app.ui -> %q, want %q", got, want)
	}
	if got, want := idx["com.example.app.api.model.search"], "api/src/main/java/com/example/app/api/model/search"; got != want {
		t.Errorf("com.example.app.api.model.search -> %q, want %q", got, want)
	}
}

// TestBuildPackageIndex_MainWinsOverVariant ensures that when a build-variant
// source set (src/debug) declares the same root package as src/main, imports of
// that package resolve to the MAIN module — a variant must never shadow main
// (which would misroute the whole app's afferent coupling onto the variant).
func TestBuildPackageIndex_MainWinsOverVariant(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"app/src/debug/java/com/example/app/DebugApp.kt": "package com.example.app\n",
		"app/src/main/java/com/example/app/App.kt":       "package com.example.app\n",
	})
	idx := BuildPackageIndex(root, files)
	if got, want := idx["com.example.app"], "app/src/main/java/com/example/app"; got != want {
		t.Errorf("com.example.app -> %q, want main source set %q (variant must not shadow main)", got, want)
	}
}

// TestBuildPackageIndex_ExcludesTestSources ensures src/test and src/androidTest
// files never enter the index (so imports don't resolve into test dirs and a main
// package isn't shadowed by a test helper sharing its name).
func TestBuildPackageIndex_ExcludesTestSources(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"app/src/main/java/de/x/Foo.kt":         "package de.x\n",
		"app/src/test/java/de/x/FooTest.kt":     "package de.x\n",
		"app/src/androidTest/java/de/y/UiIT.kt": "package de.y\n",
	})
	idx := BuildPackageIndex(root, files)

	if got, want := idx["de.x"], "app/src/main/java/de/x"; got != want {
		t.Errorf("de.x -> %q, want production dir %q", got, want)
	}
	if _, ok := idx["de.y"]; ok {
		t.Errorf("androidTest-only package de.y should not be indexed, got %q", idx["de.y"])
	}
}

// TestResolveImport walks the longest-prefix match, including nested-type imports.
func TestResolveImport(t *testing.T) {
	idx := map[string]string{
		"com.example.app.api.model.search": "api/src/main/java/com/example/app/api/model/search",
	}
	cases := []struct {
		imp    string
		want   string
		wantOK bool
	}{
		{"com.example.app.api.model.search.SearchResp", "api/src/main/java/com/example/app/api/model/search/SearchResp", true},
		{"com.example.app.api.model.search.Outer.Inner", "api/src/main/java/com/example/app/api/model/search/Outer/Inner", true},
		{"com.example.app.api.model.search.*", "api/src/main/java/com/example/app/api/model/search", true},
		{"kotlinx.coroutines.flow.Flow", "", false},
	}
	for _, c := range cases {
		got, ok := ResolveImport(c.imp, idx)
		if ok != c.wantOK || got != c.want {
			t.Errorf("ResolveImport(%q) = (%q, %v), want (%q, %v)", c.imp, got, ok, c.want, c.wantOK)
		}
	}
}

// TestModuleRole classifies production vs test source-set directories, including
// compound test-module names that compile as src/main (release-tests, ui-test-utils,
// test-lab) via the sub-token rule — and confirms it does NOT misfire on
// single-token names that merely contain "test" (latest, contest, the abtest feature).
func TestModuleRole(t *testing.T) {
	cases := map[string]string{
		"app/src/main/java/de/x":                            facts.ModuleRoleProduction,
		"app/src/test/java/de/x":                            facts.ModuleRoleTest,
		"app/src/androidTest/java/de/y":                     facts.ModuleRoleTest,
		"release-tests/src/main/java/de/x/releasetests":     facts.ModuleRoleTest,
		"ui-test-utils/src/main/java/de/x/testutils":        facts.ModuleRoleTest,
		"test-lab/src/main/java/de/x":                       facts.ModuleRoleTest,
		"app/src/main/java/de/x/latest":                     facts.ModuleRoleProduction,
		"app/src/main/java/de/x/contest":                    facts.ModuleRoleProduction,
		"business/src/main/java/de/example/business/abtest": facts.ModuleRoleProduction,

		// A PACKAGE named `test` under a src/main source set is production. Both
		// are real: Spark ships the first as production code, and the second is
		// the shape of every fixture library that publishes helpers for consumers
		// to test against. Before the source-set boundary, the segment scan saw
		// `test` and dropped them from package metrics and layer analysis.
		"sql/core/src/main/scala/org/apache/spark/sql/test": facts.ModuleRoleProduction,
		"core/src/main/scala/com/example/core/test":         facts.ModuleRoleProduction,
		"app/src/main/kotlin/de/x/spec":                     facts.ModuleRoleProduction,
		// The boundary is directional: a test-purpose MODULE name still classifies,
		// because it sits before `src/`. These are the cases above, restated as the
		// control — the rule narrows what is scanned, it does not disable it.
		"ui-test-utils/src/main/java/de/x/test": facts.ModuleRoleTest,
		// And a real test source set is unaffected whatever the package is called.
		"core/src/test/scala/com/example/core": facts.ModuleRoleTest,

		// Build DEFINITIONS, not application code: sbt compiles `project/` as a
		// second meta build and Gradle does the same with `buildSrc/`. Both are
		// real source in the language, so they reach the graph like anything else
		// and would otherwise be counted as production packages.
		"project":                       facts.ModuleRoleTooling,
		"project/plugins":               facts.ModuleRoleTooling,
		"buildSrc/src/main/kotlin/de/x": facts.ModuleRoleTooling,
		// A package named `project` further down a source tree is ordinary code;
		// only the first segment carries the meta-build meaning.
		"app/src/main/scala/com/example/project": facts.ModuleRoleProduction,
	}
	for dir, want := range cases {
		if got := ModuleRole(dir); got != want {
			t.Errorf("ModuleRole(%q) = %q, want %q", dir, got, want)
		}
	}
}

// TestBuildPackageIndex_ScalaChainedPackages pins Scala's two header forms, both of
// which produce a WRONG package rather than a missing one if mishandled — which is
// worse, because every import resolving through them lands somewhere real but
// incorrect.
//
// A chained clause names one package: `package com.example` followed by
// `package model` is `com.example.model`. Reading only the first would map every
// type in the file to `com.example`, and a sibling file that genuinely declares
// `com.example` would then fight it for the index entry.
func TestBuildPackageIndex_ScalaChainedPackages(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"core/src/main/scala/com/example/model/Order.scala": "package com.example\npackage model\n\nclass Order\n",
		"core/src/main/scala/com/example/Root.scala":        "package com.example\n\nclass Root\n",
	})
	idx := BuildPackageIndex(root, files)

	if got, want := idx["com.example.model"], "core/src/main/scala/com/example/model"; got != want {
		t.Errorf("chained package -> %q, want %q", got, want)
	}
	if got, want := idx["com.example"], "core/src/main/scala/com/example"; got != want {
		t.Errorf("plain package -> %q, want %q", got, want)
	}
}

// TestBuildPackageIndex_ScalaPackageObject pins the other trap: `package object util`
// is a DECLARATION whose name is `util`, not a clause opening a package called
// `object`. Matching it indexes a package no code ever imports and, worse, maps it
// to a real directory.
func TestBuildPackageIndex_ScalaPackageObject(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"core/src/main/scala/com/example/util/package.scala": "package com.example\n\npackage object util {\n  val VERSION = \"1.0\"\n}\n",
	})
	idx := BuildPackageIndex(root, files)

	if dir, ok := idx["object"]; ok {
		t.Errorf("`package object` was read as a package clause: indexed \"object\" -> %q", dir)
	}
	if got, want := idx["com.example"], "core/src/main/scala/com/example/util"; got != want {
		t.Errorf("enclosing package -> %q, want %q", got, want)
	}
}

// TestBuildPackageIndex_HeaderCommentsDoNotBreakTheChain checks that a licence
// header or scaladoc between chained clauses is stepped over. Apache-licensed Scala
// (spark, pekko) puts one there in every file.
func TestBuildPackageIndex_HeaderCommentsDoNotBreakTheChain(t *testing.T) {
	src := "package com.example\n\n/*\n * Licensed to the Apache Software Foundation.\n */\n// and a line comment\npackage model\n\nclass Order\n"
	root, files := writeRepo(t, map[string]string{
		"core/src/main/scala/com/example/model/Order.scala": src,
	})
	idx := BuildPackageIndex(root, files)

	if got, want := idx["com.example.model"], "core/src/main/scala/com/example/model"; got != want {
		t.Errorf("comment between chained clauses broke the chain: %q, want %q", got, want)
	}
}

// TestBuildPackageIndex_ChainStopsAtFirstDeclaration guards the other direction: a
// later `package` inside the file body (a nested package block) must not extend the
// header once real code has been seen.
func TestBuildPackageIndex_ChainStopsAtFirstDeclaration(t *testing.T) {
	src := "package com.example\n\nclass Order\n\npackage nested {\n  class Inner\n}\n"
	root, files := writeRepo(t, map[string]string{
		"core/src/main/scala/com/example/Order.scala": src,
	})
	idx := BuildPackageIndex(root, files)

	if _, bad := idx["com.example.nested"]; bad {
		t.Errorf("a package block after a declaration was folded into the header chain: %v", idx)
	}
	if got, want := idx["com.example"], "core/src/main/scala/com/example"; got != want {
		t.Errorf("com.example -> %q, want %q", got, want)
	}
}

// TestBuildModule pins the boundary the cycles explainer reads. A cycle between
// packages inside ONE module is legal in Scala — the compiler takes the module as a
// unit and imposes no package-level acyclicity, unlike Go — while sbt and Maven both
// reject a circular dependency between modules. Getting the boundary wrong reports a
// legal cycle as a build-order defect, or hides a real one.
func TestBuildModule(t *testing.T) {
	cases := map[string]string{
		// Multi-module: the prefix before the first `src/` segment names the module.
		"core/src/main/scala/com/example/core":         "core",
		"modules/mon/src/main":                         "modules/mon",
		"sql/core/src/main/scala/org/apache/spark/sql": "sql/core",
		"app/src/test/scala/com/example/app":           "app",
		"cluster/src/multi-jvm/scala/pkg":              "cluster",
		// Single-module: sources start at the repository root. A NAME, not "" —
		// callers read "" as "not attributed", and this is the case where
		// attribution matters most, since every package shares one unit.
		"src/main/scala/gitbucket/core/util": ".",
		"src/main/java/com/example":          ".",
		"src":                                ".",
		// Outside any source set: build definitions and stray files stay
		// unattributed, so a cycle reaching one keeps the build-defect wording.
		"project":         "",
		"project/plugins": "",
		"":                "",
		// The FIRST `src` wins: a package named `src` deeper in the tree is not a
		// source set.
		"core/src/main/scala/com/src/deep": "core",
	}
	for dir, want := range cases {
		if got := BuildModule(dir); got != want {
			t.Errorf("BuildModule(%q) = %q, want %q", dir, got, want)
		}
	}
}

// TestGeneratedImport: R and BuildConfig live in a Gradle module's namespace, which
// often no source file declares, and View Binding generates a lowercase
// sub-package. Resolving any of them to the longest declared prefix attached a
// feature module to the application package. Nested types and top-level functions
// are ordinary imports and must still resolve.
func TestGeneratedImport(t *testing.T) {
	idx := map[string]string{
		"com.example.app":           "app/src/main/java/com/example/app",
		"com.example.app.ui.home":   "ui/src/main/java/com/example/app/ui/home",
		"com.example.app.data":      "data/src/main/java/com/example/app/data",
		"com.example.app.data.util": "data/src/main/java/com/example/app/data/util",
	}
	cases := []struct {
		imp  string
		want bool
	}{
		{"com.example.app.ui.R", true},
		{"com.example.app.ui.R.string", true},
		{"com.example.app.data.BuildConfig", true},
		{"com.example.app.databinding.ActivityMainBinding", true},
		{"com.example.app.ui.home.HomeScreen", false},
		{"com.example.app.data.util.formatScore", false},
		{"com.example.app.data.Outer.Inner", false},
		{"com.example.app.ui.home.*", false},
		{"retrofit2.Retrofit", false},
	}
	for _, c := range cases {
		if got := GeneratedImport(c.imp, idx); got != c.want {
			t.Errorf("GeneratedImport(%q) = %v, want %v", c.imp, got, c.want)
		}
		if c.want {
			if dir, ok := ResolveImport(c.imp, idx); ok {
				t.Errorf("ResolveImport(%q) = %q, want no directory for generated code", c.imp, dir)
			}
		}
	}
}

// A Kotlin Multiplatform test source set is named for its target, and is kept out
// of the index like src/test. A package segment that merely ends in "Test" is not.
func TestIsTestSourceKnowsMultiplatformTestSets(t *testing.T) {
	for path, want := range map[string]bool{
		"lib/src/test/kotlin/a/B.kt":             true,
		"lib/src/jvmTest/kotlin/a/B.kt":          true,
		"lib/src/commonTest/kotlin/a/B.kt":       true,
		"lib/src/androidUnitTest/kotlin/a/B.kt":  true,
		"src/androidTest/java/a/B.java":          true,
		"lib/src/commonJvmAndroid/kotlin/a/B.kt": false,
		"lib/src/main/kotlin/a/loadTest/B.kt":    false,
		"lib/src/jvmMain/kotlin/a/B.kt":          false,
		"loadTest/src/main/kotlin/a/B.kt":        false,
	} {
		if got := isTestSource(path); got != want {
			t.Errorf("isTestSource(%q) = %v, want %v", path, got, want)
		}
	}
}

// A name an import resolved into a package's preferred directory moves to the one
// sibling that declares it, and stays where it is when none or two do.
func TestSplitTarget(t *testing.T) {
	dirs := map[string][]string{"com.acme.c": {"support/src/main/kotlin/com/acme/c", "lib/src/common/kotlin/com/acme/c", "lib/src/jvm/kotlin/com/acme/c"}}
	siblings := SplitSiblings(dirs)
	declared := map[string]bool{
		"lib/src/common/kotlin/com/acme/c.guarded": true,
		"lib/src/common/kotlin/com/acme/c.Twice":   true,
		"lib/src/jvm/kotlin/com/acme/c.Twice":      true,
	}
	exists := func(n string) bool { return declared[n] }
	for in, want := range map[string]string{
		"support/src/main/kotlin/com/acme/c.guarded":        "lib/src/common/kotlin/com/acme/c.guarded",
		"support/src/main/kotlin/com/acme/c.Twice":          "support/src/main/kotlin/com/acme/c.Twice",
		"support/src/main/kotlin/com/acme/c.Nowhere":        "support/src/main/kotlin/com/acme/c.Nowhere",
		"lib/src/common/kotlin/com/acme/c.guarded":          "lib/src/common/kotlin/com/acme/c.guarded",
		"elsewhere/x.guarded":                               "elsewhere/x.guarded",
		"support/src/main/kotlin/com/acme/c.guarded.nested": "support/src/main/kotlin/com/acme/c.guarded.nested",
	} {
		if got := SplitTarget(in, siblings, exists); got != want {
			t.Errorf("SplitTarget(%q) = %q, want %q", in, got, want)
		}
	}
}
