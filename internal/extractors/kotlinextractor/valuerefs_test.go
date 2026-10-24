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
