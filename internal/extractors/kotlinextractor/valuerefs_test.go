package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A class, object or enum used as a value or in a type test is named by the code
// that uses it, also when imported by its nested name; a name the repository does
// not declare, a function of a type's name, and a member called through its type,
// add nothing.
func TestKotlinTypeUsedAsAValueIsNamed(t *testing.T) {
	const dir = "lib/src/main/kotlin/com/acme"
	ff := extractKotlinRepo(t, map[string]string{
		dir + "/Effect.kt": `package com.acme

sealed interface Effect {
    object Shutdown : Effect
    class Close(val code: Int) : Effect
}

enum class Level { NONE, BODY }

object Defaults

object Analytics {
    fun log() {}
}

fun String(vararg codePoints: Int): String = ""
`,
		dir + "/Server.kt": `package com.acme

import com.acme.Effect.Shutdown
import java.util.concurrent.TimeUnit

class Server {
    var level = Level.NONE

    fun handle(e: Effect) {
        when (e) {
            Shutdown -> stop()
            is Effect.Close -> stop()
        }
        level = Level.BODY
        configure(Defaults, TimeUnit.SECONDS)
        configure(String.CASE_INSENSITIVE_ORDER, 0)
        Analytics.log()
    }

    fun stop() {}
    fun configure(d: Any, u: Any) {}
}
`,
	})
	handle := ktSym(t, ff, "Server.handle")
	for _, want := range []string{dir + ".Effect.Shutdown", dir + ".Effect.Close", dir + ".Level", dir + ".Defaults"} {
		if !handle.HasRelation(facts.RelNames, want) {
			t.Errorf("handle relations = %v, want names %s", handle.Relations, want)
		}
	}
	for _, r := range handle.Relations {
		if r.Kind == facts.RelNames && (r.Target == dir+".Analytics" || r.Target == dir+".String" || r.Target == "TimeUnit") {
			t.Errorf("handle names %s, which it calls or does not declare", r.Target)
		}
	}
	for _, f := range ff {
		if _, kept := f.Prop(propValueRefs); kept {
			t.Errorf("%s: the walker's note leaked into the facts", f.Name)
		}
	}
}

// A body-less annotation class under annotations parses as an expression; it is
// declared all the same, and what its annotations name, a class literal included,
// is referenced by it. An annotation on a function names its argument types too.
func TestKotlinAnnotationClassAndArguments(t *testing.T) {
	const dir = "lib/src/main/kotlin/com/acme"
	ff := extractKotlinRepo(t, map[string]string{
		dir + "/StartStop.kt": `package com.acme

import org.junit.jupiter.api.extension.ExtendWith

@Target(AnnotationTarget.FIELD)
@Retention(AnnotationRetention.RUNTIME)
@ExtendWith(StartStopExtension::class)
annotation class StartStop

class StartStopExtension

class Runner

class Suite {
    @RunWith(Runner::class)
    fun run() {}
}
`,
	})
	ann := ktSym(t, ff, "com/acme.StartStop")
	if ann.PropString("symbol_kind") != facts.SymbolClass {
		t.Errorf("StartStop symbol_kind = %v, want class", ann.PropString("symbol_kind"))
	}
	if !ann.HasRelation(facts.RelNames, dir+".StartStopExtension") {
		t.Errorf("StartStop relations = %v, want names StartStopExtension", ann.Relations)
	}
	if run := ktSym(t, ff, "Suite.run"); !run.HasRelation(facts.RelNames, dir+".Runner") {
		t.Errorf("run relations = %v, want names Runner", run.Relations)
	}
}

// A class literal names its type, never a member called `class`, and what is read
// off it (`.java`, `.simpleName`) is reflection, not a member of the repository. A
// call on a class literal (a KClass extension) and an ordinary property read still
// reference their member by name.
func TestKotlinClassLiteralIsNoCall(t *testing.T) {
	ff := extractAST(t, `package pkg
class A {
    fun f(user: User) {
        val m = ctx.getSystemService(Manager::class.java)
        val c = Foo::class
        val n = Foo::class.simpleName
        val u = user.name
        val d = Foo::class.delegatedData()
    }
}
`, false)
	f, ok := findFact(ff, "pkg.A.f")
	if !ok {
		t.Fatal("no pkg.A.f")
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && (r.Target == "class" || r.Target == "java" || r.Target == "simpleName") {
			t.Errorf("f calls %q, a class literal's reflection", r.Target)
		}
	}
	for _, want := range []string{"getSystemService", "name", "delegatedData"} {
		if !f.HasRelation(facts.RelCalls, want) {
			t.Errorf("f relations = %v, want %s kept", f.Relations, want)
		}
	}
}

// An object used as a value inside this grammar's expressions is named: an operand
// of a comparison, an elvis or a `to` pair, the receiver of a call to a member it
// does not declare (`Key.takeIf {}`), and the type argument of a generic call the
// grammar misreads as two comparisons (`entry<Key> {}`). A member called through
// its type is already a call edge, and adds no names edge.
func TestKotlinObjectUsedAsAValueInAnExpressionIsNamed(t *testing.T) {
	const dir = "lib/src/main/kotlin/com/acme"
	ff := extractKotlinRepo(t, map[string]string{
		dir + "/Keys.kt": `package com.acme

object HomeKey
object SearchKey
object FallbackKey
object BookmarksKey
object ProfileKey
object Flags {
    fun on() = true
}
`,
		dir + "/Nav.kt": `package com.acme

val TOP = mapOf(HomeKey to 1)

class Nav {
    fun current(x: Any) = x == SearchKey
    fun orDefault(x: Any?) = x ?: FallbackKey
    fun unread() = BookmarksKey.takeIf { true }
    fun route() {
        entry<ProfileKey> { }
    }
    fun check() = Flags.on()
    fun entry(a: Any) {}
}
`,
	})
	for sym, want := range map[string]string{
		"Nav.current":   dir + ".SearchKey",
		"Nav.orDefault": dir + ".FallbackKey",
		"Nav.unread":    dir + ".BookmarksKey",
		"Nav.route":     dir + ".ProfileKey",
		"acme.TOP":      dir + ".HomeKey",
	} {
		if f := ktSym(t, ff, sym); !f.HasRelation(facts.RelNames, want) {
			t.Errorf("%s relations = %v, want names %s", sym, f.Relations, want)
		}
	}
	if f := ktSym(t, ff, "Nav.check"); f.HasRelation(facts.RelNames, dir+".Flags") {
		t.Errorf("check names Flags, whose member it calls: %v", f.Relations)
	}
	for _, f := range ff {
		if _, kept := f.Prop(propReceiverRefs); kept {
			t.Errorf("%s: the walker's note leaked into the facts", f.Name)
		}
	}
}
