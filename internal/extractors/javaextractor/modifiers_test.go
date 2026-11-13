package javaextractor

// Java keywords are read from the `modifiers` node, not from that node's text.
//
// The text span covers annotation arguments as well as keywords, and Java spells things
// inside those arguments exactly like modifiers. Two traps, both hit in practice: javac's
// lint categories — `@SuppressWarnings("static")`, the routine way to silence "the static
// method … should be accessed in a static way" — marked every method and field it
// annotated as `static`; and plain data — a column named `final_price`, a named query
// called `public.find` — made ordinary fields look like constants or exported
// declarations. Nothing downstream can tell a keyword from a string that happens to
// contain one.

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestJavaStaticModifierIsNotAnAnnotationArgument(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/Silent.java": `
package com.acme;

public class Silent {
  @SuppressWarnings("static")
  public void instanceMethod() {}

  @SuppressWarnings({"static", "unchecked"})
  public static void staticMethod() {}

  @SuppressWarnings("static")
  public final String LOUD = "shout";

  public static final String QUIET = "whisper";
}
`,
	})

	look := func(suffix string) facts.Fact {
		t.Helper()
		for _, f := range ff {
			if strings.HasSuffix(f.Name, suffix) {
				return f
			}
		}
		t.Fatalf("no fact ending in %q", suffix)
		return facts.Fact{}
	}

	inst := look(".Silent.instanceMethod")
	if s, ok := inst.Props["static"].(bool); ok && s {
		t.Error("instanceMethod: static = true, want absent — the only static here is a @SuppressWarnings category")
	}

	stat := look(".Silent.staticMethod")
	if s, _ := stat.Props["static"].(bool); !s {
		t.Errorf("staticMethod: static = %v, want true — a real modifier still counts next to the annotation", stat.Props["static"])
	}

	// The same text bug fed the static-constant value capture, which is what lets
	// `@Table(name = QUIET)` resolve to a field later. A final-but-not-static field
	// used to be captured as if it were a static constant.
	loud := look(".Silent.LOUD")
	if v, ok := loud.Props["value"].(string); ok && v != "" {
		t.Errorf("LOUD: value = %q, want unset — a final String that is not static is not a static constant", v)
	}
	quiet := look(".Silent.QUIET")
	if v, _ := quiet.Props["value"].(string); v != "whisper" {
		t.Errorf("QUIET: value = %q, want the literal — the static-constant capture must survive", quiet.Props["value"])
	}
}

// The same trap, hit through a JPA column name rather than a lint category. Annotation
// arguments sit inside the `modifiers` text span, and real Java writes modifier words into
// them all the time: a column called `final_price`, a named query called `public.find`.
// Both used to be read as keywords, and the damage is user-visible: an ordinary private
// field was recorded as a constant, and a private field carrying `@NamedQuery(name =
// "public.find")` was recorded as exported — which is the prop that tells a reader
// "may be used outside this snapshot, verify before deleting".
func TestJavaKeywordsAreNotReadFromAnnotationArguments(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/Item.java": `
package com.acme;

public class Item {
  @Column(name = "final_price")
  private String price;

  @NamedQuery(name = "public.find", query = "select x")
  private static final String QUERY = "select x";

  private String plain;
}
`,
	})

	look := func(suffix string) facts.Fact {
		t.Helper()
		for _, f := range ff {
			if strings.HasSuffix(f.Name, suffix) {
				return f
			}
		}
		t.Fatalf("no fact ending in %q", suffix)
		return facts.Fact{}
	}

	price := look(".Item.price")
	if sk, _ := price.Props["symbol_kind"].(string); sk != facts.SymbolVariable {
		t.Errorf("price: symbol_kind = %q, want %q — `final` inside a column name is not a modifier", sk, facts.SymbolVariable)
	}

	q := look(".Item.QUERY")
	if sk, _ := q.Props["symbol_kind"].(string); sk != facts.SymbolConstant {
		t.Errorf("QUERY: symbol_kind = %q, want %q — a real static final is still a constant", sk, facts.SymbolConstant)
	}
	if e, _ := q.Props["exported"].(bool); e {
		t.Error("QUERY: exported = true, want false — the field is private, and the only `public` here is a query name")
	}
	if v, _ := q.Props["value"].(string); v != "select x" {
		t.Errorf("QUERY: value = %q, want the literal — the keyword fix must not drop the constant capture", q.Props["value"])
	}

	plain := look(".Item.plain")
	if sk, _ := plain.Props["symbol_kind"].(string); sk != facts.SymbolVariable {
		t.Errorf("plain: symbol_kind = %q, want %q", sk, facts.SymbolVariable)
	}
}
