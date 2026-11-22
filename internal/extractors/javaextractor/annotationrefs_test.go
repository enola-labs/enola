package javaextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// annotationRepo declares annotation types in three packages and uses each the ways
// Java resolves a name: same package, single-type import, on-demand import, fully
// qualified, and a member type of the enclosing class.
var annotationRepo = map[string]string{
	"page/src/main/java/com/example/page/Menu.java": `package com.example.page;
public @interface Menu { String name(); }
`,
	"page/src/main/java/com/example/page/HomePage.java": `package com.example.page;
@Menu(name = "home")
public class HomePage {}
`,
	"rest/src/main/java/com/example/rest/Param.java": `package com.example.rest;
public @interface Param { String value() default ""; }
`,
	"rest/src/main/java/com/example/rest/Override.java": `package com.example.rest;
public @interface Override {}
`,
	"rest/src/main/java/com/example/rest/Test.java": `package com.example.rest;
public @interface Test {}
`,
	"svc/src/main/java/com/example/svc/Svc.java": `package com.example.svc;
import com.example.rest.Param;
import org.springframework.stereotype.Service;
import org.junit.Test;
@Service
public class Svc {
    public String find(@Param("id") String id) { return id; }
    @Override
    public String toString() { return ""; }
    @Test
    void check() {}
}
`,
	"svc/src/main/java/com/example/svc/Wild.java": `package com.example.svc;
import com.example.rest.*;
public class Wild {
    @Param
    private String field;
    @com.example.page.Menu(name = "wild")
    void menu() {}
}
`,
	"nest/src/main/java/com/example/nest/Outer.java": `package com.example.nest;
public class Outer {
    public @interface Inner {}
    @Inner
    void a() {}
}
`,
	"nest/src/main/java/com/example/nest/User.java": `package com.example.nest;
@Outer.Inner
public class User {}
`,
}

const (
	menuCanon  = "page/src/main/java/com/example/page.Menu"
	paramCanon = "rest/src/main/java/com/example/rest.Param"
	innerCanon = "nest/src/main/java/com/example/nest.Outer.Inner"
)

// TestJavaAnnotationUseNamesTheAnnotationType: an annotation type used only as an
// annotation is referenced by what it annotates, resolved as javac resolves the name.
func TestJavaAnnotationUseNamesTheAnnotationType(t *testing.T) {
	ff := extractAll(t, annotationRepo)
	for _, tc := range []struct{ owner, target, how string }{
		{"page/src/main/java/com/example/page.HomePage", menuCanon, "same package, on the class"},
		{"svc/src/main/java/com/example/svc.Svc.find", paramCanon, "single-type import, on a parameter"},
		{"svc/src/main/java/com/example/svc.Wild", paramCanon, "on-demand import, on a field"},
		{"svc/src/main/java/com/example/svc.Wild.menu", menuCanon, "fully qualified, on a method"},
		{"nest/src/main/java/com/example/nest.Outer.a", innerCanon, "member type of the enclosing class"},
		{"nest/src/main/java/com/example/nest.User", innerCanon, "qualified by its enclosing type"},
	} {
		f, ok := findFactKind(ff, facts.KindSymbol, tc.owner)
		if !ok {
			t.Fatalf("no symbol %s", tc.owner)
		}
		if !hasRelation(f, facts.RelNames, tc.target) {
			t.Errorf("%s (%s): want names -> %s, got %v", tc.owner, tc.how, tc.target, f.Relations)
		}
	}
}

// TestJavaAnnotationUseDoesNotBindFrameworkNames: a JDK or framework annotation
// never binds to a same-named repository type. An import of another package
// shadows it, and java.lang's own annotations are not looked up in the repository.
func TestJavaAnnotationUseDoesNotBindFrameworkNames(t *testing.T) {
	ff := extractAll(t, annotationRepo)
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelNames {
				continue
			}
			switch r.Target {
			case "rest/src/main/java/com/example/rest.Override", "rest/src/main/java/com/example/rest.Test":
				t.Errorf("%s names %s: a JDK or framework annotation bound to a repository type", f.Name, r.Target)
			case menuCanon, paramCanon, innerCanon:
			default:
				t.Errorf("%s names %s: only resolved repository annotation types may be named", f.Name, r.Target)
			}
		}
	}
}

// TestJavaTestRefsCountRepositoryAnnotations: an annotation a test applies is a test
// reference when the name resolves to a production type of the repository, and only
// then: @Test from JUnit stays the harness even when production declares a Test.
func TestJavaTestRefsCountRepositoryAnnotations(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for k, v := range annotationRepo {
		files[k] = v
	}
	files["page/src/test/java/com/example/page/HomePageTest.java"] = `package com.example.page;
import com.example.rest.Param;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.*;
class HomePageTest {
    @Menu(name = "t")
    static class Fixture {}
    @Test
    @DisplayName("x")
    void renders(@Param("p") String p) {}
    @Override
    public String toString() { return ""; }
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
		if filepath.Base(rel) == "HomePageTest.java" {
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
	for _, want := range []string{"Menu", "Param"} {
		if !got[want] {
			t.Errorf("test annotation %s not recorded as a test ref; got %v", want, sortedTargets(got))
		}
	}
	for _, no := range []string{"Test", "DisplayName", "Override"} {
		if got[no] {
			t.Errorf("harness annotation %s recorded as a test ref", no)
		}
	}
}
