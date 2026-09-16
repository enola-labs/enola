package tsextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// testRefTargets returns the RelCalls targets of a single test-ref fact, failing
// the test unless exactly one fact was produced and it has the shape the
// plugin.TestRefExtractor contract demands: kind test_ref, name == file == relFile,
// language "typescript", and no relation other than "calls".
func testRefTargets(t *testing.T, ff []facts.Fact, relFile string) map[string]bool {
	t.Helper()
	if len(ff) != 1 {
		t.Fatalf("want exactly 1 test_ref fact, got %d: %+v", len(ff), ff)
	}
	f := ff[0]
	if f.Kind != facts.KindTestRef {
		t.Fatalf("kind = %q, want %q", f.Kind, facts.KindTestRef)
	}
	if f.Name != relFile || f.File != relFile {
		t.Fatalf("name/file = %q/%q, want %q", f.Name, f.File, relFile)
	}
	if lang, _ := f.Props["language"].(string); lang != "typescript" {
		t.Fatalf("props[language] = %v, want \"typescript\"", f.Props["language"])
	}
	out := map[string]bool{}
	for _, r := range f.Relations {
		if r.Kind != facts.RelCalls {
			t.Fatalf("relation kind = %q, want only %q", r.Kind, facts.RelCalls)
		}
		out[r.Target] = true
	}
	return out
}

// TestExtractTestRefs_ImportedCallResolvesToProductionSymbol pins the dominant TS
// idiom: a co-located *.test.ts imports a production function and calls it. The
// emitted target must equal the production symbol's fact name ("<dir>.<name>"),
// which is what the dead-code detector matches against — so a helper exercised only
// by its test keeps an incoming edge. (v103)
func TestExtractTestRefs_ImportedCallResolvesToProductionSymbol(t *testing.T) {
	dir := setupTSProject(t, map[string]string{
		"src/util.ts":      `export function formatTag(s: string): string { return s.trim(); }`,
		"src/util.test.ts": `import { formatTag } from './util';` + "\n" + `test('formats', () => { formatTag(' x '); });`,
	}, false)

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"src/util.test.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := testRefTargets(t, ff, "src/util.test.ts")

	if !got["src.formatTag"] {
		t.Errorf("missing target src.formatTag; got %v", got)
	}
}

// TestExtractTestRefs_AliasAndNamespaceImportsResolve proves ExtractTestRefs
// reconstructs the tsconfig path aliases (collectTSAliasRoots) and reuses the
// production namespace-member resolver, so a helper reached through a "~/…" alias or
// an `import * as ns` member access is still credited. (v103)
func TestExtractTestRefs_AliasAndNamespaceImportsResolve(t *testing.T) {
	dir := setupTSProject(t, map[string]string{
		"tsconfig.json":       `{"compilerOptions":{"paths":{"~/*":["./src/*"]}}}`,
		"src/helper.ts":       `export function helper(): number { return 1; }`,
		"src/util/ns.ts":      `export function build(): number { return 2; }`,
		"tests/thing.test.ts": `import { helper } from '~/helper';` + "\n" + `import * as ns from '~/util/ns';` + "\n" + `test('t', () => { helper(); ns.build(); });`,
	}, false)

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"tests/thing.test.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := testRefTargets(t, ff, "tests/thing.test.ts")

	if !got["src.helper"] {
		t.Errorf("missing alias-imported target src.helper; got %v", got)
	}
	if !got["src/util.build"] {
		t.Errorf("missing namespace-member target src/util.build; got %v", got)
	}
}

// TestExtractTestRefs_EmitsOnlyTestRefNoSymbols guards the two invariants the
// plugin.TestRefExtractor contract states: reference-only facts (never a symbol,
// module or route fact — test code must not become a dead-code candidate), and no
// target for an external import that no production symbol backs. (v103)
func TestExtractTestRefs_EmitsOnlyTestRefNoSymbols(t *testing.T) {
	dir := setupTSProject(t, map[string]string{
		"src/svc.ts":      `export function doWork(): void {}`,
		"src/svc.spec.ts": `import { render } from '@testing-library/react';` + "\n" + `import { doWork } from './svc';` + "\n" + `test('w', () => { doWork(); });`,
	}, false)

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"src/svc.spec.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ff {
		if f.Kind != facts.KindTestRef {
			t.Fatalf("emitted a non-test_ref fact: %+v", f)
		}
	}
	got := testRefTargets(t, ff, "src/svc.spec.ts")

	if !got["src.doWork"] {
		t.Errorf("missing target src.doWork; got %v", got)
	}
	// `render` is imported from an external module (skipped) and never called, so it
	// binds no internal target — an unused external import must not be credited.
	if got["render"] || got["src.render"] {
		t.Errorf("unused external import render must not become a target; got %v", got)
	}
}

// TestExtractTestRefs_IgnoresNonTestFiles is the guard for the engine's hand-off.
// tsextractor IS a plugin.FileOwner (for production caching), so runTestRefExtractors
// scopes files to isTypeScriptFile — which owns non-test .ts too, plus other
// languages' specs may still arrive. ExtractTestRefs must filter to *.test/spec.ts(x)
// itself and must not treat a production .ts file (or a Ruby spec) as a test. (v103)
func TestExtractTestRefs_IgnoresNonTestFiles(t *testing.T) {
	dir := setupTSProject(t, map[string]string{
		"src/svc.ts": `export function doWork(): void {}`,
	}, false)

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{
		"src/svc.ts",         // production source, not a test
		"spec/thing_spec.rb", // Ruby's, not ours
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ff) != 0 {
		t.Fatalf("want no facts from files tsextractor must not treat as tests, got %+v", ff)
	}
}

// TestExtractTestRefs_ReferenceFreeFileYieldsNoFact: a test file that references no
// production code produces no fact at all, rather than an empty one.
func TestExtractTestRefs_ReferenceFreeFileYieldsNoFact(t *testing.T) {
	dir := setupTSProject(t, map[string]string{
		"src/nothing.test.ts": `const x: number = 1;`,
	}, false)

	ff, err := New().ExtractTestRefs(context.Background(), dir, []string{"src/nothing.test.ts"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ff) != 0 {
		t.Fatalf("want no fact for a reference-free test file, got %+v", ff)
	}
}

func TestIsTSTestFile_JSVariantsAndTestsDirectory(t *testing.T) {
	for _, file := range []string{"src/a.test.mjs", "src/a.spec.js", "web/src/__tests__/custom.clienttest.ts"} {
		if !isTSTestFile(file) {
			t.Errorf("%s was not classified as a test", file)
		}
	}
	if isTSTestFile("src/contest.ts") {
		t.Error("production file classified as a test")
	}
}
