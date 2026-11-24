package kotlinextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestKotlinTestRefsRecordRepositoryAnnotations: an annotation a test applies is a
// use of its type when its name resolves, through the file's imports or its own
// package, to a type production code declares. The harness's annotations, default
// imports, and a library annotation that shares a name with a production type
// record nothing.
func TestKotlinTestRefsRecordRepositoryAnnotations(t *testing.T) {
	prod := map[string]bool{
		"com.ex.lib.SpecScope":     true, // same package as the test
		"com.ex.di.Qualifier":      true, // imported
		"com.ex.di.Named":          true, // imported under an alias
		"com.ex.lib.Outer":         true, // holds a nested annotation
		"com.ex.di.Full":           true, // written fully qualified
		"com.ex.other.Config":      true, // shares the name of a library annotation
		"com.ex.lib.Deprecated":    true, // shares a default-imported name
		"com.ex.shared.Wildcarded": true,
	}
	src := `package com.ex.lib

import org.junit.Test
import org.junit.Rule
import org.robolectric.annotation.Config
import com.ex.di.Qualifier
import com.ex.di.Named as Tagged
import com.ex.shared.*

@Config(sdk = [33])
class ServiceTest {
  @get:Rule val folder = TemporaryFolder()

  @SpecScope @Qualifier @Tagged @Outer.Inner @com.ex.di.Full(1) @Wildcarded
  @Deprecated("x") @Test
  fun runs() {}

  @LocalMarker fun other() {}
}

annotation class LocalMarker
`
	got := map[string]bool{}
	for _, f := range testRefsFromFile([]byte(src), "lib/src/test/kotlin/com/ex/lib/ServiceTest.kt", prod) {
		for _, r := range f.Relations {
			got[r.Target] = true
		}
	}
	for _, want := range []string{"SpecScope", "Qualifier", "Named", "Inner", "Full", "Wildcarded"} {
		if !got[want] {
			t.Errorf("missing annotation use %q; have %v", want, sortedKeys(got))
		}
	}
	for _, banned := range []string{"Test", "Rule", "Config", "Deprecated", "LocalMarker", "Tagged"} {
		if got[banned] {
			t.Errorf("annotation %q recorded; have %v", banned, sortedKeys(got))
		}
	}
}

// TestKotlinExtractTestRefsResolvesAnnotationsAgainstProductionSources checks the
// production type set is read from prodFiles: an annotation class declared in
// main code and applied only by a test is a test use, one declared nowhere is not.
func TestKotlinExtractTestRefsResolvesAnnotationsAgainstProductionSources(t *testing.T) {
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
	write("lib/src/main/kotlin/com/ex/lib/Markers.kt", "package com.ex.lib\n\n@Retention(AnnotationRetention.RUNTIME)\nannotation class SpecScope\n\nclass Service { fun run() = 1 }\n")
	write("lib/src/main/java/com/ex/lib/JavaMarker.java", "package com.ex.lib;\n\npublic @interface JavaMarker {}\n")
	write("lib/src/test/kotlin/com/ex/lib/ServiceTest.kt", "package com.ex.lib\n\nimport org.junit.Test\n\nclass ServiceTest {\n  @SpecScope @JavaMarker @Elsewhere @Test\n  fun runs() { Service().run() }\n}\n")

	got, err := New().ExtractTestRefs(context.Background(), dir,
		[]string{"lib/src/test/kotlin/com/ex/lib/ServiceTest.kt"},
		[]string{"lib/src/main/kotlin/com/ex/lib/Markers.kt", "lib/src/main/java/com/ex/lib/JavaMarker.java"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d facts, want 1", len(got))
	}
	targets := kotlinRelTargets(got[0])
	for _, want := range []string{"SpecScope", "JavaMarker", "Service", "run"} {
		if !targets[want] {
			t.Errorf("missing %q in %v", want, sortedKeys(targets))
		}
	}
	for _, banned := range []string{"Elsewhere", "Test"} {
		if targets[banned] {
			t.Errorf("%q recorded in %v", banned, sortedKeys(targets))
		}
	}
}
