package kotlinextractor

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func callsFrom(syms map[string]facts.Fact, name string) []string {
	var out []string
	for _, r := range syms[name].Relations {
		if r.Kind == facts.RelCalls {
			out = append(out, r.Target)
		}
	}
	slices.Sort(out)
	return out
}

// TestOperatorExtensionOnLibraryTypeIsCalled: `a - b` on a parameter of an
// imported library type calls the operator extension declared on that type, in
// the same package and through an import; an operand of another type, of unknown
// type, or a file that does not see the extension, calls nothing.
func TestOperatorExtensionOnLibraryTypeIsCalled(t *testing.T) {
	syms := operatorRepo(t, map[string]string{
		"src/main/kotlin/app/Time.kt": `package app

import kotlinx.datetime.LocalDateTime

operator fun LocalDateTime.minus(other: LocalDateTime): Long = 0

fun delay(at: LocalDateTime, now: LocalDateTime): Long = at - now
`,
		"src/main/kotlin/app/Other.kt": `package app

import other.lib.LocalDateTime

fun unrelated(a: LocalDateTime, b: LocalDateTime) = a - b

fun untyped(a: Int) = a - 1
`,
		"src/main/kotlin/feature/Uses.kt": `package feature

import kotlinx.datetime.LocalDateTime
import app.minus

fun imported(a: LocalDateTime, b: LocalDateTime) = a - b
`,
		"src/main/kotlin/hidden/Uses.kt": `package hidden

import kotlinx.datetime.LocalDateTime

fun notVisible(a: LocalDateTime, b: LocalDateTime) = a - b
`,
	})
	want := []string{"src/main/kotlin/app.minus"}
	for _, caller := range []string{"src/main/kotlin/app.delay", "src/main/kotlin/feature.imported"} {
		if got := callsFrom(syms, caller); !slices.Contains(got, want[0]) {
			t.Errorf("%s calls %v, want %v among them", caller, got, want)
		}
	}
	for _, caller := range []string{"src/main/kotlin/app.unrelated", "src/main/kotlin/app.untyped", "src/main/kotlin/hidden.notVisible"} {
		if got := callsFrom(syms, caller); slices.Contains(got, want[0]) {
			t.Errorf("%s calls %v, want no minus", caller, got)
		}
	}
}

// TestOperatorMemberConventions: indexing, `in`, compound assignment, unary minus
// and comparison call the member operators of a repository type, inherited ones
// included; a member that is not an operator is not called.
func TestOperatorMemberConventions(t *testing.T) {
	syms := operatorRepo(t, map[string]string{
		"src/main/kotlin/app/Grid.kt": `package app

open class Base {
    operator fun contains(x: Int): Boolean = true
}

class Grid : Base() {
    operator fun get(i: Int): Int = i
    operator fun set(i: Int, v: Int) {}
    operator fun plusAssign(o: Grid) {}
    operator fun unaryMinus(): Grid = this
    fun compareTo(o: Grid): Int = 0
}

class User(private val grid: Grid) {
    fun use(other: Grid) {
        val a = grid[1]
        grid[2] = 3
        val b = 4 in grid
        grid += other
        val c = -other
        val d = grid < other
    }
}
`,
	})
	got := callsFrom(syms, "src/main/kotlin/app.User.use")
	for _, w := range []string{"src/main/kotlin/app.Grid.get", "src/main/kotlin/app.Grid.set", "src/main/kotlin/app.Base.contains",
		"src/main/kotlin/app.Grid.plusAssign", "src/main/kotlin/app.Grid.unaryMinus"} {
		if !slices.Contains(got, w) {
			t.Errorf("use calls %v, missing %s", got, w)
		}
	}
	if slices.Contains(got, "src/main/kotlin/app.Grid.compareTo") {
		t.Errorf("use calls a compareTo that is no operator: %v", got)
	}
}

func operatorRepo(t *testing.T, files map[string]string) map[string]facts.Fact {
	t.Helper()
	dir, rel := writeKotlinRepo(t, files)
	all, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]facts.Fact{}
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			out[f.Name] = f
		}
	}
	return out
}
