package kotlinextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// recursiveBySuffix reports the recursive_self flag of the one symbol whose name ends
// in suffix, failing the test when there is not exactly one.
func recursiveBySuffix(t *testing.T, ff []facts.Fact, suffix string) bool {
	t.Helper()
	var hit []facts.Fact
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			hit = append(hit, f)
		}
	}
	if len(hit) != 1 {
		var names []string
		for _, f := range ff {
			names = append(names, f.Name)
		}
		t.Fatalf("want one symbol ending %q, got %d of %v", suffix, len(hit), names)
	}
	return hit[0].Props["recursive_self"] == true
}

// A Gradle convention plugin's apply(target) calls Project.apply(plugin = …) through
// a `with` receiver: same name, same arity, a different function. The named argument
// is the proof, because `plugin` is not a parameter of the function it sits in.
func TestKtRecursion_ForeignNamedArgumentIsNotSelfCall(t *testing.T) {
	ff := extractAST(t, `
class LibraryConventionPlugin : Plugin<Project> {
    override fun apply(target: Project) {
        with(target) {
            apply(plugin = "com.android.library")
            apply(plugin = "org.jetbrains.kotlin.android")
        }
    }
}
`, false)
	if recursiveBySuffix(t, ff, "LibraryConventionPlugin.apply") {
		t.Errorf("apply(plugin = …) is Project.apply, not the plugin's own apply(target)")
	}
}

func TestKtRecursion_OwnNamedArgumentStillFlagged(t *testing.T) {
	ff := extractAST(t, `
class Walker {
    fun walk(depth: Int) {
        if (depth > 0) walk(depth = depth - 1)
    }
    fun count(n: Int): Int = if (n == 0) 0 else count(n - 1) + 1
}
`, false)
	for _, name := range []string{"Walker.walk", "Walker.count"} {
		if !recursiveBySuffix(t, ff, name) {
			t.Errorf("%s calls itself and is not flagged recursive_self", name)
		}
	}
}
