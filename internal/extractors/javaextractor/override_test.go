package javaextractor

// Java records `override` from the `@Override` annotation and from nothing else.
//
// The annotation is a compiler-checked claim — javac rejects it on a method that
// overrides nothing — so it is a fact rather than a guess, and it is what keeps an
// override out of find_orphans: an overriding method is reached through its supertype
// and never by its own name. The annotation is also optional, so this is deliberately
// high precision with partial recall. Inferring the omitted ones would mean matching
// method names against the supertype graph, which decides dead-code output on a bare
// name match; nothing here pins that, because nothing here should imply it works.

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestJavaOverrideComesFromTheAnnotation(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/Shapes.java": `
package com.acme;

public class Shapes {
  @Override public String toString() { return "shapes"; }

  @java.lang.Override public int hashCode() { return 0; }

  @Override
  public boolean equals(Object o) { return true; }

  @Override public void draw() {}

  public void notAnOverride() {}
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

	for _, suffix := range []string{".toString", ".hashCode", ".equals", ".draw"} {
		f := look(suffix)
		if o, _ := f.Props["override"].(bool); !o {
			t.Errorf("%s: override = %v, want true", suffix, f.Props["override"])
		}
	}

	// The gate is the annotation, not "this looks like a lifecycle method": a plain
	// method beside them gets no prop, so the exclusion cannot leak onto ordinary code.
	plain := look(".notAnOverride")
	if o, ok := plain.Props["override"].(bool); ok && o {
		t.Error("notAnOverride: override = true, want absent — no annotation, no prop")
	}
}
