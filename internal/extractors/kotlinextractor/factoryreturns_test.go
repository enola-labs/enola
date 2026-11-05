package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestKotlinCallOnAFactorysResultIsAnEdge(t *testing.T) {
	ff := extractKotlinRepo(t, map[string]string{
		"src/main/kotlin/com/acme/engine/Engine.kt": `package com.acme.engine

class Engine {
    fun run(): Int = step()
    fun step(): Int = 1
    fun session(): Session = Session()
}

class Session {
    fun commit() {}
}

class Client {
    fun newBuilder(): Builder = Builder()
    fun call() {}

    class Builder {
        fun timeout(ms: Int) = apply { }
        fun self() = this
        fun build(): Client = Client()
    }
}
`,
		"src/main/kotlin/com/acme/engine/Factory.kt": `package com.acme.engine

fun makeEngine(): Engine = Engine()

fun inferred() = Engine()

suspend fun nullable(): Engine? = null

object Registry {
    fun default(): Engine = Engine()
}

class Pool {
    companion object {
        fun create(): Engine = Engine()
    }
}
`,
		"src/main/kotlin/com/acme/api/Api.kt": `package com.acme.api

import com.acme.engine.Client
import com.acme.engine.Engine
import com.acme.engine.Pool
import com.acme.engine.Registry
import com.acme.engine.inferred
import com.acme.engine.makeEngine
import com.acme.engine.nullable

class Api {
    fun imported(): Int { val e = makeEngine(); return e.run() }
    fun fromInferred(): Int { val e = inferred(); return e.run() }
    suspend fun fromNullable(): Int? { val e = nullable(); return e?.run() }
    fun fromObject(): Int { val e = Registry.default(); return e.run() }
    fun fromCompanion(): Int { val e = Pool.create(); return e.run() }
    fun chained() { makeEngine().session().commit() }
    fun builder(c: Client) { val d = c.newBuilder().timeout(5).self().build(); d.call() }
    fun nested() { val d = Client.Builder().timeout(1).build(); d.call() }
    private fun local(): Engine = makeEngine()
    fun sameClass(): Int { val e = local(); return e.run() }
}
`,
	})

	run := "com/acme/engine.Engine.run"
	for _, name := range []string{"Api.imported", "Api.fromInferred", "Api.fromNullable", "Api.fromObject", "Api.fromCompanion", "Api.sameClass"} {
		if f := ktSym(t, ff, name); !ktRelSuffix(f, facts.RelCalls, run) {
			t.Errorf("%s must call Engine.run; relations: %v", name, f.Relations)
		}
	}
	if f := ktSym(t, ff, "Api.chained"); !ktRelSuffix(f, facts.RelCalls, "com/acme/engine.Session.commit") {
		t.Errorf("a chain is typed step by step; relations: %v", f.Relations)
	}
	for _, name := range []string{"Api.builder", "Api.nested"} {
		if f := ktSym(t, ff, name); !ktRelSuffix(f, facts.RelCalls, "com/acme/engine.Client.call") {
			t.Errorf("%s: a builder returns its class from apply and this, and a nested name is the nested class; relations: %v", name, f.Relations)
		}
	}
	for _, f := range ff {
		if f.PropAny(propReturnType) != nil {
			t.Errorf("%s keeps %s, which must not leave the extractor", f.Name, propReturnType)
		}
	}
}
