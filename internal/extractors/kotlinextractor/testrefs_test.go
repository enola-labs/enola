package kotlinextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

var _ plugin.TestRefExtractor = (*KotlinExtractor)(nil)

func kotlinTestRefTargets(t *testing.T, relFile, src string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range testRefsFromFile([]byte(src), relFile) {
		for _, r := range f.Relations {
			out[r.Target] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestKotlinTestRefsCaptureProductionReferences pins what the pass is for: an
// accessor written for the module's tests, an object the tests drive, and a member
// reached through a local all keep an inbound edge once test sources are ignored.
func TestKotlinTestRefsCaptureProductionReferences(t *testing.T) {
	src := `package okhttp3

import okhttp3.internal.buildCache
import okhttp3.internal.finishedAccessor
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.Assertions.assertEquals

class CacheTest {
  private lateinit var cache: Cache

  @Test fun corrupt() = runTest {
    cache = buildCache("/cache/".toPath(), Int.MAX_VALUE.toLong(), fileSystem)
    val migrated = ListToMapMigration.migrate(prefs)
    assertEquals(true, migrated.hasDoneListToMapMigration)
    dispatcher.finishedAccessor(call)
    val parse = ::parseCookie
    if (cache is DiskCache) { }
    navigator.navigate(BookmarksNavKey)
    val keys = mapOf(ForYouNavKey to 1)
    assertEquals(InterestsNavKey, navigator.current)
  }
}
`
	got := kotlinTestRefTargets(t, "okhttp/src/jvmTest/kotlin/okhttp3/CacheTest.kt", src)
	for _, want := range []string{
		"buildCache",                  // an imported top-level accessor, called bare
		"ListToMapMigration.migrate",  // a call on an object; orphans folds the receiver into the type
		"migrate",                     // and the bare member
		"hasDoneListToMapMigration",   // a property read off a local
		"dispatcher.finishedAccessor", // an extension called on a local
		"finishedAccessor",            // and its bare form
		"parseCookie",                 // a callable reference
		"Cache",                       // a declared type
		"DiskCache",                   // an is-check
		"BookmarksNavKey",             // an object passed as an argument
		"ForYouNavKey",                // an object in a `to` pair
		"InterestsNavKey",             // an object compared in an assertion
	} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedKeys(got))
		}
	}
}

// TestKotlinTestRefsDropHarnessAndOwnDeclarations keeps the reference set about the
// code under test: the harness's words, a harness receiver's members, imports,
// annotations, stdlib builtins and the test file's own helpers never vouch for a
// production symbol of the same name.
func TestKotlinTestRefsDropHarnessAndOwnDeclarations(t *testing.T) {
	src := `package com.example

import com.example.data.verify
import io.mockk.every
import kotlin.test.assertTrue

@RunWith(AndroidJUnit4::class)
class RepoTest {
  private val fake = FakeStore()

  @Before fun setUp() { helper() }

  @Test fun loads() = runTest {
    every { store.load() } returns listOf(1)
    Mockito.verify(store).save()
    assertThat(repo.items()).isEqualTo(emptyList())
    assertTrue(fake.wasCalled)
    composeTestRule.onNodeWithText("x").performClick()
  }

  @Test fun histogram() {
    val histogram = HistogramConfig()
    histogram.setPercentiles(null)
  }

  private fun helper() {}
}

class FakeStore { val wasCalled = true; fun reset() {} }
`
	got := kotlinTestRefTargets(t, "app/src/test/kotlin/com/example/RepoTest.kt", src)
	for _, banned := range []string{
		"every", "returns", "listOf", "runTest", "assertThat", "isEqualTo", "assertTrue",
		"emptyList", "Mockito.verify", "verify", // a harness receiver drops the bare name too
		"RunWith", "Test", "Before", "AndroidJUnit4", // annotations
		"helper", "FakeStore", "setUp", // the file's own declarations, called or named bare
		"composeTestRule.onNodeWithText", "onNodeWithText", "performClick",
		"com.example.data.verify", // an import alone is not a use
	} {
		if got[banned] {
			t.Errorf("harness or own name %q recorded; have %v", banned, sortedKeys(got))
		}
	}
	for _, want := range []string{"store.load", "load", "save", "repo.items", "items",
		"histogram.setPercentiles", "setPercentiles", // a local named like a test function is still a receiver
	} {
		if !got[want] {
			t.Errorf("missing reference %q; have %v", want, sortedKeys(got))
		}
	}
}

// TestKotlinExtractTestRefsEmitsOnlyTestRefFacts checks the plugin.TestRefExtractor
// contract end to end: one test_ref fact per Kotlin test file, named by the file,
// carrying only calls relations; files the pass does not own are skipped.
func TestKotlinExtractTestRefsEmitsOnlyTestRefFacts(t *testing.T) {
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
	write("core/src/test/kotlin/a/MigrationTest.kt", "package a\nclass MigrationTest { fun t() { ListToMapMigration.migrate(p) } }\n")
	write("core/src/test/java/a/OtherTest.java", "class OtherTest {}\n")

	got, err := New().ExtractTestRefs(context.Background(), dir,
		[]string{"core/src/test/kotlin/a/MigrationTest.kt", "core/src/test/java/a/OtherTest.java"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d facts, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Kind != facts.KindTestRef || f.Name != "core/src/test/kotlin/a/MigrationTest.kt" || f.File != f.Name {
		t.Errorf("fact = %s %q %q, want a test_ref named by its file", f.Kind, f.Name, f.File)
	}
	if f.PropString("language") != "kotlin" {
		t.Errorf("language = %q, want kotlin", f.PropString("language"))
	}
	for _, r := range f.Relations {
		if r.Kind != facts.RelCalls {
			t.Errorf("relation kind %q, want only calls", r.Kind)
		}
	}
	if !kotlinRelTargets(f)["ListToMapMigration.migrate"] {
		t.Errorf("missing ListToMapMigration.migrate in %+v", f.Relations)
	}
}

func kotlinRelTargets(f facts.Fact) map[string]bool {
	out := map[string]bool{}
	for _, r := range f.Relations {
		out[r.Target] = true
	}
	return out
}
