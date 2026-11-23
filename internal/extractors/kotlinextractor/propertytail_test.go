package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func symbolNames(all []facts.Fact) []string {
	var got []string
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			got = append(got, f.Name)
		}
	}
	return got
}

// A local property followed by a get( or set( call keeps its function and the
// function's calls: the grammar alone reads the call as the property's accessor
// and loses the function.
func TestLocalPropertyThenGetCallKeepsFunction(t *testing.T) {
	src := "package app\n" +
		"fun withGet() {\n" +
		"    val s: Svc by inject()\n" +
		"    get(\"x\") { s.one() }\n" +
		"}\n" +
		"fun withInit() {\n" +
		"    val s: Svc = Svc()\n" +
		"    get(\"y\") { s.three() }\n" +
		"}\n" +
		"fun withSet() {\n" +
		"    val m = mutableMapOf<String, Int>()\n" +
		"    set(\"k\", 1)\n" +
		"}\n" +
		"class Svc {\n" +
		"    fun one() {}\n" +
		"    fun three() {}\n" +
		"    val name: String\n" +
		"        get() = \"svc\"\n" +
		"    var size: Int = 0\n" +
		"        set(value) { field = value }\n" +
		"}\n"
	all := extractAST(t, src, false)
	for _, name := range []string{"pkg.withGet", "pkg.withInit", "pkg.withSet", "pkg.Svc", "pkg.Svc.one", "pkg.Svc.three"} {
		if _, ok := findFact(all, name); !ok {
			t.Fatalf("no %s in %v", name, symbolNames(all))
		}
	}
	f, _ := findFact(all, "pkg.withInit")
	if !hasRelation(f, "calls", "three") {
		t.Errorf("withInit lost its call to Svc.three: %v", f.Relations)
	}

	// Accessors keep their reading; strings and comments keep their text.
	for _, keep := range []string{
		"val name: String\n    get() = \"x\"\n",
		"var size: Int = 0\n    set(value) { field = value }\n",
		"var size: Int = 0\n    set(value: Int) { field = value }\n",
		"val x = \"\"\"\n    get(\"a\")\n\"\"\"\n",
		"val x = 1 // c\n/*\n    get(\"a\")\n*/\n",
		"val x = foo(\n    get(\"a\"))\n",
		"val x =\n    get(\"a\")\n",
	} {
		if got, changed := separatePropertyTails([]byte(keep)); changed {
			t.Errorf("rewrote %q to %q", keep, got)
		}
	}
	if got, _ := separatePropertyTails([]byte("val s: Svc by inject()\n    get(\"x\") {}\n")); string(got) != "val s: Svc by inject()\n   ;get(\"x\") {}\n" {
		t.Errorf("get call not separated: %q", got)
	}
}

// An explicit backing field (Kotlin 2 `field = ...`) keeps its class, the class's
// members and the field initializer's calls.
func TestExplicitBackingFieldKeepsClass(t *testing.T) {
	src := "package app\n" +
		"class WithField(private val api: Api) {\n" +
		"    val first: Int = api.a()\n" +
		"    val state: List<Int>\n" +
		"        field = mutableListOf(api.c())\n" +
		"    fun run(): Int = api.b()\n" +
		"}\n" +
		"class Api {\n" +
		"    fun a(): Int = 1\n" +
		"    fun b(): Int = 2\n" +
		"    fun c(): Int = 3\n" +
		"}\n"
	all := extractAST(t, src, false)
	for _, name := range []string{"pkg.WithField", "pkg.WithField.run", "pkg.Api"} {
		if _, ok := findFact(all, name); !ok {
			t.Fatalf("no %s in %v", name, symbolNames(all))
		}
	}
	called := map[string]bool{}
	for _, f := range all {
		for _, r := range f.Relations {
			if r.Kind == "calls" {
				called[r.Target] = true
			}
		}
	}
	for _, target := range []string{"a", "b", "c"} {
		if !called[target] {
			t.Errorf("no call to %s", target)
		}
	}

	// A setter body assigning the field keeps its text.
	for _, keep := range []string{
		"var size: Int = 0\n    set(value) {\n        field = value\n    }\n",
		"var size: Int = 0\n    set(value) {\n        if (value > 0)\n            field = value\n    }\n",
		"val ok = x\n    field = 1\n",
	} {
		if got, changed := separatePropertyTails([]byte(keep)); changed {
			t.Errorf("rewrote %q to %q", keep, got)
		}
	}
}
