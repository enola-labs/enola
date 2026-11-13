package javaextractor

// `static` is read from the modifiers node, not from the modifier text.
//
// That text span covers annotation arguments as well as keywords, and javac spells
// its lint categories like modifiers. `@SuppressWarnings("static")` — the routine
// way to silence "the static method … should be accessed in a static way" — therefore
// marked every method and field it annotated as `static`, and nothing downstream can
// tell a lint category from a real modifier.

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
