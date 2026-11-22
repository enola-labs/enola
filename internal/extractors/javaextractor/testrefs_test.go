package javaextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

var _ plugin.TestRefExtractor = (*JavaExtractor)(nil)

func javaTestRefTargets(t *testing.T, relFile, src string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range testRefsFromFile([]byte(src), relFile) {
		for _, r := range f.Relations {
			out[r.Target] = true
		}
	}
	return out
}

func sortedTargets(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestJavaTestRefsCaptureProductionReferences pins what the pass is for: a static
// factory, a constructor, a member reached through a local, a static import, a
// method reference, a mocked type and a constant all keep an inbound edge once
// test sources are ignored.
func TestJavaTestRefsCaptureProductionReferences(t *testing.T) {
	src := `package org.example.cache;

import static org.example.cache.CacheUtil.buildKey;
import static org.junit.jupiter.api.Assertions.assertEquals;
import org.junit.jupiter.api.Test;

class CacheTest {
    private CacheLoader loader = mock(CacheLoader.class);

    @Test
    void evicts() {
        LruCache cache = new LruCache(Limits.MAX_ENTRIES);
        Entry e = EntryFactory.create("k");
        cache.evictOldest();
        String key = buildKey(e);
        java.util.function.Function<String, Entry> f = Parser::parseEntry;
        assertEquals(e.weight, cache.size());
        if (e instanceof ExpiringEntry) { }
        new Registry().register(e);
    }
}
`
	got := javaTestRefTargets(t, "core/src/test/java/org/example/cache/CacheTest.java", src)
	for _, want := range []string{
		"CacheLoader",         // a mocked type, named by its class literal and declared type
		"LruCache",            // a constructor
		"Limits.MAX_ENTRIES",  // a constant read
		"MAX_ENTRIES",         // and its bare form
		"EntryFactory.create", // a static factory; orphans folds the receiver into the type
		"create",              // and the bare member
		"cache.evictOldest",   // a member called on a local
		"evictOldest",         // and its bare form
		"buildKey",            // a static import, called bare
		"Parser.parseEntry",   // a method reference
		"parseEntry",          // and its bare form
		"weight",              // a field read off a local
		"size",                // a member called on a local, inside a harness call
		"ExpiringEntry",       // an instanceof check
		"Registry",            // a constructor whose result is called
		"register",            // the member called on it
		"Entry",               // a declared type
	} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
}

// TestJavaTestRefsDropHarnessAndOwnDeclarations keeps the reference set about the
// code under test: JUnit, Mockito, AssertJ and Hamcrest words, a harness receiver's
// members, imports, annotations, java.lang types and the test file's own helpers
// never vouch for a production symbol of the same name.
func TestJavaTestRefsDropHarnessAndOwnDeclarations(t *testing.T) {
	src := `package com.example;

import com.example.data.verify;
import static org.mockito.Mockito.when;

@ExtendWith(MockitoExtension.class)
class RepoTest {
    @Mock private Store store;
    private final FakeClock clock = new FakeClock();

    @BeforeEach void setUp() { helper(); this.prepare(); super.init(); }

    @Test
    void loads() {
        when(store.load()).thenReturn(List.of(1));
        Mockito.verify(store).save();
        assertThat(repo.items()).isEqualTo(Collections.emptyList());
        String s = String.valueOf(clock.ticks);
        Assertions.assertTrue(s.isEmpty());
    }

    private void helper() {}
    private void prepare() {}
}

class FakeClock { int ticks; void reset() {} }
`
	got := javaTestRefTargets(t, "app/src/test/java/com/example/RepoTest.java", src)
	for _, banned := range []string{
		"when", "thenReturn", "assertThat", "isEqualTo", "assertTrue",
		"Mockito.verify", "verify", // a harness receiver drops the bare name too
		"Assertions.assertTrue",
		"ExtendWith", "MockitoExtension", "Mock", "BeforeEach", "Test", // annotations
		"helper", "prepare", "FakeClock", "setUp", // the file's own declarations
		"init",                                                                     // a super call is the test class's own lineage
		"String", "List", "Collections", "Collections.emptyList", "String.valueOf", // java.lang and java.util
		"com.example.data.verify", // an import alone is not a use
	} {
		if got[banned] {
			t.Errorf("harness or own name %q recorded; have %v", banned, sortedTargets(got))
		}
	}
	for _, want := range []string{"Store", "store.load", "load", "save", "repo.items", "items", "clock.ticks", "ticks"} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedTargets(got))
		}
	}
}

// TestJavaExtractTestRefsEmitsOnlyTestRefFacts checks the plugin.TestRefExtractor
// contract end to end: one test_ref fact per Java test file, named by the file,
// carrying only calls relations; files the pass does not own are skipped.
func TestJavaExtractTestRefsEmitsOnlyTestRefFacts(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("core/src/test/java/a/MigrationTest.java", "package a;\nclass MigrationTest { void t() { Migration.migrate(p); } }\n")
	write("core/src/test/kotlin/a/OtherTest.kt", "class OtherTest\n")

	got, err := New().ExtractTestRefs(context.Background(), dir,
		[]string{"core/src/test/java/a/MigrationTest.java", "core/src/test/kotlin/a/OtherTest.kt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d facts, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Kind != facts.KindTestRef || f.Name != "core/src/test/java/a/MigrationTest.java" || f.File != f.Name {
		t.Errorf("fact = %s %q %q, want a test_ref named by its file", f.Kind, f.Name, f.File)
	}
	if f.PropString("language") != "java" {
		t.Errorf("language = %q, want java", f.PropString("language"))
	}
	targets := map[string]bool{}
	for _, r := range f.Relations {
		if r.Kind != facts.RelCalls {
			t.Errorf("relation kind %q, want only calls", r.Kind)
		}
		targets[r.Target] = true
	}
	if !targets["Migration.migrate"] {
		t.Errorf("missing Migration.migrate in %+v", f.Relations)
	}
}
