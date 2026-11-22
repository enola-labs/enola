package javaextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// staticImportRepo declares a constant holder, a static utility class, an enum and
// a nested type, and a reader that reaches each only through a static import.
var staticImportRepo = map[string]string{
	"rest/src/main/java/com/example/rest/Keys.java": `package com.example.rest;
public interface Keys { String A = "a"; }
`,
	"rest/src/main/java/com/example/rest/Util.java": `package com.example.rest;
public class Util { public static boolean isStatic(int m) { return true; } }
`,
	"rest/src/main/java/com/example/rest/Color.java": `package com.example.rest;
public enum Color { RED, BLUE }
`,
	"rest/src/main/java/com/example/rest/Outer.java": `package com.example.rest;
public class Outer { public static class Inner { public static final int K = 1; } public static class Nested {} }
`,
	"svc/src/main/java/com/example/svc/Reader.java": `package com.example.svc;
import static com.example.rest.Keys.A;
import static com.example.rest.Util.*;
import static com.example.rest.Color.RED;
import static com.example.rest.Outer.Inner.K;
import static com.example.rest.Outer.Nested;
import static java.util.Objects.requireNonNull;
import static org.junit.Assert.*;
public class Reader {
    String key() { return A; }
    boolean s() { return isStatic(K); }
    Object c() { return requireNonNull(RED); }
}
`,
	// A file that statically imports only its own nested type and a library member.
	"svc/src/main/java/com/example/svc/Self.java": `package com.example.svc;
import static com.example.svc.Self.Mode.IN;
import static java.util.Objects.requireNonNull;
public class Self { enum Mode { IN, OUT } Object m() { return requireNonNull(IN); } }
`,
}

const readerFile = "svc/src/main/java/com/example/svc/Reader.java"

// TestJavaStaticImportNamesTheType: `import static T.member` and `import static T.*`
// are a use of the type T by the importing file, for a constant holder, a static
// utility class, an enum and a nested type. A static import of a type the
// repository does not declare names nothing.
func TestJavaStaticImportNamesTheType(t *testing.T) {
	ff := extractAll(t, staticImportRepo)
	ref, ok := findFactKind(ff, facts.KindFileRef, readerFile)
	if !ok {
		t.Fatalf("no file reference fact for %s", readerFile)
	}
	want := map[string]bool{
		restDir + ".Keys":        true, // a single constant
		restDir + ".Util":        true, // on demand
		restDir + ".Color":       true, // an enum constant
		restDir + ".Outer.Inner": true, // a nested type's constant
		restDir + ".Outer":       true, // a static member type of Outer
	}
	got := map[string]bool{}
	for _, r := range ref.Relations {
		if r.Kind != facts.RelNames {
			t.Errorf("unexpected relation %s -> %s", r.Kind, r.Target)
			continue
		}
		got[r.Target] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("static import: want names -> %s, got %v", w, ref.Relations)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("static import named %s, which no import of the file names as a repository type", g)
		}
	}
}

// TestJavaStaticImportOfOwnTypeNamesNothing: a file importing its own nested type's
// members does not use itself, and one whose static imports all name library types
// keeps no reference fact.
func TestJavaStaticImportOfOwnTypeNamesNothing(t *testing.T) {
	ff := extractAll(t, staticImportRepo)
	if ref, ok := findFactKind(ff, facts.KindFileRef, "svc/src/main/java/com/example/svc/Self.java"); ok {
		t.Errorf("self and library static imports produced %+v", ref)
	}
}

// TestJavaTestRefsCountStaticImports: a test reaching a production type only through
// a static import records that type, by its declaration's name; a static import of
// the harness or a library records nothing.
func TestJavaTestRefsCountStaticImports(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for k, v := range staticImportRepo {
		files[k] = v
	}
	testFile := "svc/src/test/java/com/example/svc/ReaderTest.java"
	files[testFile] = `package com.example.svc;
import static com.example.rest.Keys.A;
import static com.example.rest.Outer.Inner.*;
import static org.junit.jupiter.api.Assertions.*;
import static java.util.Objects.requireNonNull;
class ReaderTest {
    void t() { assertEquals(A, String.valueOf(K)); }
}
`
	var prod, tests []string
	for rel, src := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if rel == testFile {
			tests = append(tests, rel)
		} else {
			prod = append(prod, rel)
		}
	}
	ff, err := New().ExtractTestRefs(context.Background(), dir, tests, prod)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range ff {
		for _, r := range f.Relations {
			got[r.Target] = true
		}
	}
	for _, want := range []string{restDir + ".Keys", restDir + ".Outer.Inner"} {
		if !got[want] {
			t.Errorf("static import of %s not recorded as a test ref; got %v", want, sortedTargets(got))
		}
	}
	for _, bad := range []string{"Assertions", "Objects", "org.junit.jupiter.api.Assertions", "java.util.Objects"} {
		if got[bad] {
			t.Errorf("library static import %s recorded as a test ref", bad)
		}
	}
}
