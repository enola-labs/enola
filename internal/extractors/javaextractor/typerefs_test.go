package javaextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// typeRefRepo uses repository types the ways that name a type without calling,
// instantiating, extending or annotating with it: a class literal, a static member
// read, and a declared parameter or field type.
var typeRefRepo = map[string]string{
	"rest/src/main/java/com/example/rest/Param.java": `package com.example.rest;
public @interface Param { String DEFAULT_NONE = "\n"; String value() default DEFAULT_NONE; }
`,
	"rest/src/main/java/com/example/rest/Keys.java": `package com.example.rest;
public interface Keys { String A = "a"; interface Nested { String B = "b"; } }
`,
	"rest/src/main/java/com/example/rest/Color.java": `package com.example.rest;
public enum Color { RED, BLUE }
`,
	"rest/src/main/java/com/example/rest/Service.java": `package com.example.rest;
public @interface Service {}
`,
	"rest/src/main/java/com/example/rest/Item.java": `package com.example.rest;
public class Item {}
`,
	"rest/src/main/java/com/example/rest/Holder.java": `package com.example.rest;
public class Holder {}
`,
	"rest/src/main/java/com/example/rest/Marker.java": `package com.example.rest;
public @interface Marker { Class<?> value(); }
`,
	"svc/src/main/java/com/example/svc/Resolver.java": `package com.example.svc;
import com.example.rest.Param;
import com.example.rest.Keys;
import com.example.rest.*;
import java.util.List;
@Marker(Item.class)
public class Resolver {
    private Holder holder;
    private static final Object TYPE = Service.class;
    public Resolver(Service service) {}
    boolean empty(String v) { return Param.DEFAULT_NONE.equals(v); }
    Object anno() { return find(Param.class); }
    String key() { return Keys.Nested.B; }
    String qualified() { return com.example.rest.Keys.A; }
    boolean red(Object c) { return c == Color.RED; }
    void all(List<Item> items) {}
    Object find(Class<?> c) { return c; }
}
`,
}

const (
	restDir     = "rest/src/main/java/com/example/rest"
	resolverDir = "svc/src/main/java/com/example/svc.Resolver"
)

// TestJavaClassLiteralNamesTheType: `X.class` is a use of X, in a method body, a
// field initializer and an annotation argument.
func TestJavaClassLiteralNamesTheType(t *testing.T) {
	ff := extractAll(t, typeRefRepo)
	assertNames(t, ff, []struct{ owner, target, how string }{
		{resolverDir + ".anno", restDir + ".Param", "in a method body"},
		{resolverDir, restDir + ".Service", "in a field initializer"},
		{resolverDir, restDir + ".Item", "in an annotation argument"},
	})
}

// TestJavaStaticMemberReadNamesTheType: reading a static member through its type
// (`Param.DEFAULT_NONE`, an enum constant, a nested type's constant, a fully
// qualified constant) is a use of that type.
func TestJavaStaticMemberReadNamesTheType(t *testing.T) {
	ff := extractAll(t, typeRefRepo)
	assertNames(t, ff, []struct{ owner, target, how string }{
		{resolverDir + ".empty", restDir + ".Param", "a constant of an annotation type"},
		{resolverDir + ".red", restDir + ".Color", "an enum constant"},
		{resolverDir + ".key", restDir + ".Keys.Nested", "a nested type's constant"},
		{resolverDir + ".key", restDir + ".Keys", "the nested type's enclosing type"},
		{resolverDir + ".qualified", restDir + ".Keys", "fully qualified"},
	})
}

// TestJavaParameterAndFieldTypesNameTheType: a declared parameter or field type,
// its type arguments included, is a use of the type.
func TestJavaParameterAndFieldTypesNameTheType(t *testing.T) {
	ff := extractAll(t, typeRefRepo)
	assertNames(t, ff, []struct{ owner, target, how string }{
		{resolverDir + ".Resolver", restDir + ".Service", "a constructor parameter"},
		{resolverDir + ".all", restDir + ".Item", "a type argument of a parameter"},
		{resolverDir, restDir + ".Holder", "a field type"},
	})
}

// TestJavaTypeRefsDoNotBindShadowedNames: a name javac resolves to something else
// names no repository type: a local or field of that name (a variable obscures a
// type), a type parameter, a single-type import of another package, and a type of
// another package the file does not import.
func TestJavaTypeRefsDoNotBindShadowedNames(t *testing.T) {
	files := map[string]string{}
	for k, v := range typeRefRepo {
		files[k] = v
	}
	files["other/src/main/java/com/example/other/Shadow.java"] = `package com.example.other;
import org.external.Service;
import com.example.rest.*;
public class Shadow {
    private Object Holder;
    <Item> void generic(Item item) {}
    void imported(Service s) {}
    Object local() { Object Keys = null; return Keys.hashCode; }
    Object param(Object Color) { return Color.RED; }
    Object field() { return Holder.hashCode; }
}
`
	files["other/src/main/java/com/example/other/Unimported.java"] = `package com.example.other;
public class Unimported {
    void unimported(Color c) { Object o = Color.RED; }
}
`
	ff := extractAll(t, files)
	for _, g := range ff {
		if g.Kind != facts.KindSymbol || !strings.HasPrefix(g.Name, "other/") {
			continue
		}
		for _, r := range g.Relations {
			if r.Kind == facts.RelNames {
				t.Errorf("%s names %s: a shadowed or unimported name bound to a repository type", g.Name, r.Target)
			}
		}
	}
}

func assertNames(t *testing.T, ff []facts.Fact, cases []struct{ owner, target, how string }) {
	t.Helper()
	for _, tc := range cases {
		f, ok := findFactKind(ff, facts.KindSymbol, tc.owner)
		if !ok {
			t.Fatalf("no symbol %s", tc.owner)
		}
		if !hasRelation(f, facts.RelNames, tc.target) {
			t.Errorf("%s (%s): want names -> %s, got %v", tc.owner, tc.how, tc.target, f.Relations)
		}
	}
}
