package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Two classes whose bodies close on the line their member ends on both keep their
// symbols: the grammar alone reads the first as unfinished and loses the file.
// Strings, comments and lambdas keep their text.
func TestSameLineClosesKeepTheFile(t *testing.T) {
	src := "package app\n" +
		"class Helper { fun work() {} }\n" +
		"class Repo { val x = \"a }\"; fun save() { log { it } } } // b }\n" +
		"fun main() { Helper().work() }\n"
	all := extractAST(t, src, false)
	for _, name := range []string{"pkg.Helper", "pkg.Helper.work", "pkg.Repo", "pkg.Repo.save", "pkg.main"} {
		if _, ok := findFact(all, name); !ok {
			var got []string
			for _, f := range all {
				if f.Kind == facts.KindSymbol {
					got = append(got, f.Name)
				}
			}
			t.Fatalf("no %s in %v", name, got)
		}
	}
	if got, want := separateSameLineCloses([]byte(`val s = "x }"; f { a -> } /* y } */`)); string(got) != `val s = "x }"; f { a -> } /* y } */` || want {
		t.Errorf("rewrote %q", got)
	}
}
