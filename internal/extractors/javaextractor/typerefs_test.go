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

// TestJavaReturnAndLocalTypesNameTheType: a method's return type, its type
// arguments included, and the declared type of a local, an enhanced-for variable
// and a try-with-resources resource are uses of the type. A method's own type
// parameter and `var` name nothing.
func TestJavaReturnAndLocalTypesNameTheType(t *testing.T) {
	files := map[string]string{
		"rest/src/main/java/com/example/rest/Result.java":  "package com.example.rest;\npublic class Result {}\n",
		"rest/src/main/java/com/example/rest/Wrapped.java": "package com.example.rest;\npublic class Wrapped {}\n",
		"rest/src/main/java/com/example/rest/Local.java":   "package com.example.rest;\npublic class Local {}\n",
		"rest/src/main/java/com/example/rest/Elem.java":    "package com.example.rest;\npublic class Elem {}\n",
		"rest/src/main/java/com/example/rest/Each.java":    "package com.example.rest;\npublic class Each {}\n",
		"rest/src/main/java/com/example/rest/Res.java":     "package com.example.rest;\npublic class Res implements AutoCloseable { public void close() {} }\n",
		"rest/src/main/java/com/example/rest/T.java":       "package com.example.rest;\npublic class T {}\n",
		"svc/src/main/java/com/example/svc/Api.java": `package com.example.svc;
import com.example.rest.*;
import java.util.List;
import java.util.Optional;
public class Api {
    Result result() { return null; }
    Optional<Wrapped> wrapped() { return Optional.empty(); }
    void locals(List<Object> xs) {
        Local l = null;
        List<Elem> elems = null;
        for (Each e : this.<Each>none()) {}
        try (Res r = open()) {}
        var v = l;
    }
    <T> T generic() { return null; }
    <E> List<E> none() { return null; }
    Res open() { return null; }
}
`,
	}
	ff := extractAll(t, files)
	const api = "svc/src/main/java/com/example/svc.Api"
	const rest = "rest/src/main/java/com/example/rest"
	assertNames(t, ff, []struct{ owner, target, how string }{
		{api + ".result", rest + ".Result", "a return type"},
		{api + ".wrapped", rest + ".Wrapped", "a type argument of a return type"},
		{api + ".locals", rest + ".Local", "a local's declared type"},
		{api + ".locals", rest + ".Elem", "a type argument of a local's type"},
		{api + ".locals", rest + ".Each", "an enhanced-for variable's type"},
		{api + ".locals", rest + ".Res", "a resource's type"},
	})
	g, ok := findFactKind(ff, facts.KindSymbol, api+".generic")
	if !ok {
		t.Fatal("no symbol Api.generic")
	}
	if hasRelation(g, facts.RelNames, rest+".T") {
		t.Errorf("generic names %s.T: a method type parameter bound to a repository type", rest)
	}
}
